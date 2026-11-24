package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/openperouter"
	"github.com/kubeovn/kube-ovn/pkg/ovs"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnsb"
)

func testOpenPERouterSnapshot(t *testing.T) *openPERouterSnapshot {
	t.Helper()
	spec := &kubeovnv1.OpenPERouterIntegration{Mode: openperouter.Mode, VRF: "tenant-a", L3VNIName: "tenant-a", L3VNI: 10001, TransitCIDRs: []string{"169.254.32.0/20"}, HostSessionCIDRs: []string{"169.254.16.0/20"}}
	vpc := &kubeovnv1.Vpc{Name: "a", UID: "a", Spec: kubeovnv1.VpcSpec{OpenPERouter: spec}}
	subnet := &kubeovnv1.Subnet{Name: "pods", UID: "subnet", Spec: kubeovnv1.SubnetSpec{Vpc: "a", CIDRBlock: "10.101.3.0/24"}}
	subnet.Status.Ready("Ready", "fixture")
	bootstrap := &openperouter.Bootstrap{Index: 11, RouterMAC: "02:00:00:01:00:0b", L3VNIs: []openperouter.NativeL3VNI{{Name: "tenant-a", VRF: "tenant-a", VNI: 10001}}, Attachments: []openperouter.LocalL3Attachment{{L3VNIName: "tenant-a", LocalCIDRs: spec.HostSessionCIDRs}}}
	node, err := openperouter.BuildNode("a", "node", 11, spec)
	require.NoError(t, err)
	return &openPERouterSnapshot{vpcs: []*kubeovnv1.Vpc{vpc}, subnets: []*kubeovnv1.Subnet{subnet}, nodeUID: "node", bootstrap: bootstrap, bound: map[string]ovnsb.PortBinding{node.ReturnPort: {}}, installed: map[string]bool{node.ReturnPort: true}}
}

func TestOpenPERouterFreshSnapshotGate(t *testing.T) {
	for _, change := range []string{"missing-binding", "missing-interface", "new-vpc-uid", "changed-subnet", "node-generation", "reader-generation", "status-counter-only"} {
		t.Run(change, func(t *testing.T) {
			old, fresh := testOpenPERouterSnapshot(t), testOpenPERouterSnapshot(t)
			switch change {
			case "missing-binding":
				clear(fresh.bound)
			case "missing-interface":
				clear(fresh.installed)
			case "new-vpc-uid":
				fresh.vpcs[0].UID = "replacement"
			case "changed-subnet":
				fresh.subnets[0].Spec.CIDRBlock = "10.102.0.0/24"
			case "node-generation":
				fresh.nodeUID = "new-node"
			case "reader-generation":
				fresh.reader = new(ovs.LocalBindingClient)
			case "status-counter-only":
				fresh.subnets[0].ResourceVersion = "new-rv"
				fresh.vpcs[0].ResourceVersion = "new-rv"
			}
			err := validateOpenPERouterSnapshot(old, fresh, old.vpcs[0], "node")
			if change == "status-counter-only" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			var shared *openPERouterSharedError
			require.Equal(t, change == "node-generation" || change == "reader-generation", errors.As(err, &shared))
		})
	}
}

func TestOpenPERouterSlowBRefreshesABeforeOpen(t *testing.T) {
	r, _, paths, global := testOpenPERouterRunner(t, "ba")
	old, current := testOpenPERouterSnapshot(t), testOpenPERouterSnapshot(t)
	stale := false
	refreshed := false
	opened := false
	r.refresh = func(ctx context.Context) error {
		require.NoError(t, ctx.Err())
		if stale {
			clear(current.installed)
			refreshed = true
		}
		return nil
	}
	r.syncVpc = func(ctx context.Context, vpc *kubeovnv1.Vpc) error {
		require.NoError(t, ctx.Err())
		if vpc.Name == "b" {
			return errors.New("unprepared B")
		}
		require.True(t, refreshed, "fresh common state must be read after B closure, not reused from initial preparation")
		if err := validateOpenPERouterSnapshot(old, current, old.vpcs[0], "node"); err != nil {
			return err
		}
		opened = true
		return nil
	}
	r.closeVpc = func(ctx context.Context, uid string) error {
		require.NoError(t, ctx.Err())
		paths[uid] = false
		if uid == "b" {
			stale = true
		}
		return nil
	}
	r.once(t.Context())
	require.False(t, opened)
	require.False(t, paths["a"], "A whose real readiness disappeared must close, not open from old snapshot")
	require.Zero(t, *global, "the missing A return VIF is still a scoped failure")
}

func TestOpenPERouterSnapshotRejectsDeletingSubnet(t *testing.T) {
	old, fresh := testOpenPERouterSnapshot(t), testOpenPERouterSnapshot(t)
	now := metav1.Now()
	fresh.subnets[0].DeletionTimestamp = &now
	require.Error(t, validateOpenPERouterSnapshot(old, fresh, old.vpcs[0], "node"))
}
