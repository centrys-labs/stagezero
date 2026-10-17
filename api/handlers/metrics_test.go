package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/centrys-labs/stagezero/auth"
	"github.com/centrys-labs/stagezero/crypto"
	"github.com/centrys-labs/stagezero/db"
	"github.com/centrys-labs/stagezero/gen"
)

func TestImageCommit(t *testing.T) {
	tests := map[string]string{
		"ghcr.io/acme/api@sha256:0123456789abcdef0123456789abcdef": "0123456789ab",
		"ghcr.io/acme/api:a1b4f2c":                                 "a1b4f2c",
		"ghcr.io/acme/api:latest":                                  "",
		"ghcr.io/acme/api":                                         "",
	}
	for image, want := range tests {
		if got := imageCommit(image); got != want {
			t.Errorf("imageCommit(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestDeploymentMarkerDuration(t *testing.T) {
	created := time.Date(2026, time.August, 18, 10, 0, 0, 0, time.UTC)
	started := created.Add(15 * time.Second)
	completed := started.Add(105 * time.Second)
	marker := deploymentMarker(
		db.Service{ID: "svc_1", Name: "api"},
		db.Deployment{
			ID:          "dpl_1",
			Status:      "success",
			CreatedAt:   created,
			StartedAt:   started,
			CompletedAt: &completed,
		},
		"ghcr.io/acme/api:a1b4f2c",
		completed.Add(time.Minute),
	)
	if marker.DurationSeconds != 105 {
		t.Fatalf("duration = %d, want 105", marker.DurationSeconds)
	}
	if marker.Commit != "a1b4f2c" || marker.Status != "success" {
		t.Fatalf("unexpected marker metadata: %+v", marker)
	}

	inProgress := deploymentMarker(
		db.Service{ID: "svc_1", Name: "api"},
		db.Deployment{ID: "dpl_2", Status: "in_progress", CreatedAt: created, StartedAt: started},
		"ghcr.io/acme/api:latest",
		started.Add(30*time.Second),
	)
	if inProgress.DurationSeconds != 30 || inProgress.Status != "in_progress" {
		t.Fatalf("unexpected in-progress marker: %+v", inProgress)
	}
}

func TestProjectMetricsHistoryWindow(t *testing.T) {
	url := os.Getenv("STAGEZERO_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set STAGEZERO_TEST_DATABASE_URL to run historical metrics integration test")
	}
	if err := db.RunMigrations(url); err != nil {
		t.Fatal(err)
	}
	if err := crypto.Init(strings.Repeat("ab", 32)); err != nil {
		t.Fatal(err)
	}
	db.Init(url)
	t.Cleanup(db.Close)
	ctx := context.Background()
	prefix := fmt.Sprintf("history-%d", time.Now().UnixNano())
	user, workspace, project, env := prefix+"-user", prefix+"-workspace", prefix+"-project", prefix+"-env"
	server, key, service, longService := prefix+"-server", prefix+"-key", prefix+"-service", prefix+"-long"
	mustExec := func(query string, args ...interface{}) {
		t.Helper()
		if _, err := db.Pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'test')`, user, user+"@test.invalid")
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM deployment_logs WHERE deployment_id LIKE $1`,
			`DELETE FROM deployments WHERE id LIKE $1`,
			`DELETE FROM services WHERE id LIKE $1`,
			`DELETE FROM environments WHERE id LIKE $1`,
			`DELETE FROM projects WHERE id LIKE $1`,
			`DELETE FROM servers WHERE id LIKE $1`,
			`DELETE FROM ssh_keys WHERE id LIKE $1`,
			`DELETE FROM sessions WHERE token LIKE $1`,
			`DELETE FROM workspace_memberships WHERE id LIKE $1`,
			`DELETE FROM workspaces WHERE id LIKE $1`,
			`DELETE FROM users WHERE id LIKE $1`,
		} {
			if _, err := db.Pool.Exec(context.Background(), q, prefix+"%"); err != nil {
				t.Errorf("cleanup %s: %v", q, err)
			}
		}
	})
	mustExec(`INSERT INTO workspaces (id, name, owner_user_id) VALUES ($1, 'test', $2)`, workspace, user)
	mustExec(`INSERT INTO workspace_memberships (id, workspace_id, user_id, role) VALUES ($1, $2, $3, 'owner')`, prefix+"-membership", workspace, user)
	mustExec(`INSERT INTO projects (id, name, workspace_id) VALUES ($1, 'history', $2)`, project, workspace)
	mustExec(`INSERT INTO environments (id, name, project_id) VALUES ($1, 'production', $2)`, env, project)
	private, err := crypto.Encrypt("fake-private-key")
	if err != nil {
		t.Fatal(err)
	}
	mustExec(`INSERT INTO ssh_keys (id, name, private_key, workspace_id) VALUES ($1, 'test', $2, $3)`, key, private, workspace)
	mustExec(`INSERT INTO servers (id, name, host, ssh_user, ssh_key_id, workspace_id) VALUES ($1, 'test', '127.0.0.1', 'tester', $2, $3)`, server, key, workspace)
	for _, id := range []string{service, longService} {
		mustExec(`INSERT INTO services (id, name, image, container_name, container_port, server_id, workspace_id, project_id, environment_id) VALUES ($1, $1, 'test:latest', $1, 80, $2, $3, $4, $5)`, id, server, workspace, project, env)
	}
	if err := db.CreateSession(ctx, prefix+"-session", user, workspace, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	snapshot, err := crypto.Encrypt(fmt.Sprintf(`{"server_id":%q,"image":"example:abcdef0"}`, server))
	if err != nil {
		t.Fatal(err)
	}
	add := func(svc, name, status string, created time.Time) string {
		t.Helper()
		id := prefix + "-" + name
		mustExec(`INSERT INTO deployments (id, status, workspace_id, service_id, created_at, configuration_snapshot) VALUES ($1, $2, $3, $4, $5, $6)`, id, status, workspace, svc, created, snapshot)
		return id
	}
	now := time.Now().UTC().Truncate(time.Minute)
	from, to := now.Add(-7*24*time.Hour), now
	add(service, "expired", "success", from.Add(-20*24*time.Hour))
	add(service, "superseded", "success", from.Add(-10*24*time.Hour))
	active := add(service, "active-before-window", "success", from.Add(-24*time.Hour))
	add(service, "old-failed", "failed", from.Add(-12*time.Hour))
	atStart := add(service, "at-start", "success", from)
	for i := 0; i < 110; i++ {
		add(service, fmt.Sprintf("inside-%03d", i), "success", from.Add(time.Duration(i+1)*time.Minute))
	}
	inWindowFailure := add(service, "failed-inside", "failed", to.Add(-time.Hour))
	atEnd := add(service, "at-end", "success", to)
	add(service, "future", "success", to.Add(time.Hour))
	long := add(longService, "long-lived", "success", from.Add(-24*time.Hour))
	add(longService, "failed-while-long-running", "failed", to.Add(-time.Hour))
	rows, err := db.ListDeploymentsByServiceInWindow(ctx, service, from, to)
	if err != nil {
		t.Fatal(err)
	}
	// One overlapping predecessor, both inclusive endpoints, 110 in-window
	// successes, and a failed attempt as a marker; no expired or future rows.
	if len(rows) != 114 || rows[0].ID != active || rows[1].ID != atStart || rows[len(rows)-1].ID != atEnd {
		t.Fatalf("window returned %d deployments; first=%s last=%s", len(rows), firstID(rows), lastID(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].CreatedAt.Before(rows[i-1].CreatedAt) {
			t.Fatal("deployment history is not chronological")
		}
	}
	if rows[len(rows)-2].ID != inWindowFailure || !rows[1].CreatedAt.Equal(from) || !rows[len(rows)-1].CreatedAt.Equal(to) {
		t.Fatal("start/end boundary or failed marker missing")
	}
	longRows, err := db.ListDeploymentsByServiceInWindow(ctx, longService, from, to)
	if err != nil || len(longRows) != 2 || longRows[0].ID != long {
		t.Fatalf("failed attempt excluded long-lived active deployment: %v %+v", err, longRows)
	}
	if _, err := db.ListDeploymentsByServiceInWindow(ctx, service, to, from); err == nil {
		t.Fatal("accepted reversed window")
	}
	marker := deploymentMarker(db.Service{ID: service}, rows[1], "", to)
	if !marker.At.Equal(from) || marker.DeploymentId != atStart {
		t.Fatalf("marker lost real timestamp: %+v", marker)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/projects/"+project+"/metrics/history", nil)
	request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: prefix + "-session"})
	response := httptest.NewRecorder()
	auth.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(&Server{}).GetProjectMetricsHistory(w, r, project, gen.GetProjectMetricsHistoryParams{})
	})).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("history endpoint: %d %s", response.Code, response.Body.String())
	}
	var history gen.ProjectMetricsHistoryResponse
	if err := json.Unmarshal(response.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if len(history.DeploymentMarkers) < 114 {
		t.Fatalf("history endpoint dropped matching deployments: %d markers", len(history.DeploymentMarkers))
	}
	for i := 1; i < len(history.DeploymentMarkers); i++ {
		if history.DeploymentMarkers[i].At.Before(history.DeploymentMarkers[i-1].At) {
			t.Fatal("deployment markers are not chronological across services")
		}
	}
	var foundLong bool
	for _, svc := range history.Services {
		for _, deployment := range svc.Deployments {
			if deployment.DeploymentId == long {
				foundLong = true
			}
		}
	}
	if !foundLong {
		t.Fatal("history endpoint excluded samples from long-lived pre-window deployment")
	}
}

func firstID(deployments []db.Deployment) string {
	if len(deployments) == 0 {
		return ""
	}
	return deployments[0].ID
}

func lastID(deployments []db.Deployment) string {
	if len(deployments) == 0 {
		return ""
	}
	return deployments[len(deployments)-1].ID
}
