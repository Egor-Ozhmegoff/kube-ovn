package util

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
)

func TestExternalVXLANHostContract(t *testing.T) {
	node := &corev1.Node{Name: "worker-1", UID: "node-uid", Annotations: map[string]string{
		ExternalVXLANIPAnnotation: "10.110.191.11", ExternalVXLANMACAnnotation: "02:ee:0b:00:00:00",
	}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: "boot-1"}}}
	vpc := kubeovnv1.Vpc{Name: "tenant", UID: "vpc-uid", Spec: kubeovnv1.VpcSpec{ExternalVXLAN: &kubeovnv1.ExternalVXLANGateway{
		VNI: 220, FRR: &kubeovnv1.ExternalVXLANFRR{Bridge: "cp220", VXLAN: "cpvx220", Advertise: "adv220", VRF: "vrf220", TableID: 220, ControlPort: 60001},
		GatewayNodes: []kubeovnv1.ExternalVXLANGatewayNode{{Node: "worker-1", RoutingPort: "native220", TransitCIDR: "169.254.220.11/24"}},
	}}}
	vpcs := []kubeovnv1.Vpc{vpc}
	require.Error(t, ValidateExternalVXLANHostContract(node, nil, vpcs))
	proof, err := ExternalVXLANHostContract(node, nil, vpcs)
	require.NoError(t, err)
	node.Annotations[ExternalVXLANValidatedAnnotation] = proof
	require.NoError(t, ValidateExternalVXLANHostContract(node, nil, vpcs))
	for _, mutate := range []func(*corev1.Node, *kubeovnv1.Vpc){
		func(n *corev1.Node, _ *kubeovnv1.Vpc) { n.UID = "recreated-node" },
		func(n *corev1.Node, _ *kubeovnv1.Vpc) { n.Status.NodeInfo.BootID = "rebooted" },
		func(n *corev1.Node, _ *kubeovnv1.Vpc) { n.Annotations[ExternalVXLANIPAnnotation] = "10.110.191.12" },
		func(n *corev1.Node, _ *kubeovnv1.Vpc) {
			n.Annotations[ExternalVXLANMACAnnotation] = "02:ee:0c:00:00:00"
		},
		func(_ *corev1.Node, v *kubeovnv1.Vpc) { v.UID = "recreated-vpc" },
		func(_ *corev1.Node, v *kubeovnv1.Vpc) { v.Spec.ExternalVXLAN.FRR.Bridge = "other-bridge" },
		func(_ *corev1.Node, v *kubeovnv1.Vpc) { v.Spec.ExternalVXLAN.FRR.ControlPort++ },
		func(_ *corev1.Node, v *kubeovnv1.Vpc) { v.Spec.ExternalVXLAN.FRR.TableID++ },
		func(_ *corev1.Node, v *kubeovnv1.Vpc) {
			v.Spec.ExternalVXLAN.GatewayNodes[0].RoutingPort = "other-port"
		},
	} {
		n, v := node.DeepCopy(), vpc.DeepCopy()
		mutate(n, v)
		require.Error(t, ValidateExternalVXLANHostContract(n, nil, []kubeovnv1.Vpc{*v}))
	}
	// Health and egress changes must preserve the already validated host
	// identity so ordinary Kubernetes failover does not disable all peers.
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionUnknown}}
	vpc.Spec.ExternalVXLAN.GatewayNodes[0].Egress = new(false)
	require.NoError(t, ValidateExternalVXLANHostContract(node, nil, []kubeovnv1.Vpc{vpc}))
	subnet := kubeovnv1.Subnet{Name: "vm-net", Spec: kubeovnv1.SubnetSpec{ExternalVXLAN: &kubeovnv1.ExternalVXLAN{VNI: 504221, FRR: vpc.Spec.ExternalVXLAN.FRR}}}
	require.Error(t, ValidateExternalVXLANHostContract(node, []kubeovnv1.Subnet{subnet}, vpcs), "new interfaces require new validation")
}

func TestExternalVXLANFRRTableIDs(t *testing.T) {
	config := &kubeovnv1.ExternalVXLANFRR{Bridge: "cp220", VXLAN: "cpvx220", Advertise: "adv220", VRF: "vrf220", ControlPort: 60001}
	for _, table := range []uint32{1, 220, 230, 251, 256, 4294967294} {
		config.TableID = table
		require.NoError(t, ValidateExternalVXLANFRR(config), "table %d", table)
	}
	for _, table := range []uint32{0, 252, 253, 254, 255, 4294967295} {
		config.TableID = table
		require.Error(t, ValidateExternalVXLANFRR(config), "table %d", table)
	}
}
