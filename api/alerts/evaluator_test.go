package alerts

import (
	"testing"
	"time"
)

func TestTrackerWaitsBeforeStartingAndDeduplicates(t *testing.T) {
	tracker := NewTracker()
	rule := Rule{ID: "rule-1", Condition: ConditionCPUHigh, Threshold: 85, Duration: 10 * time.Minute}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	observation := Observation{TargetID: "svc-1", TargetName: "api", ReachabilityKnown: true, Reachable: true, MetricsKnown: true, CPUPercent: 92, ObservedAt: start}

	if got := tracker.Evaluate(rule, observation, false, time.Time{}, start).Kind; got != TransitionNone {
		t.Fatalf("first sample transition = %q, want none", got)
	}
	observation.ObservedAt = start.Add(5 * time.Minute)
	if got := tracker.Evaluate(rule, observation, false, time.Time{}, start.Add(5*time.Minute)).Kind; got != TransitionNone {
		t.Fatalf("early sample transition = %q, want none", got)
	}
	observation.ObservedAt = start.Add(10 * time.Minute)
	transition := tracker.Evaluate(rule, observation, false, time.Time{}, start.Add(10*time.Minute))
	if transition.Kind != TransitionStarted || !transition.Since.Equal(start) {
		t.Fatalf("start transition = %+v, want started at %s", transition, start)
	}
	if got := tracker.Evaluate(rule, observation, true, start, start.Add(20*time.Minute)).Kind; got != TransitionNone {
		t.Fatalf("active sample transition = %q, want none", got)
	}
}

func TestTrackerResolvesWhenConditionClears(t *testing.T) {
	tracker := NewTracker()
	rule := Rule{ID: "rule-1", Condition: ConditionMemoryHigh, Threshold: 90, Duration: time.Minute}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	transition := tracker.Evaluate(rule, Observation{TargetID: "svc-1", TargetName: "api", ReachabilityKnown: true, Reachable: true, MetricsKnown: true, MemoryPercent: 50}, true, now.Add(-5*time.Minute), now)
	if transition.Kind != TransitionResolved {
		t.Fatalf("recovery transition = %q, want resolved", transition.Kind)
	}
}

// TestTrackerIgnoresUnknownEvidence is the SZ-05 core: a failed, stale, or
// missing sample must neither start an incident nor resolve a live one.
func TestTrackerIgnoresUnknownEvidence(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		rule        Rule
		observation Observation
	}{
		{
			// Docker collector error / agent unreachable for CPU.
			name:        "resource collection failure",
			rule:        Rule{ID: "cpu", Condition: ConditionCPUHigh, Threshold: 85, Duration: 5 * time.Minute},
			observation: Observation{TargetID: "svc-1", TargetName: "api", ReachabilityKnown: true, Reachable: true, CPUPercent: 99, ObservedAt: start},
		},
		{
			// Agent replied but the sample aged out.
			name:        "stale service sample",
			rule:        Rule{ID: "mem", Condition: ConditionMemoryHigh, Threshold: 85, Duration: 5 * time.Minute},
			observation: Observation{TargetID: "svc-1", TargetName: "api", ReachabilityKnown: true, Reachable: true, MemoryPercent: 99, ObservedAt: start},
		},
		{
			// No point for the deployment at all.
			name:        "missing service sample",
			rule:        Rule{ID: "down", Condition: ConditionServiceDown, Duration: 5 * time.Minute},
			observation: Observation{TargetID: "svc-1", TargetName: "api", ObservedAt: start},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tracker := NewTracker()
			if got := tracker.Evaluate(tc.rule, tc.observation, false, time.Time{}, start).Kind; got != TransitionNone {
				t.Fatalf("unknown evidence on a clear target started %q", got)
			}
			if got := tracker.Evaluate(tc.rule, tc.observation, true, start.Add(-time.Hour), start.Add(time.Hour)).Kind; got != TransitionNone {
				t.Fatalf("unknown evidence resolved an active incident: %q", got)
			}
			if len(tracker.pending) != 0 {
				t.Fatalf("unknown evidence left pending state %#v", tracker.pending)
			}
		})
	}
}

