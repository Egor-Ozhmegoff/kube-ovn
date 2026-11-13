package ipsec

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kubeovn/kube-ovn/pkg/ovs"
)

func TestExternalVXLANDisabledIPsecCleanup(t *testing.T) {
	if os.Getenv("KUBE_OVN_EXTERNAL_VXLAN_RUNTIME_TEST") != "true" {
		t.Skip("requires an isolated OVS container without IPsec state")
	}
	database, err := ovs.NewCNIVswitchClient("unix:/run/openvswitch/db.sock")
	require.NoError(t, err)
	t.Cleanup(database.Close)
	row, err := database.IPsecConfiguration()
	require.NoError(t, err)
	require.Empty(t, row.ExternalIDs["ovn-ipsec-protection-lease"])
	require.Empty(t, row.ExternalIDs["ovn-evpn-vxlan-ports"])
	for _, key := range []string{"certificate", "private_key", "ca_cert"} {
		require.Empty(t, row.OtherConfig[key])
	}
	command := func(args ...string) {
		t.Helper()
		output, err := exec.Command("ovs-vsctl", args...).CombinedOutput()
		require.NoError(t, err, "%s", output)
	}
	command("set", "Open_vSwitch", ".", "external_ids:ovn-evpn-vxlan-ports=4789")
	t.Cleanup(func() { command("remove", "Open_vSwitch", ".", "external_ids", "ovn-evpn-vxlan-ports") })
	_, err = database.IPsecDatapathConfiguration()
	require.ErrorContains(t, err, "does not support EVPN")
	config := Configuration{
		NodeName: "external-cleanup", PodUID: "test-pod", Namespace: "kube-system",
		Kube:   fake.NewClientset(&corev1.Node{Name: "external-cleanup", UID: "test-node-uid"}),
		KeyDir: t.TempDir(), RuntimeDir: t.TempDir(), ProtectionDir: t.TempDir(),
		OVSSocket: "/run/openvswitch/db.sock", Duration: time.Hour, RequestTimeout: time.Second, CleanupOnly: true,
	}
	a, err := New(config)
	require.NoError(t, err)
	t.Cleanup(func() {
		if a.ovs != nil {
			a.ovs.Close()
		}
	})
	needed, err := a.restoreCleanupProtection(t.Context())
	require.NoError(t, err, "disabled IPsec must not block native EVPN on CNI restart")
	require.False(t, needed)
	require.Nil(t, a.protection)
	// Unknown identity evidence must still fail closed even with EVPN enabled.
	require.NoError(t, database.SetIPsecConfiguration(row.UUID, map[string]string{"certificate": "/unowned-test-certificate"}))
	t.Cleanup(func() {
		require.NoError(t, database.SetIPsecConfiguration(row.UUID, map[string]string{"certificate": ""}))
	})
	_, err = a.restoreCleanupProtection(t.Context())
	require.ErrorContains(t, err, "without their private ownership reservation")
}
