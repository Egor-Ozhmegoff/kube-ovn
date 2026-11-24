package host

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/openperouter"
	"github.com/kubeovn/kube-ovn/pkg/ovs"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/table"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/vswitch"
)

func TestCleanupDisappearingVRFSlave(t *testing.T) {
	for _, tt := range []struct {
		name                string
		closeErr, detachErr error
		wantErr             error
		wantCalls           []string
	}{
		{name: "present", wantCalls: []string{"close", "detach"}},
		{name: "gone before close", closeErr: unix.ENODEV, wantCalls: []string{"close"}},
		{name: "gone after close", detachErr: unix.ENODEV, wantCalls: []string{"close", "detach"}},
		{name: "cannot close", closeErr: unix.EPERM, wantErr: unix.EPERM, wantCalls: []string{"close"}},
		{name: "cannot detach", detachErr: unix.EPERM, wantErr: unix.EPERM, wantCalls: []string{"close", "detach"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls []string
			err := closeAndDetachVRFSlave(func() error {
				calls = append(calls, "close")
				return tt.closeErr
			}, func() error {
				calls = append(calls, "detach")
				return tt.detachErr
			})
			require.ErrorIs(t, err, tt.wantErr)
			require.Equal(t, tt.wantCalls, calls)
		})
	}
}

func TestCleanupInternalPortsAndRecreateVPC(t *testing.T) {
	provider := kernelOVS(t)
	ctx := t.Context()
	spec := kernelIntegration("tenant-a", 10001)
	vpc := &kubeovnv1.Vpc{Name: "vpc", UID: "old-uid", Spec: kubeovnv1.VpcSpec{OpenPERouter: spec}}
	bootstrap := &openperouter.Bootstrap{Index: 11, Attachments: []openperouter.LocalL3Attachment{{L3VNIName: spec.L3VNIName, LocalCIDRs: spec.HostSessionCIDRs}}, L3VNIs: []openperouter.NativeL3VNI{{Name: spec.L3VNIName, VRF: spec.VRF, VNI: spec.L3VNI}}}
	node, err := openperouter.BuildNode(vpc.Name, "node-a", bootstrap.Index, spec)
	require.NoError(t, err)
	runIP(t, "link", "add", node.HostInterface, "type", "veth", "peer", "name", "native-peer")
	runIP(t, "link", "set", node.HostInterface, "up")
	for _, address := range node.HostAddresses {
		runIP(t, "address", "add", address, "dev", node.HostInterface)
	}
	first, err := Ensure(ctx, provider, vpc, "node-a", bootstrap, 1400, []string{"10.101.3.0/24"})
	require.NoError(t, err)
	require.Zero(t, first.Egress.Attrs().Flags&net.FlagUp, "new egress must remain closed before PE routes/readback")
	require.NoError(t, first.Open())
	healthy, err := Ensure(ctx, provider, vpc, "node-a", bootstrap, 1400, []string{"10.101.3.0/24"})
	require.NoError(t, err)
	require.NotZero(t, healthy.Egress.Attrs().Flags&net.FlagUp, "healthy no-op reconcile must not flap egress")
	require.NoError(t, Fence(ctx, provider, string(vpc.UID)))
	requireOVSOwner(t, provider, first.Node.EgressInterface, "old-uid")
	requireOVSOwner(t, provider, first.Node.ReturnInterface, "old-uid")
	_, err = Ensure(ctx, provider, vpc, "node-a", bootstrap, 1400, []string{"10.101.3.0/24"})
	require.NoError(t, err, "Fence must support recovery without losing the owner")
	broken := &failDeleteProvider{Provider: provider, fail: true}
	require.ErrorContains(t, Cleanup(ctx, broken, nil, nil), "injected deletion failure")
	requireCleanup(t, provider, nil, nil)
	requireCleanup(t, provider, nil, nil)
	for _, name := range []string{first.Node.EgressInterface, first.Node.ReturnInterface} {
		require.Eventually(t, func() bool {
			i := &vswitch.Interface{Name: name}
			p := &vswitch.Port{Name: name}
			return errors.Is(provider.Table(i).Get(ctx, i), table.ErrNotFound) && errors.Is(provider.Table(p).Get(ctx, p), table.ErrNotFound)
		}, time.Second, 10*time.Millisecond)
	}
	_, err = netlink.LinkByName(node.HostInterface)
	require.NoError(t, err, "native veth must survive Kube-OVN cleanup")
	runIP(t, "link", "set", node.HostInterface, "up")
	vpc.UID = "new-uid"
	_, err = Ensure(ctx, provider, vpc, "node-a", bootstrap, 1400, []string{"10.101.3.0/24"})
	require.NoError(t, err, "same L3VNI can be reused by a new VPC UID")
	requireOVSOwner(t, provider, node.EgressInterface, "new-uid")
	requireOVSOwner(t, provider, node.ReturnInterface, "new-uid")
	requireCleanup(t, provider, nil, nil)
	runIP(t, "link", "del", node.HostInterface)
}

