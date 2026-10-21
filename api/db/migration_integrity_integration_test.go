package db

import (
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"
)

func disposableMigrationDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	raw := os.Getenv("STAGEZERO_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("set STAGEZERO_TEST_DATABASE_URL to a disposable PostgreSQL database")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("pgx", raw)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("sz11b_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Errorf("drop disposable schema: %v", err)
		}
		admin.Close()
	})
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return u.String(), db
}

func partialMigrations(t *testing.T, include func(int) bool) fs.FS {
	t.Helper()
	files := fstest.MapFS{}
	for version := 1; version <= 31; version++ {
		if !include(version) {
			continue
		}
		prefix := fmt.Sprintf("migrations/%04d_", version)
		for name, file := range migrationFiles(t) {
			if strings.HasPrefix(name, prefix) {
				files[name] = file
			}
		}
	}
	return files
}

func migratePartial(t *testing.T, db *sql.DB, include func(int) bool) {
	t.Helper()
	goose.SetBaseFS(partialMigrations(t, include))
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(db, "migrations", goose.WithAllowMissing()); err != nil {
		t.Fatal(err)
	}
}

func execMigrationTest(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func countMigrationTest(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var count int
	if err := db.QueryRow(query).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func seedMigrationData(t *testing.T, db *sql.DB, source, metrics bool) {
	t.Helper()
	for _, id := range []string{"a", "b"} {
		execMigrationTest(t, db, `INSERT INTO users(id,email,password_hash) VALUES ($1,$2,'test')`, "user-"+id, id+"@example.test")
		execMigrationTest(t, db, `INSERT INTO workspaces(id,name,owner_user_id) VALUES ($1,$2,$3)`, "workspace-"+id, "workspace "+id, "user-"+id)
		execMigrationTest(t, db, `INSERT INTO workspace_memberships(id,workspace_id,user_id,role) VALUES ($1,$2,$3,'owner')`, "member-"+id, "workspace-"+id, "user-"+id)
		execMigrationTest(t, db, `INSERT INTO ssh_keys(id,name,private_key,workspace_id) VALUES ($1,'key','test',$2)`, "key-"+id, "workspace-"+id)
		execMigrationTest(t, db, `INSERT INTO servers(id,name,host,ssh_user,ssh_key_id,workspace_id) VALUES ($1,'server','127.0.0.1','root',$2,$3)`, "server-"+id, "key-"+id, "workspace-"+id)
		execMigrationTest(t, db, `INSERT INTO projects(id,name,workspace_id) VALUES ($1,'project',$2)`, "project-"+id, "workspace-"+id)
		execMigrationTest(t, db, `INSERT INTO environments(id,name,project_id) VALUES ($1,'production',$2)`, "environment-"+id, "project-"+id)
		execMigrationTest(t, db, `INSERT INTO services(id,name,image,container_name,container_port,server_id,workspace_id,project_id,environment_id) VALUES ($1,'app','alpine:3',$2,3000,$3,$4,$5,$6)`, "service-"+id, "container-"+id, "server-"+id, "workspace-"+id, "project-"+id, "environment-"+id)
	}
	if source {
		execMigrationTest(t, db, `INSERT INTO service_sources(service_id,owner,repo,branch) VALUES ('service-a','owner','repo','Feature/X')`)
		execMigrationTest(t, db, `INSERT INTO source_project_requests(workspace_id,idempotency_key,fingerprint,response_json) VALUES ('workspace-a','1cb4705d-4e44-467a-9eb5-fd2fd6910808','hash','{"project":"project-a"}')`)
	}
	if metrics {
		execMigrationTest(t, db, `UPDATE servers SET monitoring_enabled=true,monitoring_retention_days=10 WHERE id='server-a'`)
		execMigrationTest(t, db, `INSERT INTO notification_channels(id,workspace_id,name,type,config_ciphertext) VALUES ('channel-a','workspace-a','alerts','webhook','test')`)
		execMigrationTest(t, db, `INSERT INTO alert_rules(id,workspace_id,name,condition,scope_type,server_id) VALUES ('rule-a','workspace-a','cpu','cpu_high','server','server-a')`)
		execMigrationTest(t, db, `INSERT INTO alert_rule_channels(rule_id,channel_id) VALUES ('rule-a','channel-a')`)
		execMigrationTest(t, db, `INSERT INTO alert_events(id,workspace_id,rule_id,target_id,target_name,status,started_at) VALUES ('event-a','workspace-a','rule-a','server-a','server','firing',NOW())`)
	}
}

func TestCombinedMigrationUpgradePaths(t *testing.T) {
	for _, tc := range []struct {
		name            string
		include         func(int) bool
		source, metrics bool
	}{
		{"fresh", nil, false, false},
		{"populated main", func(v int) bool { return v <= 25 }, false, false},
		{"metrics first", func(v int) bool { return v <= 25 || v >= 27 && v <= 30 }, false, true},
		{"source first", func(v int) bool { return v <= 26 || v == 31 }, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, db := disposableMigrationDB(t)
			if tc.include != nil {
				migratePartial(t, db, tc.include)
				seedMigrationData(t, db, tc.source, tc.metrics)
			}
			if err := RunMigrations(url); err != nil {
				t.Fatal(err)
			}
			if tc.include == nil {
				seedMigrationData(t, db, true, true)
			}
			if err := checkMigrationState(db); err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"users", "workspaces", "servers", "projects", "services"} {
				if count := countMigrationTest(t, db, "SELECT count(*) FROM "+table); count != 2 {
					t.Fatalf("%s count=%d", table, count)
				}
			}
			if tc.source || tc.include == nil {
				if count := countMigrationTest(t, db, `SELECT count(*) FROM service_sources`); count != 1 {
					t.Fatalf("source count=%d", count)
				}
				if count := countMigrationTest(t, db, `SELECT count(*) FROM source_project_requests`); count != 1 {
					t.Fatalf("idempotency count=%d", count)
				}
			}
			if tc.metrics || tc.include == nil {
				if count := countMigrationTest(t, db, `SELECT count(*) FROM alert_events`); count != 1 {
					t.Fatalf("alert count=%d", count)
				}
				if count := countMigrationTest(t, db, `SELECT count(*) FROM servers WHERE monitoring_retention_days=10`); count != 1 {
					t.Fatalf("monitoring retention count=%d", count)
				}
			}
			before := countMigrationTest(t, db, `SELECT count(*) FROM goose_db_version`)
			if err := RunMigrations(url); err != nil {
				t.Fatal(err)
			}
			if after := countMigrationTest(t, db, `SELECT count(*) FROM goose_db_version`); after != before {
				t.Fatalf("replay changed version rows: %d -> %d", before, after)
			}
			for _, query := range []string{
				`INSERT INTO service_sources(service_id,owner,repo,branch) VALUES ('missing','o','r','main')`,
				`INSERT INTO source_project_requests(workspace_id,idempotency_key,fingerprint,response_json) VALUES ('missing','4a3c8d08-fc43-4783-9949-d80ec80fccaa','h','{}')`,
				`UPDATE servers SET monitoring_retention_days=0 WHERE id='server-a'`,
				`INSERT INTO services(id,name,image,container_name,container_port,server_id,workspace_id,project_id,environment_id) VALUES ('duplicate','app','alpine:3','container-a',3000,'server-a','workspace-a','project-a','environment-a')`,
			} {
				if _, err := db.Exec(query); err == nil {
					t.Fatalf("constraint accepted invalid data: %s", query)
				}
			}
		})
	}
}

