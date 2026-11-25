package daemon

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"k8s.io/klog/v2"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
)

type openPERouterBudgets struct{ prepare, vpc, close time.Duration }

var defaultOpenPERouterBudgets = openPERouterBudgets{prepare: 4 * time.Second, vpc: 4 * time.Second, close: 3 * time.Second}

type openPERouterSharedError struct{ err error }

func (e *openPERouterSharedError) Error() string { return e.err.Error() }
func (e *openPERouterSharedError) Unwrap() error { return e.err }

// This runner is also the runOpenPERouter error boundary. Only failure of a
// common source/preparation reaches cleanupAll. No VPC child deadline is ever
// used as the parent of another VPC or of closure/recovery work.
type openPERouterReconciler struct {
	budgets  openPERouterBudgets
	prepare  func(context.Context) ([]*kubeovnv1.Vpc, error)
	refresh  func(context.Context) error
	syncVpc  func(context.Context, *kubeovnv1.Vpc) error
	closeVpc func(context.Context, string) error
	closeAll func(context.Context) error
	pending  map[string]bool
	timeout  func(context.Context, time.Duration) (context.Context, context.CancelFunc)
}

func (r *openPERouterReconciler) budget(ctx context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
	if r.timeout != nil {
		return r.timeout(ctx, limit)
	}
	return context.WithTimeout(ctx, limit)
}

func (r *openPERouterReconciler) run(ctx context.Context, ticks <-chan time.Time, initialize func(context.Context) bool) {
	defer r.cleanupAll(ctx)
	if ctx.Err() != nil {
		return
	}
	if initialize != nil && !initialize(ctx) {
		return
	}
	for ctx.Err() == nil {
		r.once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
	}
}

func (r *openPERouterReconciler) once(ctx context.Context) {
	if err := r.cycle(ctx); err != nil && ctx.Err() == nil {
		klog.ErrorS(err, "Reconcile common OpenPERouter state; close all local paths")
		r.cleanupAll(ctx)
	}
}

func (r *openPERouterReconciler) cleanupAll(ctx context.Context) {
	closeCtx, cancel := r.budget(context.WithoutCancel(ctx), r.budgets.close)
	defer cancel()
	if err := errors.Join(r.closeAll(closeCtx), closeCtx.Err()); err != nil {
		klog.ErrorS(err, "Close all OpenPERouter paths/guards/routes")
	}
}

func (r *openPERouterReconciler) refreshBounded(ctx context.Context) error {
	refreshCtx, cancel := r.budget(ctx, r.budgets.prepare)
	defer cancel()
	return errors.Join(r.refresh(refreshCtx), refreshCtx.Err())
}

func (r *openPERouterReconciler) closeOne(ctx context.Context, uid string) {
	if r.pending == nil {
		r.pending = make(map[string]bool)
	}
	r.pending[uid] = true
	closeCtx, cancel := r.budget(context.WithoutCancel(ctx), r.budgets.close)
	defer cancel()
	if err := errors.Join(r.closeVpc(closeCtx, uid), closeCtx.Err()); err != nil {
		klog.ErrorS(err, "Close OpenPERouter VPC paths/guards/routes; retry next cycle", "uid", uid)
		return
	}
	delete(r.pending, uid)
}

func (r *openPERouterReconciler) cycle(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	prepareCtx, cancel := r.budget(ctx, r.budgets.prepare)
	vpcs, err := r.prepare(prepareCtx)
	err = errors.Join(err, prepareCtx.Err())
	cancel()
	if err != nil {
		return err
	}
	for _, uid := range slices.Sorted(maps.Keys(r.pending)) {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.closeOne(ctx, uid)
	}
	for _, vpc := range vpcs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if r.pending[string(vpc.UID)] {
			continue
		}
		// Never reuse a preceding VPC's snapshot or budget, even in a fast cycle.
		if err := r.refreshBounded(ctx); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		vpcCtx, cancel := r.budget(ctx, r.budgets.vpc)
		err := r.syncVpc(vpcCtx, vpc)
		timedOut := vpcCtx.Err() != nil
		err = errors.Join(err, vpcCtx.Err())
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var shared *openPERouterSharedError
		sharedFailure := errors.As(err, &shared)
		ambiguous := sharedFailure && (errors.Is(shared.err, context.DeadlineExceeded) || errors.Is(shared.err, context.Canceled))
		if sharedFailure && !ambiguous {
			return err
		}
		if err != nil {
			klog.ErrorS(err, "Reconcile OpenPERouter VPC; close only its local paths", "vpc", vpc.Name)
			r.closeOne(ctx, string(vpc.UID))
		}
		// A deadline while consulting a common source is ambiguous. Probe shared
		// state with its own full budget; do not call it a cluster failure merely
		// because this VPC had no time left, or hide a real infrastructure outage.
		if timedOut || ambiguous {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := r.refreshBounded(ctx); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}
