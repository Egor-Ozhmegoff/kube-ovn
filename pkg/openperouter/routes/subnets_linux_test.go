package routes

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
)

func TestRoutedSubnetPrefixesExcludeNativeL2(t *testing.T) {
	vpc := &kubeovnv1.Vpc{Name: "a", Spec: kubeovnv1.VpcSpec{OpenPERouter: &kubeovnv1.OpenPERouterIntegration{TransitCIDRs: []string{"169.254.32.0/20"}, HostSessionCIDRs: []string{"169.254.16.0/20"}}}}
	routed := &kubeovnv1.Subnet{Name: "pods", Spec: kubeovnv1.SubnetSpec{Vpc: "a", CIDRBlock: "10.3.0.0/24,fd00:3::/64"}}
	routed.Status.Ready("Ready", "fixture")
	l2 := routed.DeepCopy()
	l2.Name = "native"
	l2.Spec.CIDRBlock = "10.1.0.0/24,fd00:1::/64"
	l2.Spec.OpenPERouterL2 = &kubeovnv1.OpenPERouterL2Attachment{VNI: 10101}
	prefixes, err := RoutedSubnetPrefixes(vpc, []*kubeovnv1.Subnet{routed, l2})
	require.NoError(t, err)
	require.Len(t, prefixes, 2)
	require.Contains(t, prefixes, netip.MustParsePrefix("10.3.0.0/24"))
	require.Contains(t, prefixes, netip.MustParsePrefix("fd00:3::/64"))
	for _, p := range []string{"10.1.0.0/24", "fd00:1::/64", "10.3.0.11/32", "fd00:3::b/128"} {
		require.NotContains(t, prefixes, netip.MustParsePrefix(p))
	}
	l2.Status.Ready("Ready", "another VM moved")
	same, err := RoutedSubnetPrefixes(vpc, []*kubeovnv1.Subnet{l2, routed})
	require.NoError(t, err)
	require.Equal(t, prefixes, same)
}

func TestRoutedSubnetConfigurationLifecycle(t *testing.T) {
	vpc := &kubeovnv1.Vpc{Name: "a", Spec: kubeovnv1.VpcSpec{OpenPERouter: &kubeovnv1.OpenPERouterIntegration{TransitCIDRs: []string{"169.254.32.0/20"}, HostSessionCIDRs: []string{"169.254.16.0/20"}}}}
	subnet := &kubeovnv1.Subnet{Name: "pods", Spec: kubeovnv1.SubnetSpec{Vpc: "a", CIDRBlock: "10.3.0.0/24"}}
	subnet.Status.Ready("Ready", "fixture")
	for _, mutate := range []func(*kubeovnv1.Subnet){
		func(s *kubeovnv1.Subnet) { now := metav1.Now(); s.DeletionTimestamp = &now },
		func(s *kubeovnv1.Subnet) { s.Spec.Vpc = "b" },
		func(s *kubeovnv1.Subnet) { s.Spec.Provider = "external.ipam" },
		func(s *kubeovnv1.Subnet) { s.Status.NotReady("not ready", "fixture") },
	} {
		changed := subnet.DeepCopy()
		mutate(changed)
		prefixes, err := RoutedSubnetPrefixes(vpc, []*kubeovnv1.Subnet{changed})
		require.NoError(t, err)
		require.Empty(t, prefixes)
	}
	subnet.Spec.NatOutgoing = true
	_, err := RoutedSubnetPrefixes(vpc, []*kubeovnv1.Subnet{subnet})
	require.ErrorContains(t, err, "NAT")
}
