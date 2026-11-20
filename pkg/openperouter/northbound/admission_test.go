package northbound

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/openperouter"
	"github.com/kubeovn/kube-ovn/pkg/openperouter/barrier"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnnb"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/table"
)

func TestAdmissionBeforePortAndConstantMACMigration(t *testing.T) {
	db := testDatabase(t)
	subnet, store, nodes := admissionFixture(t, db)
	ctx := t.Context()
	first, second := "02:00:00:00:01:11", "02:00:00:00:01:12"
	guard := &openperouter.MACGuard{}
	require.NoError(t, EnsureL2(ctx, db, testVpc(), subnet, guard, first))
	require.NoError(t, AdmitMACs(ctx, db, subnet, store))
	cm, state, err := store.Read(ctx)
	require.NoError(t, err)
	for _, node := range nodes {
		require.NoError(t, store.JoinClosed(ctx, cm.UID, node.UID, node.Name, "session"))
		require.NoError(t, store.InstallIntent(ctx, cm.UID, node.UID, "session", state.Revision))
		require.NoError(t, store.Acknowledge(ctx, cm.UID, node.UID, "session", state.Revision))
	}
	require.NoError(t, AdmitMACs(ctx, db, subnet, store, first))
	require.Eventually(t, func() bool { ok, err := ProtectedMACs(ctx, db, subnet, []string{first}); return err == nil && ok }, time.Second, 10*time.Millisecond)
	require.NoError(t, EnsureL2(ctx, db, testVpc(), subnet, guard, second))
	require.ErrorIs(t, AdmitMACs(ctx, db, subnet, store, second), barrier.ErrPending)
	_, state, err = store.Read(ctx)
	require.NoError(t, err)
	require.NoError(t, store.Acknowledge(ctx, cm.UID, nodes[0].UID, "session", state.Revision))
	require.ErrorIs(t, AdmitMACs(ctx, db, subnet, store, second), barrier.ErrPending, "delayed B prevents creation")
	port := &ovnnb.LogicalSwitchPort{Name: "new-endpoint"}
	require.ErrorIs(t, readRow(ctx, db, port), table.ErrNotFound)
	require.NoError(t, AdmitMACs(ctx, db, subnet, store, first), "migration's existing MAC does not wait for unrelated new MAC")
	require.NoError(t, store.Acknowledge(ctx, cm.UID, nodes[1].UID, "session", state.Revision))
	require.NoError(t, AdmitMACs(ctx, db, subnet, store, second))
	require.NoError(t, EnsureL2(ctx, db, testVpc(), subnet, &openperouter.MACGuard{}))
	macs, err := ReflectionMACs(ctx, db, subnet, first, second)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{first, second}, macs, "daemon/controller restart cannot shrink a proven filter set")
	require.NoError(t, Cleanup(ctx, db, nil, nil, guard))
	require.Eventually(t, func() bool {
		var rows []ovnnb.AddressSet
		return db.Table(&ovnnb.AddressSet{}).List(ctx, &rows) == nil && len(rows) == 0
	}, time.Second, 10*time.Millisecond, "desired and admitted sets follow subnet cleanup")
}

func TestLostBarrierCannotForgetAnOpenMember(t *testing.T) {
	db := testDatabase(t)
	subnet, store, _ := admissionFixture(t, db)
	ctx := t.Context()
	mac := "02:00:00:00:01:11"
	require.NoError(t, EnsureL2(ctx, db, testVpc(), subnet, &openperouter.MACGuard{}, mac))
	require.NoError(t, AdmitMACs(ctx, db, subnet, store, mac))
	row := &ovnnb.AddressSet{Name: openperouter.ReflectionAddressSet(string(subnet.UID), subnet.Name)}
	require.Eventually(t, func() bool { return db.Table(row).Get(ctx, row) == nil && row.ExternalIDs["barrier-uid"] != "" }, time.Second, 10*time.Millisecond)
	require.NoError(t, store.Maps.Delete(ctx, barrier.Name(subnet.UID), metav1.DeleteOptions{}))
	require.ErrorIs(t, AdmitMACs(ctx, db, subnet, store, mac), barrier.ErrPending)
}

func admissionFixture(t *testing.T, db *table.Database) (*kubeovnv1.Subnet, barrier.Store, []corev1.Node) {
	t.Helper()
	subnet := &kubeovnv1.Subnet{Name: "blue", UID: "subnet-blue", Spec: kubeovnv1.SubnetSpec{Vpc: "tenant-a", CIDRBlock: "10.101.1.0/24", Gateway: "10.101.1.1", OpenPERouterL2: &kubeovnv1.OpenPERouterL2Attachment{L2VNIName: "blue", VNI: 10101, GatewayMAC: "02:00:05:00:03:00"}}}
	ls := &ovnnb.LogicalSwitch{Name: subnet.Name}
	require.NoError(t, db.Table(ls).Create(t.Context(), "fixture", ls))
	require.Eventually(t, func() bool { return readRow(t.Context(), db, ls) == nil }, time.Second, 10*time.Millisecond)
	client := admissionClient()
	store := barrier.Store{Maps: client.CoreV1().ConfigMaps("kube-system"), SubnetName: subnet.Name, SubnetUID: subnet.UID}
	nodes := []corev1.Node{{Name: "a", UID: "node-a"}, {Name: "b", UID: "node-b"}}
	return subnet, store, nodes
}

func admissionClient() *fake.Clientset {
	client := fake.NewClientset()
	client.PrependReactor("create", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
		action.(ktesting.CreateAction).GetObject().(*corev1.ConfigMap).UID = "barrier-uid"
		action.(ktesting.CreateAction).GetObject().(*corev1.ConfigMap).ResourceVersion = "1"
		return false, nil, nil
	})
	return client
}

