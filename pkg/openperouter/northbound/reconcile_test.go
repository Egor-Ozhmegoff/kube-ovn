package northbound

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/database/inmemory"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
	"github.com/ovn-kubernetes/libovsdb/server"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/openperouter"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnnb"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/table"
)

type recordingProvider struct {
	table.Provider
	operations int
}

type recordingHandle struct {
	table.Handle
	owner *recordingProvider
}

func (p *recordingProvider) Table(row model.Model) table.Handle {
	return &recordingHandle{Handle: p.Provider.Table(row), owner: p}
}

func (h *recordingHandle) Transact(ctx context.Context, method string, operations ...ovsdb.Operation) error {
	h.owner.operations += len(operations)
	return h.Handle.Transact(ctx, method, operations...)
}

func testDatabase(t *testing.T) *table.Database {
	t.Helper()
	cm, err := ovnnb.FullDatabaseModel()
	require.NoError(t, err)
	dm, errs := model.NewDatabaseModel(ovnnb.Schema(), cm)
	require.Empty(t, errs)
	logger := logr.Discard()
	memory := inmemory.NewDatabase(map[string]model.ClientDBModel{ovnnb.DatabaseName: cm}, &logger)
	srv, err := server.NewOvsdbServer(memory, &logger, dm)
	require.NoError(t, err)
	directory, err := os.MkdirTemp("/tmp", "opnb-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(directory)) })
	socket := filepath.Join(directory, "nb.sock")
	done := make(chan error, 1)
	go func() { done <- srv.Serve("unix", socket) }()
	require.Eventually(t, func() bool {
		select {
		case err := <-done:
			t.Fatalf("serve in-memory OVSDB: %v", err)
		default:
		}
		return srv.Ready()
	}, 3*time.Second, 10*time.Millisecond)
	t.Cleanup(func() { srv.Close(); require.NoError(t, <-done) })
	raw, err := client.NewOVSDBClient(cm, client.WithEndpoint("unix:"+socket), client.WithLeaderOnly(false), client.WithLogger(&logger))
	require.NoError(t, err)
	require.NoError(t, raw.Connect(t.Context()))
	t.Cleanup(raw.Close)
	_, err = raw.MonitorAll(t.Context())
	require.NoError(t, err)
	return table.NewDatabase(table.Wrap(raw), 3*time.Second, table.RetryPolicy{})
}

func testVpc() *kubeovnv1.Vpc {
	return &kubeovnv1.Vpc{Name: "tenant-a", UID: "vpc-a", Spec: kubeovnv1.VpcSpec{
		OpenPERouter: &kubeovnv1.OpenPERouterIntegration{
			Mode: openperouter.Mode, VRF: "tenant-a", L3VNIName: "tenant-a", L3VNI: 10001,
			TransitCIDRs: []string{"169.254.32.0/20"}, HostSessionCIDRs: []string{"169.254.16.0/20"}, RouterASN: 64514, HostASN: 64515,
		},
	}}
}

func testNodes() []*corev1.Node {
	return []*corev1.Node{
		{Name: "node-a", Annotations: map[string]string{openperouter.NodeIndexAnnotation: "11", openperouter.NodeRouterMACAnnotation: "02:00:00:01:00:0b"}},
		{Name: "node-b", Annotations: map[string]string{openperouter.NodeIndexAnnotation: "12", openperouter.NodeRouterMACAnnotation: "02:00:00:01:00:0c"}},
	}
}

func waitSwitch(t *testing.T, db *table.Database, count int) ovnnb.LogicalSwitch {
	t.Helper()
	ls := ovnnb.LogicalSwitch{Name: "oprt-tenant-a"}
	require.Eventually(t, func() bool {
		return readRow(t.Context(), db, &ls) == nil && len(ls.Ports) == count
	}, 3*time.Second, 10*time.Millisecond)
	return ls
}

func TestTransitOwnershipAndNodeRemoval(t *testing.T) {
	db := testDatabase(t)
	ctx := context.Background()
	lr := &ovnnb.LogicalRouter{Name: "tenant-a"}
	require.NoError(t, db.Table(lr).Create(ctx, "router", lr))
	lr.UUID = ""
	require.Eventually(t, func() bool { return readRow(ctx, db, lr) == nil }, time.Second, 10*time.Millisecond)
	vpc, nodes := testVpc(), testNodes()
	defaults, err := EnsureTransit(ctx, db, vpc, nodes, "10.101.3.0/24")
	require.NoError(t, err)
	require.Equal(t, []openperouter.Route{{Prefix: "0.0.0.0/0", NextHop: "169.254.32.2"}}, defaults)
	// Joining nodes without bootstrap must not prevent existing topology updates.
	joining := &corev1.Node{}
	joining.Name = "joining"
	_, err = EnsureTransit(ctx, db, vpc, append(nodes, joining), "10.101.3.0/24")
	require.NoError(t, err)
	before := waitSwitch(t, db, 4)
	require.Len(t, before.ACLs, 2)
	var ports []ovnnb.LogicalSwitchPort
	require.NoError(t, db.Table(&ovnnb.LogicalSwitchPort{}).List(ctx, &ports))
	var local int
	for _, port := range ports {
		if port.Type == "localport" {
			local++
		}
		require.NotEqual(t, "localnet", port.Type)
	}
	require.Equal(t, 1, local)
	var groups []ovnnb.PortGroup
	require.NoError(t, db.Table(&ovnnb.PortGroup{}).List(ctx, &groups))
	require.Len(t, groups, 1)
	require.Len(t, groups[0].Ports, 2)
	for _, port := range ports {
		if port.Name == "oprb-tenant-a-node-a" || port.Name == "oprb-tenant-a-node-b" {
			require.Contains(t, groups[0].Ports, port.UUID)
		} else {
			require.NotContains(t, groups[0].Ports, port.UUID)
		}
	}
	var acls []ovnnb.ACL
	require.NoError(t, db.Table(&ovnnb.ACL{}).List(ctx, &acls))
	require.Contains(t, []string{acls[0].Match, acls[1].Match}, "inport == @"+groups[0].Name+" && ((ip4 && ip4.dst != {10.101.3.0/24}) || (ip6))")
	recording := &recordingProvider{Provider: db}
	_, err = EnsureTransit(ctx, recording, vpc, nodes, "10.101.3.0/24")
	require.NoError(t, err)
	require.Zero(t, recording.operations, "stable topology must not rewrite OVN rows")
	require.Equal(t, before.UUID, waitSwitch(t, db, 4).UUID)
	// A different Kubernetes UID cannot adopt another VPC's topology.
	foreign := vpc.DeepCopy()
	foreign.UID = "recreated"
	_, err = EnsureTransit(ctx, db, foreign, nodes)
	require.ErrorContains(t, err, "refuse to adopt")
	_, err = EnsureTransit(ctx, db, vpc, nodes[:1], "10.101.3.0/24")
	require.NoError(t, err)
	waitSwitch(t, db, 3)
	require.Eventually(t, func() bool {
		groups = nil
		return db.Table(&ovnnb.PortGroup{}).List(ctx, &groups) == nil && len(groups) == 1 && len(groups[0].Ports) == 1
	}, time.Second, 10*time.Millisecond)
	require.NoError(t, Cleanup(ctx, db, nil, nil, &openperouter.MACGuard{}))
	require.Eventually(t, func() bool {
		return errors.Is(readRow(ctx, db, &ovnnb.LogicalSwitch{Name: "oprt-tenant-a"}), table.ErrNotFound)
	}, time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool {
		groups = nil
		return db.Table(&ovnnb.PortGroup{}).List(ctx, &groups) == nil && len(groups) == 0
	}, time.Second, 10*time.Millisecond)
	require.NoError(t, readRow(ctx, db, lr), "cleanup must preserve the existing VPC router")
}

func TestL2ReflectionGuardAndOwnership(t *testing.T) {
	db := testDatabase(t)
	ctx := t.Context()
	port := &ovnnb.LogicalSwitchPort{UUID: "workload_a", Name: "vm.ns", Addresses: []string{"02:00:00:00:01:11 10.101.1.11"}}
	ls := &ovnnb.LogicalSwitch{Name: "blue", Ports: []string{port.UUID}}
	portOps, err := db.Table(port).CreateOps(port)
	require.NoError(t, err)
	switchOps, err := db.Table(ls).CreateOps(ls)
	require.NoError(t, err)
	require.NoError(t, db.Table(ls).Transact(ctx, "fixture", append(portOps, switchOps...)...))
	require.Eventually(t, func() bool { return readRow(ctx, db, ls) == nil }, time.Second, 10*time.Millisecond)
	vpc := testVpc()
	subnet := &kubeovnv1.Subnet{Name: "blue", UID: "subnet-blue", Spec: kubeovnv1.SubnetSpec{
		Vpc: vpc.Name, CIDRBlock: "10.101.1.0/24", Gateway: "10.101.1.1",
		OpenPERouterL2: &kubeovnv1.OpenPERouterL2Attachment{L2VNIName: "tenant-a-blue", VNI: 10101, GatewayMAC: "02:00:05:00:03:00"},
	}}
	guard := &openperouter.MACGuard{}
	require.NoError(t, EnsureL2(ctx, db, vpc, subnet, guard, "02:00:00:00:01:12"))
	var sets []ovnnb.AddressSet
	require.Eventually(t, func() bool {
		return db.Table(&ovnnb.AddressSet{}).List(ctx, &sets) == nil && len(sets) == 1
	}, time.Second, 10*time.Millisecond)
	require.ElementsMatch(t, []string{"02:00:00:00:01:11", "02:00:00:00:01:12"}, sets[0].Addresses)
	local := &ovnnb.LogicalSwitchPort{Name: openperouter.L2Port(subnet.Name)}
	require.NoError(t, readRow(ctx, db, local))
	require.Equal(t, "localport", local.Type)
	require.Equal(t, []string{"unknown"}, local.Addresses)
	require.NoError(t, readRow(ctx, db, ls))
	require.Len(t, ls.ACLs, 1)
	// Unknown workload source MACs cannot be safely fenced against reflection.
	require.NoError(t, readRow(ctx, db, port))
	desired := *port
	desired.Addresses = []string{"unknown"}
	require.NoError(t, db.Table(port).Update(ctx, "unknown-source", port, &desired, &desired.Addresses))
	require.Eventually(t, func() bool {
		return readRow(ctx, db, port) == nil && len(port.Addresses) == 1 && port.Addresses[0] == "unknown"
	}, time.Second, 10*time.Millisecond)
	require.ErrorContains(t, EnsureL2(ctx, db, vpc, subnet, guard), "unknown sources")
}
