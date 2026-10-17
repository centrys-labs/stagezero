package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/centrys-labs/stagezero/auth"
	"github.com/centrys-labs/stagezero/db"
	"github.com/centrys-labs/stagezero/gen"
	"github.com/centrys-labs/stagezero/respond"
	"github.com/centrys-labs/stagezero/source"
	"github.com/jackc/pgx/v5"
)

var sourceProjectKey = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89aAbB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

func sourceProjectFingerprint(req gen.CreateProjectFromSourceRequest, repo source.Repo) (string, error) {
	raw := strings.TrimSpace(req.RepoUrl)
	if !strings.Contains(raw, "://") {
		if strings.HasPrefix(strings.ToLower(raw), "github.com/") {
			raw = "https://" + raw
		} else {
			raw = "https://github.com/" + raw
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	parts := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	canonical := "https://github.com/" + repo.Owner + "/" + repo.Name
	if len(parts) > 2 {
		canonical += "/" + strings.Join(parts[2:], "/")
	}
	// Encode a fixed struct, not a map: every creation-affecting input must be
	// included, while the confirmed flag is only a validation precondition.
	payload, err := json.Marshal(struct {
		URL, Branch, Name, ContainerName, ServerID string
		ContainerPort                              int
		HostPort                                   *int
	}{canonical, repo.Branch, strings.TrimSpace(req.Name), strings.TrimSpace(req.ContainerName), req.ServerId, req.ContainerPort, req.HostPort})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func (s *Server) CreateProjectFromSource(w http.ResponseWriter, r *http.Request) {
	sc, ok := auth.GetSessionContext(r)
	if !ok || sc.UserID == "" || sc.WorkspaceID == "" {
		respond.JSON(w, http.StatusUnauthorized, gen.ErrorResponse{Error: "authentication required"})
		return
	}
	if sc.WorkspaceRole != "owner" && sc.WorkspaceRole != "developer" {
		respond.JSON(w, http.StatusForbidden, gen.ErrorResponse{Error: "insufficient permissions"})
		return
	}
	limits := s.SourceLimits
	if limits.RequestBytes == 0 {
		limits = source.DefaultAnalysisLimits()
	}
	controller := http.NewResponseController(w)
	if err := controller.SetReadDeadline(time.Now().Add(limits.Timeout)); err == nil {
		defer controller.SetReadDeadline(time.Time{})
	}
	r.Body = http.MaxBytesReader(w, r.Body, limits.RequestBytes)
	decoder := json.NewDecoder(r.Body)
	var req gen.CreateProjectFromSourceRequest
	if err := decoder.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			respond.JSON(w, 413, gen.ErrorResponse{Error: "request body too large"})
		} else {
			respond.JSON(w, 400, gen.ErrorResponse{Error: "invalid request body"})
		}
		return
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			respond.JSON(w, 413, gen.ErrorResponse{Error: "request body too large"})
		} else {
			respond.JSON(w, 400, gen.ErrorResponse{Error: "invalid request body"})
		}
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.ContainerName = strings.TrimSpace(req.ContainerName)
	if !sourceProjectKey.MatchString(req.IdempotencyKey) || req.Name == "" || !validContainerName.MatchString(req.ContainerName) || validatePorts(req.ContainerPort, req.HostPort) != "" {
		respond.JSON(w, 400, gen.ErrorResponse{Error: "invalid idempotency key, service name, container name or ports"})
		return
	}
	repo, err := source.ParseRepoURLWithBranch(req.RepoUrl, req.Branch, req.BranchConfirmed != nil && *req.BranchConfirmed)
	if err != nil {
		respond.JSON(w, 400, gen.ErrorResponse{Error: "invalid repository URL or unconfirmed branch; select and confirm a matching branch"})
		return
	}
	fingerprint, err := sourceProjectFingerprint(req, repo)
	if err != nil {
		respond.JSON(w, 400, gen.ErrorResponse{Error: "invalid repository URL"})
		return
	}
	saved, err := db.SourceProjectResult(r.Context(), sc.WorkspaceID, req.IdempotencyKey, fingerprint)
	if err == nil {
		writeSourceProjectResponse(w, saved)
		return
	}
	if errors.Is(err, db.ErrIdempotencyConflict) {
		respond.JSON(w, 409, gen.ErrorResponse{Error: err.Error()})
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		respond.JSON(w, 500, gen.ErrorResponse{Error: "failed to read import state"})
		return
	}
	server, err := db.GetServerByID(r.Context(), req.ServerId)
	if err != nil || server.WorkspaceID != sc.WorkspaceID {
		respond.JSON(w, 400, gen.ErrorResponse{Error: "server not found"})
		return
	}
	ctx, release, limits, ok := s.enterSourceAnalysis(w, r, sc.UserID)
	if !ok {
		return
	}
	defer release()
	analysis, err := s.analyzeSource(ctx, repo)
	if err != nil {
		s.respondSourceAnalysisError(w, ctx, repo, limits.Timeout, err)
		return
	}
	detected, err := json.Marshal(analysis.Info)
	if err != nil {
		respond.JSON(w, 500, gen.ErrorResponse{Error: "failed to store source detection"})
		return
	}
	saved, err = db.CreateSourceProject(ctx, db.SourceProjectInput{
		WorkspaceID: sc.WorkspaceID, Key: req.IdempotencyKey, Fingerprint: fingerprint, Name: req.Name, ContainerName: req.ContainerName,
		ContainerPort: int32(req.ContainerPort), HostPort: int32PtrFromIntPtr(req.HostPort), ServerID: req.ServerId,
		Owner: repo.Owner, Repo: repo.Name, Branch: repo.Branch, Detected: detected,
	}, func(proj db.Project, env db.Environment, svc db.Service, src db.ServiceSource) ([]byte, error) {
		service := serviceToResponse(svc, 0)
		var err error
		service.Source, err = sourceResponsePtr(src)
		if err != nil {
			return nil, err
		}
		return json.Marshal(gen.CreateProjectFromImageResponse{Project: projectToResponse(proj), Environment: environmentToResponse(env), Service: service})
	})
	if err != nil {
		switch {
		case errors.Is(err, db.ErrIdempotencyConflict):
			respond.JSON(w, 409, gen.ErrorResponse{Error: err.Error()})
		case isUniqueViolation(err):
			respond.JSON(w, 409, gen.ErrorResponse{Error: "container_name already in use on this server"})
		case errors.Is(err, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
			s.respondSourceAnalysisError(w, ctx, repo, limits.Timeout, err)
		default:
			respond.JSON(w, 500, gen.ErrorResponse{Error: "failed to create project"})
		}
		return
	}
	writeSourceProjectResponse(w, saved)
}

func writeSourceProjectResponse(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(body)
}