func TestL2VNIDeleteAndReuseOtherVRF(t *testing.T) {
	provider := kernelOVS(t)
	ctx := t.Context()
	vpc := &kubeovnv1.Vpc{Name: "vpc-a", UID: "vpc-a", Spec: kubeovnv1.VpcSpec{OpenPERouter: kernelIntegration("tenant-a", 10001)}}
	subnet := &kubeovnv1.Subnet{Name: "blue", UID: "subnet-a", Spec: kubeovnv1.SubnetSpec{Vpc: vpc.Name, CIDRBlock: "10.101.1.0/24", Gateway: "10.101.1.1", OpenPERouterL2: &kubeovnv1.OpenPERouterL2Attachment{L2VNIName: "blue", VNI: 10101, GatewayMAC: "02:00:05:00:03:00"}}}
	native := "host-e-10101"
	runIP(t, "link", "add", native, "type", "veth", "peer", "name", "pe-l2")
	bootstrap := l2Bootstrap(vpc)
	first, err := EnsureL2(ctx, provider, vpc, subnet, bootstrap, 1400, []string{"02:00:00:00:01:11"})
	require.NoError(t, err)
	require.NoError(t, first.Open())
	requireLookup(t, native, 110001)
	conflicting := vpc.DeepCopy()
	conflicting.UID = "other-owner"
	conflicting.Spec.OpenPERouter = kernelIntegration("tenant-b", 10002)
	_, err = EnsureL2(ctx, provider, conflicting, subnet, l2Bootstrap(conflicting), 1400, []string{"02:00:00:00:01:11"})
	require.ErrorContains(t, err, "foreign L2 binding", "a live L2VNI cannot move to another VRF")
	requireLookup(t, native, 110001)
	broken := &failDeleteProvider{Provider: provider, fail: true}
	require.ErrorContains(t, Cleanup(ctx, broken, map[string]bool{"vpc-a": true}, nil), "injected deletion failure")
	requireCleanup(t, provider, map[string]bool{"vpc-a": true}, nil)
	requireCleanup(t, provider, map[string]bool{"vpc-a": true}, nil)
	link, err := netlink.LinkByName(native)
	require.NoError(t, err, "native owner, not Kube-OVN, deletes this interface")
	require.Zero(t, link.Attrs().MasterIndex)
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		rules, err := netlink.RuleList(family)
		require.NoError(t, err)
		for _, rule := range rules {
			require.False(t, rule.IifName == native && rule.Priority == 900, "old lookup must be removed")
		}
	}
	runIP(t, "link", "del", native) // Simulate native configuration removal.
	runIP(t, "link", "add", native, "type", "veth", "peer", "name", "pe-l2")
	vpc.Name = "vpc-b"
	vpc.UID = "vpc-b"
	vpc.Spec.OpenPERouter = kernelIntegration("tenant-b", 10002)
	subnet.UID = "subnet-b"
	subnet.Spec.Vpc = vpc.Name
	bootstrap = l2Bootstrap(vpc)
	second, err := EnsureL2(ctx, provider, vpc, subnet, bootstrap, 1400, []string{"02:00:00:00:01:12"})
	require.NoError(t, err)
	require.NoError(t, second.Open())
	requireLookup(t, native, 110002)
	second, err = EnsureL2(ctx, provider, vpc, subnet, bootstrap, 1400, []string{"02:00:00:00:01:12"})
	require.NoError(t, err)
	require.NoError(t, second.Open())
	requireLookup(t, native, 110002)
	guardIndex := second.Guard.Attrs().Index
	runIP(t, "link", "del", native) // Native owner recreates its leg while our hook stays on the private guard.
	runIP(t, "link", "add", native, "type", "veth", "peer", "name", "pe-l2")
	second, err = EnsureL2(ctx, provider, vpc, subnet, bootstrap, 1400, []string{"02:00:00:00:01:12"})
	require.NoError(t, err)
	require.Equal(t, guardIndex, second.Guard.Attrs().Index)
	require.NoError(t, second.Open())
	requireLookup(t, native, 110002)
	requireCleanup(t, provider, nil, nil)
	runIP(t, "link", "del", native)
}

