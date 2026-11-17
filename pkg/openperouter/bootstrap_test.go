package openperouter

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
)

func exampleBootstrap(t *testing.T) *Bootstrap {
	t.Helper()
	directory := t.TempDir()
	for target, source := range map[string]string{
		"node-config.yaml":   "node-a/node-config.yaml",
		"openpe_fabric.yaml": "openpe_fabric.yaml",
	} {
		data, err := os.ReadFile(filepath.Join("../../docs/openperouter/examples", source))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(directory, target), data, 0o600))
	}
	b, err := ReadBootstrap(directory)
	require.NoError(t, err)
	return b
}

func TestNativeBootstrapContract(t *testing.T) {
	b := exampleBootstrap(t)
	spec := testIntegration()
	require.Equal(t, 11, b.Index)
	require.Equal(t, "02:00:00:01:00:0b", b.RouterMAC)
	require.NoError(t, b.Validate(spec), "native bootstrap does not require FRR discard routes")
	subnet := &kubeovnv1.Subnet{Spec: kubeovnv1.SubnetSpec{
		CIDRBlock: "10.101.1.0/24", Gateway: "10.101.1.1",
		OpenPERouterL2: &kubeovnv1.OpenPERouterL2Attachment{L2VNIName: "tenant-a-blue", VNI: 10101, GatewayMAC: "02:00:05:00:03:00"},
	}}
	require.NoError(t, b.ValidateL2(spec, subnet))
	b.L2VNIs[0].HostMaster = &struct{ Type string }{Type: "bridge"}
	require.ErrorContains(t, b.ValidateL2(spec, subnet), "competing hostMaster")
}

func TestRejectNativeConfigurationDrift(t *testing.T) {
	for name, mutate := range map[string]func(*Bootstrap){
		"duplicate L3VNI": func(b *Bootstrap) { b.L3VNIs = append(b.L3VNIs, b.L3VNIs[0]) },
		"ASN drift":       func(b *Bootstrap) { b.L3VNIs[0].HostSession.HostASN++ },
		"pool drift":      func(b *Bootstrap) { b.L3VNIs[0].HostSession.LocalCIDRs = []string{"169.254.64.0/20"} },
		"VRF drift":       func(b *Bootstrap) { b.L3VNIs[0].VRF = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			b := exampleBootstrap(t)
			mutate(b)
			require.Error(t, b.Validate(testIntegration()))
		})
	}
}

func TestRejectConflictingRoutingDomains(t *testing.T) {
	a := &kubeovnv1.Vpc{Name: "a", Spec: kubeovnv1.VpcSpec{OpenPERouter: testIntegration()}}
	b := a.DeepCopy()
	b.Name = "b"
	require.ErrorContains(t, ValidateVpcConflicts(a, []*kubeovnv1.Vpc{b}), "conflicts")
	b.Spec.OpenPERouter.VRF = "b"
	b.Spec.OpenPERouter.L3VNIName = "b"
	b.Spec.OpenPERouter.L3VNI++
	require.ErrorContains(t, ValidateVpcConflicts(a, []*kubeovnv1.Vpc{b}), "overlaps")
	b.Spec.OpenPERouter.HostSessionCIDRs = []string{"169.254.64.0/20"}
	require.NoError(t, ValidateVpcConflicts(a, []*kubeovnv1.Vpc{b}))
	subnet := &kubeovnv1.Subnet{Spec: kubeovnv1.SubnetSpec{Vpc: "a", CIDRBlock: "10.101.3.0/24"}}
	require.NoError(t, ValidateSubnetBinding(subnet, a, nil), "Pod-only network needs no L2VNI")
	subnet.Spec.NatOutgoing = true
	require.ErrorContains(t, ValidateSubnetBinding(subnet, a, nil), "NAT")
	subnet.Spec.NatOutgoing = false
	subnet.Spec.CIDRBlock = "169.254.32.0/24"
	require.ErrorContains(t, ValidateSubnetBinding(subnet, a, nil), "overlaps")
}
