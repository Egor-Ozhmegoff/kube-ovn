package routes

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
)

func linuxLab(t *testing.T) Manager {
	t.Helper()
	if os.Getenv("KUBE_OVN_NETLINK_LAB") != "true" {
		t.Skip("requires hack/openperouter/netlink-routes.sh in disposable network-none container")
	}
	_, err := os.Stat("/.dockerenv")
	require.NoError(t, err)
	require.Zero(t, os.Geteuid())
	return Manager{Namespace: "/var/run/netns/pe"}
}

func labAttachment(t *testing.T) Attachment {
	t.Helper()
	host, err := netlink.LinkByName("host-e-10001")
	require.NoError(t, err)
	return Attachment{
		UID: "lab-vpc", VRF: "tenant-a", Device: "pe-e-10001", HostIndex: host.Attrs().Index,
		HostAddresses: []string{"169.254.16.13/20", "fd00:16::d/64"}, PeerAddresses: []string{"169.254.16.1", "fd00:16::1"},
		Prefixes: labSubnetPrefixes(t),
	}
}

func labSubnetPrefixes(t *testing.T) map[netip.Prefix]struct{} {
	t.Helper()
	vpc := &kubeovnv1.Vpc{Name: "tenant-a", Spec: kubeovnv1.VpcSpec{OpenPERouter: &kubeovnv1.OpenPERouterIntegration{}}}
	routed := &kubeovnv1.Subnet{Spec: kubeovnv1.SubnetSpec{Vpc: vpc.Name, CIDRBlock: "10.101.3.0/24,fd00:3::/64"}}
	l2 := &kubeovnv1.Subnet{Spec: kubeovnv1.SubnetSpec{Vpc: vpc.Name, CIDRBlock: "10.101.1.0/24,fd00:1::/64", OpenPERouterL2: &kubeovnv1.OpenPERouterL2Attachment{VNI: 10101}}}
	routed.Status.Ready("Ready", "fixture")
	l2.Status.Ready("Ready", "fixture")
	prefixes, err := RoutedSubnetPrefixes(vpc, []*kubeovnv1.Subnet{routed, l2})
	require.NoError(t, err)
	return prefixes
}

func command(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), name, args...).CombinedOutput()
	require.NoError(t, err, string(out))
	return string(out)
}

