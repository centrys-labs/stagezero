package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/centrys-labs/stagezero/auth"
	"github.com/centrys-labs/stagezero/gen"
	"github.com/centrys-labs/stagezero/respond"
	"github.com/centrys-labs/stagezero/source"
)

const sourceAnalysisTimeout = 90 * time.Second

// sourceResolveTimeout bounds the one piece of source work a request still
// does itself: turning a branch into the commit the deployment will build.
// It is a single git wire-protocol round trip, so it is short.
const sourceResolveTimeout = 15 * time.Second

func (s *Server) AnalyzeSource(w http.ResponseWriter, r *http.Request) {
	sc, ok := auth.GetSessionContext(r)
	if !ok || sc.UserID == "" {
		respond.JSON(w, http.StatusUnauthorized, gen.ErrorResponse{Error: "authentication required"})
		return
	}
	limits := s.SourceLimits
	if limits.RequestBytes == 0 {
		limits = source.DefaultAnalysisLimits()
	}
	s.sourceGateOnce.Do(func() { s.sourceGate = source.NewAnalysisGate(limits) })
	release, allowed := s.sourceGate.Enter(sc.UserID)
	if !allowed {
		respond.JSON(w, http.StatusTooManyRequests, gen.ErrorResponse{Error: "source analysis rate or concurrency limit reached; retry later"})
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), limits.Timeout)
	defer cancel()
	r = r.WithContext(ctx)
	// Request context cancellation does not interrupt a client that stalls in
	// the middle of sending JSON; bound socket reads too.
	controller := http.NewResponseController(w)
	if err := controller.SetReadDeadline(time.Now().Add(limits.Timeout)); err == nil {
		defer controller.SetReadDeadline(time.Time{})
	}
	r.Body = http.MaxBytesReader(w, r.Body, limits.RequestBytes)
	var req gen.AnalyzeSourceRequest
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&req); err != nil {
		if ctx.Err() != nil || errors.Is(err, os.ErrDeadlineExceeded) {
			s.respondSourceAnalysisError(w, ctx, source.Repo{}, limits.Timeout, err)
			return
		}
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			respond.JSON(w, http.StatusRequestEntityTooLarge, gen.ErrorResponse{Error: "request body too large"})
			return
		}
		respond.JSON(w, http.StatusBadRequest, gen.ErrorResponse{Error: "invalid request body"})
		return
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if ctx.Err() != nil || errors.Is(err, os.ErrDeadlineExceeded) {
			s.respondSourceAnalysisError(w, ctx, source.Repo{}, limits.Timeout, err)
			return
		}
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			respond.JSON(w, http.StatusRequestEntityTooLarge, gen.ErrorResponse{Error: "request body too large"})
		} else {
			respond.JSON(w, http.StatusBadRequest, gen.ErrorResponse{Error: "invalid request body"})
		}
		return
	}

	repo, err := source.ParseRepoURL(req.RepoUrl)
	if err != nil {
		respond.JSON(w, http.StatusBadRequest, gen.ErrorResponse{Error: "repo_url must be a public github.com repository"})
		return
	}
	repo.Branch = "main"
	if req.Branch != nil {
		repo.Branch = strings.TrimSpace(*req.Branch)
		if repo.Branch == "" {
			repo.Branch = "main"
		}
	}

	analysis, err := s.analyzeSource(ctx, repo)
	if err != nil {
		s.respondSourceAnalysisError(w, ctx, repo, limits.Timeout, err)
		return
	}

	resp := gen.AnalyzeSourceResponse{
		Owner:           analysis.Repo.Owner,
		Name:            analysis.Repo.Name,
		Branch:          analysis.Repo.Branch,
		Sha:             analysis.SHA,
		Provider:        analysis.Info.Provider,
		RuntimeVersions: analysis.Info.RuntimeVersions,
		SuggestedName:   source.SuggestedName(analysis.Repo.Name),
		SuggestedPort:   source.SuggestedPort(analysis.Info.Provider),
	}
	if analysis.Info.StartCommand != "" {
		startCommand := analysis.Info.StartCommand
		resp.StartCommand = &startCommand
	}
	respond.JSON(w, http.StatusOK, resp)
}

func (s *Server) analyzeSource(ctx context.Context, repo source.Repo) (source.Analysis, error) {
	analyzer := s.SourceAnalyzer
	if analyzer == nil {
		limits := s.SourceLimits
		if limits.RequestBytes == 0 {
			limits = source.DefaultAnalysisLimits()
		}
		analyzer = source.DefaultAnalyzer{Limits: limits}
	}
	return analyzer.Analyze(ctx, repo)
}

// respondSourceAnalysisError is shared by the two callers that read a
// repository, and they allow different amounts of time for it, so the limit is
// passed in rather than assumed: a deadline message that names the wrong number
// is worse than one that names none.
func (s *Server) respondSourceAnalysisError(w http.ResponseWriter, ctx context.Context, repo source.Repo, limit time.Duration, err error) {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		respond.JSON(w, http.StatusUnprocessableEntity, gen.ErrorResponse{Error: fmt.Sprintf("reading the repository exceeded the %d second limit", int(limit.Seconds()))})
	case errors.Is(err, source.ErrInvalidBranch):
		respond.JSON(w, http.StatusBadRequest, gen.ErrorResponse{Error: "invalid branch name"})
	case errors.Is(err, source.ErrRepoNotFound):
		respond.JSON(w, http.StatusBadRequest, gen.ErrorResponse{Error: fmt.Sprintf("%s/%s could not be read; it must exist and be public", repo.Owner, repo.Name)})
	case errors.Is(err, source.ErrBranchNotFound):
		respond.JSON(w, http.StatusBadRequest, gen.ErrorResponse{Error: fmt.Sprintf("branch %q was not found", repo.Branch)})
	case errors.Is(err, source.ErrSourceTooLarge):
		respond.JSON(w, http.StatusUnprocessableEntity, gen.ErrorResponse{Error: err.Error()})
	case errors.Is(err, source.ErrOutputTooLarge):
		respond.JSON(w, http.StatusUnprocessableEntity, gen.ErrorResponse{Error: err.Error()})
	case errors.Is(err, source.ErrUnsupportedSource):
		respond.JSON(w, http.StatusUnprocessableEntity, gen.ErrorResponse{Error: err.Error()})
	case errors.Is(err, source.ErrRemoteUnavailable):
		respond.JSON(w, http.StatusBadGateway, gen.ErrorResponse{Error: "could not reach GitHub"})
	default:
		// Railpack's diagnostic is actionable (for example, an unsupported
		// package manager), so return it without hiding the reason behind 500.
		respond.JSON(w, http.StatusUnprocessableEntity, gen.ErrorResponse{Error: err.Error()})
	}
}
