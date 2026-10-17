package source

import (
	"sync"
	"time"
)

type AnalysisLimits struct {
	RequestBytes   int64
	TarballBytes   int64
	ExtractedBytes int64
	OutputBytes    int64
	Entries        int
	RatePerMinute  int
	Concurrency    int
	Timeout        time.Duration
}

func DefaultAnalysisLimits() AnalysisLimits {
	return AnalysisLimits{
		RequestBytes: 16 << 10, TarballBytes: 100 << 20, ExtractedBytes: 512 << 20,
		OutputBytes: 1 << 20,
		Entries:     20000, RatePerMinute: 6, Concurrency: 2, Timeout: 90 * time.Second,
	}
}

// AnalysisGate bounds work across every session in one API process. The API
// deployment must run one replica unless an ingress enforces shared limits.
type AnalysisGate struct {
	mu     sync.Mutex
	used   map[string][]time.Time
	active chan struct{}
	limit  int
}

func NewAnalysisGate(limits AnalysisLimits) *AnalysisGate {
	return &AnalysisGate{used: make(map[string][]time.Time), active: make(chan struct{}, limits.Concurrency), limit: limits.RatePerMinute}
}

// Enter charges a user even when the global slots are full, preventing a
// caller from spinning on 429 responses until another analysis completes.
func (g *AnalysisGate) Enter(userID string) (release func(), allowed bool) {
	g.mu.Lock()
	now := time.Now()
	for user, attempts := range g.used {
		valid := attempts[:0]
		for _, at := range attempts {
			if now.Sub(at) < time.Minute {
				valid = append(valid, at)
			}
		}
		if len(valid) == 0 {
			delete(g.used, user)
		} else {
			g.used[user] = valid
		}
	}
	if len(g.used[userID]) >= g.limit {
		g.mu.Unlock()
		return nil, false
	}
	g.used[userID] = append(g.used[userID], now)
	g.mu.Unlock()
	select {
	case g.active <- struct{}{}:
		return func() { <-g.active }, true
	default:
		return nil, false
	}
}
