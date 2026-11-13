package controller

import (
	"testing"

	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/ovs"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnnb"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnsb"
	"github.com/kubeovn/kube-ovn/pkg/util"
)

func TestExternalVXLANSecondaryAttachment(t *testing.T) {
	const provider = "vm-net.kubevirt-networks.ovn"
	subnet := &kubeovnv1.Subnet{Name: "vm-net", Spec: kubeovnv1.SubnetSpec{
		Vpc: "tenant", Provider: provider, CIDRBlock: "172.16.136.0/24", Gateway: "172.16.136.1",
		ExternalVXLAN: &kubeovnv1.ExternalVXLAN{VNI: 504231, GatewayMAC: "02:aa:00:e6:00:01"},
	}}
	pod := &corev1.Pod{Name: "vm", Namespace: "default", Annotations: map[string]string{
		nadv1.NetworkAttachmentAnnot: `[{"name":"vm-net","namespace":"kubevirt-networks","interface":"net1"}]`,
	}}
	fake, err := newFakeControllerWithOptions(t, &FakeControllerOptions{
		Subnets: []*kubeovnv1.Subnet{subnet},
		NetworkAttachments: []*nadv1.NetworkAttachmentDefinition{{
			Name: "vm-net", Namespace: "kubevirt-networks",
			Spec: nadv1.NetworkAttachmentDefinitionSpec{Config: `{"cniVersion":"0.3.1","name":"vm-net","type":"kube-ovn","provider":"vm-net.kubevirt-networks.ovn"}`},
		}},
	})
	require.NoError(t, err)
	nets, err := fake.fakeController.getPodAttachmentNet(pod)
	require.NoError(t, err)
	require.Len(t, nets, 1)
	require.Equal(t, providerTypeOriginal, nets[0].Type)
	require.Equal(t, provider, nets[0].ProviderName)
	require.Equal(t, "net1", nets[0].InterfaceName)
	require.Equal(t, subnet.Name, nets[0].Subnet.Name)
	require.NoError(t, fake.fakeController.ipam.AddOrUpdateSubnet(subnet.Name, subnet.Spec.CIDRBlock, subnet.Spec.Gateway, nil))
	ip, _, mac, err := fake.fakeController.ipam.GetRandomAddress("default/vm", "vm.default."+provider, nil, subnet.Name, "", nil, true)
	require.NoError(t, err)
	require.NotEmpty(t, ip)
	require.NotEmpty(t, mac)
	fake.fakeController.ipam.ReleaseAddressByPod("default/vm", subnet.Name)
	// Deletion must still resolve the provider if Multus removes its NAD first.
	pod.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
	require.NoError(t, fake.fakeInformers.netAttachInformer.GetStore().Delete(&nadv1.NetworkAttachmentDefinition{Name: "vm-net", Namespace: "kubevirt-networks"}))
	nets, err = fake.fakeController.getPodAttachmentNet(pod)
	require.NoError(t, err)
	require.Len(t, nets, 1)
	require.Equal(t, provider, nets[0].ProviderName)
	require.Equal(t, subnet.Name, nets[0].Subnet.Name)
}

