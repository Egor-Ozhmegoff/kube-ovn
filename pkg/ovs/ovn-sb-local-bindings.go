package ovs

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	ovsclient "github.com/kubeovn/kube-ovn/pkg/ovsdb/client"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnsb"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/table"
)

// LocalBindingClient watches primary and additional bindings on this node.
// A prepared migration target can announce only after RARP activation.
type LocalBindingClient struct {
	*table.Database
	name, chassisUUID string
	endpoint          string
	addressSets       []string
}

func NewLocalBindingClient(ctx context.Context, endpoint, chassisName string, addressSets ...string) (*LocalBindingClient, error) {
	if endpoint == "" || chassisName == "" {
		return nil, errors.New("OVN remote and chassis system-id are required")
	}
	dbModel, err := ovnsb.FullDatabaseModel()
	if err != nil {
		return nil, err
	}
	chassis := &ovnsb.Chassis{Name: chassisName}
	backend, err := ovsclient.NewOvsDbClientContext(ctx, ovnsb.DatabaseName, endpoint, dbModel, []table.MonitorOption{
		table.WithConditionalTable(chassis, model.Condition{Field: &chassis.Name, Function: ovsdb.ConditionEqual, Value: chassisName}),
	}, 3, 10)
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
	}, model.Condition{Field: &port.AdditionalChassis, Function: ovsdb.ConditionIncludes, Value: []string{chassis.UUID}}))
	if _, err := db.Monitor(ctx, monitor); err != nil {
		db.Close()
		return nil, err
	}
	for _, name := range addressSets {
		set := &ovnsb.AddressSet{}
		monitor := db.NewMonitor(table.WithConditionalTable(set, model.Condition{Field: &set.Name, Function: ovsdb.ConditionEqual, Value: name}))
		if _, err := db.Monitor(ctx, monitor); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &LocalBindingClient{Database: db, name: chassisName, chassisUUID: chassis.UUID, endpoint: endpoint, addressSets: slices.Clone(addressSets)}, nil
}

func (c *LocalBindingClient) Matches(endpoint, chassis string, addressSets ...string) bool {
	return c.endpoint == endpoint && c.name == chassis && slices.Equal(c.addressSets, addressSets)
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
		if binding.Up != nil && *binding.Up && activeBindingOwner(binding, c.chassisUUID, c.name) {
			bound[binding.LogicalPort] = binding
		}
	}
	return bound, nil
}

// ReflectionMACs refuses a missing guard instead of opening an unfiltered leg.
func (c *LocalBindingClient) ReflectionMACs(ctx context.Context, name string) ([]string, error) {
	set := &ovnsb.AddressSet{Name: name}
	if err := c.Table(set).Get(ctx, set); err != nil {
		return nil, err
	}
	return slices.Clone(set.Addresses), nil
}

// RARP activation is recorded while the target is still an additional chassis.
// The primary column alone therefore does not describe the active VM location.
func activeBindingOwner(binding ovnsb.PortBinding, chassisUUID, chassisName string) bool {
	if binding.Options["activation-strategy"] == "rarp" {
		activated := strings.Split(binding.Options["additional-chassis-activated"], ",")
		if last := activated[len(activated)-1]; last != "" && len(binding.AdditionalChassis) != 0 {
			return last == chassisName && slices.Contains(binding.AdditionalChassis, chassisUUID)
		}
	}
	return binding.Chassis != nil && *binding.Chassis == chassisUUID
}
