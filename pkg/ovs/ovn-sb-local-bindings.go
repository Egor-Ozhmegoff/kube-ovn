package ovs

import (
	"context"
	"errors"

	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnsb"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/table"
)

// LocalBindingClient watches only the current node's primary bindings. The
// prepared target of a VM migration must not independently originate a prefix.
type LocalBindingClient struct {
	*table.Database
	name, chassisUUID string
	endpoint          string
}

func NewLocalBindingClient(ctx context.Context, endpoint, chassisName string) (*LocalBindingClient, error) {
	if endpoint == "" || chassisName == "" {
		return nil, errors.New("OVN remote and chassis system-id are required")
	}
	dbModel, err := ovnsb.FullDatabaseModel()
	if err != nil {
		return nil, err
	}
	chassis := &ovnsb.Chassis{Name: chassisName}
	backend, err := connectOvsdb(ovnsb.DatabaseName, endpoint, dbModel, []table.MonitorOption{
		table.WithConditionalTable(chassis, model.Condition{Field: &chassis.Name, Function: ovsdb.ConditionEqual, Value: chassisName}),
	}, 3, 10, 0, "OpenPERouter local binding reader")
	if err != nil {
		return nil, err
	}
	db := newObservedDatabase(backend, 3, "openperouter-bindings")
	if err := db.Table(chassis).Get(ctx, chassis); err != nil {
		db.Close()
		return nil, err
	}
	port := &ovnsb.PortBinding{}
	monitor := db.NewMonitor(table.WithConditionalTable(port, model.Condition{
		Field: &port.Chassis, Function: ovsdb.ConditionEqual, Value: &chassis.UUID,
	}))
	if _, err := db.Monitor(ctx, monitor); err != nil {
		db.Close()
		return nil, err
	}
	return &LocalBindingClient{Database: db, name: chassisName, chassisUUID: chassis.UUID, endpoint: endpoint}, nil
}

func (c *LocalBindingClient) Matches(endpoint, chassis string) bool {
	return c.endpoint == endpoint && c.name == chassis
}

func (c *LocalBindingClient) BoundPorts(ctx context.Context) (map[string]ovnsb.PortBinding, error) {
	if !c.Connected() {
		return nil, errors.New("OVN local binding monitor is disconnected")
	}
	if err := c.Echo(ctx); err != nil {
		return nil, err
	}
	chassis := &ovnsb.Chassis{Name: c.name}
	if err := c.Table(chassis).Get(ctx, chassis); err != nil {
		return nil, err
	}
	if chassis.UUID != c.chassisUUID {
		return nil, errors.New("OVN chassis was recreated; reopen the conditional monitor")
	}
	var bindings []ovnsb.PortBinding
	if err := c.Table(&ovnsb.PortBinding{}).List(ctx, &bindings); err != nil {
		return nil, err
	}
	bound := make(map[string]ovnsb.PortBinding, len(bindings))
	for _, binding := range bindings {
		if binding.Chassis != nil && *binding.Chassis == c.chassisUUID && binding.Up != nil && *binding.Up {
			bound[binding.LogicalPort] = binding
		}
	}
	return bound, nil
}