func frrPrefix(t *testing.T, family, prefix string) bool {
	t.Helper()
	output := command(t, "vtysh", "-c", "show bgp vrf tenant-a "+family+" unicast json")
	var rib struct {
		Routes map[string]json.RawMessage `json:"routes"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &rib))
	_, found := rib.Routes[prefix]
	return found
}

func waitFRR(t *testing.T, family, prefix string, present bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if frrPrefix(t, family, prefix) == present {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("FRR %s presence did not reach %t", prefix, present)
}

func TestLinuxPESubnetRoutesAndFRR(t *testing.T) {
	m := linuxLab(t)
	a := labAttachment(t)
	ctx := t.Context()
	require.NoError(t, m.Sweep(ctx, nil))
	opens := 0
	reconcile := func() error {
		return m.Reconcile(ctx, a, func(s *Session) error {
			require.NoError(t, s.Check(ctx, a.UID))
			state := s.states[a.UID]
			require.EqualValues(t, 23001, state.table)
			actual, err := s.actual(ctx)
			require.NoError(t, err)
			for _, r := range actual {
				if Owned(r) {
					require.EqualValues(t, 23001, r.Table)
					if r.Prefix.Addr().Is4() {
						require.Equal(t, OwnerID(a.UID), r.Realm)
					} else {
						require.Zero(t, r.Realm)
					}
				}
			}
			opens++
			return nil
		})
	}
	require.NoError(t, reconcile())
	require.NoError(t, reconcile())
	require.Equal(t, 2, opens)
	before := command(t, "ip", "-n", "pe", "-j", "route", "show", "table", "23001")
	pods := fake.NewClientset()
	for _, node := range []string{"source", "target"} {
		_, err := pods.CoreV1().Pods("vm").Create(ctx, &corev1.Pod{Name: node, Namespace: "vm", Spec: corev1.PodSpec{NodeName: node}, Annotations: map[string]string{"ovn.kubernetes.io/ip_address": "10.101.1.11", "ovn.kubernetes.io/mac_address": "02:00:00:00:01:11"}}, metav1.CreateOptions{})
		require.NoError(t, err)
		a.Prefixes = labSubnetPrefixes(t)
		require.NoError(t, reconcile())
		require.Equal(t, before, command(t, "ip", "-n", "pe", "-j", "route", "show", "table", "23001"), "Pod/VM creation and placement must not change subnet routes")
	}
	for _, node := range []string{"source", "target"} {
		require.NoError(t, pods.CoreV1().Pods("vm").Delete(ctx, node, metav1.DeleteOptions{}))
		a.Prefixes = labSubnetPrefixes(t)
		require.NoError(t, reconcile())
		require.Equal(t, before, command(t, "ip", "-n", "pe", "-j", "route", "show", "table", "23001"))
	}
	prepared := opens
	// A foreign exact-prefix route must survive and must not authorize ingress.
	command(t, "ip", "-n", "pe", "route", "add", "10.101.4.0/24", "via", "169.254.16.13", "dev", "pe-e-10001", "table", "23001", "proto", "99")
	a.Prefixes[netip.MustParsePrefix("10.101.4.0/24")] = struct{}{}
	require.ErrorContains(t, reconcile(), "foreign route conflicts")
	require.Equal(t, prepared, opens, "failed preparation must never call Open")
	delete(a.Prefixes, netip.MustParsePrefix("10.101.4.0/24"))
	t.Log(command(t, "vtysh", "-c", "show ip route vrf tenant-a 10.101.3.0/24 json"))
	waitFRR(t, "ipv4", "10.101.3.0/24", true)
	waitFRR(t, "ipv6", "fd00:3::/64", true)
	zebra := command(t, "vtysh", "-c", "show ip route vrf tenant-a 10.101.3.0/24 json")
	require.Contains(t, strings.ReplaceAll(zebra, " ", ""), `"protocol":"kernel"`, "FRR classification must be verified, not inferred from Linux protocol number")
	for _, prefix := range []string{"10.101.3.11/32", "0.0.0.0/0", "169.254.16.0/20", "192.0.2.1/32", "198.51.100.1/32", "10.101.3.12/32", "10.101.3.13/32"} {
		require.False(t, frrPrefix(t, "ipv4", prefix), prefix)
	}
	// Lost route is repaired; changed revision removes only stale marked routes.
	command(t, "ip", "-n", "pe", "route", "del", "10.101.3.0/24", "table", "23001")
	require.NoError(t, reconcile())
	delete(a.Prefixes, netip.MustParsePrefix("10.101.3.0/24"))
	require.NoError(t, reconcile())
	waitFRR(t, "ipv4", "10.101.3.0/24", false)
	require.NoError(t, m.Sweep(ctx, nil))
	require.NoError(t, m.Sweep(ctx, nil))
	output := command(t, "ip", "-n", "pe", "route", "show", "table", "23001")
	require.Contains(t, output, "unreachable default")
	require.Contains(t, output, "192.0.2.1")
	require.Contains(t, output, "198.51.100.1")
}

func TestLinuxSubnetRecreationAndProcessRestart(t *testing.T) {
	m := linuxLab(t)
	a := labAttachment(t)
	ctx := t.Context()
	require.NoError(t, m.Reconcile(ctx, a, func(s *Session) error {
		command(t, "ip", "-n", "pe", "link", "del", "pe-e-10001")
		require.Error(t, s.Check(ctx, a.UID))
		return nil
	}))
	// Native owner recreates the veth; Kube-OVN only rediscovers it.
	command(t, "ip", "link", "add", "host-e-10001", "type", "veth", "peer", "name", "pe-e-10001", "netns", "pe")
	for _, args := range [][]string{{"address", "add", "169.254.16.13/20", "dev", "host-e-10001"}, {"-6", "address", "add", "fd00:16::d/64", "dev", "host-e-10001", "nodad"}, {"link", "set", "host-e-10001", "up"}, {"-n", "pe", "link", "set", "pe-e-10001", "master", "tenant-a"}, {"-n", "pe", "address", "add", "169.254.16.1/20", "dev", "pe-e-10001"}, {"-n", "pe", "-6", "address", "add", "fd00:16::1/64", "dev", "pe-e-10001", "nodad"}, {"-n", "pe", "link", "set", "pe-e-10001", "up"}} {
		command(t, "ip", args...)
	}
	a = labAttachment(t)
	require.NoError(t, m.Reconcile(ctx, a, func(s *Session) error {
		command(t, "ip", "-n", "pe", "link", "set", "pe-e-10001", "nomaster")
		require.Error(t, s.Check(ctx, a.UID))
		return nil
	}))
	command(t, "ip", "-n", "pe", "link", "set", "pe-e-10001", "master", "tenant-a")
	command(t, "ip", "-n", "pe", "-6", "address", "replace", "fd00:16::1/64", "dev", "pe-e-10001", "nodad")
	// Start a distinct process using production route manager, then SIGKILL it.
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLinuxRouteOwnerChild$")
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	child.Env = append(os.Environ(), "KUBE_OVN_ROUTE_CHILD=true")
	require.NoError(t, child.Start())
	require.Eventually(t, func() bool { _, err := os.Stat("/tmp/netlink-child-ready"); return err == nil }, 10*time.Second, 100*time.Millisecond)
	require.NoError(t, child.Process.Kill())
	require.Error(t, child.Wait())
	require.Contains(t, command(t, "ip", "-n", "pe", "route", "show", "table", "23001"), "10.101.3.0/24", "SIGKILL does NOT withdraw kernel routes")
	require.NoError(t, m.Sweep(ctx, nil)) // startup path before API caches
	require.NotContains(t, command(t, "ip", "-n", "pe", "route", "show", "table", "23001"), "10.101.3.0/24")
	require.NoError(t, m.Reconcile(ctx, a, nil))
	command(t, "ip", "-n", "pe", "link", "set", "pe-e-10001", "nomaster")
	command(t, "ip", "-n", "pe", "link", "del", "tenant-a")
	command(t, "ip", "-n", "pe", "link", "add", "tenant-a", "type", "vrf", "table", "23002")
	command(t, "ip", "-n", "pe", "link", "set", "tenant-a", "up")
	command(t, "ip", "-n", "pe", "link", "set", "pe-e-10001", "master", "tenant-a")
	command(t, "ip", "-n", "pe", "-6", "address", "replace", "fd00:16::1/64", "dev", "pe-e-10001", "nodad")
	require.NoError(t, m.Reconcile(ctx, a, nil))
	require.Contains(t, command(t, "ip", "-n", "pe", "route", "show", "table", "23002"), "10.101.3.0/24")
	require.NoError(t, m.Sweep(ctx, nil))
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, m.Reconcile(cancelled, a, nil), context.Canceled)
}

func TestLinuxRouteOwnerChild(t *testing.T) {
	if os.Getenv("KUBE_OVN_ROUTE_CHILD") != "true" {
		t.Skip("subprocess only")
	}
	m := linuxLab(t)
	require.NoError(t, m.Reconcile(t.Context(), labAttachment(t), nil))
	require.NoError(t, os.WriteFile("/tmp/netlink-child-ready", []byte("ready"), 0o600))
	for {
		time.Sleep(time.Second)
	}
}

func TestLinuxNoLocalBGP(t *testing.T) {
	linuxLab(t)
	// No listener/session is created by Manager. FRR itself may listen for fabric.
	sockets := command(t, "ip", "netns", "exec", "pe", "ss", "-tn")
	require.False(t, strings.Contains(sockets, "169.254.16.13:179"))
	require.False(t, strings.Contains(sockets, "169.254.16.1:179"))
}

func TestLinuxScopedSubnetRoutesAndLegacyCleanup(t *testing.T) {
	m := linuxLab(t)
	a := labAttachment(t)
	ctx := t.Context()
	require.NoError(t, m.Reconcile(ctx, a, nil))
	b := a
	b.UID = "other-vpc"
	b.Prefixes = map[netip.Prefix]struct{}{netip.MustParsePrefix("10.102.0.0/16"): {}, netip.MustParsePrefix("fd00:102::/64"): {}}
	// A different owner in the same table is preserved; table/master validation
	// itself rejects actual VRF sharing in real attachment configurations.
	require.NoError(t, m.Reconcile(ctx, b, nil))
	require.NoError(t, m.Withdraw(ctx, b.UID))
	require.Contains(t, command(t, "ip", "-n", "pe", "route", "show", "table", "23002"), "10.101.3.0/24")
	require.NotContains(t, command(t, "ip", "-n", "pe", "route", "show", "table", "23002"), "10.102.0.0/16")
	command(t, "ip", "-n", "pe", "route", "add", "10.101.3.11/32", "via", "169.254.16.13", "dev", a.Device, "table", "23002", "proto", "201", "metric", "42760", "realms", "0x4b000001")
	command(t, "ip", "-n", "pe", "-6", "route", "add", "fd00:3::b/128", "via", "fd00:16::d", "dev", a.Device, "table", "23002", "proto", "201", "metric", "42760")
	command(t, "ip", "-n", "pe", "route", "add", "192.0.2.23/32", "via", "169.254.16.13", "dev", a.Device, "table", "23002", "proto", "201", "metric", "100")
	require.NoError(t, m.PurgeLegacy(ctx))
	require.NoError(t, m.PurgeLegacy(ctx))
	output := command(t, "ip", "-n", "pe", "route", "show", "table", "23002")
	require.NotContains(t, output, "10.101.3.11")
	require.Contains(t, output, "192.0.2.23")
	require.Contains(t, output, "10.101.3.0/24")
	require.NotContains(t, command(t, "ip", "-n", "pe", "-6", "route", "show", "table", "23002"), "fd00:3::b")
	require.NoError(t, m.Sweep(ctx, nil))
}

func TestLinuxNamespaceReplacement(t *testing.T) {
	m := linuxLab(t)
	a := labAttachment(t)
	require.NoError(t, m.Reconcile(t.Context(), a, func(s *Session) error {
		command(t, "ip", "netns", "del", "pe")
		command(t, "ip", "netns", "add", "pe")
		require.ErrorContains(t, s.Check(t.Context(), a.UID), "namespace generation changed")
		return nil
	}))
	require.Error(t, m.Reconcile(t.Context(), a, nil), "new namespace has no attachment; stale state cannot reopen it")
	require.NoError(t, m.Sweep(t.Context(), nil))
}
