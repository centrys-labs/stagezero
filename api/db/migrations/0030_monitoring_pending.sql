-- +goose Up
ALTER TABLE servers
    ADD COLUMN monitoring_pending TEXT,
    ADD COLUMN monitoring_pending_fqdn TEXT;

CREATE UNIQUE INDEX idx_servers_monitoring_pending_fqdn
    ON servers (lower(monitoring_pending_fqdn))
    WHERE monitoring_pending_fqdn IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_servers_monitoring_pending_fqdn;
ALTER TABLE servers
    DROP COLUMN monitoring_pending_fqdn,
    DROP COLUMN monitoring_pending;
