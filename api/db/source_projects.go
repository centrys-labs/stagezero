package db

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/centrys-labs/stagezero/db/sqlcgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrIdempotencyConflict = errors.New("idempotency key belongs to a different request")

type SourceProjectInput struct {
	WorkspaceID, Key, Fingerprint string
	Name, ContainerName           string
	ContainerPort                 int32
	HostPort                      *int32
	ServerID, Owner, Repo, Branch string
	Detected                      json.RawMessage
}

func SourceProjectResult(ctx context.Context, workspaceID, key, fingerprint string) ([]byte, error) {
	var existing, response string
	err := Pool.QueryRow(ctx, `SELECT fingerprint, response_json FROM source_project_requests WHERE workspace_id = $1 AND idempotency_key = $2`, workspaceID, key).Scan(&existing, &response)
	if err != nil {
		return nil, err
	}
	if existing != fingerprint {
		return nil, ErrIdempotencyConflict
	}
	return []byte(response), nil
}

// CreateSourceProject holds the workspace lock through the second idempotency
// check and the response insert. Network analysis must finish before calling it.
func CreateSourceProject(ctx context.Context, in SourceProjectInput, response func(Project, Environment, Service, ServiceSource) ([]byte, error)) ([]byte, error) {
	tx, err := Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	q := Queries.WithTx(tx)
	if err := q.LockWorkspaceProjectNames(ctx, in.WorkspaceID); err != nil {
		return nil, err
	}
	var oldHash, oldResponse string
	err = tx.QueryRow(ctx, `SELECT fingerprint, response_json FROM source_project_requests WHERE workspace_id = $1 AND idempotency_key = $2`, in.WorkspaceID, in.Key).Scan(&oldHash, &oldResponse)
	if err == nil {
		if oldHash != in.Fingerprint {
			return nil, ErrIdempotencyConflict
		}
		return []byte(oldResponse), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	name, err := generateUniqueProjectName(ctx, workspaceNameExists(q, in.WorkspaceID))
	if err != nil {
		return nil, err
	}
	pr, err := q.CreateProject(ctx, sqlcgen.CreateProjectParams{Name: name, WorkspaceID: in.WorkspaceID})
	if err != nil {
		return nil, err
	}
	er, err := q.CreateEnvironment(ctx, sqlcgen.CreateEnvironmentParams{Name: DefaultEnvironmentName, ProjectID: pr.ID})
	if err != nil {
		return nil, err
	}
	sr, err := q.CreateSourceService(ctx, sqlcgen.CreateSourceServiceParams{
		Name: in.Name, ContainerName: in.ContainerName, ContainerPort: in.ContainerPort,
		HostPort: pgInt4FromInt32Ptr(in.HostPort), ServerID: in.ServerID, WorkspaceID: in.WorkspaceID,
		Kind: "application", ProjectID: pr.ID, EnvironmentID: er.ID,
	})
	if err != nil {
		return nil, err
	}
	src, err := q.CreateServiceSource(ctx, sqlcgen.CreateServiceSourceParams{
		ServiceID: sr.ID, Provider: "github", Owner: in.Owner, Repo: in.Repo, Branch: in.Branch,
		RootDir: pgtype.Text{}, Detected: in.Detected,
	})
	if err != nil {
		return nil, err
	}
	bytes, err := response(Project{ID: pr.ID, Name: pr.Name, WorkspaceID: pr.WorkspaceID, CreatedAt: pr.CreatedAt, UpdatedAt: pr.UpdatedAt},
		Environment{ID: er.ID, Name: er.Name, ProjectID: er.ProjectID, CreatedAt: er.CreatedAt, UpdatedAt: er.UpdatedAt},
		Service{ID: sr.ID, Name: sr.Name, Image: sr.Image, ContainerName: sr.ContainerName, ContainerPort: sr.ContainerPort, HostPort: int32PtrFromPgInt4(sr.HostPort), ServerID: sr.ServerID, WorkspaceID: sr.WorkspaceID, Kind: sr.Kind, ProjectID: sr.ProjectID, EnvironmentID: sr.EnvironmentID, CreatedAt: sr.CreatedAt, UpdatedAt: sr.UpdatedAt, HasDeployed: sr.HasDeployed},
		serviceSourceFromRow(src))
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO source_project_requests (workspace_id,idempotency_key,fingerprint,response_json) VALUES ($1,$2,$3,$4)`, in.WorkspaceID, in.Key, in.Fingerprint, string(bytes))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return bytes, nil
}
