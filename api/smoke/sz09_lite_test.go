// Package smoke holds the SZ-09 Lite smoke test: the real API binary, a
// disposable PostgreSQL database, and a trusted public GitHub fixture.
//
// The test is skipped unless SZ09_LITE=1, so ordinary CI never reaches the
// network, a database, or Docker. The GitHub-hosted runner job that executes
// it lives in .github/workflows/sz09-lite.yml.
//
// What this proves and what it does not: it proves the import path end to end
// (analysis, branch/SHA resolution, project creation, idempotency, error
// handling, cancellation) and, when SZ09_LITE_BUILD=1, that the pinned
// Railpack plan builds with BuildKit. It does not deploy over SSH: the
// deployment path needs a separate disposable target host.
package smoke

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/centrys-labs/stagezero/source"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/crypto/ssh"
)

const apiBaseURL = "http://127.0.0.1:8080"

type analyzeRequest struct {
	RepoURL         string  `json:"repo_url"`
	Branch          *string `json:"branch,omitempty"`
	BranchConfirmed *bool   `json:"branch_confirmed,omitempty"`
}

type analyzeResponse struct {
	Owner           string            `json:"owner"`
	Name            string            `json:"name"`
	Branch          string            `json:"branch"`
	Provider        string            `json:"provider"`
	RuntimeVersions map[string]string `json:"runtime_versions"`
	Sha             string            `json:"sha"`
	SuggestedName   string            `json:"suggested_name"`
	SuggestedPort   int               `json:"suggested_port"`
}

type createRequest struct {
	IdempotencyKey  string  `json:"idempotency_key"`
	Name            string  `json:"name"`
	ContainerName   string  `json:"container_name"`
	ContainerPort   int     `json:"container_port"`
	RepoURL         string  `json:"repo_url"`
	Branch          *string `json:"branch,omitempty"`
	BranchConfirmed *bool   `json:"branch_confirmed,omitempty"`
	ServerID        string  `json:"server_id"`
}