func TestRebindKeepsTerminalDropOnPartialFailure(t *testing.T) {
	requireDisposable(t)
	runIP(t, "link", "add", "host-e-10101", "type", "veth", "peer", "name", "pe-l2")
	require.NoError(t, EnsureIsolation("host-e-10101", 110001))
	// A foreign IPv6 lookup causes failure after IPv4 progress. It is never deleted.
	rules, err := netlink.RuleList(netlink.FAMILY_V6)
	require.NoError(t, err)
	for _, rule := range rules {
		if rule.Priority == 900 && rule.IifName == "host-e-10101" {
			own := isolationRules(rule.IifName, 110001, netlink.FAMILY_V6)[1]
			require.NoError(t, netlink.RuleDel(own))
		}
	}
	foreign := isolationRules("host-e-10101", 9999, netlink.FAMILY_V6)[1]
	foreign.Protocol = 99
	require.NoError(t, netlink.RuleAdd(foreign))
	require.ErrorContains(t, RebindIsolation("host-e-10101", 110002), "foreign lookup")
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		rules, err := netlink.RuleList(family)
		require.NoError(t, err)
		protected := false
		for _, rule := range rules {
			if rule.IifName == "host-e-10101" && rule.Priority == 899 && rule.Protocol == routeProtocol {
				protected = true
			}
		}
		require.True(t, protected, "temporary terminal drop must survive partial failure")
	}
	require.NoError(t, netlink.RuleDel(foreign)) // Only its owner removes the foreign fixture rule.
	require.NoError(t, RebindIsolation("host-e-10101", 110002))
	require.NoError(t, finishIsolation("host-e-10101"))
	requireLookup(t, "host-e-10101", 110002)
	runIP(t, "link", "del", "host-e-10101")
}

func kernelIntegration(vrf string, vni int32) *kubeovnv1.OpenPERouterIntegration {
	return &kubeovnv1.OpenPERouterIntegration{Mode: openperouter.Mode, VRF: vrf, L3VNIName: vrf, L3VNI: vni, TransitCIDRs: []string{"169.254.32.0/20"}, HostSessionCIDRs: []string{"169.254.16.0/20"}}
}

func l2Bootstrap(vpc *kubeovnv1.Vpc) *openperouter.Bootstrap {
	l2 := openperouter.NativeL2VNI{Name: "blue", VNI: 10101, GatewayMAC: "02:00:05:00:03:00", GatewayIPs: []string{"10.101.1.1/24"}}
	l2.RoutingDomain.Type = "L3VNI"
	l2.RoutingDomain.L3VNI.Name = vpc.Spec.OpenPERouter.L3VNIName
	return &openperouter.Bootstrap{L2VNIs: []openperouter.NativeL2VNI{l2}}
}

func requireLookup(t *testing.T, name string, tableID int) {
	t.Helper()
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		rules, err := netlink.RuleList(family)
		require.NoError(t, err)
		lookup, terminal := false, false
		for _, rule := range rules {
			if rule.IifName != name {
				continue
			}
			if rule.Priority == 900 {
				require.Equal(t, tableID, rule.Table)
				lookup = true
			}
			if rule.Priority == 901 && rule.Protocol == routeProtocol {
				terminal = true
			}
		}
		require.True(t, lookup)
		require.True(t, terminal)
	}
}

