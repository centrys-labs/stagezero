package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/centrys-labs/stagezero/crypto"
	"github.com/centrys-labs/stagezero/db/sqlcgen"
	"github.com/jackc/pgx/v5/pgtype"
)

type AppServer struct {
	ID                    string           `json:"id"`
	Name                  string           `json:"name"`
	Host                  string           `json:"host"`
	Port                  int32            `json:"port"`
	SSHUser               string           `json:"ssh_user"`
	SSHKeyID              string           `json:"ssh_key_id"`
	WorkspaceID           string           `json:"workspace_id"`
	ProxyStatus           string           `json:"proxy_status"`
	ProxyLastReconciledAt *time.Time       `json:"proxy_last_reconciled_at"`
	ProxyLastError        *string          `json:"proxy_last_error"`
	CreatedAt             time.Time        `json:"created_at"`
	Monitoring            ServerMonitoring `json:"monitoring"`
}

type ServerMonitoring struct {
	Enabled          bool       `json:"enabled"`
	Port             int32      `json:"port"`
	RetentionDays    int32      `json:"retention_days"`
	FQDN             *string    `json:"fqdn"`
	Status           string     `json:"status"`
	LastReconciledAt *time.Time `json:"last_reconciled_at"`
	LastError        *string    `json:"last_error"`
	CleanupAt        *time.Time `json:"cleanup_at"`
}

type ServerWithKey struct {
	AppServer
	PrivateKey   string `json:"-"`
	ControlToken string `json:"-"`
	ReaderToken  string `json:"-"`
}

type MonitoringConfig struct {
	Enabled       bool
	Port          int32
	RetentionDays int32
	FQDN          string
	Status        string
	LastError     string
	CleanupAt     *time.Time
}

type MonitoringIntent struct {
	Config       MonitoringConfig
	ControlToken string
	ReaderToken  string
}

var ErrMonitoringBusy = errors.New("monitoring operation in progress")