type createResponse struct {
	Project struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"project"`
	Environment struct {
		ID string `json:"id"`
	} `json:"environment"`
	Service struct {
		ID            string `json:"id"`
		ContainerName string `json:"container_name"`
	} `json:"service"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// tenant is the disposable workspace the smoke test acts as.
type tenant struct {
	userID      string
	workspaceID string
	serverID    string
	token       string
}

func TestSZ09Lite(t *testing.T) {
	if os.Getenv("SZ09_LITE") != "1" {
		t.Skip("set SZ09_LITE=1 to run the SZ-09 Lite smoke test")
	}
	dsn := mustEnv(t, "STAGEZERO_TEST_DATABASE_URL")
	owner, name := mustFixture(t)
	branch := "main"
	if v := strings.TrimSpace(os.Getenv("SZ09_FIXTURE_BRANCH")); v != "" {
		branch = v
	}
	repoURL := "https://github.com/" + owner + "/" + name

	apiDir, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve api directory: %v", err)
	}
	work := t.TempDir()
	apiBin := filepath.Join(work, "stagezero-api")
	migrateBin := filepath.Join(work, "stagezero-migrate")
	buildGo(t, apiDir, apiBin, ".")
	buildGo(t, apiDir, migrateBin, "./cmd/migrate")

	// The combined migration gate has to pass on a fresh database before the
	// API is allowed to serve anything.
	if out, err := run(apiDir, migrateBin, []string{"up"}, "DATABASE_URL="+dsn); err != nil {
		t.Fatalf("migrate up failed: %v\n%s", err, out)
	}

	diagDir := strings.TrimSpace(os.Getenv("SZ09_DIAG_DIR"))
	if diagDir == "" {
		diagDir = work
	}
	if err := os.MkdirAll(diagDir, 0o700); err != nil {
		t.Fatalf("create diagnostics directory: %v", err)
	}
	apiTmp := filepath.Join(work, "api-tmp")
	if err := os.MkdirAll(apiTmp, 0o700); err != nil {
		t.Fatalf("create api temporary directory: %v", err)
	}

	encryptionKey := randomHex(32)
	logPath := filepath.Join(diagDir, "api.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create api log: %v", err)
	}
	redactor := &redactingWriter{w: logFile, secrets: []string{dsn, databasePassword(dsn), encryptionKey}}
	cmd := exec.Command(apiBin)
	cmd.Dir = apiDir
	cmd.Env = append(os.Environ(),
		"DATABASE_URL="+dsn,
		"ENCRYPTION_KEY="+encryptionKey,
		"COOKIE_SECURE=false",
		"TMPDIR="+apiTmp,
		// The smoke test runs several analyses in a row; the production rate
		// limit would answer 429 before the checks finish. One concurrent
		// analysis is kept on purpose: a slot leaked by a cancelled request
		// then shows up as a 429 in the follow-up check.
		"SOURCE_ANALYSIS_RATE_PER_MINUTE=200",
		"SOURCE_ANALYSIS_CONCURRENCY=1",
	)
	cmd.Stdout = redactor
	cmd.Stderr = redactor
	if err := cmd.Start(); err != nil {
		t.Fatalf("start api: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
		_ = logFile.Close()
		if t.Failed() {
			if content, err := os.ReadFile(logPath); err == nil {
				t.Logf("api log:\n%s", content)
			}
		}
	})

	client := newClient(t, "")
	waitReady(t, client)

	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	tn := seedTenant(t, conn)
	client = newClient(t, tn.token)

	var sha string
	t.Run("source-analysis", func(t *testing.T) {
		status, raw := doJSON(t, client, http.MethodPost, "/api/source/analyze", analyzeRequest{RepoURL: repoURL, Branch: &branch})
		if status != http.StatusOK {
			t.Fatalf("analyze status = %d, want 200: %s", status, raw)
		}
		var resp analyzeResponse
		decode(t, raw, &resp)
		if resp.Owner != owner || resp.Name != name {
			t.Errorf("analyze repo = %s/%s, want %s/%s", resp.Owner, resp.Name, owner, name)
		}
		if resp.Branch != branch {
			t.Errorf("analyze branch = %q, want %q", resp.Branch, branch)
		}
		if len(resp.Sha) != 40 {
			t.Errorf("analyze sha = %q, want 40 hex characters", resp.Sha)
		}
		if resp.Provider == "" {
			t.Error("analyze provider is empty; the pinned Railpack analysis did not run")
		}
		if want := expectedSHA(t, owner, name, branch); want != "" && resp.Sha != want {
			t.Errorf("analyze sha = %s, want %s", resp.Sha, want)
		}
		sha = resp.Sha

		if branch == "main" {
			status, raw := doJSON(t, client, http.MethodPost, "/api/source/analyze", analyzeRequest{RepoURL: repoURL})
			if status != http.StatusOK {
				t.Fatalf("analyze without branch status = %d, want 200: %s", status, raw)
			}
			var resp analyzeResponse
			decode(t, raw, &resp)
			if resp.Branch != "main" {
				t.Errorf("analyze without branch = %q, want the default branch main", resp.Branch)
			}
		}
	})

	t.Run("analysis-errors", func(t *testing.T) {
		missing := "sz09-lite-missing-branch"
		status, raw := doJSON(t, client, http.MethodPost, "/api/source/analyze", analyzeRequest{RepoURL: repoURL, Branch: &missing})
		if status != http.StatusBadRequest {
			t.Errorf("missing branch status = %d, want 400: %s", status, raw)
		} else if !strings.Contains(errorMessage(t, raw), "was not found") {
			t.Errorf("missing branch error = %q, want it to mention the branch was not found", errorMessage(t, raw))
		}

		missingRepo := owner + "/sz09-lite-missing-repository"
		status, raw = doJSON(t, client, http.MethodPost, "/api/source/analyze", analyzeRequest{RepoURL: "https://github.com/" + missingRepo})
		if status != http.StatusBadRequest {
			t.Errorf("missing repository status = %d, want 400: %s", status, raw)
		} else if !strings.Contains(errorMessage(t, raw), "could not be read") {
			t.Errorf("missing repository error = %q, want it to mention the repository could not be read", errorMessage(t, raw))
		}

		status, raw = doJSON(t, client, http.MethodPost, "/api/source/analyze", analyzeRequest{RepoURL: "https://example.com/not-github"})
		if status != http.StatusBadRequest {
			t.Errorf("non github url status = %d, want 400: %s", status, raw)
		}
	})

	var created createResponse
	var createdBody []byte
	key := uuid.NewString()
	t.Run("project-creation", func(t *testing.T) {
		before := projectCounts(t, conn, tn.workspaceID)
		body := createRequest{
			IdempotencyKey: key,
			Name:           "sz09-lite-service",
			ContainerName:  "sz09-lite-container",
			ContainerPort:  3000,
			RepoURL:        repoURL,
			Branch:         &branch,
			ServerID:       tn.serverID,
		}
		status, raw := doJSON(t, client, http.MethodPost, "/api/projects/from-source", body)
		if status != http.StatusCreated {
			t.Fatalf("create status = %d, want 201: %s", status, raw)
		}
		createdBody = raw
		decode(t, raw, &created)
		if created.Project.ID == "" || created.Environment.ID == "" || created.Service.ID == "" {
			t.Fatalf("create response is missing ids: %s", raw)
		}
		after := projectCounts(t, conn, tn.workspaceID)
		if after.projects != before.projects+1 || after.services != before.services+1 || after.sources != before.sources+1 {
			t.Errorf("create wrote %+v, want one project, service and source more than %+v", after, before)
		}
		var ownerName, repoName, sourceBranch string
		if err := conn.QueryRow(`SELECT owner, repo, branch FROM service_sources WHERE service_id = $1`, created.Service.ID).Scan(&ownerName, &repoName, &sourceBranch); err != nil {
			t.Fatalf("read service_sources: %v", err)
		}
		if ownerName != owner || repoName != name || sourceBranch != branch {
			t.Errorf("service_sources = %s/%s@%s, want %s/%s@%s", ownerName, repoName, sourceBranch, owner, name, branch)
		}
		var detected string
		if err := conn.QueryRow(`SELECT detected::text FROM service_sources WHERE service_id = $1`, created.Service.ID).Scan(&detected); err != nil {
			t.Fatalf("read detected source: %v", err)
		}
		if !strings.Contains(detected, "provider") {
			t.Errorf("detected source = %q, want the analysis result", detected)
		}
	})

	t.Run("idempotency-replay", func(t *testing.T) {
		body := createRequest{
			IdempotencyKey: key,
			Name:           "sz09-lite-service",
			ContainerName:  "sz09-lite-container",
			ContainerPort:  3000,
			RepoURL:        repoURL,
			Branch:         &branch,
			ServerID:       tn.serverID,
		}
		status, raw := doJSON(t, client, http.MethodPost, "/api/projects/from-source", body)
		if status != http.StatusCreated {
			t.Fatalf("replay status = %d, want 201: %s", status, raw)
		}
		if !bytes.Equal(raw, createdBody) {
			t.Errorf("replay body = %s, want the byte identical first response %s", raw, createdBody)
		}
		counts := projectCounts(t, conn, tn.workspaceID)
		if counts.projects != 1 || counts.services != 1 || counts.sources != 1 {
			t.Errorf("replay wrote %+v, want one project, service and source", counts)
		}
	})

	t.Run("idempotency-conflict", func(t *testing.T) {
		body := createRequest{
			IdempotencyKey: key,
			Name:           "sz09-lite-other-name",
			ContainerName:  "sz09-lite-container",
			ContainerPort:  3000,
			RepoURL:        repoURL,
			Branch:         &branch,
			ServerID:       tn.serverID,
		}
		status, raw := doJSON(t, client, http.MethodPost, "/api/projects/from-source", body)
		if status != http.StatusConflict {
			t.Fatalf("conflict status = %d, want 409: %s", status, raw)
		}
		if !strings.Contains(errorMessage(t, raw), "idempotency key") {
			t.Errorf("conflict error = %q, want it to mention the idempotency key", errorMessage(t, raw))
		}
	})

	t.Run("creation-is-atomic", func(t *testing.T) {
		before := projectCounts(t, conn, tn.workspaceID)
		failedKey := uuid.NewString()
		body := createRequest{
			IdempotencyKey: failedKey,
			Name:           "sz09-lite-never-created",
			ContainerName:  "sz09-lite-never",
			ContainerPort:  3000,
			RepoURL:        "https://github.com/" + owner + "/sz09-lite-missing-repository",
			Branch:         &branch,
			ServerID:       tn.serverID,
		}
		status, raw := doJSON(t, client, http.MethodPost, "/api/projects/from-source", body)
		if status != http.StatusBadRequest {
			t.Fatalf("failed create status = %d, want 400: %s", status, raw)
		}
		after := projectCounts(t, conn, tn.workspaceID)
		if after != before {
			t.Errorf("failed create left %+v behind, want %+v", after, before)
		}
		var stored int
		if err := conn.QueryRow(`SELECT count(*) FROM source_project_requests WHERE idempotency_key = $1`, failedKey).Scan(&stored); err != nil {
			t.Fatalf("count idempotency rows: %v", err)
		}
		if stored != 0 {
			t.Errorf("failed create stored %d idempotency rows, want 0", stored)
		}
	})

	t.Run("cancellation-and-cleanup", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBaseURL+"/api/source/analyze", bytes.NewReader([]byte(`{"repo_url":"`+repoURL+`"}`)))
		if err != nil {
			t.Fatalf("build cancel request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			t.Fatal("cancelled request returned a response; the analysis was not interrupted")
		}

		waitFor(t, 20*time.Second, func() bool {
			entries, err := os.ReadDir(apiTmp)
			if err != nil {
				return false
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "stagezero-source-") {
					return false
				}
			}
			return true
		}, "the cancelled analysis left a temporary source tree behind")

		// A leaked analysis slot would keep answering 429 here. The cancelled
		// request needs a moment to unwind, so the check waits for it.
		deadline := time.Now().Add(30 * time.Second)
		for {
			status, raw := doJSON(t, client, http.MethodPost, "/api/source/analyze", analyzeRequest{RepoURL: repoURL, Branch: &branch})
			if status == http.StatusOK {
				break
			}
			if status != http.StatusTooManyRequests {
				t.Fatalf("analyze after cancellation status = %d, want 200: %s", status, raw)
			}
			if time.Now().After(deadline) {
				t.Fatal("the cancelled analysis kept its slot; the follow-up analysis is still rate limited")
			}
			time.Sleep(time.Second)
		}
		waitReady(t, client)
	})

	if os.Getenv("SZ09_LITE_BUILD") == "1" {
		t.Run("railpack-build", func(t *testing.T) {
			if sha == "" {
				t.Skip("no resolved sha")
			}
			buildFixture(t, apiDir, owner, name, sha)
		})
	}
}