func requireOVSOwner(t *testing.T, provider table.Provider, name, uid string) {
	t.Helper()
	require.Eventually(t, func() bool {
		i := &vswitch.Interface{Name: name}
		p := &vswitch.Port{Name: name}
		return provider.Table(i).Get(t.Context(), i) == nil && provider.Table(p).Get(t.Context(), p) == nil && i.ExternalIDs[openperouter.OwnerKey+"-uid"] == uid && p.ExternalIDs[openperouter.OwnerKey+"-uid"] == uid
	}, time.Second, 10*time.Millisecond)
}

func requireDisposable(t *testing.T) {
	t.Helper()
	if os.Getenv("KUBE_OVN_DISPOSABLE_LAB") != "true" {
		t.Skip("requires privileged disposable network-none container")
	}
	_, err := os.Stat("/.dockerenv")
	require.NoError(t, err)
	require.Zero(t, os.Geteuid())
}

func runIP(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "ip", args...).CombinedOutput()
	require.NoError(t, err, string(out))
}

func kernelOVS(t *testing.T) *ovs.VswitchClient {
	t.Helper()
	requireDisposable(t)
	dir := t.TempDir()
	socket := "/var/run/openvswitch/db.sock"
	require.NoError(t, os.MkdirAll(filepath.Dir(socket), 0o755))
	commands := [][]string{{"ovsdb-tool", "create", dir + "/ovs.db", "/usr/share/openvswitch/vswitch.ovsschema"}, {"ovsdb-server", "--detach", "--pidfile=" + dir + "/db.pid", "--unixctl=" + dir + "/db.ctl", "--remote=punix:" + socket, dir + "/ovs.db"}, {"ovs-vsctl", "--no-wait", "init"}, {"ovs-vswitchd", "--detach", "--pidfile=" + dir + "/vs.pid", "--unixctl=" + dir + "/vs.ctl", "unix:" + socket}, {"ovs-vsctl", "add-br", "br-int"}}
	for _, args := range commands {
		out, err := exec.CommandContext(t.Context(), args[0], args[1:]...).CombinedOutput()
		require.NoError(t, err, string(out))
	}
	client, err := ovs.NewVswitchClient("unix:"+socket, 5, 5)
	require.NoError(t, err)
	t.Cleanup(func() {
		client.Close()
		for _, ctl := range []string{"vs", "db"} {
			out, err := exec.Command("ovs-appctl", "-t", dir+"/"+ctl+".ctl", "exit").CombinedOutput()
			require.NoError(t, err, string(out))
		}
	})
	return client
}

type failDeleteProvider struct {
	table.Provider
	fail bool
}
type failDeleteHandle struct {
	table.Handle
	owner *failDeleteProvider
}

func (p *failDeleteProvider) Table(m model.Model) table.Handle {
	return &failDeleteHandle{Handle: p.Provider.Table(m), owner: p}
}

func (h *failDeleteHandle) Transact(ctx context.Context, method string, ops ...ovsdb.Operation) error {
	if h.owner.fail && method == "vswitch-port-delete" {
		h.owner.fail = false
		return errors.New("injected deletion failure")
	}
	return h.Handle.Transact(ctx, method, ops...)
}

// OVS changes the link set asynchronously. An interrupted kernel dump must be
// discarded and retried, never accepted as a complete cleanup snapshot. Retry
// only that documented transient result; every other error fails immediately.
func requireCleanup(t *testing.T, provider table.Provider, vpcs, subnets map[string]bool) {
	t.Helper()
	var err error
	require.Eventually(t, func() bool {
		err = Cleanup(t.Context(), provider, vpcs, subnets)
		return !errors.Is(err, netlink.ErrDumpInterrupted)
	}, time.Second, 10*time.Millisecond, "kernel dump must stabilize")
	require.NoError(t, err)
}
