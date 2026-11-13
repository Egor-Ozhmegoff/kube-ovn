package daemon

import (
	"errors"
	"net"
	"os"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	kubeovnfake "github.com/kubeovn/kube-ovn/pkg/client/clientset/versioned/fake"
	"github.com/kubeovn/kube-ovn/pkg/util"
)

func TestExternalVXLANFRRMapping(t *testing.T) {
	require.NoError(t, validateExternalVXLANFRRMapping("unrelated=eth1,native-220-worker-1=cp220", "native-220-worker-1", "cp220"))
	for _, mappings := range []string{"", "native-220-worker-1=eth1", "native-220-worker-1=cp220,native-220-worker-1=cp220"} {
		require.Error(t, validateExternalVXLANFRRMapping(mappings, "native-220-worker-1", "cp220"))
	}
}

func TestExternalVXLANFRRAttachmentRequiresDeclaredIngress(t *testing.T) {
	frr := &kubeovnv1.ExternalVXLANFRR{Bridge: "cp220", VXLAN: "cpvx220", Advertise: "adv220", ControlPort: 60001, VRF: "vrf220", TableID: 220}
	vpc := &kubeovnv1.Vpc{Name: "tenant", Spec: kubeovnv1.VpcSpec{ExternalVXLAN: &kubeovnv1.ExternalVXLANGateway{
		VNI: 220, FRR: frr,
		GatewayNodes: []kubeovnv1.ExternalVXLANGatewayNode{{Node: "worker-1", TransitCIDR: "169.254.220.11/24", RoutingPort: "native-220-worker-1", Egress: new(false)}},
	}}}
	subnet := &kubeovnv1.Subnet{Spec: kubeovnv1.SubnetSpec{Vpc: "tenant", ExternalVXLAN: &kubeovnv1.ExternalVXLAN{VNI: 504221, FRR: frr}}}
	node := &corev1.Node{Name: "worker-1", Annotations: map[string]string{util.ExternalVXLANIPAnnotation: "10.110.191.11"}}
	proof, err := util.ExternalVXLANHostContract(node, []kubeovnv1.Subnet{*subnet}, []kubeovnv1.Vpc{*vpc})
	require.NoError(t, err)
	node.Annotations[util.ExternalVXLANValidatedAnnotation] = proof
	handler := cniServerHandler{Config: &Configuration{NodeName: "worker-1"}, KubeClient: kubefake.NewClientset(node), KubeOvnClient: kubeovnfake.NewSimpleClientset(vpc, subnet)}
	require.NoError(t, handler.validateExternalVXLANAttachment(subnet), "egress:false is a valid VM host identity")
	node.Annotations[util.ExternalVXLANValidatedAnnotation] = "stale"
	_, err = handler.KubeClient.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.ErrorContains(t, handler.validateExternalVXLANAttachment(subnet), "has not validated")
	handler.Config.NodeName = "worker-2"
	require.ErrorContains(t, handler.validateExternalVXLANAttachment(subnet), "requires an ingress identity")
	subnet.Spec.ExternalVXLAN = nil
	require.ErrorContains(t, handler.validateExternalVXLANAttachment(subnet), "requires an ingress identity", "ordinary VPC subnets also make the transit LS local")
	vpc.Spec.ExternalVXLAN.FRR = nil
	_, err = handler.KubeOvnClient.KubeovnV1().Vpcs().Update(t.Context(), vpc, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Error(t, handler.validateExternalVXLANAttachment(subnet), "missing FRR must not bypass validation")
}

func TestExternalVXLANValidationPublication(t *testing.T) {
	node := &corev1.Node{Name: "worker-1", Annotations: map[string]string{"unrelated": "preserved"}}
	client := kubefake.NewClientset(node)
	c := &Controller{config: &Configuration{KubeClient: client}}
	require.NoError(t, c.finishExternalVXLANValidation(node, "validated-contract", nil))
	current, err := client.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "validated-contract", current.Annotations[util.ExternalVXLANValidatedAnnotation])
	client.ClearActions()
	require.NoError(t, c.finishExternalVXLANValidation(current, "validated-contract", nil))
	require.Empty(t, client.Actions(), "unchanged proof must not trigger Node update loops")
	validationErr := errors.New("host interface parameters changed")
	require.ErrorIs(t, c.finishExternalVXLANValidation(current, "validated-contract", validationErr), validationErr)
	current, err = client.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Empty(t, current.Annotations[util.ExternalVXLANValidatedAnnotation], "failed revalidation must revoke approval")
	require.Equal(t, "preserved", current.Annotations["unrelated"])
}

