package openperouter

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
)

func TestLocalEgressAndSeparateReturn(t *testing.T) {
	spec := testIntegration()
	transit, err := BuildTransit("tenant-a", spec)
	require.NoError(t, err)
	require.Equal(t, []Route{{Prefix: "0.0.0.0/0", NextHop: "169.254.32.2"}}, transit.Defaults)
	nodeA, err := BuildNode("tenant-a", "node-a", 11, spec)
	require.NoError(t, err)
	nodeB, err := BuildNode("tenant-a", "node-b", 12, spec)
	require.NoError(t, err)
	require.NotEqual(t, nodeA.ReturnPort, nodeB.ReturnPort)
	require.NotEqual(t, nodeA.ReturnMAC, nodeB.ReturnMAC)
	require.NotEqual(t, nodeA.ReturnAddresses, nodeB.ReturnAddresses)
	require.Equal(t, nodeA.EgressInterface, nodeB.EgressInterface)
	require.Equal(t, "host-e-10001", nodeA.HostInterface)
	// Golden allocation from the pinned OpenPERouter VethIPsFromPool:
	// PE uses .1, and host uses nodeIndex+2. Workload availability is not an
	// input to this configuration-only allocation.
	require.Equal(t, []string{"169.254.16.1"}, nodeA.PeerAddresses)
	require.Equal(t, []string{"169.254.16.13/20"}, nodeA.HostAddresses)
	require.Equal(t, []string{"169.254.32.14/20"}, nodeA.ReturnAddresses)
}

func TestThousandNodesHaveUniqueReturnEndpoints(t *testing.T) {
	spec := testIntegration()
	addresses, macs := make(map[string]bool), make(map[string]bool)
	for index := range 1000 {
		node, err := BuildNode("tenant-a", "node", index, spec)
		require.NoError(t, err)
		addr := node.ReturnAddresses[0]
		require.False(t, addresses[addr])
		require.False(t, macs[node.ReturnMAC])
		addresses[addr], macs[node.ReturnMAC] = true, true
	}
	_, err := BuildNode("tenant-a", "last-node", MaxNodeIndex, spec)
	require.NoError(t, err)
	_, err = BuildNode("tenant-a", "invalid", MaxNodeIndex+1, spec)
	require.Error(t, err)
}

func TestDualStackTransit(t *testing.T) {
	spec := testIntegration()
	spec.TransitCIDRs = append(spec.TransitCIDRs, "fd00:32::/64")
	spec.HostSessionCIDRs = append(spec.HostSessionCIDRs, "fd00:16::/64")
	transit, err := BuildTransit("tenant-a", spec)
	require.NoError(t, err)
	require.Contains(t, transit.Defaults, Route{Prefix: "::/0", NextHop: "fd00:32::2"})
	node, err := BuildNode("tenant-a", "node-a", 11, spec)
	require.NoError(t, err)
	require.Contains(t, node.HostAddresses, "fd00:16::d/64")
	require.Contains(t, node.ReturnAddresses, "fd00:32::e/64")
}

func TestRejectUnsafeTopology(t *testing.T) {
	tests := map[string]func(*kubeovnv1.OpenPERouterIntegration){
		"wrong mode":        func(s *kubeovnv1.OpenPERouterIntegration) { s.Mode = "localnet" },
		"missing VRF":       func(s *kubeovnv1.OpenPERouterIntegration) { s.VRF = "" },
		"invalid VRF":       func(s *kubeovnv1.OpenPERouterIntegration) { s.VRF = "x/y" },
		"same ASN":          func(s *kubeovnv1.OpenPERouterIntegration) { s.HostASN = s.RouterASN },
		"overlapping pools": func(s *kubeovnv1.OpenPERouterIntegration) { s.TransitCIDRs = s.HostSessionCIDRs },
		"IPv4 no session":   func(s *kubeovnv1.OpenPERouterIntegration) { s.HostSessionCIDRs = []string{"fd00:16::/64"} },
		"duplicate family":  func(s *kubeovnv1.OpenPERouterIntegration) { s.TransitCIDRs = append(s.TransitCIDRs, "169.254.48.0/20") },
		"noncanonical":      func(s *kubeovnv1.OpenPERouterIntegration) { s.TransitCIDRs = []string{"169.254.32.1/20"} },
		"too small":         func(s *kubeovnv1.OpenPERouterIntegration) { s.TransitCIDRs = []string{"169.254.32.0/24"} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			spec := testIntegration()
			mutate(spec)
			require.Error(t, Validate(spec))
		})
	}
	_, err := poolAddress(netip.MustParsePrefix("192.0.2.0/24"), 255)
	require.Error(t, err, "IPv4 broadcast cannot be an endpoint")
}

func testIntegration() *kubeovnv1.OpenPERouterIntegration {
	return &kubeovnv1.OpenPERouterIntegration{
		Mode: Mode, VRF: "tenant-a", L3VNIName: "tenant-a", L3VNI: 10001,
		TransitCIDRs: []string{"169.254.32.0/20"}, HostSessionCIDRs: []string{"169.254.16.0/20"},
		RouterASN: 64514, HostASN: 64515,
	}
}
