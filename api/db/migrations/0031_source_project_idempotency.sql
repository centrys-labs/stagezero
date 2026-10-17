-- +goose Up
CREATE TABLE source_project_requests (
    workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    idempotency_key UUID NOT NULL,
    fingerprint TEXT NOT NULL,
    response_json TEXT NOT NULL,
    PRIMARY KEY (workspace_id, idempotency_key)
);

-- +goose Down
DROP TABLE source_project_requests;