func TestExternalVXLANFRRPreservesHostState(t *testing.T) {
	externalVXLANFRRTestNamespace(t)
	config := &kubeovnv1.ExternalVXLANFRR{Bridge: "cp220", VXLAN: "cpvx220", Advertise: "adv220", ControlPort: 60001, VRF: "vrf220", TableID: 220}
	vrf := &netlink.Vrf{Name: config.VRF, Table: 220}
	if err := netlink.LinkAdd(vrf); errors.Is(err, unix.EOPNOTSUPP) {
		t.Skip("test kernel lacks CONFIG_NET_VRF; full FRR/IP Route Exchange requires a Linux VRF-capable host")
	} else {
		require.NoError(t, err)
	}
	require.NoError(t, netlink.LinkSetUp(vrf))
	bridge := &netlink.Bridge{Name: config.Bridge, MasterIndex: vrf.Index}
	require.NoError(t, netlink.LinkAdd(bridge))
	require.NoError(t, netlink.LinkSetUp(bridge))
	vxlan := &netlink.Vxlan{Name: config.VXLAN, MasterIndex: bridge.Index, VxlanId: 220, Port: config.ControlPort, SrcAddr: net.ParseIP("10.110.191.11")}
	require.NoError(t, netlink.LinkAdd(vxlan))
	require.NoError(t, netlink.LinkSetUp(vxlan))
	advertise := &netlink.Dummy{Name: config.Advertise, MasterIndex: bridge.Index}
	require.NoError(t, netlink.LinkAdd(advertise))
	require.NoError(t, netlink.LinkSetUp(advertise))
	mac, err := net.ParseMAC("02:ff:00:00:00:01")
	require.NoError(t, err)
	neighbor := &netlink.Neigh{LinkIndex: vxlan.Index, Family: unix.AF_BRIDGE, IP: net.ParseIP("10.110.191.100"), HardwareAddr: mac, State: netlink.NUD_NOARP | netlink.NUD_REACHABLE, Flags: netlink.NTF_SELF | netlink.NTF_EXT_LEARNED, Type: unix.RTN_UNICAST}
	require.NoError(t, netlink.NeighSet(neighbor))
	before, err := netlink.NeighList(vxlan.Index, unix.AF_BRIDGE)
	require.NoError(t, err)
	for range 3 {
		require.NoError(t, validateExternalVXLANFRRLinks(220, config, "10.110.191.11"))
	}
	after, err := netlink.NeighList(vxlan.Index, unix.AF_BRIDGE)
	require.NoError(t, err)
	require.Equal(t, before, after, "FRR's entries must survive reconciliation")
	require.NoError(t, netlink.LinkSetAlias(bridge, util.ExternalVXLANOwner+":220"))
	require.ErrorContains(t, validateExternalVXLANFRRLinks(220, config, "10.110.191.11"), "still belongs to static")
	require.NoError(t, netlink.LinkSetAlias(bridge, ""))
	config.ControlPort++
	require.Error(t, validateExternalVXLANFRRLinks(220, config, "10.110.191.11"))
	config.ControlPort--
	unexpected := &netlink.Dummy{Name: "workload", MasterIndex: bridge.Index}
	require.NoError(t, netlink.LinkAdd(unexpected))
	require.ErrorContains(t, validateExternalVXLANFRRLinks(220, config, "10.110.191.11"), "unexpected port")
}

func externalVXLANFRRTestNamespace(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root and CAP_SYS_ADMIN/CAP_NET_ADMIN in an isolated Linux test container")
	}
	runtime.LockOSThread()
	original, err := netns.Get()
	require.NoError(t, err)
	namespace, err := netns.New()
	if err != nil {
		require.NoError(t, original.Close())
		runtime.UnlockOSThread()
		t.Skipf("network namespaces unavailable: %v", err)
	}
	t.Cleanup(func() {
		require.NoError(t, netns.Set(original))
		require.NoError(t, namespace.Close())
		require.NoError(t, original.Close())
		runtime.UnlockOSThread()
	})
	vtep := &netlink.Dummy{Name: "vtep-test"}
	require.NoError(t, netlink.LinkAdd(vtep))
	require.NoError(t, netlink.AddrAdd(vtep, &netlink.Addr{IPNet: &net.IPNet{IP: net.ParseIP("10.110.191.11"), Mask: net.CIDRMask(32, 32)}}))
	require.NoError(t, netlink.LinkSetUp(vtep))
}
