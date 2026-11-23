package openperouter

import (
	"context"
	"maps"
	"sync"
	"time"
)

type timingKey struct{}

// ReconcileTiming records inclusive wall time and call counts. Nested stages
// overlap: their durations must not be added to estimate the cycle duration.
type ReconcileTiming struct {
	mu     sync.Mutex
	stages map[string]StageTiming
}

type StageTiming struct {
	Calls    int           `json:"calls"`
	Duration time.Duration `json:"duration"`
}

func WithReconcileTiming(ctx context.Context) (context.Context, *ReconcileTiming) {
	timing := &ReconcileTiming{stages: make(map[string]StageTiming)}
	return context.WithValue(ctx, timingKey{}, timing), timing
}

// Measure returns a completion callback for a stage, including failed calls.
// Without a cycle collector it has no effect on callers outside the daemon.
func Measure(ctx context.Context, stage string) func() {
	timing, ok := ctx.Value(timingKey{}).(*ReconcileTiming)
	if !ok {
		return func() {}
	}
	started := time.Now()
	return func() {
		timing.mu.Lock()
		defer timing.mu.Unlock()
		value := timing.stages[stage]
		value.Calls++
		value.Duration += time.Since(started)
		timing.stages[stage] = value
	}
}

func (t *ReconcileTiming) Snapshot() map[string]StageTiming {
	t.mu.Lock()
	defer t.mu.Unlock()
	return maps.Clone(t.stages)
}