func TestCombinedMigrationFailures(t *testing.T) {
	for _, tc := range []struct{ name, want, mutate string }{
		{"unapplied latest version", "migration 0028 is not applied", `INSERT INTO goose_db_version(version_id,is_applied) VALUES (28,false)`},
		{"unexpected version", "unexpected applied migration version 32", `INSERT INTO goose_db_version(version_id,is_applied) VALUES (32,true)`},
		{"dropped unique index", "idx_alert_events_active", `DROP INDEX idx_alert_events_active`},
		{"dropped retention check", "monitoring_retention_days", `ALTER TABLE servers DROP CONSTRAINT servers_monitoring_retention_days_check`},
		{"dropped scope check", "scope target check", `ALTER TABLE alert_rules DROP CONSTRAINT alert_rules_check`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, db := disposableMigrationDB(t)
			if err := RunMigrations(url); err != nil {
				t.Fatal(err)
			}
			execMigrationTest(t, db, tc.mutate)
			if err := RunMigrations(url); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
	t.Run("missing effective version", func(t *testing.T) {
		url, db := disposableMigrationDB(t)
		if err := RunMigrations(url); err != nil {
			t.Fatal(err)
		}
		execMigrationTest(t, db, `DELETE FROM goose_db_version WHERE version_id=28`)
		if err := checkMigrationState(db); err == nil || !strings.Contains(err.Error(), "missing applied migration 0028") {
			t.Fatalf("history check: %v", err)
		}
		// Goose tries to reapply 0028 and fails on existing tables; the entry point must also fail closed.
		if err := RunMigrations(url); err == nil {
			t.Fatal("migration with missing effective version succeeded")
		}
	})
	t.Run("missing SQL before Goose", func(t *testing.T) {
		url, db := disposableMigrationDB(t)
		files := migrationFiles(t)
		delete(files, "migrations/0028_alerts.sql")
		if err := runMigrations(url, files); err == nil || !strings.Contains(err.Error(), "missing migration 0028") {
			t.Fatalf("preflight: %v", err)
		}
		if count := countMigrationTest(t, db, `SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='goose_db_version'`); count != 0 {
			t.Fatalf("Goose modified database despite missing SQL: %d", count)
		}
	})
}
