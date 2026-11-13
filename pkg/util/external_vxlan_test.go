package util

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
)

func externalVXLANFixture() *kubeovnv1.ExternalVXLANGateway {
	return &kubeovnv1.ExternalVXLANGateway{
		VNI:         220,
		FRR:         &kubeovnv1.ExternalVXLANFRR{Bridge: "cp220", VXLAN: "cpvx220", Advertise: "adv220", ControlPort: 60001, VRF: "vrf220", TableID: 220},
		GatewayNode: "worker-1", TransitCIDR: "169.254.220.11/24", RoutingPort: "native-220-worker-1",
	}
}

func TestExternalVXLANFRRValidation(t *testing.T) {
	config := externalVXLANFixture()
	config.RoutingPort = "native-220-worker-1"
	config.FRR = &kubeovnv1.ExternalVXLANFRR{Bridge: "cp220", VXLAN: "cpvx220", Advertise: "adv220", ControlPort: 60001, VRF: "vrf220", TableID: 220}
	require.NoError(t, ValidateExternalVXLANGateway(config))
	require.Equal(t, "native-220-worker-1", ExternalVXLANGatewayNodes(config)[0].RoutingPort)
	for _, mutate := range []func(*kubeovnv1.ExternalVXLANGateway){
		func(c *kubeovnv1.ExternalVXLANGateway) { c.FRR.ControlPort = 4789 },
		func(c *kubeovnv1.ExternalVXLANGateway) { c.FRR.TableID = 254 },
		func(c *kubeovnv1.ExternalVXLANGateway) { c.FRR.Advertise = c.FRR.VXLAN },
		func(c *kubeovnv1.ExternalVXLANGateway) { c.RoutingPort = "" },
	} {
		configCopy := config.DeepCopy()
		mutate(configCopy)
		require.Error(t, ValidateExternalVXLANGateway(configCopy))
	}
}

func TestExternalVXLANGatewayNodes(t *testing.T) {
	legacy := externalVXLANFixture()
	require.Equal(t, []kubeovnv1.ExternalVXLANGatewayNode{{Node: "worker-1", TransitCIDR: "169.254.220.11/24", RoutingPort: "native-220-worker-1"}}, ExternalVXLANGatewayNodes(legacy))
	multi := legacy.DeepCopy()
	multi.GatewayNode, multi.TransitCIDR, multi.RoutingPort = "", "", ""
	multi.GatewayNodes = []kubeovnv1.ExternalVXLANGatewayNode{{Node: "worker-1", TransitCIDR: "169.254.220.11/24", RoutingPort: "native-220-worker-1"}, {Node: "worker-2", TransitCIDR: "169.254.220.12/24", RoutingPort: "native-220-worker-2"}}
	require.NoError(t, ValidateExternalVXLANGateway(multi))
	for _, mutate := range []func(*kubeovnv1.ExternalVXLANGateway){
		func(c *kubeovnv1.ExternalVXLANGateway) { c.GatewayNode = "worker-1" },
		func(c *kubeovnv1.ExternalVXLANGateway) { c.TransitCIDR = "169.254.220.11/24" },
		func(c *kubeovnv1.ExternalVXLANGateway) { c.GatewayNodes[1].Node = "worker-1" },
		func(c *kubeovnv1.ExternalVXLANGateway) { c.GatewayNodes[1].TransitCIDR = "169.254.220.11/32" },
		func(c *kubeovnv1.ExternalVXLANGateway) { c.GatewayNodes[1].TransitCIDR = "2001:db8::1/64" },
		func(c *kubeovnv1.ExternalVXLANGateway) { c.GatewayNodes[1].Node = "" },
	} {
		configCopy := multi.DeepCopy()
		mutate(configCopy)
		require.Error(t, ValidateExternalVXLANGateway(configCopy))
	}
}