func TestUnregisteredNodeDoesNotBlockAdmission(t *testing.T) {
	db := testDatabase(t)
	subnet, store, _ := admissionFixture(t, db)
	client := admissionClient()
	store.Maps = client.CoreV1().ConfigMaps("kube-system")
	ctx := t.Context()
	guard := &openperouter.MACGuard{}
	first, second, third := "02:00:00:00:01:11", "02:00:00:00:01:12", "02:00:00:00:01:13"
	require.NoError(t, EnsureL2(ctx, db, testVpc(), subnet, guard, first))
	require.NoError(t, AdmitMACs(ctx, db, subnet, store, first), "first run with no attachments is safe")
	cm, state, err := store.Read(ctx)
	require.NoError(t, err)
	require.Empty(t, state.Members)
	require.NoError(t, store.JoinAfterClose(ctx, cm.UID, "healthy", "healthy", "session", func() error { return nil }))
	require.NoError(t, store.ProtectAndOpen(ctx, cm.UID, "healthy", "session", state.Revision, func() error { return nil }, func() error { return nil }))
	_, err = client.CoreV1().Nodes().Create(ctx, &corev1.Node{Name: "unprepared", UID: "new-node"}, metav1.CreateOptions{})
	require.NoError(t, err)
	// Simulate a placeholder persisted by the previous all-Nodes Publish.
	cm, state, err = store.Read(ctx)
	require.NoError(t, err)
	state.Members["new-node"] = barrier.Member{Name: "unprepared", Phase: barrier.Pending}
	data, err := json.Marshal(state)
	require.NoError(t, err)
	cm.Data["state"] = string(data)
	_, err = store.Maps.Update(ctx, cm, metav1.UpdateOptions{})
	require.NoError(t, err)
	admit := func(mac string) {
		t.Helper()
		require.NoError(t, EnsureL2(ctx, db, testVpc(), subnet, guard, mac))
		require.NoError(t, AdmitMACs(ctx, db, subnet, store))
		_, state, err = store.Read(ctx)
		require.NoError(t, err)
		require.Len(t, state.Members, 1)
		require.NotContains(t, state.Members, "new-node")
		require.NoError(t, store.ProtectAndOpen(ctx, cm.UID, "healthy", "session", state.Revision, func() error { return nil }, func() error { return nil }))
		require.NoError(t, AdmitMACs(ctx, db, subnet, store, mac))
	}
	admit(second)
	require.NoError(t, client.CoreV1().Nodes().Delete(ctx, "unprepared", metav1.DeleteOptions{}))
	admit(third)
}

func TestJoinAndFinalOpenInterleavingsUseAdmission(t *testing.T) {
	db := testDatabase(t)
	subnet, store, _ := admissionFixture(t, db)
	ctx := t.Context()
	guard := &openperouter.MACGuard{}
	first, second := "02:00:00:00:01:11", "02:00:00:00:01:12"
	require.NoError(t, EnsureL2(ctx, db, testVpc(), subnet, guard, first))
	require.NoError(t, AdmitMACs(ctx, db, subnet, store))
	cm, state, err := store.Read(ctx)
	require.NoError(t, err)
	oldRevision := state.Revision
	// Join races a Ready read: a new Closed member cannot forward. Once it
	// commits Pending, admission sees it until its exact filter is acknowledged.
	require.NoError(t, store.Ready(ctx, oldRevision))
	require.NoError(t, store.JoinAfterClose(ctx, cm.UID, "joining", "joining", "session", func() error { return nil }))
	require.NoError(t, AdmitMACs(ctx, db, subnet, store, first))
	require.NoError(t, store.ProtectAndOpen(ctx, cm.UID, "joining", "session", oldRevision, func() error { return nil }, func() error {
		// Publish lands after the final API authorization, before physical Open.
		require.NoError(t, EnsureL2(ctx, db, testVpc(), subnet, guard, second))
		require.ErrorIs(t, AdmitMACs(ctx, db, subnet, store, second), barrier.ErrPending)
		return nil
	}))
	require.ErrorIs(t, store.ProtectAndOpen(ctx, cm.UID, "joining", "session", oldRevision, func() error { t.Fatal("stale revision installed"); return nil }, func() error { t.Fatal("stale revision opened"); return nil }), barrier.ErrPending)
	_, state, err = store.Read(ctx)
	require.NoError(t, err)
	require.NoError(t, store.ProtectAndOpen(ctx, cm.UID, "joining", "session", state.Revision, func() error {
		require.ErrorIs(t, AdmitMACs(ctx, db, subnet, store, second), barrier.ErrPending, "Pending blocks admission while preparation is delayed")
		return nil
	}, func() error { return nil }))
	require.NoError(t, AdmitMACs(ctx, db, subnet, store, second))
	// Losing the old Node object does not close its edge; a new UID is a
	// separate participant, even if Kubernetes reuses the same Node name.
	require.NoError(t, store.JoinAfterClose(ctx, cm.UID, "new-uid", "joining", "new-session", func() error { return nil }))
	third := "02:00:00:00:01:13"
	require.NoError(t, EnsureL2(ctx, db, testVpc(), subnet, guard, third))
	require.ErrorIs(t, AdmitMACs(ctx, db, subnet, store, third), barrier.ErrPending)
	require.NoError(t, store.Close(ctx, cm.UID, "joining", "session"))
	require.NoError(t, AdmitMACs(ctx, db, subnet, store, third), "only confirmed closure unblocks the old participant")
}
