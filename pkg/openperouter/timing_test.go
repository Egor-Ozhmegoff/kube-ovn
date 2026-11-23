package openperouter

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReconcileTiming(t *testing.T) {
	ctx, timing := WithReconcileTiming(t.Context())
	var workers sync.WaitGroup
	for range 26 {
		workers.Go(func() { defer Measure(ctx, "l2")() })
	}
	workers.Wait()
	first := timing.Snapshot()
	require.Equal(t, 26, first["l2"].Calls)
	require.Positive(t, first["l2"].Duration)
	delete(first, "l2")
	require.Equal(t, 26, timing.Snapshot()["l2"].Calls)
	Measure(context.Background(), "unobserved")()
	require.Len(t, timing.Snapshot(), 1)
}