// buildFixture runs the pinned Railpack plan through BuildKit directly. It
// proves the build frontend accepts the plan; it does not deploy anything and
// it does not exercise the deployment path that runs on a target host.
func buildFixture(t *testing.T, apiDir, owner, name, sha string) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	frontend := frontendImage(t, apiDir)

	dir, cleanup, err := source.Fetch(ctx, source.Repo{Owner: owner, Name: name}, sha)
	if err != nil {
		t.Fatalf("fetch fixture: %v", err)
	}
	defer cleanup()

	plan, info, err := source.Prepare(ctx, dir, nil)
	if err != nil {
		t.Fatalf("railpack prepare: %v", err)
	}
	if info.Provider == "" {
		t.Error("railpack prepare returned no provider")
	}
	var document map[string]any
	if err := json.Unmarshal(plan.Raw, &document); err != nil {
		t.Fatalf("railpack plan is not JSON: %v", err)
	}

	planFile := filepath.Join(t.TempDir(), "railpack-plan.json")
	if err := os.WriteFile(planFile, plan.Raw, 0o600); err != nil {
		t.Fatalf("write railpack plan: %v", err)
	}
	image := "stagezero/sz09-lite:" + sha
	out, err := run("", "docker", []string{
		"buildx", "build",
		"--build-arg", "BUILDKIT_SYNTAX=" + frontend,
		"-f", planFile,
		"--build-arg", "cache-key=sz09-lite",
		"--output", "type=docker,name=" + image,
		dir,
	})
	if err != nil {
		t.Fatalf("buildx build with the pinned frontend %s failed: %v\n%s", frontend, err, out)
	}
	t.Cleanup(func() { _, _ = run("", "docker", []string{"rmi", "-f", image}) })
	if out, err := run("", "docker", []string{"image", "inspect", image}); err != nil {
		t.Fatalf("built image is missing: %v\n%s", err, out)
	}
}

