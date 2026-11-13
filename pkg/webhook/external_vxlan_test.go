package webhook

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	ovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
)

func externalVXLANAdmissionRequest(t *testing.T, object any) admission.Request {
	t.Helper()
	data, err := json.Marshal(object)
	require.NoError(t, err)
	return admission.Request{
		Object: runtime.RawExtension{Raw: data}, OldObject: runtime.RawExtension{Raw: data},
	}
}

func TestExternalVXLANSecondaryAdmission(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, ovnv1.AddToScheme(scheme))
	hook := &ValidatingHook{decoder: admission.NewDecoder(scheme), cache: &mockCache{}}
	subnet := &ovnv1.Subnet{
		APIVersion: "kubeovn.io/v1", Kind: "Subnet",
		Name: "vm-net", Spec: ovnv1.SubnetSpec{
			Vpc: "tenant", Provider: "vm-net.kubevirt-networks.ovn",
			CIDRBlock: "172.16.136.0/24", Gateway: "172.16.136.1", GatewayType: ovnv1.GWDistributedType,
			ExternalVXLAN: &ovnv1.ExternalVXLAN{
				VNI: 504231, GatewayMAC: "02:aa:00:e6:00:01",
				FRR: &ovnv1.ExternalVXLANFRR{Bridge: "cp504231", VXLAN: "cpvx504231", Advertise: "adv504231", VRF: "vrf230", TableID: 230, ControlPort: 60007},
			},
		},
	}
	for _, vlan := range []string{"", "kv-231"} {
		subnet.Spec.Vlan = vlan
		req := externalVXLANAdmissionRequest(t, subnet)
		require.Equal(t, vlan == "", hook.SubnetCreateHook(context.Background(), req).Allowed)
		require.Equal(t, vlan == "", hook.SubnetUpdateHook(context.Background(), req).Allowed)
	}
}

func TestExternalVXLANGatewayAdmissionCompatibility(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, ovnv1.AddToScheme(scheme))
	hook := &ValidatingHook{decoder: admission.NewDecoder(scheme), cache: &mockCache{}}
	legacy := &ovnv1.ExternalVXLANGateway{
		VNI: 220, GatewayNode: "worker-1", TransitCIDR: "169.254.220.11/24", RoutingPort: "native-220-worker-1",
		FRR: &ovnv1.ExternalVXLANFRR{Bridge: "cp220", VXLAN: "cpvx220", Advertise: "adv220", ControlPort: 60001, VRF: "vrf220", TableID: 220},
	}
	multi := legacy.DeepCopy()
	multi.GatewayNode, multi.TransitCIDR, multi.RoutingPort = "", "", ""
	multi.GatewayNodes = []ovnv1.ExternalVXLANGatewayNode{{Node: "worker-1", TransitCIDR: "169.254.220.11/24", RoutingPort: "native-220-worker-1"}, {Node: "worker-2", TransitCIDR: "169.254.220.12/24", RoutingPort: "native-220-worker-2"}}
	dynamic := multi.DeepCopy()
	dynamic.FRR = &ovnv1.ExternalVXLANFRR{Bridge: "cp220", VXLAN: "cpvx220", Advertise: "adv220", ControlPort: 60001, VRF: "vrf220", TableID: 220}
	invalid := dynamic.DeepCopy()
	invalid.FRR = nil
	for i, config := range []*ovnv1.ExternalVXLANGateway{legacy, multi, dynamic, invalid} {
		vpc := &ovnv1.Vpc{APIVersion: "kubeovn.io/v1", Kind: "Vpc", Name: "tenant", Spec: ovnv1.VpcSpec{ExternalVXLAN: config}}
		req := externalVXLANAdmissionRequest(t, vpc)
		require.Equal(t, i != 3, hook.VpcCreateHook(context.Background(), req).Allowed)
		require.Equal(t, i != 3, hook.VpcUpdateHook(context.Background(), req).Allowed)
	}
}
