package host

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"

	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"

	"github.com/kubeovn/kube-ovn/pkg/ovsdb/table"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/vswitch"
)

// The kernel fixture has no external interfaces and refuses ordinary hosts.
func TestIsolationAcrossVRFLoss(t *testing.T) {
	if os.Getenv("KUBE_OVN_DISPOSABLE_LAB") != "true" {
		t.Skip("requires disposable privileged network-none container")
	}
	require.Zero(t, os.Geteuid())
	_, err := os.Stat("/.dockerenv")
	require.NoError(t, err)
	links, err := netlink.LinkList()
	require.NoError(t, err)
	require.Len(t, links, 1)
	ctx := t.Context()
	ip := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput()
		require.NoError(t, err, string(out))
		return string(out)
	}
	ip("link", "add", "tenant-a", "type", "vrf", "table", "110001")
	ip("link", "set", "tenant-a", "up")
	ip("link", "set", "tenant-a", "alias", "kube-ovn/openperouter/test-vpc")
	ip("link", "add", "host-e-10001", "type", "veth", "peer", "name", "sender")
	ip("link", "set", "host-e-10001", "master", "tenant-a")
	ip("link", "set", "host-e-10001", "up")
	ip("address", "add", "192.0.2.1/24", "dev", "host-e-10001")
	ip("route", "add", "default", "dev", "lo")
	ip("-6", "route", "add", "default", "dev", "lo")
	require.NoError(t, EnsureIsolation("host-e-10001", 110001))
	require.NoError(t, EnsureIsolation("host-e-10001", 110001), "reconciliation must be idempotent")
	// The tenant table deliberately has no default. The per-interface terminal
	// rule must stop lookup both before and after losing/deleting the VRF.
	assertDrop := func() {
		t.Helper()
		for _, args := range [][]string{
			{"route", "get", "203.0.113.1", "from", "192.0.2.2", "iif", "host-e-10001"},
			{"-6", "route", "get", "2001:db8::1", "from", "fd00::2", "iif", "host-e-10001"},
		} {
			out, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput()
			require.Error(t, err, string(out))
			require.Contains(t, string(out), "unreachable")
		}
	}
	assertDrop()
	ip("link", "set", "host-e-10001", "nomaster")
	assertDrop()
	ip("link", "set", "host-e-10001", "master", "tenant-a")
	require.NoError(t, Cleanup(ctx, emptyAttachmentProvider{}, nil, nil))
	native, err := netlink.LinkByName("host-e-10001")
	require.NoError(t, err)
	require.Zero(t, native.Attrs().MasterIndex)
	require.Zero(t, native.Attrs().Flags&net.FlagUp, "native ingress is closed before detaching")
	ip("link", "set", "host-e-10001", "up")
	assertDrop()
	ip("link", "del", "host-e-10001")
	ip("link", "add", "host-e-10001", "type", "veth", "peer", "name", "sender")
	ip("link", "set", "host-e-10001", "up")
	assertDrop()
	ip("link", "del", "host-e-10001")
}

// Teardown must also handle a persisted VRF after OVS already removed its
// internal ports. All state transitions under test use the real Linux kernel.
type emptyAttachmentProvider struct{}

type emptyAttachmentTable struct{ table.Handle }

func (emptyAttachmentProvider) Table(model.Model) table.Handle { return emptyAttachmentTable{} }

func (emptyAttachmentTable) Filter(_ context.Context, _, result any) error {
	rows, ok := result.(*[]vswitch.Interface)
	if !ok {
		return fmt.Errorf("unexpected table result %T", result)
	}
	*rows = nil
	return nil
}

func TestReleaseInstallsMissingTerminal(t *testing.T) {
	requireDisposable(t)
	const name = "host-e-10101"
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		require.NoError(t, netlink.RuleAdd(isolationRules(name, 110001, family)[1]))
	}
	require.NoError(t, ReleaseIsolation(name))
	require.NoError(t, ReleaseIsolation(name))
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		rules, err := netlink.RuleList(family)
		require.NoError(t, err)
		terminal := false
		for _, rule := range rules {
			if rule.IifName != name {
				continue
			}
			require.NotEqual(t, 900, rule.Priority, "old lookup must be removed")
			if rule.Priority == 901 && rule.Protocol == routeProtocol {
				terminal = true
			}
		}
		require.True(t, terminal, "cleanup must recover the terminal drop even for a partial/old attachment")
	}
}
