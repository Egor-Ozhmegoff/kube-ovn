package daemon

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
	"github.com/ovn-kubernetes/libovsdb/ovsdb/serverdb"
	"github.com/ovn-kubernetes/libovsdb/server"
	"github.com/stretchr/testify/require"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/ovs"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnsb"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/table"
)

type reflectionSBFixture struct {
	endpoint string
	writer   *table.Database
}

func newReflectionSBFixture(t *testing.T) *reflectionSBFixture {
	t.Helper()
	cm, err := ovnsb.FullDatabaseModel()
	require.NoError(t, err)
	dm, errs := model.NewDatabaseModel(ovnsb.Schema(), cm)
	require.Empty(t, errs)
	serverModel, err := serverdb.FullDatabaseModel()
	require.NoError(t, err)
	serverSchema := serverdb.Schema()
	serverDB, errs := model.NewDatabaseModel(serverSchema, serverModel)
	require.Empty(t, errs)
	logger := logr.Discard()
	memory := inmemory.NewDatabase(map[string]model.ClientDBModel{ovnsb.DatabaseName: cm, serverSchema.Name: serverModel}, &logger)
	srv, err := server.NewOvsdbServer(memory, &logger, dm, serverDB)
	require.NoError(t, err)
	dir, err := os.MkdirTemp("/tmp", "opclass-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	endpoint := "unix:" + filepath.Join(dir, "sb.sock")
	done := make(chan error, 1)
	go func() { done <- srv.Serve("unix", filepath.Join(dir, "sb.sock")) }()
	require.Eventually(t, srv.Ready, 3*time.Second, time.Millisecond)
	t.Cleanup(func() { srv.Close(); require.NoError(t, <-done) })
	raw, err := client.NewOVSDBClient(cm, client.WithEndpoint(endpoint), client.WithLeaderOnly(false), client.WithLogger(&logger))
	require.NoError(t, err)
	require.NoError(t, raw.Connect(t.Context()))
	t.Cleanup(raw.Close)
	_, err = raw.MonitorAll(t.Context())
	require.NoError(t, err)
	db := table.NewDatabase(table.Wrap(raw), time.Second, table.RetryPolicy{})
	encapOps, err := db.Create(&ovnsb.Encap{UUID: "encap", Type: "vxlan", IP: "192.0.2.1"})
	require.NoError(t, err)
	chassisOps, err := db.Create(&ovnsb.Chassis{Name: "node", Encaps: []string{"encap"}})
	require.NoError(t, err)
	_, err = db.TransactResults(t.Context(), append(encapOps, chassisOps...)...)
	require.NoError(t, err)
	return &reflectionSBFixture{endpoint: endpoint, writer: db}
}

func (f *reflectionSBFixture) reader(t *testing.T, names ...string) *ovs.LocalBindingClient {
	t.Helper()
	reader, err := ovs.NewLocalBindingClient(t.Context(), f.endpoint, "node", names...)
	require.NoError(t, err)
	t.Cleanup(reader.Close)
	return reader
}

func (f *reflectionSBFixture) addSet(t *testing.T, name string) {
	t.Helper()
	require.NoError(t, f.writer.CreateAndTransact(t.Context(), "fixture", &ovnsb.AddressSet{Name: name, Addresses: []string{"02:00:00:00:00:01"}}))
}

// Exercise real subscription replacement and real monitored AddressSet reads,
// then the same snapshot/Open checks and error boundary used by runOpenPERouter.
func TestOpenPERouterSubscriptionChurnDoesNotFenceA(t *testing.T) {
	for _, change := range []string{"add", "remove"} {
		t.Run(change, func(t *testing.T) {
			f := newReflectionSBFixture(t)
			f.addSet(t, "a")
			f.addSet(t, "b")
			names, next := []string{"a"}, []string{"a", "b"}
			if change == "remove" {
				names, next = next, names
			}
			reader := f.reader(t, names...)
			original := reader
			expected, current := testOpenPERouterSnapshot(t), testOpenPERouterSnapshot(t)
			b := &kubeovnv1.Subnet{Name: "b", UID: "b", Spec: kubeovnv1.SubnetSpec{Vpc: "b", OpenPERouterL2: &kubeovnv1.OpenPERouterL2Attachment{L2VNIName: "b", VNI: 102, GatewayMAC: "02:00:05:00:03:00"}}}
			if change == "remove" {
				expected.subnets = append(expected.subnets, b)
			}
			expected.generation, expected.guardNames = reader.Generation(), names
			r, _, paths, global := testOpenPERouterRunner(t, "ab")
			opened := false
			r.syncVpc = func(ctx context.Context, vpc *kubeovnv1.Vpc) error {
				if vpc.Name != "a" {
					return nil
				}
				// B changes between the initial A snapshot and A's final Open check.
				if change == "add" {
					current.subnets = append(current.subnets, b)
				}
				if err := ovs.RefreshLocalBindingClient(ctx, &reader, f.endpoint, "node", next...); err != nil {
					return err
				}
				defer reader.Close()
				require.NotSame(t, original, reader)
				require.False(t, original.Connected())
				current.generation, current.guardNames = reader.Generation(), next
				if err := adoptOpenPERouterSnapshot(expected, current, expected.vpcs[0], "node"); err != nil {
					return err
				}
				require.Equal(t, next, expected.guardNames)
				macs, err := openPERouterReflectionMACs(ctx, reader, "a")
				if err != nil {
					return err
				}
				require.Equal(t, []string{"02:00:00:00:00:01"}, macs)
				if err := validateOpenPERouterDataplane(expected, expected.vpcs[0], reader, current.bound, current.installed, "node"); err != nil {
					return err
				}
				opened = true
				return nil
			}
			r.once(t.Context())
			require.True(t, opened)
			require.True(t, paths["a"])
			require.Zero(t, *global)
		})
	}
}

func TestOpenPERouterMissingAddressSetIsScopedAndRecovers(t *testing.T) {
	f := newReflectionSBFixture(t)
	f.addSet(t, "a")
	reader := f.reader(t, "a", "b")
	for _, order := range []string{"ab", "ba"} {
		t.Run(order, func(t *testing.T) {
			r, _, paths, global := testOpenPERouterRunner(t, order)
			guards := map[string]bool{"a": true, "b": true}
			routes := map[string]bool{"a": true, "b": true}
			r.closeVpc = func(ctx context.Context, uid string) error {
				require.NoError(t, ctx.Err())
				paths[uid], guards[uid], routes[uid] = false, false, false
				return nil
			}
			r.syncVpc = func(ctx context.Context, vpc *kubeovnv1.Vpc) error {
				macs, err := openPERouterReflectionMACs(ctx, reader, vpc.Name)
				if err != nil {
					require.ErrorIs(t, err, table.ErrNotFound)
					require.Nil(t, macs)
					return err
				}
				require.NotEmpty(t, macs)
				paths[vpc.Name], guards[vpc.Name], routes[vpc.Name] = true, true, true
				return nil
			}
			r.once(t.Context())
			require.Zero(t, *global)
			require.True(t, paths["a"] && guards["a"] && routes["a"])
			require.False(t, paths["b"] || guards["b"] || routes["b"])
		})
	}
	f.addSet(t, "b")
	require.Eventually(t, func() bool { _, err := reader.ReflectionMACs(t.Context(), "b"); return err == nil }, time.Second, time.Millisecond)
	r, _, paths, global := testOpenPERouterRunner(t, "ba")
	paths["b"] = false
	r.syncVpc = func(ctx context.Context, vpc *kubeovnv1.Vpc) error {
		_, err := openPERouterReflectionMACs(ctx, reader, vpc.Name)
		if err == nil {
			paths[vpc.Name] = true
		}
		return err
	}
	r.once(t.Context())
	require.Zero(t, *global)
	require.True(t, paths["a"] && paths["b"])
}

func TestOpenPERouterActualSBFailuresUseCommonBoundary(t *testing.T) {
	for _, failure := range []string{"disconnect", "missing-chassis", "read-timeout", "vpc-timeout"} {
		t.Run(failure, func(t *testing.T) {
			f := newReflectionSBFixture(t)
			reader := f.reader(t, "b")
			r, clock, _, global := testOpenPERouterRunner(t, "ba")
			probes := 0
			r.refresh = func(ctx context.Context) error { require.NoError(t, ctx.Err()); probes++; return nil }
			if failure == "disconnect" {
				reader.Close()
			}
			if failure == "missing-chassis" {
				require.NoError(t, f.writer.DeleteAndTransact(t.Context(), "fixture", &ovnsb.Chassis{Name: "node"}))
				require.Eventually(t, func() bool { return errors.Is(reader.CheckConnection(t.Context()), table.ErrNotFound) }, time.Second, time.Millisecond)
			}
			r.syncVpc = func(ctx context.Context, vpc *kubeovnv1.Vpc) error {
				if vpc.Name == "a" {
					return nil
				}
				if failure == "vpc-timeout" {
					clock.vpc[len(clock.vpc)-1].expire(context.DeadlineExceeded)
				}
				if failure == "read-timeout" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
					defer cancel()
				}
				_, err := openPERouterReflectionMACs(ctx, reader, "b")
				require.Error(t, err)
				return err
			}
			r.once(t.Context())
			if failure == "disconnect" || failure == "missing-chassis" {
				require.Equal(t, 1, *global)
			} else {
				require.Zero(t, *global)
				require.GreaterOrEqual(t, probes, 3)
			}
		})
	}
}

