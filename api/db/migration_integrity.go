package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"strings"
)

const migrationManifestDigest = "17cbd182ce0b76c85d770a522c446d1db0e317404e87d32568456833cf71809e"

type migrationEntry struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}

func checkMigrationBundle(bundle fs.FS) error {
	manifest, err := fs.ReadFile(bundle, "migrations/manifest-0031.json")
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(manifest)) != migrationManifestDigest {
		return fmt.Errorf("manifest digest mismatch")
	}
	decoder := json.NewDecoder(strings.NewReader(string(manifest)))
	decoder.DisallowUnknownFields()
	var entries []migrationEntry
	if err := decoder.Decode(&entries); err != nil {
		return fmt.Errorf("decode manifest: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("manifest has trailing data: %v", err)
	}
	if len(entries) != 31 {
		return fmt.Errorf("manifest has %d migrations, expected 31", len(entries))
	}
	actual, err := fs.ReadDir(bundle, "migrations")
	if err != nil {
		return fmt.Errorf("read migration directory: %w", err)
	}
	files := map[string]bool{}
	for _, item := range actual {
		if item.IsDir() {
			return fmt.Errorf("unexpected migration directory %s", item.Name())
		}
		if strings.HasSuffix(item.Name(), ".sql") {
			files[item.Name()] = true
		}
	}
	for i, entry := range entries {
		version := i + 1
		if !strings.HasPrefix(entry.File, fmt.Sprintf("%04d_", version)) || !strings.HasSuffix(entry.File, ".sql") {
			return fmt.Errorf("manifest version %04d has invalid filename %q", version, entry.File)
		}
		if !files[entry.File] {
			return fmt.Errorf("missing migration %s", entry.File)
		}
		delete(files, entry.File)
		if len(entry.SHA256) != 64 {
			return fmt.Errorf("invalid digest for %s", entry.File)
		}
		if _, err := hex.DecodeString(entry.SHA256); err != nil || strings.ToLower(entry.SHA256) != entry.SHA256 {
			return fmt.Errorf("invalid digest for %s", entry.File)
		}
		data, err := fs.ReadFile(bundle, "migrations/"+entry.File)
		if err != nil {
			return fmt.Errorf("read %s: %w", entry.File, err)
		}
		if fmt.Sprintf("%x", sha256.Sum256(data)) != entry.SHA256 {
			return fmt.Errorf("digest mismatch for %s", entry.File)
		}
	}
	for file := range files {
		return fmt.Errorf("unexpected migration %s", file)
	}
	return nil
}

func checkMigrationState(db *sql.DB) error {
	rows, err := db.Query(`SELECT version_id, is_applied FROM (
		SELECT DISTINCT ON (version_id) version_id, is_applied FROM goose_db_version ORDER BY version_id, id DESC
	) AS latest ORDER BY version_id`)
	if err != nil {
		return fmt.Errorf("read effective migration history: %w", err)
	}
	defer rows.Close()
	seen := make(map[int64]bool, 31)
	for rows.Next() {
		var version int64
		var applied bool
		if err := rows.Scan(&version, &applied); err != nil {
			return err
		}
		if version == 0 {
			continue
		}
		if version < 1 || version > 31 {
			return fmt.Errorf("unexpected applied migration version %d", version)
		}
		if !applied {
			return fmt.Errorf("migration %04d is not applied", version)
		}
		seen[version] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read effective migration history: %w", err)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for version := int64(1); version <= 31; version++ {
		if !seen[version] {
			return fmt.Errorf("missing applied migration %04d", version)
		}
	}
	return checkMigrationSchema(db)
}

