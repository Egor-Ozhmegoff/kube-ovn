package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
)

func TestOpenPERouterVPCReconcileIsolation(t *testing.T) {
	a := &kubeovnv1.Vpc{Name: "a", UID: "a"}
	b := &kubeovnv1.Vpc{Name: "b", UID: "b"}
	for _, order := range [][]*kubeovnv1.Vpc{{b, a}, {a, b}} {
		open := map[string]bool{"a": true, "b": true}
		var synced, closed []string
		require.NoError(t, reconcileOpenPERouterVPCs(t.Context(), order, func(vpc *kubeovnv1.Vpc) error {
			synced = append(synced, vpc.Name)
			if vpc == b {
				return errors.New("return port or attachment unavailable")
			}
			open[vpc.Name] = true
			return nil
		}, func(ctx context.Context, uid string) error {
			require.NoError(t, ctx.Err())
			open[uid] = false
			closed = append(closed, uid)
			return nil
		}))
		require.ElementsMatch(t, []string{"a", "b"}, synced)
		require.Equal(t, []string{"b"}, closed)
		require.True(t, open["a"])
		require.False(t, open["b"], "previously opened B must close")
	}
}

func TestOpenPERouterVPCClosureFailureDoesNotCloseHealthyVPC(t *testing.T) {
	vpcs := []*kubeovnv1.Vpc{{Name: "b", UID: "b"}, {Name: "a", UID: "a"}}
	synced := false
	require.NoError(t, reconcileOpenPERouterVPCs(t.Context(), vpcs, func(vpc *kubeovnv1.Vpc) error {
		if vpc.Name == "b" {
			return errors.New("unprepared B")
		}
		synced = true
		return nil
	}, func(_ context.Context, uid string) error {
		require.Equal(t, "b", uid)
		return errors.New("B API guard close failed")
	}))
	require.True(t, synced)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, reconcileOpenPERouterVPCs(ctx, vpcs, func(*kubeovnv1.Vpc) error { t.Fatal("must not use expired common snapshot"); return nil }, func(context.Context, string) error { return nil }), context.Canceled)
}