// WithServerMonitoringLock serializes remote changes across API replicas. The
// acquired connection owns the advisory lock until it is explicitly released.
func WithServerMonitoringLock(ctx context.Context, serverID string, change func() error) (err error) {
	conn, err := Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 741852))`, serverID).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return ErrMonitoringBusy
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		var unlocked bool
		if unlockErr := conn.QueryRow(cleanupCtx, `SELECT pg_advisory_unlock(hashtextextended($1, 741852))`, serverID).Scan(&unlocked); unlockErr != nil || !unlocked {
			// A pooled connection must never be returned with a session lock held.
			_ = conn.Conn().Close(cleanupCtx)
			if unlockErr == nil {
				unlockErr = errors.New("advisory lock was not held")
			}
			err = errors.Join(err, fmt.Errorf("release monitoring lock: %w", unlockErr))
		}
	}()
	return change()
}

// StageServerMonitoring keeps the committed credentials in service while an
// encrypted desired configuration is retried after an interrupted operation.
func StageServerMonitoring(ctx context.Context, serverID string, intent MonitoringIntent) error {
	data, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	ciphertext, err := crypto.Encrypt(string(data))
	if err != nil {
		return err
	}
	status := "provisioning"
	if !intent.Config.Enabled {
		status = "disabling"
	}
	result, err := Pool.Exec(ctx, `UPDATE servers SET monitoring_pending = $2, monitoring_pending_fqdn = NULLIF($3, ''),
		monitoring_status = $4, monitoring_last_error = NULL, monitoring_last_reconciled_at = NOW()
		WHERE id = $1 AND monitoring_pending IS NULL
		AND NOT EXISTS (SELECT 1 FROM servers s WHERE s.id <> $1 AND lower(s.monitoring_fqdn) = lower($3))`,
		serverID, ciphertext, intent.Config.FQDN, status)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrMonitoringBusy
	}
	return nil
}

func PendingServerMonitoring(ctx context.Context, serverID string) (MonitoringIntent, string, error) {
	var ciphertext string
	if err := Pool.QueryRow(ctx, `SELECT monitoring_pending FROM servers WHERE id = $1 AND monitoring_pending IS NOT NULL`, serverID).Scan(&ciphertext); err != nil {
		return MonitoringIntent{}, "", err
	}
	text, err := crypto.Decrypt(ciphertext)
	if err != nil {
		return MonitoringIntent{}, "", err
	}
	var intent MonitoringIntent
	if err := json.Unmarshal([]byte(text), &intent); err != nil {
		return MonitoringIntent{}, "", err
	}
	return intent, ciphertext, nil
}

func CommitServerMonitoring(ctx context.Context, serverID, pendingCiphertext string, intent MonitoringIntent) error {
	control, err := encryptOptional(intent.ControlToken)
	if err != nil {
		return err
	}
	reader, err := encryptOptional(intent.ReaderToken)
	if err != nil {
		return err
	}
	var cleanupAt interface{}
	if intent.Config.CleanupAt != nil {
		cleanupAt = intent.Config.CleanupAt.UTC()
	}
	result, err := Pool.Exec(ctx, `UPDATE servers SET monitoring_enabled = $3, monitoring_port = $4,
		monitoring_retention_days = $5, monitoring_fqdn = NULLIF($6, ''), monitoring_control_token = $7,
		monitoring_reader_token = $8, monitoring_status = $9, monitoring_last_reconciled_at = NOW(),
		monitoring_last_error = NULL, monitoring_cleanup_at = $10, monitoring_pending = NULL,
		monitoring_pending_fqdn = NULL WHERE id = $1 AND monitoring_pending = $2`,
		serverID, pendingCiphertext, intent.Config.Enabled, intent.Config.Port, intent.Config.RetentionDays,
		intent.Config.FQDN, control, reader, intent.Config.Status, cleanupAt)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrMonitoringBusy
	}
	return nil
}

func MarkMonitoringRetry(ctx context.Context, serverID string) error {
	_, err := Pool.Exec(ctx, `UPDATE servers SET monitoring_last_error = 'monitoring reconciliation pending',
		monitoring_last_reconciled_at = NOW() WHERE id = $1 AND monitoring_pending IS NOT NULL`, serverID)
	return err
}

func ListPendingMonitoring(ctx context.Context) ([]string, error) {
	rows, err := Pool.Query(ctx, `SELECT id FROM servers WHERE monitoring_pending IS NOT NULL
		AND monitoring_last_reconciled_at < NOW() - INTERVAL '1 minute' ORDER BY monitoring_last_reconciled_at LIMIT 20`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func ServerMonitoringPending(ctx context.Context, serverID string) (bool, error) {
	var pending bool
	err := Pool.QueryRow(ctx, `SELECT monitoring_pending IS NOT NULL FROM servers WHERE id = $1`, serverID).Scan(&pending)
	return pending, err
}

func CreateServer(ctx context.Context, name, host string, port int32, sshUser, sshKeyID, workspaceID string) (AppServer, error) {
	row, err := Queries.CreateServer(ctx, sqlcgen.CreateServerParams{
		Name: name, Host: host, Port: port, SshUser: sshUser, SshKeyID: sshKeyID, WorkspaceID: workspaceID,
	})
	if err != nil {
		return AppServer{}, err
	}
	return newAppServer(
		row.ID, row.Name, row.Host, row.Port, row.SshUser, row.SshKeyID, row.WorkspaceID,
		row.ProxyStatus, row.ProxyLastReconciledAt, row.ProxyLastError, row.CreatedAt,
		row.MonitoringEnabled, row.MonitoringPort, row.MonitoringRetentionDays,
		row.MonitoringFqdn, row.MonitoringStatus, row.MonitoringLastReconciledAt, row.MonitoringLastError, row.MonitoringCleanupAt,
	), nil
}

func GetServerByID(ctx context.Context, id string) (AppServer, error) {
	row, err := Queries.GetServerByID(ctx, id)
	if err != nil {
		return AppServer{}, err
	}
	return newAppServer(
		row.ID, row.Name, row.Host, row.Port, row.SshUser, row.SshKeyID, row.WorkspaceID,
		row.ProxyStatus, row.ProxyLastReconciledAt, row.ProxyLastError, row.CreatedAt,
		row.MonitoringEnabled, row.MonitoringPort, row.MonitoringRetentionDays,
		row.MonitoringFqdn, row.MonitoringStatus, row.MonitoringLastReconciledAt, row.MonitoringLastError, row.MonitoringCleanupAt,
	), nil
}

func ListServersByWorkspace(ctx context.Context, workspaceID string) ([]AppServer, error) {
	rows, err := Queries.ListServersByWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	servers := make([]AppServer, len(rows))
	for index, row := range rows {
		servers[index] = newAppServer(
			row.ID, row.Name, row.Host, row.Port, row.SshUser, row.SshKeyID, row.WorkspaceID,
			row.ProxyStatus, row.ProxyLastReconciledAt, row.ProxyLastError, row.CreatedAt,
			row.MonitoringEnabled, row.MonitoringPort, row.MonitoringRetentionDays,
			row.MonitoringFqdn, row.MonitoringStatus, row.MonitoringLastReconciledAt, row.MonitoringLastError, row.MonitoringCleanupAt,
		)
	}
	return servers, nil
}

func SetServerProxyReady(ctx context.Context, serverID, status string) error {
	return Queries.SetServerProxyReady(ctx, sqlcgen.SetServerProxyReadyParams{ID: serverID, ProxyStatus: status})
}

func SetServerProxyError(ctx context.Context, serverID, status, lastError string) error {
	return Queries.SetServerProxyError(ctx, sqlcgen.SetServerProxyErrorParams{
		ID: serverID, ProxyStatus: status, ProxyLastError: pgtype.Text{String: lastError, Valid: true},
	})
}

func GetServerWithKey(ctx context.Context, id string) (ServerWithKey, error) {
	row, err := Queries.GetServerWithKey(ctx, id)
	if err != nil {
		return ServerWithKey{}, err
	}
	privateKey, err := crypto.Decrypt(row.PrivateKey)
	if err != nil {
		return ServerWithKey{}, fmt.Errorf("decrypt private key: %w", err)
	}
	controlToken, err := decryptOptional(row.MonitoringControlToken)
	if err != nil {
		return ServerWithKey{}, fmt.Errorf("decrypt monitoring control token: %w", err)
	}
	readerToken, err := decryptOptional(row.MonitoringReaderToken)
	if err != nil {
		return ServerWithKey{}, fmt.Errorf("decrypt monitoring reader token: %w", err)
	}
	return ServerWithKey{
		AppServer: newAppServer(
			row.ID, row.Name, row.Host, row.Port, row.SshUser, row.SshKeyID, row.WorkspaceID,
			"", pgtype.Timestamptz{}, pgtype.Text{}, row.CreatedAt,
			row.MonitoringEnabled, row.MonitoringPort, row.MonitoringRetentionDays,
			row.MonitoringFqdn, row.MonitoringStatus, row.MonitoringLastReconciledAt, row.MonitoringLastError, row.MonitoringCleanupAt,
		),
		PrivateKey: privateKey, ControlToken: controlToken, ReaderToken: readerToken,
	}, nil
}

func SetServerMonitoring(ctx context.Context, serverID string, config MonitoringConfig, controlToken, readerToken string) error {
	controlCiphertext, err := encryptOptional(controlToken)
	if err != nil {
		return fmt.Errorf("encrypt monitoring control token: %w", err)
	}
	readerCiphertext, err := encryptOptional(readerToken)
	if err != nil {
		return fmt.Errorf("encrypt monitoring reader token: %w", err)
	}
	cleanupAt := time.Unix(0, 0).UTC()
	if config.CleanupAt != nil {
		cleanupAt = config.CleanupAt.UTC()
	}
	return Queries.SetServerMonitoring(ctx, sqlcgen.SetServerMonitoringParams{
		MonitoringEnabled: config.Enabled, MonitoringPort: config.Port, MonitoringRetentionDays: config.RetentionDays,
		MonitoringFqdn:         config.FQDN,
		MonitoringControlToken: controlCiphertext, MonitoringReaderToken: readerCiphertext,
		MonitoringStatus: config.Status, MonitoringLastError: config.LastError, MonitoringCleanupAt: cleanupAt, ID: serverID,
	})
}

func MonitoringFQDNInUse(ctx context.Context, serverID, fqdn string) (bool, error) {
	return Queries.ServerFQDNInUse(ctx, sqlcgen.ServerFQDNInUseParams{ID: serverID, Fqdn: fqdn})
}

func ListMonitoringCleanupDue(ctx context.Context) ([]ServerWithKey, error) {
	rows, err := Queries.ListMonitoringCleanupDue(ctx)
	if err != nil {
		return nil, err
	}
	servers := make([]ServerWithKey, 0, len(rows))
	for _, row := range rows {
		privateKey, decryptErr := crypto.Decrypt(row.PrivateKey)
		if decryptErr != nil {
			return nil, fmt.Errorf("decrypt SSH key for server %s: %w", row.ID, decryptErr)
		}
		servers = append(servers, ServerWithKey{
			AppServer: newAppServer(
				row.ID, row.Name, row.Host, row.Port, row.SshUser, row.SshKeyID, row.WorkspaceID,
				"", pgtype.Timestamptz{}, pgtype.Text{}, row.CreatedAt,
				row.MonitoringEnabled, row.MonitoringPort, row.MonitoringRetentionDays,
				row.MonitoringFqdn, row.MonitoringStatus, row.MonitoringLastReconciledAt, row.MonitoringLastError, row.MonitoringCleanupAt,
			),
			PrivateKey: privateKey,
		})
	}
	return servers, nil
}

func ClearServerMonitoringData(ctx context.Context, serverID string) error {
	return Queries.ClearServerMonitoringData(ctx, serverID)
}

func newAppServer(id, name, host string, port int32, sshUser, sshKeyID, workspaceID, proxyStatus string,
	proxyReconciledAt pgtype.Timestamptz, proxyLastError pgtype.Text, createdAt time.Time,
	monitoringEnabled bool, monitoringPort, retentionDays int32, fqdn pgtype.Text,
	monitoringStatus string, monitoringReconciledAt pgtype.Timestamptz, monitoringLastError pgtype.Text,
	monitoringCleanupAt pgtype.Timestamptz) AppServer {
	return AppServer{
		ID: id, Name: name, Host: host, Port: port, SSHUser: sshUser, SSHKeyID: sshKeyID, WorkspaceID: workspaceID,
		ProxyStatus: proxyStatus, ProxyLastReconciledAt: timePtrFromPgTimestamptz(proxyReconciledAt),
		ProxyLastError: stringPtrFromPgText(proxyLastError), CreatedAt: createdAt,
		Monitoring: ServerMonitoring{
			Enabled: monitoringEnabled, Port: monitoringPort, RetentionDays: retentionDays,
			FQDN: stringPtrFromPgText(fqdn), Status: monitoringStatus,
			LastReconciledAt: timePtrFromPgTimestamptz(monitoringReconciledAt),
			LastError:        stringPtrFromPgText(monitoringLastError), CleanupAt: timePtrFromPgTimestamptz(monitoringCleanupAt),
		},
	}
}

func encryptOptional(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	return crypto.Encrypt(value)
}

func decryptOptional(value pgtype.Text) (string, error) {
	if !value.Valid || value.String == "" {
		return "", nil
	}
	return crypto.Decrypt(value.String)
}