// frontendImage reads the build frontend the deployment pins out of the
// deployment source, so the smoke test checks the image that is really used
// instead of a second copy of it.
func frontendImage(t *testing.T, apiDir string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(apiDir, "jobs", "deploy.go"))
	if err != nil {
		t.Fatalf("read the deployment source: %v", err)
	}
	var code []string
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code = append(code, line)
	}
	match := regexp.MustCompile(`ghcr\.io/railwayapp/railpack-frontend:[A-Za-z0-9._-]+`).FindString(strings.Join(code, "\n"))
	if match == "" {
		t.Fatal("the deployment source does not pin a railpack frontend image")
	}
	return match
}

type projectCount struct {
	projects int
	services int
	sources  int
}

func projectCounts(t *testing.T, conn *sql.DB, workspaceID string) projectCount {
	t.Helper()
	var counts projectCount
	if err := conn.QueryRow(`SELECT count(*) FROM projects WHERE workspace_id = $1`, workspaceID).Scan(&counts.projects); err != nil {
		t.Fatalf("count projects: %v", err)
	}
	if err := conn.QueryRow(`SELECT count(*) FROM services WHERE workspace_id = $1`, workspaceID).Scan(&counts.services); err != nil {
		t.Fatalf("count services: %v", err)
	}
	if err := conn.QueryRow(`SELECT count(*) FROM service_sources JOIN services ON services.id = service_sources.service_id WHERE services.workspace_id = $1`, workspaceID).Scan(&counts.sources); err != nil {
		t.Fatalf("count service sources: %v", err)
	}
	return counts
}

