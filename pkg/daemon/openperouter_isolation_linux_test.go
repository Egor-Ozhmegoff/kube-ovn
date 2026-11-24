package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/openperouter"
	"github.com/kubeovn/kube-ovn/pkg/openperouter/barrier"
	"github.com/kubeovn/kube-ovn/pkg/openperouter/routes"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnsb"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/table"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/vswitch"
)

// Only the OVS row snapshot is stubbed. Fence uses real Linux links; guard Close
// and per-VPC route withdrawal run through the production implementations.
type isolationRows struct {
	table.Provider
	rows []vswitch.Interface
	fail error
}
type isolationHandle struct {
	table.Handle
	rows *isolationRows
}

func (p *isolationRows) Table(model.Model) table.Handle { return &isolationHandle{rows: p} }
func (h *isolationHandle) Filter(_ context.Context, predicate, out any) error {
	if h.rows.fail != nil {
		return h.rows.fail
	}
	filter := predicate.(func(*vswitch.Interface) bool)
	for _, r := range h.rows.rows {
		if filter(&r) {
			*out.(*[]vswitch.Interface) = append(*out.(*[]vswitch.Interface), r)
		}
	}
	return nil
}

func isolationIP(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput()
	require.NoError(t, err, string(out))
	return string(out)
}

func isolationGuards(t *testing.T) *openPERouterGuards {
	t.Helper()
	client := fake.NewClientset()
	guards := &openPERouterGuards{session: "session", nodeUID: "node", joined: make(map[types.UID]guardRegistration)}
	for _, uid := range []string{"a", "b"} {
		data, err := json.Marshal(barrier.State{SubnetUID: uid, Revision: "r1", Members: map[string]barrier.Member{"node": {Name: "node", Session: "session", Phase: barrier.Active, Revision: "r1"}}})
		require.NoError(t, err)
		cm, err := client.CoreV1().ConfigMaps("kube-system").Create(t.Context(), &corev1.ConfigMap{Name: barrier.Name(types.UID(uid)), UID: types.UID("barrier-" + uid), ResourceVersion: "1", OwnerReferences: []metav1.OwnerReference{{APIVersion: "kubeovn.io/v1", Kind: "Subnet", Name: uid, UID: types.UID(uid)}}, Data: map[string]string{"state": string(data)}}, metav1.CreateOptions{})
		require.NoError(t, err)
		guards.joined[types.UID(uid)] = guardRegistration{store: barrier.Store{Maps: client.CoreV1().ConfigMaps("kube-system"), SubnetName: uid, SubnetUID: types.UID(uid)}, uid: cm.UID, vpcUID: uid}
	}
	return guards
}

func isolationAttachment(t *testing.T, uid string, index int) routes.Attachment {
	t.Helper()
	host, peer, vrf := "host-"+uid, "pe-"+uid, "vrf-"+uid
	hostIP := fmt.Sprintf("169.254.%d.13/24", index)
	peerIP := fmt.Sprintf("169.254.%d.1", index)
	isolationIP(t, "-n", "isolation-pe", "link", "add", vrf, "type", "vrf", "table", strconv.Itoa(23000+index))
	isolationIP(t, "-n", "isolation-pe", "link", "set", vrf, "up")
	isolationIP(t, "link", "add", host, "type", "veth", "peer", "name", peer, "netns", "isolation-pe")
	isolationIP(t, "address", "add", hostIP, "dev", host)
	isolationIP(t, "link", "set", host, "up")
	isolationIP(t, "-n", "isolation-pe", "link", "set", peer, "master", vrf)
	isolationIP(t, "-n", "isolation-pe", "address", "add", peerIP+"/24", "dev", peer)
	isolationIP(t, "-n", "isolation-pe", "link", "set", peer, "up")
	isolationIP(t, "link", "add", "path-"+uid, "type", "dummy")
	isolationIP(t, "link", "set", "path-"+uid, "up")
	link, err := netlink.LinkByName(host)
	require.NoError(t, err)
	return routes.Attachment{UID: uid, VRF: vrf, Device: peer, HostIndex: link.Attrs().Index, HostAddresses: []string{hostIP}, PeerAddresses: []string{peerIP}, Prefixes: map[netip.Prefix]struct{}{netip.MustParsePrefix(fmt.Sprintf("10.%d.0.0/24", index)): {}}}
}

