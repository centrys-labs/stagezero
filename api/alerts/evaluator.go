package alerts

import (
	"fmt"
	"sync"
	"time"
)

const (
	ConditionCPUHigh           = "cpu_high"
	ConditionMemoryHigh        = "memory_high"
	ConditionDiskLow           = "disk_low"
	ConditionServiceDown       = "service_down"
	ConditionServerUnreachable = "server_unreachable"
)

type Rule struct {
	ID          string
	WorkspaceID string
	Name        string
	Condition   string
	Threshold   float64
	Duration    time.Duration
}

// Observation is a single probe of a target. The *Known flags say whether the
// paired value is a verified fact or just a zero default. A collection failure
// leaves them false, and unknown evidence must never be read as "recovered".
type Observation struct {
	TargetID   string
	TargetName string
	Suppressed bool

	// ReachabilityKnown reports that the target was positively reached or not.
	ReachabilityKnown bool
	Reachable         bool

	// ServiceStateKnown reports that Docker returned a current container state.
	ServiceStateKnown bool
	ServiceRunning    bool

	// MetricsKnown reports that a fresh, complete usage sample backs the
	// CPU/memory/disk percentages.
	MetricsKnown bool

	CPUPercent      float64
	MemoryPercent   float64
	DiskUsedPercent float64
	ObservedAt      time.Time
}

type TransitionKind string

const (
	TransitionNone     TransitionKind = "none"
	TransitionStarted  TransitionKind = "started"
	TransitionResolved TransitionKind = "resolved"
)

type Transition struct {
	Kind       TransitionKind
	Since      time.Time
	Value      float64
	TargetID   string
	TargetName string
}

type pendingState struct {
	since time.Time
	value float64
}

// Tracker keeps only the quiet-period state. Fired incidents are persisted in
// Postgres, so a restart never causes a second notification for the same issue.
type Tracker struct {
	mu      sync.Mutex
	pending map[string]pendingState
}

func NewTracker() *Tracker {
	return &Tracker{pending: make(map[string]pendingState)}
}

func (t *Tracker) Evaluate(rule Rule, observation Observation, active bool, activeSince time.Time, now time.Time) Transition {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = now
	}
	if observation.Suppressed {
		return Transition{Kind: TransitionNone, TargetID: observation.TargetID, TargetName: observation.TargetName}
	}
	key := rule.ID + "\x00" + observation.TargetID
	if !evidenceKnown(rule.Condition, observation) {
		// A failed or stale collection is not evidence of anything. Clearing the
		// pending window stops non-consecutive samples counting as one quiet
		// period, and returning none leaves any active incident open until a
		// verified healthy sample closes it.
		t.mu.Lock()
		delete(t.pending, key)
		t.mu.Unlock()
		return Transition{Kind: TransitionNone, TargetID: observation.TargetID, TargetName: observation.TargetName}
	}
	violated, value := violation(rule, observation)

	t.mu.Lock()
	defer t.mu.Unlock()
	if !violated {
		delete(t.pending, key)
		if active {
			return Transition{Kind: TransitionResolved, Since: activeSince, Value: value, TargetID: observation.TargetID, TargetName: observation.TargetName}
		}
		return Transition{Kind: TransitionNone, TargetID: observation.TargetID, TargetName: observation.TargetName}
	}

	if active {
		delete(t.pending, key)
		return Transition{Kind: TransitionNone, Value: value, TargetID: observation.TargetID, TargetName: observation.TargetName}
	}

	pending, found := t.pending[key]
	if !found {
		t.pending[key] = pendingState{since: observation.ObservedAt, value: value}
		return Transition{Kind: TransitionNone, Since: observation.ObservedAt, Value: value, TargetID: observation.TargetID, TargetName: observation.TargetName}
	}
	if rule.Duration <= 0 || !now.Before(pending.since.Add(rule.Duration)) {
		delete(t.pending, key)
		return Transition{Kind: TransitionStarted, Since: pending.since, Value: value, TargetID: observation.TargetID, TargetName: observation.TargetName}
	}
	pending.value = value
	t.pending[key] = pending
	return Transition{Kind: TransitionNone, Since: pending.since, Value: value, TargetID: observation.TargetID, TargetName: observation.TargetName}
}

// evidenceKnown reports whether the observation carries verified evidence for
// the rule's condition.
func evidenceKnown(condition string, observation Observation) bool {
	switch condition {
	case ConditionCPUHigh, ConditionMemoryHigh, ConditionDiskLow:
		return observation.MetricsKnown
	case ConditionServiceDown:
		return observation.ServiceStateKnown
	case ConditionServerUnreachable:
		return observation.ReachabilityKnown
	default:
		return false
	}
}

func violation(rule Rule, observation Observation) (bool, float64) {
	switch rule.Condition {
	case ConditionCPUHigh:
		return observation.Reachable && observation.CPUPercent >= rule.Threshold, observation.CPUPercent
	case ConditionMemoryHigh:
		return observation.Reachable && observation.MemoryPercent >= rule.Threshold, observation.MemoryPercent
	case ConditionDiskLow:
		return observation.Reachable && observation.DiskUsedPercent >= rule.Threshold, observation.DiskUsedPercent
	case ConditionServiceDown:
		return observation.Reachable && !observation.ServiceRunning, 0
	case ConditionServerUnreachable:
		return !observation.Reachable, 0
	default:
		return false, 0
	}
}

func ValidateRule(rule Rule) error {
	switch rule.Condition {
	case ConditionCPUHigh, ConditionMemoryHigh, ConditionDiskLow:
		if rule.Threshold <= 0 || rule.Threshold > 100 {
			return fmt.Errorf("threshold must be greater than 0 and at most 100 for %s", rule.Condition)
		}
	case ConditionServiceDown, ConditionServerUnreachable:
		if rule.Threshold != 0 {
			return fmt.Errorf("threshold must be zero for %s", rule.Condition)
		}
	default:
		return fmt.Errorf("unsupported alert condition %q", rule.Condition)
	}
	if rule.Duration < time.Minute || rule.Duration > 30*24*time.Hour {
		return fmt.Errorf("duration must be between 60 and 2592000 seconds")
	}
	return nil
}
