package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/centrys-labs/stagezero/auth"
	"github.com/centrys-labs/stagezero/crypto"
	"github.com/centrys-labs/stagezero/db"
	"github.com/centrys-labs/stagezero/gen"
)

// Use a disposable Postgres database: STAGEZERO_TEST_DATABASE_URL=postgres://...
func TestNotificationChannelLifecycleAndAuthorization(t *testing.T) {
	url := os.Getenv("STAGEZERO_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set STAGEZERO_TEST_DATABASE_URL for database-backed channel security tests")
	}
	if err := db.RunMigrations(url); err != nil {
		t.Fatal(err)
	}
	if err := crypto.Init(strings.Repeat("ab", 32)); err != nil {
		t.Fatal(err)
	}
	db.Init(url)
	t.Cleanup(db.Close)
	t.Cleanup(func() {
		for _, query := range []string{
			`DELETE FROM notification_channels WHERE workspace_id IN ('primary', 'other')`,
			`DELETE FROM sessions WHERE user_id IN ('owner', 'viewer', 'outsider')`,
			`DELETE FROM workspace_memberships WHERE user_id IN ('owner', 'viewer', 'outsider')`,
			`DELETE FROM workspaces WHERE id IN ('primary', 'other')`,
			`DELETE FROM users WHERE id IN ('owner', 'viewer', 'outsider')`,
		} {
			if _, err := db.Pool.Exec(context.Background(), query); err != nil {
				t.Errorf("test database cleanup: %v", err)
			}
		}
	})
	ctx := context.Background()
	for _, id := range []string{"owner", "viewer", "outsider"} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'not-used')`, id, id+"@test.invalid"); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"primary", "other"} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO workspaces (id, name, owner_user_id) VALUES ($1, $1, $2)`, id, map[string]string{"primary": "owner", "other": "outsider"}[id]); err != nil {
			t.Fatal(err)
		}
	}
	for _, who := range []struct{ id, workspace, role string }{
		{"owner", "primary", "owner"}, {"viewer", "primary", "viewer"}, {"outsider", "other", "owner"},
	} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO workspace_memberships (id, workspace_id, user_id, role) VALUES ($1, $2, $3, $4)`, "member-"+who.id, who.workspace, who.id, who.role); err != nil {
			t.Fatal(err)
		}
		if err := db.CreateSession(ctx, who.id+"-session", who.id, who.workspace, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	gen.HandlerWithOptions(&Server{}, gen.StdHTTPServerOptions{BaseRouter: mux, Middlewares: []gen.MiddlewareFunc{func(next http.Handler) http.Handler { return auth.RequireAuth(next) }}})
	call := func(who, method, path string, body interface{}) *httptest.ResponseRecorder {
		t.Helper()
		var input []byte
		if body != nil {
			var err error
			input, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(input))
		if who != "" {
			req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: who + "-session"})
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	assertStatus := func(want int, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != want {
			t.Fatalf("status %d, want %d: %s", rec.Code, want, rec.Body.String())
		}
	}
	secret := "secret-in-webhook-url"
	var delivered int
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, secret) {
			delivered++
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hook.Close()
	create := call("owner", "POST", "/api/notification-channels", map[string]interface{}{
		"name": "Team", "type": "discord", "config": map[string]interface{}{"url": hook.URL + "/" + secret},
	})
	assertStatus(http.StatusCreated, create)
	if strings.Contains(create.Body.String(), secret) || strings.Contains(create.Body.String(), "***") {
		t.Fatalf("create response leaked credential: %s", create.Body.String())
	}
	var channel gen.NotificationChannelResponse
	if err := json.Unmarshal(create.Body.Bytes(), &channel); err != nil {
		t.Fatal(err)
	}
	if len(channel.ConfiguredCredentials) != 1 || channel.ConfiguredCredentials[0] != "url" {
		t.Fatalf("configured credential metadata = %#v", channel.ConfiguredCredentials)
	}
	var encrypted string
	if err := db.Pool.QueryRow(ctx, `SELECT config_ciphertext FROM notification_channels WHERE id = $1`, channel.Id).Scan(&encrypted); err != nil || strings.Contains(encrypted, secret) {
		t.Fatalf("notification configuration not encrypted: %v", err)
	}
	path := "/api/notification-channels/" + channel.Id
	list := call("viewer", "GET", "/api/notification-channels", nil)
	assertStatus(http.StatusOK, list)
	if strings.Contains(list.Body.String(), secret) {
		t.Fatalf("list response leaked credential: %s", list.Body.String())
	}
	for _, who := range []string{"viewer", "outsider"} {
		for _, operation := range []struct {
			method, path string
			body         interface{}
		}{
			{"PUT", path, map[string]interface{}{"name": "changed", "type": "discord", "config": map[string]interface{}{}}},
			{"DELETE", path, nil}, {"POST", path + "/test", nil},
		} {
			status := http.StatusForbidden
			if who == "outsider" {
				status = http.StatusNotFound
			}
			assertStatus(status, call(who, operation.method, operation.path, operation.body))
		}
	}
	assertStatus(http.StatusForbidden, call("viewer", "POST", "/api/notification-channels", map[string]interface{}{}))
	assertStatus(http.StatusUnauthorized, call("", "GET", "/api/notification-channels", nil))
	assertStatus(http.StatusOK, call("outsider", "GET", "/api/notification-channels", nil))
	if got := call("outsider", "GET", "/api/notification-channels", nil).Body.String(); strings.Contains(got, "Team") {
		t.Fatalf("cross-workspace channel listed: %s", got)
	}
	update := func(config map[string]interface{}) *httptest.ResponseRecorder {
		return call("owner", "PUT", path, map[string]interface{}{"name": "Team", "type": "discord", "config": config})
	}
	assertStatus(http.StatusOK, update(map[string]interface{}{}))
	assertStatus(http.StatusNoContent, call("owner", "POST", path+"/test", nil))
	if delivered != 1 {
		t.Fatalf("preserved URL delivered %d notifications, want one", delivered)
	}
	assertStatus(http.StatusBadRequest, update(map[string]interface{}{"url": nil}))
	assertStatus(http.StatusBadRequest, update(map[string]interface{}{"url": ""}))
	assertStatus(http.StatusOK, update(map[string]interface{}{"url": hook.URL + "/replacement"}))
	stored, err := db.GetNotificationChannel(ctx, channel.Id)
	if err != nil || stored.Config["url"] != hook.URL+"/replacement" {
		t.Fatalf("replacement not stored: %v", err)
	}
	assertStatus(http.StatusNoContent, call("owner", "DELETE", path, nil))
	assertStatus(http.StatusNotFound, call("owner", "POST", path+"/test", nil))

	mail := call("owner", "POST", "/api/notification-channels", map[string]interface{}{
		"name": "Mail", "type": "email", "config": map[string]interface{}{"smtp_host": "mail.test", "from": "a@test.invalid", "to": "b@test.invalid", "password": "old-password", "username": "login"},
	})
	assertStatus(http.StatusCreated, mail)
	if strings.Contains(mail.Body.String(), "old-password") || strings.Contains(mail.Body.String(), "***") {
		t.Fatalf("mail response exposed credential: %s", mail.Body.String())
	}
	if err := json.Unmarshal(mail.Body.Bytes(), &channel); err != nil {
		t.Fatal(err)
	}
	path = "/api/notification-channels/" + channel.Id
	for _, patch := range []map[string]interface{}{{"password": "new-password"}, {"password": nil}} {
		rec := call("owner", "PUT", path, map[string]interface{}{"name": "Mail", "type": "email", "config": patch})
		assertStatus(http.StatusOK, rec)
		if strings.Contains(rec.Body.String(), "password\":\"new-password") || strings.Contains(rec.Body.String(), "***") {
			t.Fatalf("update leaked credential: %s", rec.Body.String())
		}
	}
	stored, err = db.GetNotificationChannel(ctx, channel.Id)
	if err != nil || stored.Config["password"] != nil || stored.Config["username"] != "login" {
		t.Fatalf("optional credential removal failed: %v, config=%#v", err, stored.Config)
	}
	var wg sync.WaitGroup
	for _, patch := range []map[string]interface{}{{"smtp_host": "other.test"}, {"password": "concurrent-secret"}} {
		wg.Add(1)
		go func(patch map[string]interface{}) {
			defer wg.Done()
			result := call("owner", "PUT", path, map[string]interface{}{"name": "Mail", "type": "email", "config": patch})
			if result.Code != http.StatusOK {
				t.Errorf("concurrent update: %d %s", result.Code, result.Body.String())
			}
		}(patch)
	}
	wg.Wait()
	stored, err = db.GetNotificationChannel(ctx, channel.Id)
	if err != nil || stored.Config["password"] != "concurrent-secret" || stored.Config["smtp_host"] != "other.test" {
		t.Fatalf("lost concurrent update: %v, config=%#v", err, stored.Config)
	}
	failing := call("owner", "POST", "/api/notification-channels", map[string]interface{}{
		"name": "Failure", "type": "telegram", "config": map[string]interface{}{
			"bot_token": "failure-secret", "chat_id": "123", "api_url": "http://127.0.0.1:1/failure-secret",
		},
	})
	assertStatus(http.StatusCreated, failing)
	var failingChannel gen.NotificationChannelResponse
	if err := json.Unmarshal(failing.Body.Bytes(), &failingChannel); err != nil {
		t.Fatal(err)
	}
	failureResponse := call("owner", "POST", "/api/notification-channels/"+failingChannel.Id+"/test", nil)
	assertStatus(http.StatusBadGateway, failureResponse)
	if strings.Contains(failureResponse.Body.String(), "failure-secret") {
		t.Fatalf("provider failure leaked secret: %s", failureResponse.Body.String())
	}
}