func TestOpenPERouterScopedFailureLinux(t *testing.T) {
	if os.Getenv("KUBE_OVN_SUBNET_ISOLATION_LAB") != "true" {
		t.Skip("requires disposable network-none container")
	}
	_, err := os.Stat("/.dockerenv")
	require.NoError(t, err)
	require.Zero(t, os.Geteuid())
	isolationIP(t, "netns", "add", "isolation-pe")
	t.Cleanup(func() {
		isolationIP(t, "netns", "del", "isolation-pe")
		for _, uid := range []string{"a", "b"} {
			isolationIP(t, "link", "del", "path-"+uid)
		}
	})
	m := routes.Manager{Namespace: "/var/run/netns/isolation-pe"}
	a, b := isolationAttachment(t, "a", 1), isolationAttachment(t, "b", 2)
	guards := isolationGuards(t)
	rows := &isolationRows{}
	for _, uid := range []string{"a", "b"} {
		rows.rows = append(rows.rows, vswitch.Interface{Name: "path-" + uid, Type: "internal", ExternalIDs: map[string]string{openperouter.OwnerKey + "-uid": uid}})
	}
	c := &Controller{vswitchTables: rows}
	vpcs := []*kubeovnv1.Vpc{{Name: "b", UID: "b"}, {Name: "a", UID: "a"}}
	configs := map[string]routes.Attachment{"a": a, "b": b}
	bound := map[string]ovnsb.PortBinding{"return-a": {}}
	installed := map[string]bool{"return-a": true}
	// Unprepared B, then loss of readiness of an already open B.
	for _, previouslyReady := range []bool{false, true} {
		if previouslyReady {
			require.NoError(t, m.Reconcile(t.Context(), b, nil))
			isolationIP(t, "link", "set", "path-b", "up")
		}
		require.NoError(t, reconcileOpenPERouterVPCs(t.Context(), vpcs, func(vpc *kubeovnv1.Vpc) error {
			if err := openPERouterReturnReady("return-"+vpc.Name, bound, installed); err != nil {
				return err
			}
			return m.Reconcile(t.Context(), configs[vpc.Name], nil)
		}, func(ctx context.Context, uid string) error { return c.closeOpenPERouterVPC(ctx, uid, guards, m) }))
		for _, uid := range []string{"a", "b"} {
			link, err := netlink.LinkByName("path-" + uid)
			require.NoError(t, err)
			require.Equal(t, uid == "a", link.Attrs().Flags&net.FlagUp != 0)
			registration := guards.joined[types.UID(uid)]
			_, state, err := registration.store.Read(t.Context())
			require.NoError(t, err)
			expected := barrier.Closed
			if uid == "a" {
				expected = barrier.Active
			}
			require.Equal(t, expected, state.Members["node"].Phase)
		}
		require.Contains(t, isolationIP(t, "-n", "isolation-pe", "route", "show", "table", "23001"), "10.1.0.0/24")
		require.NotContains(t, isolationIP(t, "-n", "isolation-pe", "route", "show", "table", "23002"), "10.2.0.0/24")
	}
	// Return binding is healthy, but B loses its actual native PE attachment.
	require.NoError(t, m.Reconcile(t.Context(), b, nil))
	bound["return-b"], installed["return-b"] = ovnsb.PortBinding{}, true
	isolationIP(t, "-n", "isolation-pe", "link", "set", b.Device, "down")
	require.NoError(t, reconcileOpenPERouterVPCs(t.Context(), vpcs, func(vpc *kubeovnv1.Vpc) error {
		if err := openPERouterReturnReady("return-"+vpc.Name, bound, installed); err != nil {
			return err
		}
		return m.Reconcile(t.Context(), configs[vpc.Name], nil)
	}, func(ctx context.Context, uid string) error { return c.closeOpenPERouterVPC(ctx, uid, guards, m) }))
	require.Contains(t, isolationIP(t, "-n", "isolation-pe", "route", "show", "table", "23001"), "10.1.0.0/24")
	require.NotContains(t, isolationIP(t, "-n", "isolation-pe", "route", "show", "table", "23002"), "10.2.0.0/24")
	isolationIP(t, "-n", "isolation-pe", "link", "set", b.Device, "up")
	// Failed fencing must still attempt B withdrawal, never report Closed for A.
	require.NoError(t, m.Reconcile(t.Context(), b, nil))
	rows.fail = errors.New("OVS snapshot unavailable")
	require.Error(t, c.closeOpenPERouterVPC(t.Context(), "b", guards, m))
	require.NotContains(t, isolationIP(t, "-n", "isolation-pe", "route", "show", "table", "23002"), "10.2.0.0/24")
	require.Contains(t, isolationIP(t, "-n", "isolation-pe", "route", "show", "table", "23001"), "10.1.0.0/24")
}
