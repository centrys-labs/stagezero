package monitoring

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/centrys-labs/stagezero/crypto"
	"github.com/centrys-labs/stagezero/db"
	"github.com/jackc/pgx/v5"
)

func TestMonitoringChangeSurvivesFailuresAndRetries(t *testing.T) {
	url := os.Getenv("STAGEZERO_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set STAGEZERO_TEST_DATABASE_URL to a disposable Postgres database")
	}
	ctx := context.Background()
	setup, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS sz07_monitoring`); err != nil {
		t.Fatal(err)
	}
	setup.Close(ctx)
	url += "&search_path=sz07_monitoring"
	if err := db.RunMigrations(url); err != nil {
		t.Fatal(err)
	}
	if err := crypto.Init(strings.Repeat("af", 32)); err != nil {
		t.Fatal(err)
	}
	db.Init(url)
	t.Cleanup(db.Close)
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users(id,email,password_hash) VALUES('sz07-user','sz07@test.invalid','unused')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO workspaces(id,name,owner_user_id) VALUES('sz07-workspace','test','sz07-user')`); err != nil {
		t.Fatal(err)
	}
	key, err := crypto.Encrypt("not a real SSH key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO ssh_keys(id,name,private_key,workspace_id) VALUES('sz07-key','test',$1,'sz07-workspace')`, key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, query := range []string{
			`DELETE FROM servers WHERE workspace_id='sz07-workspace'`,
			`DELETE FROM ssh_keys WHERE id='sz07-key'`,
			`DELETE FROM workspaces WHERE id='sz07-workspace'`,
			`DELETE FROM users WHERE id='sz07-user'`,
		} {
			if _, err := db.Pool.Exec(context.Background(), query); err != nil {
				t.Errorf("test cleanup: %v", err)
			}
		}
	})
	seed := func(id string) db.ServerWithKey {
		t.Helper()
		if _, err := db.Pool.Exec(ctx, `INSERT INTO servers(id,name,host,ssh_user,ssh_key_id,workspace_id)
			VALUES($1,$1,'127.0.0.1','test','sz07-key','sz07-workspace')`, id); err != nil {
			t.Fatal(err)
		}
		if err := db.SetServerMonitoring(ctx, id, db.MonitoringConfig{Enabled: true, Port: 9184, RetentionDays: 7, Status: "ready"}, "old-control", "old-reader"); err != nil {
			t.Fatal(err)
		}
		server, err := db.GetServerWithKey(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return server
	}
	intent := func(reader string) db.MonitoringIntent {
		return db.MonitoringIntent{Config: db.MonitoringConfig{Enabled: true, Port: 9184, RetentionDays: 7, Status: "ready"}, ControlToken: "old-control", ReaderToken: reader}
	}
	t.Run("remote failure retains old credentials and retries encrypted intent", func(t *testing.T) {
		server := seed("sz07-remote")
		failure := errors.New("provider said new-reader-secret in a URL")
		if err := changeServerMonitoring(ctx, server, intent("new-reader-secret"), func(context.Context, db.ServerWithKey, db.MonitoringIntent) error { return failure }); !errors.Is(err, failure) {
			t.Fatalf("change error = %v", err)
		}
		var ciphertext string
		if err := db.Pool.QueryRow(ctx, `SELECT monitoring_pending FROM servers WHERE id=$1`, server.ID).Scan(&ciphertext); err != nil || strings.Contains(ciphertext, "new-reader-secret") {
			t.Fatalf("pending secret not encrypted: %v", err)
		}
		if pending, err := db.ServerMonitoringPending(ctx, server.ID); err != nil || !pending {
			t.Fatalf("pending operation not visible to handler: %t, %v", pending, err)
		}
		current, err := db.GetServerWithKey(ctx, server.ID)
		if err != nil || current.ReaderToken != "old-reader" || current.Monitoring.Status != "provisioning" || current.Monitoring.LastError == nil || strings.Contains(*current.Monitoring.LastError, "new-reader-secret") {
			t.Fatalf("committed reader lost or error leaked secret: %#v, %v", current.Monitoring, err)
		}
		if err := changeServerMonitoring(ctx, current, intent("other-reader"), nil); !errors.Is(err, db.ErrMonitoringBusy) {
			t.Fatalf("overwrote pending change: %v", err)
		}
		if err := db.CommitServerMonitoring(ctx, server.ID, "wrong-ciphertext", intent("new-reader-secret")); !errors.Is(err, db.ErrMonitoringBusy) {
			t.Fatalf("stale commit succeeded: %v", err)
		}
		if _, err := db.Pool.Exec(ctx, `UPDATE servers SET monitoring_last_reconciled_at=NOW()-INTERVAL '2 minutes' WHERE id=$1`, server.ID); err != nil {
			t.Fatal(err)
		}
		pending, err := db.ListPendingMonitoring(ctx)
		if err != nil || len(pending) != 1 || pending[0] != server.ID {
			t.Fatalf("durable retry not scheduled: %v, %#v", err, pending)
		}
		if err := retryPendingServerMonitoring(ctx, server.ID, func(_ context.Context, old db.ServerWithKey, desired db.MonitoringIntent) error {
			if old.ReaderToken != "old-reader" || desired.ReaderToken != "new-reader-secret" {
				t.Fatalf("wrong old/desired state: %q, %q", old.ReaderToken, desired.ReaderToken)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		current, err = db.GetServerWithKey(ctx, server.ID)
		if err != nil || current.ReaderToken != "new-reader-secret" || current.Monitoring.Status != "ready" {
			t.Fatalf("retry not committed: %v, %#v", err, current.Monitoring)
		}
		if _, _, err := db.PendingServerMonitoring(ctx, server.ID); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("pending intent not removed: %v", err)
		}
		if pending, err := db.ServerMonitoringPending(ctx, server.ID); err != nil || pending {
			t.Fatalf("completed operation still pending: %t, %v", pending, err)
		}
	})
	t.Run("cancellation cannot stop retry bookkeeping", func(t *testing.T) {
		server := seed("sz07-cancel")
		cancelCtx, cancel := context.WithCancel(ctx)
		if err := changeServerMonitoring(cancelCtx, server, intent("after-cancel"), func(context.Context, db.ServerWithKey, db.MonitoringIntent) error {
			cancel()
			return context.Canceled
		}); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
		if err := retryPendingServerMonitoring(ctx, server.ID, func(context.Context, db.ServerWithKey, db.MonitoringIntent) error { return nil }); err != nil {
			t.Fatal(err)
		}
		current, err := db.GetServerWithKey(ctx, server.ID)
		if err != nil || current.ReaderToken != "after-cancel" {
			t.Fatalf("canceled change was lost: %v, %q", err, current.ReaderToken)
		}
	})
	t.Run("disable remains enabled until remote succeeds", func(t *testing.T) {
		server := seed("sz07-disable")
		cleanup := time.Now().UTC().Add(7 * 24 * time.Hour)
		intent := db.MonitoringIntent{Config: db.MonitoringConfig{Enabled: false, Port: 9184, RetentionDays: 7, Status: "disabled", CleanupAt: &cleanup}, ControlToken: server.ControlToken, ReaderToken: server.ReaderToken}
		if err := changeServerMonitoring(ctx, server, intent, func(context.Context, db.ServerWithKey, db.MonitoringIntent) error { return errors.New("offline") }); err == nil {
			t.Fatal("offline disable reported success")
		}
		current, err := db.GetServerWithKey(ctx, server.ID)
		if err != nil || !current.Monitoring.Enabled {
			t.Fatalf("committed state changed before remote success: %v", err)
		}
		if err := retryPendingServerMonitoring(ctx, server.ID, func(context.Context, db.ServerWithKey, db.MonitoringIntent) error { return nil }); err != nil {
			t.Fatal(err)
		}
		current, err = db.GetServerWithKey(ctx, server.ID)
		if err != nil || current.Monitoring.Enabled || current.Monitoring.CleanupAt == nil {
			t.Fatalf("disable not committed after retry: %v, %#v", err, current.Monitoring)
		}
	})
	t.Run("commit failure keeps pending rotation for retry", func(t *testing.T) {
		server := seed("sz07-commit")
		other := seed("sz07-conflict")
		change := intent("rotated-reader")
		change.Config.FQDN = "metrics.test.invalid"
		if err := db.StageServerMonitoring(ctx, server.ID, change); err != nil {
			t.Fatal(err)
		}
		// Simulate a committed FQDN appearing after staging but before the
		// remote operation completes. The CAS must not publish the new token.
		if _, err := db.Pool.Exec(ctx, `UPDATE servers SET monitoring_fqdn=$2 WHERE id=$1`, other.ID, change.Config.FQDN); err != nil {
			t.Fatal(err)
		}
		if err := retryPendingServerMonitoring(ctx, server.ID, func(context.Context, db.ServerWithKey, db.MonitoringIntent) error { return nil }); err == nil {
			t.Fatal("conflicting FQDN committed")
		}
		current, err := db.GetServerWithKey(ctx, server.ID)
		if err != nil || current.ReaderToken != "old-reader" {
			t.Fatalf("rotated secret was published after failed commit: %v, %q", err, current.ReaderToken)
		}
		if _, _, err := db.PendingServerMonitoring(ctx, server.ID); err != nil {
			t.Fatalf("pending rotation was lost: %v", err)
		}
		if _, err := db.Pool.Exec(ctx, `UPDATE servers SET monitoring_fqdn=NULL WHERE id=$1`, other.ID); err != nil {
			t.Fatal(err)
		}
		if err := retryPendingServerMonitoring(ctx, server.ID, func(context.Context, db.ServerWithKey, db.MonitoringIntent) error { return nil }); err != nil {
			t.Fatal(err)
		}
		current, err = db.GetServerWithKey(ctx, server.ID)
		if err != nil || current.ReaderToken != "rotated-reader" || current.Monitoring.FQDN == nil || *current.Monitoring.FQDN != change.Config.FQDN {
			t.Fatalf("pending rotation did not converge: %v, %#v", err, current.Monitoring)
		}
	})
	t.Run("concurrent changes cannot overwrite one another", func(t *testing.T) {
		server := seed("sz07-concurrent")
		entered, release := make(chan struct{}), make(chan struct{})
		result := make(chan error, 1)
		go func() {
			result <- changeServerMonitoring(ctx, server, intent("first-reader"), func(context.Context, db.ServerWithKey, db.MonitoringIntent) error {
				close(entered)
				<-release
				return nil
			})
		}()
		<-entered
		if err := changeServerMonitoring(ctx, server, intent("second-reader"), nil); !errors.Is(err, db.ErrMonitoringBusy) {
			t.Errorf("concurrent change did not fail closed: %v", err)
		}
		close(release)
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		current, err := db.GetServerWithKey(ctx, server.ID)
		if err != nil || current.ReaderToken != "first-reader" {
			t.Fatalf("lost first change: %v, %q", err, current.ReaderToken)
		}
		if err := changeServerMonitoring(ctx, server, intent("stale-reader"), nil); !errors.Is(err, db.ErrMonitoringBusy) {
			t.Fatalf("accepted a stale snapshot: %v", err)
		}
	})
}
