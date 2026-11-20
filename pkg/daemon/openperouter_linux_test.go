package daemon

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnsb"
)

func TestOpenPERouterOnlyAdvertisesFreshInstalledWorkloads(t *testing.T) {
	subnet := &kubeovnv1.Subnet{Name: "pods", Spec: kubeovnv1.SubnetSpec{Vpc: "tenant-a", CIDRBlock: "10.101.3.0/24"}}
	subnet.Status.Ready("Ready", "fixture")
	pod := corev1.Pod{Name: "probe", Namespace: "ns", Annotations: map[string]string{
		"ovn.kubernetes.io/ip_address":     "10.101.3.11",
		"ovn.kubernetes.io/allocated":      "true",
		"ovn.kubernetes.io/logical_switch": "pods",
	}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	binding := ovnsb.PortBinding{LogicalPort: "probe.ns", MAC: []string{"02:00:00:00:03:11 10.101.3.11"}}
	bound := map[string]ovnsb.PortBinding{binding.LogicalPort: binding}
	installed := map[string]bool{binding.LogicalPort: true}
	collect := func(p corev1.Pod) map[netip.Prefix]struct{} {
		return localOpenPERouterPrefixes([]corev1.Pod{p}, []*kubeovnv1.Subnet{subnet}, "tenant-a", bound, installed)
	}
	require.Contains(t, collect(pod), netip.MustParsePrefix("10.101.3.11/32"))
	delete(installed, binding.LogicalPort)
	require.Empty(t, collect(pod), "OVS interface disappearance withdraws")
	installed[binding.LogicalPort] = true
	delete(bound, binding.LogicalPort)
	require.Empty(t, collect(pod), "a prepared target without a primary binding cannot announce")
	bound[binding.LogicalPort] = binding
	unavailable := *pod.DeepCopy()
	unavailable.Status.Conditions = nil
	require.Contains(t, collect(unavailable), netip.MustParsePrefix("10.101.3.11/32"), "application readiness must not gate network reachability")
	unavailable.Status.Phase = corev1.PodPending
	require.Contains(t, collect(unavailable), netip.MustParsePrefix("10.101.3.11/32"), "installed networking is independent of container startup")
	unavailable = *pod.DeepCopy()
	now := metav1.Now()
	unavailable.DeletionTimestamp = &now
	require.Empty(t, collect(unavailable))
	unavailable = *pod.DeepCopy()
	unavailable.Annotations["ovn.kubernetes.io/ip_address"] = "10.101.3.12"
	require.Empty(t, collect(unavailable), "annotation alone does not prove SB ownership")
	unavailable.Annotations["ovn.kubernetes.io/ip_address"] = "203.0.113.11"
	bound[binding.LogicalPort] = ovnsb.PortBinding{MAC: []string{"02:00:00:00:03:11 203.0.113.11"}}
	require.Empty(t, collect(unavailable), "outside subnet must not be exported")
}