// TestTrackerRequiresConsecutiveEvidence: an evidence gap must reset the quiet
// window, so a violation that is only sampled twice with a failure between
// never fires.
func TestTrackerRequiresConsecutiveEvidence(t *testing.T) {
	tracker := NewTracker()
	rule := Rule{ID: "cpu", Condition: ConditionCPUHigh, Threshold: 85, Duration: 3 * time.Minute}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	violating := Observation{TargetID: "svc-1", TargetName: "api", ReachabilityKnown: true, Reachable: true, MetricsKnown: true, CPUPercent: 99, ObservedAt: start}

	if got := tracker.Evaluate(rule, violating, false, time.Time{}, start).Kind; got != TransitionNone {
		t.Fatalf("first sample transition = %q", got)
	}
	unknown := Observation{TargetID: "svc-1", TargetName: "api", ReachabilityKnown: true, Reachable: true, ObservedAt: start.Add(2 * time.Minute)}
	if got := tracker.Evaluate(rule, unknown, false, time.Time{}, start.Add(2*time.Minute)).Kind; got != TransitionNone {
		t.Fatalf("unknown sample transition = %q", got)
	}
	violating.ObservedAt = start.Add(3 * time.Minute)
	if got := tracker.Evaluate(rule, violating, false, time.Time{}, start.Add(3*time.Minute)).Kind; got != TransitionNone {
		t.Fatalf("violation fired across an evidence gap: %q", got)
	}
}

func TestTrackerStartsVerifiedStoppedAndAgentUnreachable(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	down := NewTracker()
	downRule := Rule{ID: "down", Condition: ConditionServiceDown, Duration: time.Minute}
	stopped := Observation{TargetID: "svc-1", TargetName: "api", ReachabilityKnown: true, Reachable: true, ServiceStateKnown: true, ServiceRunning: false, ObservedAt: start}
	if got := down.Evaluate(downRule, stopped, false, time.Time{}, start).Kind; got != TransitionNone {
		t.Fatalf("first verified stopped sample = %q, want none", got)
	}
	stopped.ObservedAt = start.Add(time.Minute)
	if got := down.Evaluate(downRule, stopped, false, time.Time{}, start.Add(time.Minute)).Kind; got != TransitionStarted {
		t.Fatalf("verified stopped service transition = %q, want started", got)
	}

	unreachable := NewTracker()
	unreachableRule := Rule{ID: "unreachable", Condition: ConditionServerUnreachable, Duration: time.Minute}
	offline := Observation{TargetID: "srv-1", TargetName: "web", ReachabilityKnown: true, Reachable: false, ObservedAt: start}
	if got := unreachable.Evaluate(unreachableRule, offline, false, time.Time{}, start).Kind; got != TransitionNone {
		t.Fatalf("first offline sample = %q, want none", got)
	}
	offline.ObservedAt = start.Add(time.Minute)
	if got := unreachable.Evaluate(unreachableRule, offline, false, time.Time{}, start.Add(time.Minute)).Kind; got != TransitionStarted {
		t.Fatalf("known unreachable server transition = %q, want started", got)
	}
}

func TestTrackerResolvesOnlyOnFreshHealthyEvidence(t *testing.T) {
	tracker := NewTracker()
	rule := Rule{ID: "down", Condition: ConditionServiceDown, Duration: time.Minute}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	healthy := Observation{TargetID: "svc-1", TargetName: "api", ReachabilityKnown: true, Reachable: true, ServiceStateKnown: true, ServiceRunning: true, ObservedAt: start.Add(time.Hour)}
	if got := tracker.Evaluate(rule, healthy, true, start, start.Add(time.Hour)).Kind; got != TransitionResolved {
		t.Fatalf("fresh healthy sample transition = %q, want resolved", got)
	}
}

func TestValidateRule(t *testing.T) {
	if err := ValidateRule(Rule{Condition: ConditionServiceDown, Threshold: 1, Duration: 5 * time.Minute}); err == nil {
		t.Fatal("service down with threshold should be rejected")
	}
	if err := ValidateRule(Rule{Condition: ConditionCPUHigh, Threshold: 85, Duration: 5 * time.Minute}); err != nil {
		t.Fatalf("valid CPU rule rejected: %v", err)
	}
}
