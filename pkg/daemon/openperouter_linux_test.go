package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
)

// Controlled expiry avoids scheduler-dependent sleeps. Production uses real
// WithTimeout; this factory retains cancellation propagation from daemon stop.
type controlledOpenPERouterBudget struct {
	context.Context
	expire context.CancelCauseFunc
}

func (c *controlledOpenPERouterBudget) Err() error {
	if errors.Is(context.Cause(c.Context), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return c.Context.Err()
}

type openPERouterTestClock struct {
	prepared, vpc, closing []*controlledOpenPERouterBudget
}

func (c *openPERouterTestClock) factory(parent context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	result := &controlledOpenPERouterBudget{Context: ctx, expire: cancel}
	switch limit {
	case time.Second:
		c.prepared = append(c.prepared, result)
	case 2 * time.Second:
		c.vpc = append(c.vpc, result)
	case 3 * time.Second:
		c.closing = append(c.closing, result)
	}
	return result, func() { cancel(context.Canceled) }
}

func testOpenPERouterRunner(t *testing.T, order string) (*openPERouterReconciler, *openPERouterTestClock, map[string]bool, *int) {
	t.Helper()
	clock := &openPERouterTestClock{}
	vpcs := []*kubeovnv1.Vpc{{Name: "a", UID: "a"}, {Name: "b", UID: "b"}}
	if order == "ba" {
		vpcs[0], vpcs[1] = vpcs[1], vpcs[0]
	}
	paths := map[string]bool{"a": true, "b": true}
	global := new(int)
	r := &openPERouterReconciler{budgets: openPERouterBudgets{prepare: time.Second, vpc: 2 * time.Second, close: 3 * time.Second}, timeout: clock.factory}
	r.prepare = func(ctx context.Context) ([]*kubeovnv1.Vpc, error) { require.NoError(t, ctx.Err()); return vpcs, nil }
	r.refresh = func(ctx context.Context) error { require.NoError(t, ctx.Err()); return nil }
	r.syncVpc = func(ctx context.Context, vpc *kubeovnv1.Vpc) error {
		require.NoError(t, ctx.Err())
		paths[vpc.Name] = true
		return nil
	}
	r.closeVpc = func(ctx context.Context, uid string) error {
		require.NoError(t, ctx.Err())
		paths[uid] = false
		return nil
	}
	r.closeAll = func(ctx context.Context) error {
		require.NoError(t, ctx.Err())
		*global++
		paths["a"], paths["b"] = false, false
		return nil
	}
	return r, clock, paths, global
}

func TestOpenPERouterTimeoutIsolation(t *testing.T) {
	for _, order := range []string{"ab", "ba"} {
		for _, failure := range []string{"slow-close", "vpc-timeout", "shared-read-at-budget-expiry"} {
			t.Run(order+"/"+failure, func(t *testing.T) {
				r, clock, paths, global := testOpenPERouterRunner(t, order)
				var synced []string
				r.syncVpc = func(ctx context.Context, vpc *kubeovnv1.Vpc) error {
					require.NoError(t, ctx.Err())
					synced = append(synced, vpc.Name)
					if vpc.Name == "a" {
						return nil
					}
					if failure != "slow-close" {
						clock.vpc[len(clock.vpc)-1].expire(context.DeadlineExceeded)
						if failure == "shared-read-at-budget-expiry" {
							return &openPERouterSharedError{err: ctx.Err()}
						}
						return ctx.Err()
					}
					return errors.New("return port not ready")
				}
				r.closeVpc = func(ctx context.Context, uid string) error {
					require.Equal(t, "b", uid)
					require.NoError(t, ctx.Err())
					paths[uid] = false
					if failure == "slow-close" {
						clock.closing[len(clock.closing)-1].expire(context.DeadlineExceeded)
						return ctx.Err()
					}
					return nil
				}
				r.once(t.Context()) // Exact common error handler used by runOpenPERouter.
				require.Zero(t, *global, "local budget exhaustion must not invoke global Fence/Sweep")
				require.ElementsMatch(t, []string{"a", "b"}, synced)
				require.True(t, paths["a"])
				require.False(t, paths["b"])
				require.Error(t, clock.prepared[0].Err(), "the completed common context is already cancelled")
				require.Equal(t, failure == "slow-close", r.pending["b"])
			})
		}
	}
}

func TestOpenPERouterFailedClosureRetriesWithoutGlobalFencing(t *testing.T) {
	r, _, paths, global := testOpenPERouterRunner(t, "ba")
	failing := true
	calls := map[string]int{}
	r.syncVpc = func(_ context.Context, vpc *kubeovnv1.Vpc) error {
		calls[vpc.Name]++
		if vpc.Name == "b" {
			return errors.New("not ready")
		}
		return nil
	}
	r.closeVpc = func(ctx context.Context, uid string) error {
		require.NoError(t, ctx.Err())
		require.Equal(t, "b", uid)
		if failing {
			return errors.New("fencing/API/withdrawal failed")
		}
		paths[uid] = false
		return nil
	}
	r.once(t.Context())
	require.True(t, r.pending["b"])
	r.once(t.Context())
	require.Equal(t, 1, calls["b"], "failed closure must be retried before any ordinary B reconcile")
	require.Equal(t, 2, calls["a"])
	require.True(t, paths["a"])
	require.Zero(t, *global)
	failing = false
	r.syncVpc = func(ctx context.Context, vpc *kubeovnv1.Vpc) error {
		require.NoError(t, ctx.Err())
		calls[vpc.Name]++
		return nil
	}
	r.once(t.Context())
	require.Empty(t, r.pending)
	require.Equal(t, 2, calls["b"])
	require.Zero(t, *global)
}

func TestOpenPERouterCommonFailureStillFencesAll(t *testing.T) {
	for _, failure := range []string{"prepare-timeout", "refresh-error", "shared-sync-error", "outage-during-local-timeout"} {
		t.Run(failure, func(t *testing.T) {
			r, clock, paths, global := testOpenPERouterRunner(t, "ba")
			synced := 0
			r.syncVpc = func(ctx context.Context, _ *kubeovnv1.Vpc) error {
				synced++
				if failure == "shared-sync-error" {
					return &openPERouterSharedError{err: errors.New("OVN connection failed before Open")}
				}
				clock.vpc[len(clock.vpc)-1].expire(context.DeadlineExceeded)
				return ctx.Err()
			}
			switch failure {
			case "prepare-timeout":
				r.prepare = func(ctx context.Context) ([]*kubeovnv1.Vpc, error) {
					clock.prepared[0].expire(context.DeadlineExceeded)
					return nil, ctx.Err()
				}
			case "shared-sync-error":
			case "refresh-error":
				r.refresh = func(context.Context) error { return errors.New("SB disconnected") }
			default:
				count := 0
				r.refresh = func(context.Context) error {
					count++
					if count > 1 {
						return errors.New("API unavailable on independent probe")
					}
					return nil
				}
			}
			r.once(t.Context())
			require.Equal(t, 1, *global)
			require.False(t, paths["a"])
			require.False(t, paths["b"])
			if failure != "outage-during-local-timeout" && failure != "shared-sync-error" {
				require.Zero(t, synced)
			} else {
				require.Equal(t, 1, synced)
			}
		})
	}
}

func TestOpenPERouterStopUsesIndependentCleanup(t *testing.T) {
	for _, phase := range []string{"prepare", "sync", "close", "between-cycles"} {
		t.Run(phase, func(t *testing.T) {
			r, _, _, global := testOpenPERouterRunner(t, "ba")
			ctx, stop := context.WithCancel(t.Context())
			defer stop()
			var synced []string
			prepare := r.prepare
			r.prepare = func(ctx context.Context) ([]*kubeovnv1.Vpc, error) {
				if phase == "prepare" {
					stop()
					return nil, ctx.Err()
				}
				return prepare(ctx)
			}
			r.syncVpc = func(ctx context.Context, vpc *kubeovnv1.Vpc) error {
				synced = append(synced, vpc.Name)
				if phase == "sync" {
					stop()
					return ctx.Err()
				}
				if phase == "close" {
					return errors.New("not ready")
				}
				if phase == "between-cycles" && vpc.Name == "a" {
					stop()
				}
				return nil
			}
			r.closeVpc = func(ctx context.Context, _ string) error {
				stop()
				require.NoError(t, ctx.Err(), "closure must survive daemon stop")
				return nil
			}
			r.run(ctx, make(chan time.Time), nil)
			require.Equal(t, 1, *global, "run must execute final cleanup with a fresh budget")
			if phase != "between-cycles" {
				require.NotContains(t, synced, "a", "no ordinary processing after stop")
			}
		})
	}
}

func TestOpenPERouterRealBudgetTimeout(t *testing.T) {
	r, _, paths, global := testOpenPERouterRunner(t, "ba")
	r.timeout = nil
	r.budgets = openPERouterBudgets{prepare: time.Second, vpc: 20 * time.Millisecond, close: time.Second}
	var synced []string
	r.syncVpc = func(ctx context.Context, vpc *kubeovnv1.Vpc) error {
		synced = append(synced, vpc.Name)
		if vpc.Name == "b" {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	r.once(t.Context())
	require.Equal(t, []string{"b", "a"}, synced)
	require.True(t, paths["a"])
	require.Zero(t, *global)
}

// Retain earlier component fixtures, but route them through the production
// error boundary as well, so unexpected global Fence/Sweep fails their tests.
func runOpenPERouterIsolation(ctx context.Context, vpcs []*kubeovnv1.Vpc, syncVpc func(*kubeovnv1.Vpc) error, closeVpc func(context.Context, string) error) error {
	var global error
	clock := &openPERouterTestClock{}
	r := &openPERouterReconciler{
		budgets: openPERouterBudgets{prepare: time.Second, vpc: 2 * time.Second, close: 3 * time.Second}, timeout: clock.factory,
		prepare: func(context.Context) ([]*kubeovnv1.Vpc, error) { return vpcs, nil }, refresh: func(context.Context) error { return nil },
		syncVpc: func(_ context.Context, vpc *kubeovnv1.Vpc) error { return syncVpc(vpc) }, closeVpc: func(ctx context.Context, uid string) error {
			err := closeVpc(ctx, uid)
			if uid == "b" {
				clock.closing[len(clock.closing)-1].expire(context.DeadlineExceeded)
			}
			return err
		},
		closeAll: func(context.Context) error { global = errors.New("unexpected global Fence/Sweep"); return nil },
	}
	r.once(ctx)
	return errors.Join(global, ctx.Err())
}

func TestOpenPERouterStopDuringInitialization(t *testing.T) {
	r, _, _, global := testOpenPERouterRunner(t, "ba")
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	r.prepare = func(context.Context) ([]*kubeovnv1.Vpc, error) {
		t.Fatal("must not prepare after stop")
		return nil, nil
	}
	r.run(ctx, make(chan time.Time), func(context.Context) bool { stop(); return false })
	require.Equal(t, 1, *global)
}

func TestOpenPERouterKnownGlobalFailureAtLocalExpiry(t *testing.T) {
	r, clock, paths, global := testOpenPERouterRunner(t, "ba")
	r.syncVpc = func(ctx context.Context, _ *kubeovnv1.Vpc) error {
		require.NoError(t, ctx.Err())
		clock.vpc[len(clock.vpc)-1].expire(context.DeadlineExceeded)
		return &openPERouterSharedError{err: errors.New("shared namespace or chassis generation lost")}
	}
	r.once(t.Context())
	require.Equal(t, 1, *global)
	require.False(t, paths["a"])
}

func TestOpenPERouterClosureReservesWithdrawalBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	fenceCtx, finishFence := openPERouterFenceBudget(ctx)
	defer finishFence()
	parentDeadline, _ := ctx.Deadline()
	fenceDeadline, _ := fenceCtx.Deadline()
	require.True(t, fenceDeadline.Before(parentDeadline), "fencing/ACK must not consume the route-withdrawal allowance")
	finishFence()
	require.ErrorIs(t, fenceCtx.Err(), context.Canceled)
	require.NoError(t, ctx.Err(), "withdrawal uses the still-live outer closure context")
}
