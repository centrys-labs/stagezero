package monitoring

import (
	"context"
	"errors"
	"time"

	"github.com/centrys-labs/stagezero/config"
	"github.com/centrys-labs/stagezero/db"
	"github.com/centrys-labs/stagezero/ssh"
)

// A pending change is durable before the SSH operation starts. Keeping the
// committed tokens separate lets readers use the old agent until the new one
// is running and the database commits the replacement.
func ChangeServerMonitoring(ctx context.Context, expected db.ServerWithKey, intent db.MonitoringIntent) error {
	return changeServerMonitoring(ctx, expected, intent, applyServerMonitoring)
}

func RetryPendingServerMonitoring(ctx context.Context, id string) error {
	return retryPendingServerMonitoring(ctx, id, applyServerMonitoring)
}

type monitoringApply func(context.Context, db.ServerWithKey, db.MonitoringIntent) error

func changeServerMonitoring(ctx context.Context, expected db.ServerWithKey, intent db.MonitoringIntent, apply monitoringApply) error {
	return db.WithServerMonitoringLock(ctx, expected.ID, func() error {
		current, err := db.GetServerWithKey(ctx, expected.ID)
		if err != nil {
			return err
		}
		if !sameMonitoringVersion(current.Monitoring.LastReconciledAt, expected.Monitoring.LastReconciledAt) {
			return db.ErrMonitoringBusy
		}
		if err := db.StageServerMonitoring(ctx, expected.ID, intent); err != nil {
			return err
		}
		return applyPendingServerMonitoring(ctx, expected.ID, apply)
	})
}

func sameMonitoringVersion(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func retryPendingServerMonitoring(ctx context.Context, id string, apply monitoringApply) error {
	return db.WithServerMonitoringLock(ctx, id, func() error {
		return applyPendingServerMonitoring(ctx, id, apply)
	})
}

func applyPendingServerMonitoring(ctx context.Context, id string, apply monitoringApply) error {
	intent, ciphertext, err := db.PendingServerMonitoring(ctx, id)
	if err != nil {
		return err
	}
	server, err := db.GetServerWithKey(ctx, id)
	if err != nil {
		return err
	}
	if err := apply(ctx, server, intent); err != nil {
		// The request can already be cancelled; keep pending intent and schedule a
		// retry using a fresh bounded database context without storing SSH output.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return errors.Join(err, db.MarkMonitoringRetry(cleanupCtx, id))
	}
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := db.CommitServerMonitoring(commitCtx, id, ciphertext, intent); err != nil {
		retryCtx, retryCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer retryCancel()
		return errors.Join(err, db.MarkMonitoringRetry(retryCtx, id))
	}
	return nil
}

func applyServerMonitoring(ctx context.Context, server db.ServerWithKey, intent db.MonitoringIntent) error {
	client, err := ssh.NewClient(ssh.ServerConfig{Host: server.Host, Port: int(server.Port), User: server.SSHUser, PrivateKey: server.PrivateKey})
	if err != nil {
		return err
	}
	defer client.Close()
	if err := client.DetectDocker(); err != nil {
		return err
	}
	if !intent.Config.Enabled {
		return Disable(ctx, client, server.ID)
	}
	return Enable(ctx, client, server.ID, Config{
		Image: config.C.MonitoringImage, HostPort: int(intent.Config.Port),
		RetentionDays: int(intent.Config.RetentionDays), FQDN: intent.Config.FQDN,
		ControlToken: intent.ControlToken, ReaderToken: intent.ReaderToken,
	})
}
