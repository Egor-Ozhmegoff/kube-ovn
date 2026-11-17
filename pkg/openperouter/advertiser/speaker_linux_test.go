package advertiser

import (
	"encoding/json"
	"net/netip"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/stretchr/testify/require"
)

// The fixture must create a private host VRF and an independent FRR namespace.
// This opt-in test must never discover or connect to a configured cluster.
func TestSpeakerLinuxVRFSession(t *testing.T) {
	if os.Getenv("KUBE_OVN_OPENPEROUTER_BGP_LAB") != "true" {
		t.Skip("requires the disposable FRR/VRF lab fixture")
	}
	require.Zero(t, os.Geteuid())
	_, err := os.Stat("/.dockerenv")
	require.NoError(t, err, "integration fixture must be inside a disposable container")
	ctx := t.Context()
	sessions := []Session{{Local: netip.MustParseAddr("169.254.16.13"), Peer: netip.MustParseAddr("169.254.16.1")}}
	speaker, err := New(ctx, "tenant-a", 64515, 64514, "10.255.0.11", sessions)
	require.NoError(t, err)
	t.Cleanup(func() {
		if speaker != nil {
			speaker.Close()
		}
	})
	peerUp := func() bool {
		established := false
		err := speaker.server.ListPeer(ctx, &api.ListPeerRequest{}, func(peer *api.Peer) {
			established = peer.GetState().GetSessionState() == api.PeerState_SESSION_STATE_ESTABLISHED
		})
		return err == nil && established
	}
	require.Eventually(t, peerUp, 30*time.Second, 100*time.Millisecond)
	prefix := netip.MustParsePrefix("10.101.3.11/32")
	require.NoError(t, speaker.Sync(map[netip.Prefix]struct{}{prefix: {}}))
	require.Eventually(t, func() bool { return frrHasPrefix(t, prefix.String()) }, 5*time.Second, 100*time.Millisecond)
	require.Eventually(t, func() bool {
		received := false
		err := speaker.server.ListPath(apiutil.ListPathRequest{TableType: api.TableType_TABLE_TYPE_ADJ_IN, Name: "169.254.16.1", Family: bgp.RF_IPv4_UC, EnableFiltered: true}, func(nlri bgp.NLRI, _ []*apiutil.Path) {
			if route, ok := nlri.(*bgp.IPAddrPrefix); ok && route.Prefix == netip.MustParsePrefix("203.0.113.0/24") {
				received = true
			}
		})
		return err == nil && received
	}, 5*time.Second, 100*time.Millisecond)
	imported := false
	require.NoError(t, speaker.server.ListPath(apiutil.ListPathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Family: bgp.RF_IPv4_UC}, func(nlri bgp.NLRI, _ []*apiutil.Path) {
		if route, ok := nlri.(*bgp.IPAddrPrefix); ok && route.Prefix == netip.MustParsePrefix("203.0.113.0/24") {
			imported = true
		}
	}))
	require.False(t, imported, "external prefixes must be rejected by the host speaker")
	require.NoError(t, speaker.Sync(nil))
	require.Eventually(t, func() bool { return !frrHasPrefix(t, prefix.String()) }, 5*time.Second, 100*time.Millisecond)
	speaker.Close()
	speaker = nil
	// A new process/server has no workload advertisements until fresh Sync.
	speaker, err = New(ctx, "tenant-a", 64515, 64514, "10.255.0.11", sessions)
	require.NoError(t, err)
	require.Empty(t, speaker.paths)
	require.Eventually(t, peerUp, 30*time.Second, 100*time.Millisecond)
	require.Never(t, func() bool { return frrHasPrefix(t, prefix.String()) }, 2*time.Second, 100*time.Millisecond)
}

func frrHasPrefix(t *testing.T, prefix string) bool {
	t.Helper()
	output, err := exec.CommandContext(t.Context(), "vtysh", "-c", "show bgp vrf tenant-a ipv4 unicast json").Output()
	require.NoError(t, err, "FRR must remain available during withdrawal checks")
	var rib struct {
		Routes map[string]json.RawMessage `json:"routes"`
	}
	require.NoError(t, json.Unmarshal(output, &rib))
	_, found := rib.Routes[prefix]
	return found
}