func TestExternalVXLANFRRGatewayReadiness(t *testing.T) {
	config := &kubeovnv1.ExternalVXLANGateway{
		VNI:          220,
		FRR:          &kubeovnv1.ExternalVXLANFRR{Bridge: "cp220", VXLAN: "cpvx220", Advertise: "adv220", ControlPort: 60001, VRF: "vrf220", TableID: 220},
		GatewayNodes: []kubeovnv1.ExternalVXLANGatewayNode{{Node: "worker-1", TransitCIDR: "169.254.220.11/24", RoutingPort: "native-220-worker-1"}},
	}
	node := &corev1.Node{Name: "worker-1", Annotations: map[string]string{
		util.ExternalVXLANIPAnnotation: "10.110.191.11", util.ExternalVXLANMACAnnotation: "02:ee:0b:00:00:00",
	}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	vpc := &kubeovnv1.Vpc{Name: "tenant", Spec: kubeovnv1.VpcSpec{ExternalVXLAN: config}}
	proof, err := util.ExternalVXLANHostContract(node, nil, []kubeovnv1.Vpc{*vpc})
	require.NoError(t, err)
	node.Annotations[util.ExternalVXLANValidatedAnnotation] = proof
	fake, err := newFakeControllerWithOptions(t, &FakeControllerOptions{Nodes: []*corev1.Node{node}, Vpcs: []*kubeovnv1.Vpc{vpc}})
	require.NoError(t, err)
	fake.mockOvnSbClient.EXPECT().GetChassisByHost(node.Name).Return(&ovnsb.Chassis{Name: "hv1"}, nil).Times(3)
	for _, status := range []corev1.ConditionStatus{corev1.ConditionTrue, corev1.ConditionUnknown, corev1.ConditionTrue} {
		node = node.DeepCopy()
		node.Status.Conditions[0].Status = status
		require.NoError(t, fake.fakeInformers.nodeInformer.Informer().GetStore().Update(node))
		fake.mockOvnClient.EXPECT().ReconcileExternalVXLANRouterGateways("tenant", gomock.Any(), config).DoAndReturn(
			func(_ string, gateways []ovs.ExternalVXLANRouterGateway, _ *kubeovnv1.ExternalVXLANGateway) error {
				if status == corev1.ConditionTrue {
					require.Len(t, gateways, 1)
					require.Equal(t, "02:ee:0b:00:00:00", gateways[0].MAC)
				} else {
					require.Len(t, gateways, 1, "FRR must retain the VM host RMAC for type-2 return traffic")
					require.False(t, gateways[0].Active)
				}
				return nil
			},
		)
		require.NoError(t, fake.fakeController.reconcileExternalVXLANVpc(vpc))
		require.Equal(t, status == corev1.ConditionTrue, len(vpc.Status.ExternalVXLANReadyNodes) == 1)
	}
	require.NoError(t, fake.fakeInformers.nodeInformer.Informer().GetStore().Delete(node))
	fake.mockOvnClient.EXPECT().ReconcileExternalVXLANRouterGateways("tenant", gomock.Len(0), config).Return(nil)
	require.NoError(t, fake.fakeController.reconcileExternalVXLANVpc(vpc))
}

func TestExternalVXLANNodeGatewayIdentity(t *testing.T) {
	node := &corev1.Node{Name: "worker-1", Annotations: map[string]string{
		util.ExternalVXLANIPAnnotation: "10.110.191.11", util.ExternalVXLANMACAnnotation: "02:ee:0b:00:00:00",
	}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	fake, err := newFakeControllerWithOptions(t, &FakeControllerOptions{Nodes: []*corev1.Node{node}})
	require.NoError(t, err)
	spec := kubeovnv1.ExternalVXLANGatewayNode{Node: node.Name, TransitCIDR: "169.254.220.11/24"}
	vpc := &kubeovnv1.Vpc{Spec: kubeovnv1.VpcSpec{ExternalVXLAN: &kubeovnv1.ExternalVXLANGateway{GatewayNodes: []kubeovnv1.ExternalVXLANGatewayNode{spec}}}}
	gateway, err := fake.fakeController.externalVXLANNodeGateway(vpc, spec, node)
	require.NoError(t, err)
	require.True(t, gateway.Active)
	require.Equal(t, node.Name, gateway.Node)
	require.Equal(t, "02:ee:0b:00:00:00", gateway.MAC)
	spec.Egress = new(false)
	gateway, err = fake.fakeController.externalVXLANNodeGateway(vpc, spec, node)
	require.NoError(t, err)
	require.False(t, gateway.Active, "explicit egress:false retains only the ingress identity")
	spec.Egress = nil
	node.Status.Conditions[0].Status = corev1.ConditionUnknown
	gateway, err = fake.fakeController.externalVXLANNodeGateway(vpc, spec, node)
	require.NoError(t, err)
	require.False(t, gateway.Active)
	vpc.Spec.ExternalVXLAN.GatewayNodes = nil
	gateway, err = fake.fakeController.externalVXLANNodeGateway(vpc, spec, node)
	require.NoError(t, err)
	require.Empty(t, gateway.Node, "legacy port names must be retained")
}

func TestExternalVXLANHostContractGatesActivation(t *testing.T) {
	frr := &kubeovnv1.ExternalVXLANFRR{Bridge: "cp220", VXLAN: "cpvx220", Advertise: "adv220", VRF: "vrf220", TableID: 220, ControlPort: 60001}
	vpc := &kubeovnv1.Vpc{Name: "tenant", Spec: kubeovnv1.VpcSpec{ExternalVXLAN: &kubeovnv1.ExternalVXLANGateway{
		VNI: 220, FRR: frr, GatewayNode: "worker-1", TransitCIDR: "169.254.220.11/24", RoutingPort: "native220",
	}}}
	subnet := &kubeovnv1.Subnet{Name: "vm-net", Spec: kubeovnv1.SubnetSpec{Vpc: vpc.Name, ExternalVXLAN: &kubeovnv1.ExternalVXLAN{VNI: 504221, FRR: frr}}}
	first := &corev1.Node{Name: "worker-1", Annotations: map[string]string{util.ExternalVXLANIPAnnotation: "10.110.191.11", util.ExternalVXLANMACAnnotation: "02:ee:0b:00:00:00"}}
	second := &corev1.Node{Name: "worker-2", Annotations: map[string]string{util.ExternalVXLANIPAnnotation: "10.110.191.12", util.ExternalVXLANMACAnnotation: "02:ee:0c:00:00:00"}}
	fc, err := newFakeControllerWithOptions(t, &FakeControllerOptions{
		Nodes: []*corev1.Node{first, second}, Vpcs: []*kubeovnv1.Vpc{vpc}, Subnets: []*kubeovnv1.Subnet{subnet},
		IPs: []*kubeovnv1.IP{{Name: "vm-on-worker-2", Spec: kubeovnv1.IPSpec{Subnet: subnet.Name, NodeName: second.Name}}},
	})
	require.NoError(t, err)
	deny := func() {
		t.Helper()
		// No non-nil config, chassis lookup or router creation is allowed.
		fc.mockOvnClient.EXPECT().DeactivateExternalVXLANRouter(vpc.Name).Return(nil)
		fc.mockOvnClient.EXPECT().ReconcileExternalVXLANSwitch(subnet.Name, gomock.Nil()).Return(nil)
		require.ErrorContains(t, fc.fakeController.ensureExternalVXLANHostContracts(vpc), "has not validated")
	}
	deny()
	approve := func(node *corev1.Node) {
		t.Helper()
		proof, err := util.ExternalVXLANHostContract(node, []kubeovnv1.Subnet{*subnet}, []kubeovnv1.Vpc{*vpc})
		require.NoError(t, err)
		node.Annotations[util.ExternalVXLANValidatedAnnotation] = proof
		require.NoError(t, fc.fakeInformers.nodeInformer.Informer().GetStore().Update(node.DeepCopy()))
	}
	approve(first)
	deny() // An externally annotated VM host also needs validation.
	approve(second)
	require.NoError(t, fc.fakeController.ensureExternalVXLANHostContracts(vpc))
	withoutVTEP := second.DeepCopy()
	delete(withoutVTEP.Annotations, util.ExternalVXLANIPAnnotation)
	require.NoError(t, fc.fakeInformers.nodeInformer.Informer().GetStore().Update(withoutVTEP))
	require.ErrorContains(t, fc.fakeController.validateExternalVXLANHostContracts(vpc), "no external VTEP identity", "existing workload allocations must not bypass validation")
	approve(second)
	first.Annotations[util.ExternalVXLANValidatedAnnotation] = ""
	require.NoError(t, fc.fakeInformers.nodeInformer.Informer().GetStore().Update(first.DeepCopy()))
	deny() // Runtime validation failure withdraws existing native exchange.
	approve(first)
	frr.Bridge = "changed-bridge"
	require.NoError(t, fc.fakeInformers.vpcInformer.Informer().GetStore().Update(vpc.DeepCopy()))
	deny() // Old acknowledgement cannot authorize a changed interface.
}

func TestExternalVXLANReadyNodesPersistThroughVpcReconciliation(t *testing.T) {
	config := &kubeovnv1.ExternalVXLANGateway{VNI: 220, GatewayNode: "worker-1", TransitCIDR: "169.254.220.11/24", FRR: &kubeovnv1.ExternalVXLANFRR{Bridge: "cp220", VXLAN: "cpvx220", Advertise: "adv220", ControlPort: 60001, VRF: "vrf220", TableID: 220}, RoutingPort: "native-220-worker-1"}
	vpc := &kubeovnv1.Vpc{Name: "tenant", Finalizers: []string{util.KubeOVNControllerFinalizer}, Spec: kubeovnv1.VpcSpec{ExternalVXLAN: config}}
	node := &corev1.Node{Name: "worker-1", Annotations: map[string]string{
		util.ExternalVXLANIPAnnotation: "10.110.191.11", util.ExternalVXLANMACAnnotation: "02:ee:0b:00:00:00",
	}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	proof, err := util.ExternalVXLANHostContract(node, nil, []kubeovnv1.Vpc{*vpc})
	require.NoError(t, err)
	node.Annotations[util.ExternalVXLANValidatedAnnotation] = proof
	fc, err := newFakeControllerWithOptions(t, &FakeControllerOptions{Vpcs: []*kubeovnv1.Vpc{vpc}, Nodes: []*corev1.Node{node}})
	require.NoError(t, err)
	controller := fc.fakeController
	controller.config.ClusterRouter = "other-router"
	controller.config.EnableEipSnat = false
	fc.mockOvnSbClient.EXPECT().GetChassisByHost("worker-1").Return(&ovnsb.Chassis{Name: "hv1"}, nil)
	fc.mockOvnClient.EXPECT().CreateLogicalRouterWithOptions("tenant", gomock.Any()).Return(nil)
	fc.mockOvnClient.EXPECT().ReconcileExternalVXLANRouterGateways("tenant", gomock.Len(1), config).Return(nil)
	fc.mockOvnClient.EXPECT().ListLogicalRouterStaticRoutes("tenant", nil, nil, "", gomock.Any()).Return(nil, nil)
	fc.mockOvnClient.EXPECT().ListLogicalRouterPolicies("tenant", -1, nil, true).Return(nil, nil)
	fc.mockOvnClient.EXPECT().ReconcileBFDLogicalRouterPort("tenant", "bfd@tenant", nil, nil, false).Return(nil)
	require.NoError(t, controller.handleAddOrUpdateVpc("tenant"))
	got, err := controller.config.KubeOvnClient.KubeovnV1().Vpcs().Get(t.Context(), "tenant", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, []string{"worker-1"}, got.Status.ExternalVXLANReadyNodes)
}

func TestExternalVXLANRetainsIngressWhileVMIPsRemain(t *testing.T) {
	frr := &kubeovnv1.ExternalVXLANFRR{Bridge: "cp220", VXLAN: "cpvx220", Advertise: "adv220", ControlPort: 60001, VRF: "vrf220", TableID: 220}
	vpc := &kubeovnv1.Vpc{Name: "tenant", Spec: kubeovnv1.VpcSpec{ExternalVXLAN: &kubeovnv1.ExternalVXLANGateway{
		VNI: 220, FRR: frr,
		GatewayNodes: []kubeovnv1.ExternalVXLANGatewayNode{{Node: "worker-2", TransitCIDR: "169.254.220.12/24", RoutingPort: "native-220-worker-2"}},
	}}}
	node := &corev1.Node{Name: "worker-1", Annotations: map[string]string{
		util.ExternalVXLANIPAnnotation: "10.110.191.11", util.ExternalVXLANMACAnnotation: "02:ee:0b:00:00:00",
	}}
	ip := &kubeovnv1.IP{Name: "vm.default.vm-net.default.ovn", Spec: kubeovnv1.IPSpec{Subnet: "vm-net", NodeName: node.Name}}
	subnet := &kubeovnv1.Subnet{Name: "vm-net", Spec: kubeovnv1.SubnetSpec{Vpc: "tenant", ExternalVXLAN: &kubeovnv1.ExternalVXLAN{VNI: 504221, FRR: frr}}}
	fc, err := newFakeControllerWithOptions(t, &FakeControllerOptions{Nodes: []*corev1.Node{node}, Subnets: []*kubeovnv1.Subnet{subnet}, IPs: []*kubeovnv1.IP{ip}})
	require.NoError(t, err)
	fc.mockOvnClient.EXPECT().ListLogicalRouterPorts(gomock.Any(), gomock.Nil()).Return([]ovnnb.LogicalRouterPort{{
		Name: "tenant-evx-transit-tenant-worker-1", MAC: "02:ee:0b:00:00:00", Networks: []string{"169.254.220.11/24"},
		Options: map[string]string{"dynamic-routing-port-name": "native-220-worker-1"}, ExternalIDs: map[string]string{util.ExternalVXLANNode: "worker-1"},
	}}, nil).Times(2)
	fc.mockOvnSbClient.EXPECT().GetChassisByHost("worker-1").Return(&ovnsb.Chassis{Name: "hv1"}, nil).Times(2)
	retained, err := fc.fakeController.externalVXLANIngressGateways(vpc, nil)
	require.NoError(t, err)
	require.Len(t, retained, 1)
	require.Equal(t, "worker-1", retained[0].Host)
	require.False(t, retained[0].Active)
	require.Equal(t, "02:ee:0b:00:00:00", retained[0].MAC)
	// A remaining ordinary VPC workload allocation also blocks premature
	// ingress removal, including during external attachment reconfiguration.
	subnet = subnet.DeepCopy()
	subnet.Spec.ExternalVXLAN = nil
	require.NoError(t, fc.fakeInformers.subnetInformer.Informer().GetStore().Update(subnet))
	retained, err = fc.fakeController.externalVXLANIngressGateways(vpc, nil)
	require.NoError(t, err)
	require.Len(t, retained, 1)
	require.NoError(t, fc.fakeInformers.ipInformer.Informer().GetStore().Delete(ip))
	retained, err = fc.fakeController.externalVXLANIngressGateways(vpc, nil)
	require.NoError(t, err)
	require.Empty(t, retained, "release/move of the final VPC IP allows stale ingress cleanup")
}
