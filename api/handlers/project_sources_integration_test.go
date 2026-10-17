package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/centrys-labs/stagezero/auth"
	"github.com/centrys-labs/stagezero/db"
	"github.com/centrys-labs/stagezero/gen"
	"github.com/centrys-labs/stagezero/handlers"
	"github.com/centrys-labs/stagezero/source"
)

// STAGEZERO_TEST_DATABASE_URL must point to a disposable PostgreSQL database.
func TestSourceProjectAtomicReplayAndAuthorization(t *testing.T) {
	url := os.Getenv("STAGEZERO_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set STAGEZERO_TEST_DATABASE_URL to run source-project integration tests")
	}
	if err := db.RunMigrations(url); err != nil {
		t.Fatal(err)
	}
	db.Init(url)
	t.Cleanup(db.Close)
	ctx := context.Background()
	suffix := fmt.Sprintf("sz08-%d", time.Now().UnixNano())
	owner, other, workspace, otherWorkspace := suffix+"-owner", suffix+"-other", suffix+"-workspace", suffix+"-other-workspace"
	for _, user := range []string{owner, other} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'test')`, user, user+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	for _, pair := range [][2]string{{workspace, owner}, {otherWorkspace, other}} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO workspaces (id,name,owner_user_id) VALUES ($1,$1,$2)`, pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO workspace_memberships (id,workspace_id,user_id,role) VALUES ($1,$2,$3,'owner')`, "member-"+pair[0], pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO sessions (token,user_id,workspace_id,expires_at) VALUES ($1,$2,$3,NOW() + INTERVAL '1 hour')`, pair[1]+"-session", pair[1], pair[0]); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO ssh_keys (id,name,private_key,workspace_id) VALUES ($1,'key','encrypted',$2)`, "key-"+pair[0], pair[0]); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool.Exec(ctx, `INSERT INTO servers (id,name,host,ssh_user,ssh_key_id,workspace_id) VALUES ($1,'server','127.0.0.1','root',$2,$3)`, "server-"+pair[0], "key-"+pair[0], pair[0]); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, query := range []string{
			`DELETE FROM source_project_requests WHERE workspace_id IN ($1,$2)`,
			`DELETE FROM service_sources WHERE service_id IN (SELECT id FROM services WHERE workspace_id IN ($1,$2))`,
			`DELETE FROM services WHERE workspace_id IN ($1,$2)`,
			`DELETE FROM environments WHERE project_id IN (SELECT id FROM projects WHERE workspace_id IN ($1,$2))`,
			`DELETE FROM projects WHERE workspace_id IN ($1,$2)`,
			`DELETE FROM servers WHERE workspace_id IN ($1,$2)`,
			`DELETE FROM ssh_keys WHERE workspace_id IN ($1,$2)`,
			`DELETE FROM sessions WHERE workspace_id IN ($1,$2)`,
			`DELETE FROM workspace_memberships WHERE workspace_id IN ($1,$2)`,
			`DELETE FROM workspaces WHERE id IN ($1,$2)`,
			`DELETE FROM users WHERE id IN ($3,$4)`,
		} {
			first, second := workspace, otherWorkspace
			if strings.HasPrefix(query, `DELETE FROM users`) {
				first, second = owner, other
				query = `DELETE FROM users WHERE id IN ($1,$2)`
			}
			if _, err := db.Pool.Exec(context.Background(), query, first, second); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	})
	var calls atomic.Int32
	server := &handlers.Server{SourceAnalyzer: fakeSourceAnalyzer{analyze: func(_ context.Context, repo source.Repo) (source.Analysis, error) {
		calls.Add(1)
		return source.Analysis{Repo: repo, SHA: strings.Repeat("a", 40), Info: source.Info{Provider: "go", RuntimeVersions: map[string]string{"go": "1.25"}}}, nil
	}}}
	mux := gen.HandlerWithOptions(server, gen.StdHTTPServerOptions{BaseRouter: http.NewServeMux(), Middlewares: []gen.MiddlewareFunc{func(next http.Handler) http.Handler { return auth.RequireAuth(next) }}})
	call := func(user, method, path string, payload any) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		if user != "" {
			req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: user + "-session"})
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	assertStatus := func(rec *httptest.ResponseRecorder, want int) {
		t.Helper()
		if rec.Code != want {
			t.Fatalf("status = %d, want %d: %s", rec.Code, want, rec.Body.String())
		}
	}
	path := "/api/projects/from-source"
	body := map[string]any{"idempotency_key": "1cb4705d-4e44-467a-9eb5-fd2fd6910808", "name": "example", "container_name": "example-app", "container_port": 3000, "server_id": "server-" + workspace, "repo_url": "https://github.com/Owner/Repo/tree/Feature/Next", "branch": "Feature/Next", "branch_confirmed": true}
	assertStatus(call("", "POST", path, body), http.StatusUnauthorized)
	assertStatus(call(other, "POST", path, body), http.StatusBadRequest)
	if calls.Load() != 0 {
		t.Fatal("unauthorized requests reached analyzer")
	}
	missingConfirmation := make(map[string]any)
	for k, v := range body {
		missingConfirmation[k] = v
	}
	missingConfirmation["branch_confirmed"] = false
	assertStatus(call(owner, "POST", path, missingConfirmation), http.StatusBadRequest)
	first := call(owner, "POST", path, body)
	assertStatus(first, http.StatusCreated)
	var result gen.CreateProjectFromImageResponse
	if err := json.Unmarshal(first.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Service.Source == nil || result.Service.Source.Branch != "Feature/Next" || result.Service.ProjectId != result.Project.Id || result.Service.EnvironmentId != result.Environment.Id {
		t.Fatalf("inconsistent creation response: %+v", result)
	}
	replay := call(owner, "POST", path, body)
	assertStatus(replay, http.StatusCreated)
	if replay.Body.String() != first.Body.String() || calls.Load() != 1 {
		t.Fatalf("lost-response replay changed result or reanalyzed: %d calls", calls.Load())
	}
	changed := make(map[string]any)
	for k, v := range body {
		changed[k] = v
	}
	changed["name"] = "different"
	assertStatus(call(owner, "POST", path, changed), http.StatusConflict)
	if calls.Load() != 1 {
		t.Fatal("different-payload replay reached analyzer")
	}
	var projects int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM projects WHERE workspace_id = $1`, workspace).Scan(&projects); err != nil || projects != 1 {
		t.Fatalf("project count = %d, err = %v", projects, err)
	}
	// Existing container collision must roll the project, environment and idempotency row back.
	collision := make(map[string]any)
	for k, v := range body {
		collision[k] = v
	}
	collision["idempotency_key"] = "6a53f14c-d165-4a86-96f9-b2367b19b912"
	assertStatus(call(owner, "POST", path, collision), http.StatusConflict)
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM projects WHERE workspace_id = $1`, workspace).Scan(&projects); err != nil || projects != 1 {
		t.Fatalf("partial project after failed service: count=%d err=%v", projects, err)
	}
	var requests int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM source_project_requests WHERE workspace_id = $1`, workspace).Scan(&requests); err != nil || requests != 1 {
		t.Fatalf("committed failed idempotency row: count=%d err=%v", requests, err)
	}
	if _, err := db.CreateSourceProject(ctx, db.SourceProjectInput{
		WorkspaceID: workspace, Key: "8f2fd49e-e675-4ac4-9929-8a2a36202eea", Fingerprint: "test", Name: "broken",
		ContainerName: "serialize-error", ContainerPort: 3000, ServerID: "server-" + workspace,
		Owner: "Owner", Repo: "Repo", Branch: "main", Detected: json.RawMessage(`{}`),
	}, func(db.Project, db.Environment, db.Service, db.ServiceSource) ([]byte, error) {
		return nil, errors.New("serialize failed")
	}); err == nil {
		t.Fatal("serialized response failure committed")
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM projects WHERE workspace_id = $1`, workspace).Scan(&projects); err != nil || projects != 1 {
		t.Fatalf("project created despite response failure: count=%d err=%v", projects, err)
	}
	// An authorized workspace can reuse the same UUID without disclosing another workspace's snapshot.
	otherBody := make(map[string]any)
	for k, v := range body {
		otherBody[k] = v
	}
	otherBody["server_id"] = "server-" + otherWorkspace
	otherBody["container_name"] = "other-app"
	otherResult := call(other, "POST", path, otherBody)
	assertStatus(otherResult, http.StatusCreated)
	if strings.Contains(otherResult.Body.String(), result.Project.Id) {
		t.Fatal("cross-workspace idempotency replay")
	}
	concurrent := make(map[string]any)
	for k, v := range body {
		concurrent[k] = v
	}
	concurrent["container_name"] = "concurrent-app"
	concurrent["idempotency_key"] = "117bda45-5232-4cb3-a18e-0908ae5792c7"
	results := make(chan *httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- call(owner, "POST", path, concurrent)
		}()
	}
	wg.Wait()
	close(results)
	var concurrentBody string
	for rec := range results {
		assertStatus(rec, http.StatusCreated)
		if concurrentBody != "" && concurrentBody != rec.Body.String() {
			t.Fatal("concurrent same-key requests returned different results")
		}
		concurrentBody = rec.Body.String()
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM projects WHERE workspace_id = $1`, workspace).Scan(&projects); err != nil || projects != 2 {
		t.Fatalf("concurrent duplicate projects: count=%d err=%v", projects, err)
	}
	// Source images cannot be changed through the ordinary service update API.
	update := map[string]any{"name": "example", "image": "attacker/image:latest", "container_name": "example-app", "container_port": 3000, "server_id": "server-" + workspace}
	assertStatus(call(owner, "PUT", "/api/services/"+result.Service.Id, update), http.StatusConflict)
	stored, err := db.GetServiceByID(ctx, result.Service.Id)
	if err != nil || stored.Image != result.Service.Image {
		t.Fatalf("source image changed after rejected override: service=%+v err=%v", stored, err)
	}
	imageService, err := db.CreateService(ctx, "image", "alpine:3", "image-app", 3000, nil, "server-"+workspace, workspace, "application", result.Project.Id, result.Environment.Id)
	if err != nil {
		t.Fatal(err)
	}
	update["name"], update["image"], update["container_name"] = "image", "alpine:4", "image-app"
	assertStatus(call(owner, "PUT", "/api/services/"+imageService.ID, update), http.StatusOK)
	stored, err = db.GetServiceByID(ctx, imageService.ID)
	if err != nil || stored.Image != "alpine:4" {
		t.Fatalf("image-based service cannot edit image: service=%+v err=%v", stored, err)
	}

	// The new endpoint and AnalyzeSource use the same per-user rate bucket.
	limits := source.DefaultAnalysisLimits()
	limits.RatePerMinute = 1
	limited := &handlers.Server{SourceLimits: limits, SourceAnalyzer: fakeSourceAnalyzer{analyze: func(_ context.Context, repo source.Repo) (source.Analysis, error) {
		return source.Analysis{Repo: repo, SHA: strings.Repeat("a", 40), Info: source.Info{Provider: "go"}}, nil
	}}}
	limitedMux := gen.HandlerWithOptions(limited, gen.StdHTTPServerOptions{BaseRouter: http.NewServeMux(), Middlewares: []gen.MiddlewareFunc{func(next http.Handler) http.Handler { return auth.RequireAuth(next) }}})
	limitedCall := func(path string, payload any) *httptest.ResponseRecorder {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", path, bytes.NewReader(encoded))
		req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: owner + "-session"})
		rec := httptest.NewRecorder()
		limitedMux.ServeHTTP(rec, req)
		return rec
	}
	assertStatus(limitedCall("/api/source/analyze", map[string]any{"repo_url": "owner/demo"}), http.StatusOK)
	newBody := make(map[string]any)
	for key, value := range body {
		newBody[key] = value
	}
	newBody["idempotency_key"] = "78e5a475-1889-4b19-82ef-6c529f02e492"
	newBody["container_name"] = "limited-app"
	assertStatus(limitedCall(path, newBody), http.StatusTooManyRequests)
	oversized := map[string]any{"padding": strings.Repeat("x", 17*1024)}
	assertStatus(limitedCall(path, oversized), http.StatusRequestEntityTooLarge)
}
