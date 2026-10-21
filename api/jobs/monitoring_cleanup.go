package jobs

import (
	"context"
	"errors"
	"github.com/centrys-labs/stagezero/telemetry"
	"time"

	"github.com/centrys-labs/stagezero/db"
	"github.com/centrys-labs/stagezero/monitoring"
	"github.com/centrys-labs/stagezero/ssh"
	"github.com/jackc/pgx/v5"
)

const monitoringCleanupInterval = time.Hour

// Pending monitoring changes survive API cancellation and process restarts.
func StartMonitoringReconciler(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		reconcileMonitoring(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func reconcileMonitoring(ctx context.Context) {
	ids, err := db.ListPendingMonitoring(ctx)
	if err != nil {
		telemetry.Printf("Monitoring reconciliation: list pending changes failed")
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		attempt, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err := monitoring.RetryPendingServerMonitoring(attempt, id)
		cancel()
		if err != nil && !errors.Is(err, db.ErrMonitoringBusy) && !errors.Is(err, pgx.ErrNoRows) {
			// SSH output can include secrets. Never log the raw error.
			telemetry.Printf("Monitoring reconciliation pending for server=%s", id)
		}
	}
}

func StartMonitoringCleanup(ctx context.Context) {
	ticker := time.NewTicker(monitoringCleanupInterval)
	defer ticker.Stop()
	for {
		cleanupMonitoringData(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func cleanupMonitoringData(ctx context.Context) {
	servers, err := db.ListMonitoringCleanupDue(ctx)
	if err != nil {
		telemetry.Printf("Monitoring cleanup: list servers: %v", err)
		return
	}
	for _, server := range servers {
		client, err := ssh.NewClient(ssh.ServerConfig{Host: server.Host, Port: int(server.Port), User: server.SSHUser, PrivateKey: server.PrivateKey})
		if err != nil {
			telemetry.Printf("Monitoring cleanup: connect server=%s: %v", server.ID, err)
			continue
		}
		err = monitoring.DeleteLocalData(ctx, client)
		client.Close()
		if err != nil {
			telemetry.Printf("Monitoring cleanup: delete data server=%s: %v", server.ID, err)
			continue
		}
		if err := db.ClearServerMonitoringData(ctx, server.ID); err != nil {
			telemetry.Printf("Monitoring cleanup: clear database server=%s: %v", server.ID, err)
		}
	}
}
