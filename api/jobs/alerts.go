package jobs

import (
	"context"
	"errors"
	"fmt"
	"github.com/centrys-labs/stagezero/telemetry"
	"strconv"
	"strings"
	"time"

	"github.com/centrys-labs/stagezero/alerts"
	"github.com/centrys-labs/stagezero/db"
	"github.com/centrys-labs/stagezero/monitoring"
	"github.com/centrys-labs/stagezero/ssh"
	"github.com/jackc/pgx/v5"
)

const alertEvaluationInterval = time.Minute

// staleSampleAfter is how old an agent sample may be before it stops counting
// as evidence. The agent publishes every 15s, so two minutes tolerates several
// missed cycles while still catching a wedged collector.
const staleSampleAfter = 2 * time.Minute

func sampledFresh(sampledAtMillis int64) bool {
	if sampledAtMillis <= 0 {
		return false
	}
	return time.Since(time.UnixMilli(sampledAtMillis)) <= staleSampleAfter
}

func StartAlertEvaluator(ctx context.Context) {
	tracker := alerts.NewTracker()
	ticker := time.NewTicker(alertEvaluationInterval)
	defer ticker.Stop()
	for {
		evaluateAlerts(ctx, tracker)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func evaluateAlerts(ctx context.Context, tracker *alerts.Tracker) {
	rules, err := db.ListEnabledAlertRules(ctx)
	if err != nil {
		telemetry.Printf("alert evaluator: list rules: %v", err)
		return
	}
	for _, rule := range rules {
		observation, err := observeRuleTarget(ctx, rule)
		if err != nil {
			telemetry.Printf("alert evaluator: rule=%s: %v", rule.ID, err)
			continue
		}
		active, activeErr := db.FindActiveAlertEvent(ctx, rule.ID, observation.TargetID)
		activeFound := activeErr == nil
		if activeErr != nil && !errors.Is(activeErr, pgx.ErrNoRows) {
			telemetry.Printf("alert evaluator: active event rule=%s target=%s: %v", rule.ID, observation.TargetID, activeErr)
			continue
		}
		transition := tracker.Evaluate(alerts.Rule{
			ID: rule.ID, WorkspaceID: rule.WorkspaceID, Name: rule.Name, Condition: rule.Condition,
			Threshold: rule.Threshold, Duration: time.Duration(rule.DurationSeconds) * time.Second,
		}, observation, activeFound, active.StartedAt, time.Now().UTC())
		switch transition.Kind {
		case alerts.TransitionStarted:
			event, err := db.CreateAlertEvent(ctx, rule.WorkspaceID, rule.ID, observation.TargetID, observation.TargetName, transition.Since, transition.Value)
			if err != nil {
				telemetry.Printf("alert evaluator: create event rule=%s target=%s: %v", rule.ID, observation.TargetID, err)
				continue
			}
			sendRuleNotifications(ctx, rule, alerts.Message{
				Title:     fmt.Sprintf("[StageZero] %s - %s", rule.Name, observation.TargetName),
				Body:      fmt.Sprintf("Condition %s is active at %.2f (threshold %.2f).", rule.Condition, transition.Value, rule.Threshold),
				StartedAt: event.StartedAt,
			})
		case alerts.TransitionResolved:
			resolvedAt := time.Now().UTC()
			event, err := db.ResolveAlertEvent(ctx, active.ID, resolvedAt, transition.Value)
			if err != nil {
				telemetry.Printf("alert evaluator: resolve event=%s: %v", active.ID, err)
				continue
			}
			duration := resolvedAt.Sub(event.StartedAt)
			sendRuleNotifications(ctx, rule, alerts.Message{
				Title:    fmt.Sprintf("[StageZero] Recovered - %s", observation.TargetName),
				Body:     fmt.Sprintf("Condition %s recovered after %s.", rule.Condition, duration.Round(time.Second)),
				Resolved: true, StartedAt: event.StartedAt, ResolvedAt: resolvedAt,
			})
		}
	}
}

func sendRuleNotifications(ctx context.Context, rule db.AlertRule, message alerts.Message) {
	for _, channelID := range rule.ChannelIDs {
		channel, err := db.GetNotificationChannel(ctx, channelID)
		if err != nil {
			telemetry.Printf("alert notification: channel=%s: %v", channelID, err)
			continue
		}
		if err := alerts.Send(ctx, alerts.Channel{ID: channel.ID, Name: channel.Name, Type: channel.Type, Enabled: channel.Enabled, Config: channel.Config}, message); err != nil {
			telemetry.Printf("alert notification: channel=%s rule=%s: %v", channelID, rule.ID, err)
		}
	}
}

func observeRuleTarget(ctx context.Context, rule db.AlertRule) (alerts.Observation, error) {
	if rule.ScopeType == "server" && rule.ServerID != nil {
		return observeServer(ctx, *rule.ServerID)
	}
	if rule.ScopeType == "service" && rule.ServiceID != nil {
		return observeService(ctx, *rule.ServiceID)
	}
	return alerts.Observation{}, fmt.Errorf("rule has no valid target")
}

// monitoringTarget describes how to reach a server's agent: through the SSH
// connection StageZero already uses for that server, to the loopback port the agent
// publishes on.
func monitoringTarget(server db.ServerWithKey) monitoring.Target {
	return monitoring.Target{
		SSH:  ssh.ServerConfig{Host: server.Host, Port: int(server.Port), User: server.SSHUser, PrivateKey: server.PrivateKey},
		Port: int(server.Monitoring.Port),
	}
}

func observeServer(ctx context.Context, serverID string) (alerts.Observation, error) {
	server, err := db.GetServerWithKey(ctx, serverID)
	if err != nil {
		return alerts.Observation{TargetID: serverID, TargetName: serverID}, err
	}
	observation := alerts.Observation{TargetID: server.ID, TargetName: server.Name, ObservedAt: time.Now().UTC()}
	if server.Monitoring.Enabled {
		latest, err := monitoring.GetServerLatest(ctx, monitoringTarget(server), server.ReaderToken)
		if err != nil {
			// The agent is the only reporter on the host; failing to reach it is
			// positive evidence the server is unreachable.
			observation.ReachabilityKnown = true
			observation.Reachable = false
			return observation, nil
		}
		observation.ReachabilityKnown = true
		observation.Reachable = true
		if sampledFresh(latest.SampledAt) {
			observation.MetricsKnown = true
			observation.DiskUsedPercent = latest.DiskUsedPercent
		}
		return observation, nil
	}
	client, err := ssh.NewClient(ssh.ServerConfig{Host: server.Host, Port: int(server.Port), User: server.SSHUser, PrivateKey: server.PrivateKey})
	if err != nil {
		observation.ReachabilityKnown = true
		observation.Reachable = false
		return observation, nil
	}
	defer client.Close()
	// SSH answered, so the host is reachable either way. A missing docker is a
	// provisioning problem, not a disk or reachability signal.
	if err := client.DetectDocker(); err != nil {
		observation.ReachabilityKnown = true
		observation.Reachable = true
		return observation, nil
	}
	observation.ReachabilityKnown = true
	observation.Reachable = true
	return observation, nil
}

func observeService(ctx context.Context, serviceID string) (alerts.Observation, error) {
	service, err := db.GetServiceByID(ctx, serviceID)
	if err != nil {
		return alerts.Observation{}, err
	}
	observation := alerts.Observation{TargetID: service.ID, TargetName: service.Name, ObservedAt: time.Now().UTC()}
	inProgress, err := db.ServiceHasInProgressDeployment(ctx, service.ID)
	if err != nil {
		return observation, err
	}
	if inProgress {
		// Deploys intentionally replace containers. Freeze rule state until the
		// deployment settles so routine cutovers never produce false incidents.
		observation.Suppressed = true
		return observation, nil
	}
	// Nothing has ever run here, and no active deployment means nothing is
	// running: both are verified "not running", not a missing sample.
	if !service.HasDeployed {
		return verifiedStopped(observation), nil
	}
	deployment, config, err := db.GetActiveDeploymentConfig(ctx, service.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return verifiedStopped(observation), nil
		}
		return observation, err
	}
	server, err := db.GetServerWithKey(ctx, service.ServerID)
	if err != nil {
		return observation, err
	}
	containerName := ContainerNameForDeployment(config, deployment.ID)
	if server.Monitoring.Enabled {
		latest, err := monitoring.GetLatestAll(ctx, monitoringTarget(server), server.ReaderToken)
		if err != nil {
			observation.ReachabilityKnown = true
			observation.Reachable = false
			return observation, nil
		}
		observation.ReachabilityKnown = true
		observation.Reachable = true
		for _, point := range latest.Points {
			if point.DeploymentID != deployment.ID {
				continue
			}
			if !sampledFresh(point.SampledAt) {
				// Stale sample: the collector is stuck, not the service.
				return observation, nil
			}
			observation.ServiceStateKnown = true
			observation.ServiceRunning = point.State == "running"
			observation.MetricsKnown = true
			observation.CPUPercent = point.CPUPercent
			if point.MemoryLimitBytes > 0 {
				observation.MemoryPercent = float64(point.MemoryUsedBytes) * 100 / float64(point.MemoryLimitBytes)
			}
			return observation, nil
		}
		// No sample for this deployment yet: unknown, never "stopped".
		return observation, nil
	}
	client, err := ssh.NewClient(ssh.ServerConfig{Host: server.Host, Port: int(server.Port), User: server.SSHUser, PrivateKey: server.PrivateKey})
	if err != nil {
		observation.ReachabilityKnown = true
		observation.Reachable = false
		return observation, nil
	}
	defer client.Close()
	if err := client.DetectDocker(); err != nil {
		observation.ReachabilityKnown = true
		observation.Reachable = true
		return observation, nil
	}
	observation.ReachabilityKnown = true
	observation.Reachable = true
	status, err := client.Run(ctx, client.DockerBin()+" inspect --format '{{.State.Status}}' "+ssh.ShellQuote(containerName)+" 2>/dev/null || true")
	if err != nil {
		// Could not read the container state at all.
		return observation, nil
	}
	observation.ServiceStateKnown = true
	observation.ServiceRunning = strings.TrimSpace(status) == "running"
	if !observation.ServiceRunning {
		return observation, nil
	}
	stats, err := client.Run(ctx, client.DockerBin()+" stats --no-stream --format '{{.CPUPerc}}|{{.MemUsage}}' "+ssh.ShellQuote(containerName))
	if err != nil {
		// State is known, usage is not.
		return observation, nil
	}
	if parseServiceStats(&observation, stats) {
		observation.MetricsKnown = true
	}
	return observation, nil
}

func verifiedStopped(observation alerts.Observation) alerts.Observation {
	observation.ReachabilityKnown = true
	observation.Reachable = true
	observation.ServiceStateKnown = true
	observation.ServiceRunning = false
	return observation
}

func parseServiceStats(observation *alerts.Observation, value string) bool {
	parts := strings.SplitN(strings.TrimSpace(value), "|", 2)
	if len(parts) != 2 {
		return false
	}
	observation.CPUPercent, _ = strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(parts[0]), "%"), 64)
	memory := strings.Split(parts[1], " /")
	if len(memory) != 2 {
		return false
	}
	used, usedOK := parseMetricBytes(strings.TrimSpace(memory[0]))
	limit, limitOK := parseMetricBytes(strings.TrimSpace(memory[1]))
	if usedOK && limitOK && limit > 0 {
		observation.MemoryPercent = float64(used) * 100 / float64(limit)
	}
	return true
}

func parseMetricBytes(value string) (int64, bool) {
	units := []struct {
		suffix string
		factor float64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"kB", 1e3}, {"B", 1}}
	for _, unit := range units {
		if strings.HasSuffix(value, unit.suffix) {
			number, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(value, unit.suffix)), 64)
			if err != nil {
				return 0, false
			}
			return int64(number * unit.factor), true
		}
	}
	return 0, false
}