func TestOpenPERouterRealSourceGenerationBlocksOpen(t *testing.T) {
	for _, phase := range []string{"l3", "l2", "l3-chassis", "l2-chassis"} {
		t.Run(phase, func(t *testing.T) {
			f := newReflectionSBFixture(t)
			old := f.reader(t, "a")
			current := f.reader(t, "a") // Unplanned new connection, unchanged subscriptions.
			if phase == "l3-chassis" || phase == "l2-chassis" {
				chassis := &ovnsb.Chassis{Name: "node"}
				require.NoError(t, f.writer.Table(chassis).Get(t.Context(), chassis))
				require.NoError(t, f.writer.CreateAndTransact(t.Context(), "fixture", &ovnsb.Chassis{Name: "replacement-node", Encaps: chassis.Encaps}))
				require.NoError(t, ovs.RefreshLocalBindingClient(t.Context(), &current, f.endpoint, "replacement-node", "a"))
				defer current.Close()
			}
			expected, fresh := testOpenPERouterSnapshot(t), testOpenPERouterSnapshot(t)
			expected.generation = old.Generation()
			fresh.generation = current.Generation()
			r, _, paths, global := testOpenPERouterRunner(t, "ab")
			opened := false
			r.syncVpc = func(ctx context.Context, _ *kubeovnv1.Vpc) error {
				require.NoError(t, ctx.Err())
				var err error
				if phase == "l3" || phase == "l3-chassis" {
					err = adoptOpenPERouterSnapshot(expected, fresh, expected.vpcs[0], "node")
				} else {
					err = validateOpenPERouterDataplane(expected, expected.vpcs[0], current, fresh.bound, fresh.installed, "node")
				}
				if err == nil {
					opened = true
				}
				return err
			}
			r.once(t.Context())
			require.False(t, opened)
			require.Equal(t, 1, *global)
			require.False(t, paths["a"])
		})
	}
}
