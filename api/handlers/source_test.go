package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/centrys-labs/stagezero/auth"
	"github.com/centrys-labs/stagezero/gen"
	"github.com/centrys-labs/stagezero/handlers"
	"github.com/centrys-labs/stagezero/source"
)

type fakeSourceAnalyzer struct {
	analyze func(context.Context, source.Repo) (source.Analysis, error)
}

func (f fakeSourceAnalyzer) Analyze(ctx context.Context, repo source.Repo) (source.Analysis, error) {
	return f.analyze(ctx, repo)
}

func TestAnalyzeSourceReturnsServerSideFacts(t *testing.T) {
	sha := strings.Repeat("a", 40)
	server := &handlers.Server{SourceAnalyzer: fakeSourceAnalyzer{analyze: func(_ context.Context, repo source.Repo) (source.Analysis, error) {
		if repo != (source.Repo{Owner: "owner", Name: "demo", Branch: "main"}) {
			t.Fatalf("repo = %+v", repo)
		}
		return source.Analysis{
			Repo: repo,
			SHA:  sha,
			Info: source.Info{Provider: "node", RuntimeVersions: map[string]string{"node": "22.11.0"}, StartCommand: "node server.js"},
		}, nil
	}}}

	req := authenticatedSourceRequest(`{"repo_url":"owner/demo"}`)
	rec := httptest.NewRecorder()
	server.AnalyzeSource(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got gen.AnalyzeSourceResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Owner != "owner" || got.Name != "demo" || got.Branch != "main" || got.Sha != sha || got.Provider != "node" || got.RuntimeVersions["node"] != "22.11.0" || got.SuggestedName != "demo" || got.SuggestedPort != 3000 || got.StartCommand == nil || *got.StartCommand != "node server.js" {
		t.Fatalf("response = %+v", got)
	}
}

func TestAnalyzeSourceRequiresConfirmedRefFromOriginalURL(t *testing.T) {
	var calls int
	server := &handlers.Server{SourceAnalyzer: fakeSourceAnalyzer{analyze: func(_ context.Context, repo source.Repo) (source.Analysis, error) {
		calls++
		if repo.Branch != "Feature/X" {
			t.Fatalf("branch = %q", repo.Branch)
		}
		return source.Analysis{Repo: repo, SHA: strings.Repeat("a", 40), Info: source.Info{Provider: "go"}}, nil
	}}}
	for _, body := range []string{
		`{"repo_url":"https://github.com/owner/demo/tree/main","branch":"main"}`,
		`{"repo_url":"https://github.com/owner/demo/tree/Feature/X/src","branch":"main","branch_confirmed":true}`,
		`{"repo_url":"https://github.com/owner/demo/blob/Feature/X/README.md","branch":"Feature/X","branch_confirmed":false}`,
	} {
		rec := httptest.NewRecorder()
		server.AnalyzeSource(rec, authenticatedSourceRequest(body))
		if rec.Code != http.StatusBadRequest || calls != 0 {
			t.Fatalf("unconfirmed branch: status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	server.AnalyzeSource(rec, authenticatedSourceRequest(`{"repo_url":"https://github.com/owner/demo/tree/Feature/X/src","branch":"Feature/X","branch_confirmed":true}`))
	if rec.Code != http.StatusOK || calls != 1 {
		t.Fatalf("confirmed branch: status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
	}
}

func TestCreateServiceFromSourceRejectsUnconfirmedRefBeforeDatabase(t *testing.T) {
	server := &handlers.Server{SourceAnalyzer: fakeSourceAnalyzer{analyze: func(context.Context, source.Repo) (source.Analysis, error) {
		t.Fatal("unconfirmed URL reached analyzer")
		return source.Analysis{}, nil
	}}}
	req := httptest.NewRequest(http.MethodPost, "/api/services/from-source", strings.NewReader(`{"name":"demo","container_name":"demo","container_port":3000,"repo_url":"https://github.com/owner/demo/tree/main","branch":"main"}`))
	req = req.WithContext(auth.WithSessionContext(req.Context(), auth.SessionContext{UserID: "source-test-user", WorkspaceRole: "owner"}))
	rec := httptest.NewRecorder()
	server.CreateServiceFromSource(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAnalyzeSourceRouteRequiresSession(t *testing.T) {
	server := &handlers.Server{SourceAnalyzer: fakeSourceAnalyzer{analyze: func(_ context.Context, repo source.Repo) (source.Analysis, error) {
		return source.Analysis{Repo: repo, SHA: strings.Repeat("b", 40), Info: source.Info{Provider: "go"}}, nil
	}}}
	mux := newTestMuxForServer(server)
	req := httptest.NewRequest(http.MethodPost, "/api/source/analyze", strings.NewReader(`{"repo_url":"owner/demo"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func newTestMuxForServer(server *handlers.Server) http.Handler {
	// Use the same spec-derived authentication decision as production.
	return gen.HandlerWithOptions(server, gen.StdHTTPServerOptions{
		BaseRouter:  http.NewServeMux(),
		Middlewares: []gen.MiddlewareFunc{testAuthMiddleware},
	})
}

func authenticatedSourceRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/source/analyze", strings.NewReader(body))
	return req.WithContext(auth.WithSessionContext(req.Context(), auth.SessionContext{UserID: "source-test-user"}))
}

func TestAnalyzeSourceMapsErrors(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		err    error
		status int
		want   string
	}{
		{"invalid URL", `{"repo_url":"https://gitlab.com/owner/demo"}`, nil, http.StatusBadRequest, "public github.com"},
		{"missing branch", `{"repo_url":"owner/demo","branch":"feature/missing"}`, source.ErrBranchNotFound, http.StatusBadRequest, "feature/missing"},
		{"GitHub unavailable", `{"repo_url":"owner/demo"}`, source.ErrRemoteUnavailable, http.StatusBadGateway, "could not reach GitHub"},
		{"unsupported", `{"repo_url":"owner/demo"}`, source.ErrUnsupportedSource, http.StatusUnprocessableEntity, "could not determine"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &handlers.Server{SourceAnalyzer: fakeSourceAnalyzer{analyze: func(_ context.Context, repo source.Repo) (source.Analysis, error) {
				if errors.Is(tt.err, source.ErrBranchNotFound) && repo.Branch != "feature/missing" {
					t.Fatalf("repo branch = %q", repo.Branch)
				}
				return source.Analysis{}, tt.err
			}}}
			req := authenticatedSourceRequest(tt.body)
			rec := httptest.NewRecorder()
			server.AnalyzeSource(rec, req)
			if rec.Code != tt.status || !strings.Contains(rec.Body.String(), tt.want) {
				t.Fatalf("status = %d, body = %s; want %d containing %q", rec.Code, rec.Body.String(), tt.status, tt.want)
			}
		})
	}
}

func TestAnalyzeSourceTimeoutIsExplicit(t *testing.T) {
	server := &handlers.Server{SourceAnalyzer: fakeSourceAnalyzer{analyze: func(ctx context.Context, _ source.Repo) (source.Analysis, error) {
		<-ctx.Done()
		return source.Analysis{}, ctx.Err()
	}}}
	// The production timeout is deliberately long; canceling the request still
	// exercises the same explicit deadline response path without waiting 90s.
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	req := authenticatedSourceRequest(`{"repo_url":"owner/demo"}`).WithContext(auth.WithSessionContext(ctx, auth.SessionContext{UserID: "source-test-user"}))
	rec := httptest.NewRecorder()
	server.AnalyzeSource(rec, req)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "90 second") {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestAnalyzeSourceRejectsLargeBody(t *testing.T) {
	server := &handlers.Server{SourceAnalyzer: fakeSourceAnalyzer{analyze: func(context.Context, source.Repo) (source.Analysis, error) {
		t.Fatal("oversized request reached analyzer")
		return source.Analysis{}, nil
	}}}
	rec := httptest.NewRecorder()
	server.AnalyzeSource(rec, authenticatedSourceRequest(`{"repo_url":"owner/demo","padding":"`+strings.Repeat("x", 17*1024)+`"}`))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestAnalyzeSourceRateAndConcurrency(t *testing.T) {
	limits := source.DefaultAnalysisLimits()
	limits.RatePerMinute, limits.Concurrency = 2, 1
	started, finish := make(chan struct{}), make(chan struct{})
	server := &handlers.Server{SourceLimits: limits, SourceAnalyzer: fakeSourceAnalyzer{analyze: func(_ context.Context, repo source.Repo) (source.Analysis, error) {
		started <- struct{}{}
		<-finish
		return source.Analysis{Repo: repo, Info: source.Info{Provider: "go"}}, nil
	}}}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		server.AnalyzeSource(rec, authenticatedSourceRequest(`{"repo_url":"owner/demo"}`))
		done <- rec
	}()
	<-started
	request := func(user string) *httptest.ResponseRecorder {
		req := authenticatedSourceRequest(`{"repo_url":"owner/demo"}`)
		req = req.WithContext(auth.WithSessionContext(req.Context(), auth.SessionContext{UserID: user}))
		rec := httptest.NewRecorder()
		server.AnalyzeSource(rec, req)
		return rec
	}
	if rec := request("other-user"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("global concurrency status = %d", rec.Code)
	}
	if rec := request("source-test-user"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("concurrent user status = %d", rec.Code)
	}
	close(finish)
	if rec := <-done; rec.Code != http.StatusOK {
		t.Fatalf("first request status = %d", rec.Code)
	}
	if rec := request("source-test-user"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("per-user rate status = %d", rec.Code)
	}
}

func TestAnalyzeSourceConfigurableDeadline(t *testing.T) {
	limits := source.DefaultAnalysisLimits()
	limits.Timeout = 5 * time.Millisecond
	server := &handlers.Server{SourceLimits: limits, SourceAnalyzer: fakeSourceAnalyzer{analyze: func(ctx context.Context, _ source.Repo) (source.Analysis, error) {
		<-ctx.Done()
		return source.Analysis{}, ctx.Err()
	}}}
	rec := httptest.NewRecorder()
	server.AnalyzeSource(rec, authenticatedSourceRequest(`{"repo_url":"owner/demo"}`))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestAnalyzeSourceTimesOutStalledRequestBody(t *testing.T) {
	limits := source.DefaultAnalysisLimits()
	limits.Timeout = 30 * time.Millisecond
	server := &handlers.Server{SourceLimits: limits, SourceAnalyzer: fakeSourceAnalyzer{analyze: func(context.Context, source.Repo) (source.Analysis, error) {
		t.Fatal("stalled body reached analyzer")
		return source.Analysis{}, nil
	}}}
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server.AnalyzeSource(w, r.WithContext(auth.WithSessionContext(r.Context(), auth.SessionContext{UserID: "u"})))
	}))
	defer httpServer.Close()
	reader, writer := io.Pipe()
	defer writer.Close()
	req, err := http.NewRequest(http.MethodPost, httpServer.URL+"/api/source/analyze", reader)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: time.Second}
	go func() { _, _ = writer.Write([]byte(`{"repo_url":"`)) }()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}