func TestExternalVXLANNodeReadiness(t *testing.T) {
	require.False(t, ExternalVXLANNodeReady(nil))
	node := &corev1.Node{}
	require.False(t, ExternalVXLANNodeReady(node))
	for _, status := range []corev1.ConditionStatus{corev1.ConditionTrue, corev1.ConditionFalse, corev1.ConditionUnknown} {
		node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}
		require.Equal(t, status == corev1.ConditionTrue, ExternalVXLANNodeReady(node))
	}
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}, {Type: corev1.NodeNetworkUnavailable, Status: corev1.ConditionTrue}}
	require.False(t, ExternalVXLANNodeReady(node))
}

func TestExternalVXLANSecondaryProvider(t *testing.T) {
	subnet := kubeovnv1.Subnet{Spec: kubeovnv1.SubnetSpec{Vpc: "tenant", Provider: "vm-net.kubevirt-networks.ovn", CIDRBlock: "172.16.136.0/24", GatewayType: kubeovnv1.GWDistributedType, ExternalVXLAN: &kubeovnv1.ExternalVXLAN{VNI: 504231, GatewayMAC: "02:aa:00:e6:00:01", FRR: externalVXLANFixture().FRR}}}
	require.NoError(t, ValidateSubnet(subnet))
	for _, provider := range []string{"vm-net.kubevirt-networks", ".kubevirt-networks.ovn", "vm-net..ovn", "vm-net.kubevirt-networks.macvlan"} {
		subnetCopy := subnet.DeepCopy()
		subnetCopy.Spec.Provider = provider
		require.Error(t, ValidateExternalVXLANSubnet(*subnetCopy))
	}
	subnet.Spec.Vlan = "kv-231"
	require.Error(t, ValidateExternalVXLANSubnet(subnet), "provider is not a VLAN/ProviderNetwork attachment")
}

func TestValidateExternalVXLANGateway(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*kubeovnv1.ExternalVXLANGateway)
	}{
		{"zero VNI", func(c *kubeovnv1.ExternalVXLANGateway) { c.VNI = 0 }},
		{"oversized VNI", func(c *kubeovnv1.ExternalVXLANGateway) { c.VNI = 1 << 24 }},
		{"no FRR", func(c *kubeovnv1.ExternalVXLANGateway) { c.FRR = nil }},
		{"missing gateway node", func(c *kubeovnv1.ExternalVXLANGateway) { c.GatewayNode = "" }},
		{"VM MAC on transit", func(c *kubeovnv1.ExternalVXLANGateway) { c.GatewayMAC = "02:aa:00:00:00:01" }},
		{"bad transit", func(c *kubeovnv1.ExternalVXLANGateway) { c.TransitCIDR = "bad" }},
	}
	require.NoError(t, ValidateExternalVXLANGateway(externalVXLANFixture()))
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := externalVXLANFixture()
			test.mutate(config)
			require.Error(t, ValidateExternalVXLANGateway(config))
		})
	}
}

func TestValidateExternalVXLANAnycastSubnet(t *testing.T) {
	subnet := kubeovnv1.Subnet{Spec: kubeovnv1.SubnetSpec{Vpc: "tenant", CIDRBlock: "172.16.136.0/24", ExternalVXLAN: &kubeovnv1.ExternalVXLAN{
		VNI: 504231, GatewayMAC: "02:aa:00:00:00:01", FRR: externalVXLANFixture().FRR,
	}}}
	require.NoError(t, ValidateExternalVXLANSubnet(subnet))
	for _, mutate := range []func(*kubeovnv1.Subnet){
		func(s *kubeovnv1.Subnet) { s.Name = "evx-transit-conflict" },
		func(s *kubeovnv1.Subnet) { s.Spec.Vlan = "vlan1" },
		func(s *kubeovnv1.Subnet) { s.Spec.NatOutgoing = true },
		func(s *kubeovnv1.Subnet) { s.Spec.GatewayType = kubeovnv1.GWCentralizedType },
		func(s *kubeovnv1.Subnet) { s.Spec.Vpc = DefaultVpc },
		func(s *kubeovnv1.Subnet) { s.Spec.ExternalVXLAN.GatewayMAC = "" },
	} {
		subnetCopy := subnet.DeepCopy()
		mutate(subnetCopy)
		require.Error(t, ValidateExternalVXLANSubnet(*subnetCopy))
	}
}