func seedTenant(t *testing.T, conn *sql.DB) tenant {
	t.Helper()
	tn := tenant{
		userID:      "smoke-user-" + randomHex(6),
		workspaceID: "smoke-workspace-" + randomHex(6),
		serverID:    "srv-smoke-" + randomHex(6),
		token:       randomHex(32),
	}
	keyID := "sk-" + randomHex(6)
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, $3)`, []any{tn.userID, tn.userID + "@example.invalid", "unused"}},
		{`INSERT INTO workspaces (id, name, owner_user_id) VALUES ($1, $2, $3)`, []any{tn.workspaceID, "smoke", tn.userID}},
		{`INSERT INTO workspace_memberships (id, workspace_id, user_id, role) VALUES ($1, $2, $3, 'owner')`, []any{"wm-" + randomHex(6), tn.workspaceID, tn.userID}},
		{`INSERT INTO ssh_keys (id, name, private_key, workspace_id) VALUES ($1, $2, $3, $4)`, []any{keyID, "smoke", disposableSSHKey(t), tn.workspaceID}},
		{`INSERT INTO servers (id, name, host, port, ssh_user, ssh_key_id, workspace_id) VALUES ($1, $2, '127.0.0.1', 22, 'root', $3, $4)`, []any{tn.serverID, "smoke-target", keyID, tn.workspaceID}},
		{`INSERT INTO sessions (token, user_id, workspace_id, expires_at) VALUES ($1, $2, $3, NOW() + INTERVAL '1 hour')`, []any{tn.token, tn.userID, tn.workspaceID}},
	}
	for _, statement := range statements {
		if _, err := conn.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("seed %q: %v", statement.query, err)
		}
	}
	return tn
}

// disposableSSHKey generates a throwaway key so the repository holds no
// private key material at all.
func disposableSSHKey(t *testing.T) string {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ssh key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(private, "sz09-lite")
	if err != nil {
		t.Fatalf("marshal ssh key: %v", err)
	}
	return string(pem.EncodeToMemory(block))
}

func newClient(t *testing.T, token string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("create cookie jar: %v", err)
	}
	if token != "" {
		base, err := url.Parse(apiBaseURL)
		if err != nil {
			t.Fatalf("parse base url: %v", err)
		}
		jar.SetCookies(base, []*http.Cookie{{Name: "session", Value: token, Path: "/"}})
	}
	return &http.Client{Jar: jar, Timeout: 5 * time.Minute}
}

func waitReady(t *testing.T, client *http.Client) {
	t.Helper()
	waitFor(t, 60*time.Second, func() bool {
		resp, err := client.Get(apiBaseURL + "/readyz")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode == http.StatusOK
	}, "the api never became ready")
}

func doJSON(t *testing.T, client *http.Client, method, path string, body any) (int, []byte) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	req, err := http.NewRequest(method, apiBaseURL+path, bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, raw
}

func decode(t *testing.T, raw []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("decode response %s: %v", raw, err)
	}
}

func errorMessage(t *testing.T, raw []byte) string {
	t.Helper()
	var resp errorResponse
	decode(t, raw, &resp)
	return resp.Error
}

func mustEnv(t *testing.T, key string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		t.Fatalf("%s is required", key)
	}
	return value
}

func mustFixture(t *testing.T) (string, string) {
	t.Helper()
	fixture := mustEnv(t, "SZ09_FIXTURE_REPO")
	owner, name, found := strings.Cut(fixture, "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") {
		t.Fatalf("SZ09_FIXTURE_REPO must be owner/name, got %q", fixture)
	}
	return owner, name
}

// expectedSHA pins the commit the import has to resolve to. The operator can
// set SZ09_FIXTURE_SHA; otherwise the remote ref is read directly.
func expectedSHA(t *testing.T, owner, name, branch string) string {
	t.Helper()
	if pinned := strings.TrimSpace(os.Getenv("SZ09_FIXTURE_SHA")); pinned != "" {
		return pinned
	}
	out, err := run("", "git", []string{"ls-remote", "https://github.com/" + owner + "/" + name, "refs/heads/" + branch})
	if err != nil {
		t.Fatalf("read remote ref: %v\n%s", err, out)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		t.Fatalf("remote ref refs/heads/%s of %s/%s not found", branch, owner, name)
	}
	return fields[0]
}

func buildGo(t *testing.T, dir, output, pkg string) {
	t.Helper()
	if out, err := run(dir, "go", []string{"build", "-o", output, pkg}); err != nil {
		t.Fatalf("go build %s failed: %v\n%s", pkg, err, out)
	}
}

func run(dir, name string, args []string, env ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal(message)
}

func randomHex(size int) string {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buffer)
}

func databasePassword(dsn string) string {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return ""
	}
	password, _ := parsed.User.Password()
	return password
}

// redactingWriter keeps disposable credentials out of the uploaded log.
type redactingWriter struct {
	w       io.Writer
	secrets []string
}

func (r *redactingWriter) Write(p []byte) (int, error) {
	text := string(p)
	for _, secret := range r.secrets {
		if secret == "" {
			continue
		}
		text = strings.ReplaceAll(text, secret, "[redacted]")
	}
	return r.w.Write([]byte(text))
}