func checkMigrationSchema(db *sql.DB) error {
	for _, table := range []string{"service_sources", "source_project_requests", "notification_channels", "alert_rules", "alert_rule_channels", "alert_events"} {
		if err := requireCatalog(db, "table "+table, `SELECT EXISTS(SELECT 1 FROM pg_class r JOIN pg_namespace n ON n.oid=r.relnamespace WHERE n.nspname=current_schema() AND r.relname=$1 AND r.relkind IN ('r','p'))`, table); err != nil {
			return err
		}
	}
	for _, column := range []string{"monitoring_enabled", "monitoring_port", "monitoring_retention_days", "monitoring_fqdn", "monitoring_control_token", "monitoring_reader_token", "monitoring_status", "monitoring_last_reconciled_at", "monitoring_last_error", "monitoring_cleanup_at", "monitoring_pending", "monitoring_pending_fqdn"} {
		if err := requireCatalog(db, "servers."+column, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='servers' AND column_name=$1)`, column); err != nil {
			return err
		}
	}
	var removed bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='servers' AND column_name='monitoring_private_address')`).Scan(&removed); err != nil {
		return err
	}
	if removed {
		return fmt.Errorf("removed servers.monitoring_private_address still exists")
	}
	for _, constraint := range []struct{ table, kind, reference, detail string }{
		{"service_sources", "p", "", ""}, {"service_sources", "f", "services", ""},
		{"source_project_requests", "p", "", ""}, {"source_project_requests", "f", "workspaces", ""},
		{"servers", "c", "", "monitoring_retention_days"},
		{"notification_channels", "f", "workspaces", ""},
		{"alert_rules", "f", "workspaces", ""}, {"alert_rules", "f", "servers", ""}, {"alert_rules", "f", "services", ""},
		{"alert_rules", "c", "", "scope_type"},
		{"alert_rule_channels", "p", "", ""}, {"alert_rule_channels", "f", "alert_rules", ""}, {"alert_rule_channels", "f", "notification_channels", ""},
		{"alert_events", "f", "workspaces", ""}, {"alert_events", "f", "alert_rules", ""},
	} {
		name := fmt.Sprintf("%s %s constraint -> %s %s", constraint.table, constraint.kind, constraint.reference, constraint.detail)
		query := `WITH target AS MATERIALIZED (SELECT c.oid FROM pg_constraint c JOIN pg_class r ON r.oid=c.conrelid JOIN pg_namespace n ON n.oid=r.relnamespace WHERE n.nspname=current_schema() AND r.relname=$1 AND c.contype=$2 AND ($3='' OR c.confrelid=to_regclass($3))) SELECT EXISTS(SELECT 1 FROM target WHERE $4='' OR pg_get_constraintdef(target.oid) LIKE '%' || $4 || '%')`
		if err := requireCatalog(db, name, query, constraint.table, constraint.kind, constraint.reference, constraint.detail); err != nil {
			return err
		}
	}
	if err := requireCatalog(db, "alert_rules scope target check", `WITH target AS MATERIALIZED (SELECT c.oid FROM pg_constraint c JOIN pg_class r ON r.oid=c.conrelid JOIN pg_namespace n ON n.oid=r.relnamespace WHERE n.nspname=current_schema() AND r.relname='alert_rules' AND c.contype='c') SELECT EXISTS(SELECT 1 FROM target WHERE pg_get_constraintdef(oid) LIKE '%scope_type%' AND pg_get_constraintdef(oid) LIKE '%server_id%' AND pg_get_constraintdef(oid) LIKE '%service_id%')`); err != nil {
		return err
	}
	for _, index := range []struct{ table, name string }{
		{"servers", "idx_servers_monitoring_fqdn"}, {"servers", "idx_servers_monitoring_pending_fqdn"}, {"alert_events", "idx_alert_events_active"},
	} {
		query := `SELECT EXISTS(SELECT 1 FROM pg_index i JOIN pg_class x ON x.oid=i.indexrelid JOIN pg_class r ON r.oid=i.indrelid JOIN pg_namespace n ON n.oid=r.relnamespace WHERE n.nspname=current_schema() AND r.relname=$1 AND x.relname=$2 AND i.indisunique AND i.indisvalid AND i.indpred IS NOT NULL)`
		if err := requireCatalog(db, "unique partial index "+index.name, query, index.table, index.name); err != nil {
			return err
		}
	}
	return nil
}

func requireCatalog(db *sql.DB, name, query string, args ...any) error {
	var present bool
	if err := db.QueryRow(query, args...).Scan(&present); err != nil {
		return fmt.Errorf("inspect %s: %w", name, err)
	}
	if !present {
		return fmt.Errorf("missing %s", name)
	}
	return nil
}
