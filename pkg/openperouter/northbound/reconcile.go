package northbound

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"net"
	"reflect"
	"slices"
	"strconv"
	"strings"

	uuidpkg "github.com/google/uuid"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
	corev1 "k8s.io/api/core/v1"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/openperouter"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnnb"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/table"
)

// EnsureTransit creates only a switch, a distributed router connection and VIFs.
// No gateway chassis or external fabric route is installed in OVN.
func EnsureTransit(ctx context.Context, provider table.Provider, vpc *kubeovnv1.Vpc, nodes []*corev1.Node, returnCIDRs ...string) ([]openperouter.Route, error) {
	t, err := openperouter.BuildTransit(vpc.Name, vpc.Spec.OpenPERouter)
	if err != nil {
		return nil, err
	}
	if provider == nil {
		return nil, errors.New("OpenPERouter requires northbound tables")
	}
	ids := map[string]string{openperouter.OwnerKey: vpc.Name, openperouter.OwnerKey + "-uid": string(vpc.UID)}
	tx := nbTransaction{ctx: ctx, provider: provider, ids: ids}
	hash := sha256.Sum256([]byte(vpc.Name + "/" + string(vpc.UID)))
	egressFence, err := tx.fence(fmt.Sprintf("opre_%x", hash[:12]), fmt.Sprintf("inport == %q", t.EgressPort))
	if err != nil {
		return nil, err
	}
	returnGroup := fmt.Sprintf("oprrpg_%x", hash[:12])
	returnMatch, err := returnFenceMatch(returnGroup, returnCIDRs)
	if err != nil {
		return nil, err
	}
	returnFence, err := tx.fence(fmt.Sprintf("oprb_%x", hash[:12]), returnMatch)
	if err != nil {
		return nil, err
	}
	portIDs := make([]string, 0, len(nodes)+2)
	lrp := &ovnnb.LogicalRouterPort{Name: t.RouterPort, MAC: t.RouterMAC, Networks: t.RouterNetworks}
	if err := tx.row(lrp); err != nil {
		return nil, err
	}
	peer := &ovnnb.LogicalSwitchPort{Name: t.SwitchPort, Type: "router", Addresses: []string{"router"}, Options: map[string]string{"router-port": t.RouterPort}}
	if err := tx.row(peer); err != nil {
		return nil, err
	}
	portIDs = append(portIDs, peer.UUID)
	local := &ovnnb.LogicalSwitchPort{Name: t.EgressPort, Type: "localport", Addresses: []string{addresses(t.EgressMAC, t.EgressAddresses)}}
	if err := tx.row(local); err != nil {
		return nil, err
	}
	portIDs = append(portIDs, local.UUID)
	var returnPortIDs []string
	indices := make(map[int]string)
	routerMACs := make(map[string]string)
	for _, node := range nodes {
		// Bootstrap annotations are installed together before this node joins.
		if node.Annotations[openperouter.NodeIndexAnnotation] == "" || node.Annotations[openperouter.NodeRouterMACAnnotation] == "" {
			continue
		}
		index, err := strconv.Atoi(node.Annotations[openperouter.NodeIndexAnnotation])
		if err != nil {
			return nil, fmt.Errorf("node %s requires %s: %w", node.Name, openperouter.NodeIndexAnnotation, err)
		}
		if other, exists := indices[index]; exists {
			return nil, fmt.Errorf("nodes %s and %s share OpenPERouter index %d", other, node.Name, index)
		}
		indices[index] = node.Name
		mac, err := net.ParseMAC(node.Annotations[openperouter.NodeRouterMACAnnotation])
		if err != nil || len(mac) != 6 || mac[0]&1 != 0 || mac.String() == "00:00:00:00:00:00" {
			return nil, fmt.Errorf("node %s requires a nonzero unicast %s", node.Name, openperouter.NodeRouterMACAnnotation)
		}
		if other, exists := routerMACs[mac.String()]; exists {
			return nil, fmt.Errorf("nodes %s and %s share router MAC %s", other, node.Name, mac)
		}
		routerMACs[mac.String()] = node.Name
		n, err := openperouter.BuildNode(vpc.Name, node.Name, index, vpc.Spec.OpenPERouter)
		if err != nil {
			return nil, err
		}
		port := &ovnnb.LogicalSwitchPort{
			Name: n.ReturnPort, Addresses: []string{addresses(n.ReturnMAC, n.ReturnAddresses)},
			Options: map[string]string{"requested-chassis": node.Name},
		}
		if err := tx.row(port); err != nil {
			return nil, err
		}
		portIDs = append(portIDs, port.UUID)
		returnPortIDs = append(returnPortIDs, port.UUID)
	}
	// Positive membership avoids relying on a negated router-port lookup.
	// One group serves all return VIFs in this VRF and is updated atomically.
	if err := tx.row(&ovnnb.PortGroup{Name: returnGroup, Ports: returnPortIDs}); err != nil {
		return nil, err
	}
	// Return VIFs removed from this owned switch lose their strong reference.
	// The OVN schema garbage-collects the orphan rows in the same transaction.
	slices.Sort(portIDs)
	if err := tx.row(&ovnnb.LogicalSwitch{Name: t.Switch, Ports: portIDs, ACLs: []string{egressFence, returnFence}}); err != nil {
		return nil, err
	}
	lr := &ovnnb.LogicalRouter{Name: vpc.Name}
	if err := readRow(ctx, provider, lr); err != nil {
		return nil, err
	}
	if !slices.Contains(lr.Ports, lrp.UUID) {
		ops, err := provider.Table(lr).MutateOps(lr, model.Mutation{Field: &lr.Ports, Mutator: ovsdb.MutateOperationInsert, Value: []string{lrp.UUID}})
		if err != nil {
			return nil, err
		}
		tx.ops = append(tx.ops, ops...)
	}
	if err := provider.Table(lr).Transact(ctx, "openperouter-transit", tx.ops...); err != nil {
		return nil, err
	}
	return t.Defaults, nil
}

type nbTransaction struct {
	ctx      context.Context
	provider table.Provider
	ids      map[string]string
	ops      []ovsdb.Operation
}

// row prepares an owned-row upsert without submitting it. All ownership checks
// complete before the first database mutation, including on partial retries.
func (tx *nbTransaction) row(desired model.Model) error {
	var current model.Model
	switch d := desired.(type) {
	case *ovnnb.LogicalSwitch:
		current = &ovnnb.LogicalSwitch{Name: d.Name}
	case *ovnnb.LogicalSwitchPort:
		current = &ovnnb.LogicalSwitchPort{Name: d.Name}
	case *ovnnb.LogicalRouterPort:
		current = &ovnnb.LogicalRouterPort{Name: d.Name}
	case *ovnnb.PortGroup:
		current = &ovnnb.PortGroup{Name: d.Name}
	default:
		return fmt.Errorf("unsupported OpenPERouter row %T", desired)
	}
	t := tx.provider.Table(desired)
	err := readRow(tx.ctx, tx.provider, current)
	if err != nil && !errors.Is(err, table.ErrNotFound) {
		return err
	}
	if err == nil {
		_, ids := rowIdentity(current)
		if ids[openperouter.OwnerKey] != tx.ids[openperouter.OwnerKey] || ids[openperouter.OwnerKey+"-uid"] != tx.ids[openperouter.OwnerKey+"-uid"] {
			return fmt.Errorf("refuse to adopt an unowned OpenPERouter row %T", current)
		}
	}
	uuid, _ := rowIdentity(desired)
	if errors.Is(err, table.ErrNotFound) {
		*uuid = "op_" + strings.ReplaceAll(uuidpkg.NewString(), "-", "")
		setRowIDs(desired, maps.Clone(tx.ids))
		ops, err := t.CreateOps(desired)
		tx.ops = append(tx.ops, ops...)
		return err
	}
	currentUUID, ids := rowIdentity(current)
	*uuid = *currentUUID
	merged := maps.Clone(ids)
	maps.Copy(merged, tx.ids)
	setRowIDs(desired, merged)
	fields := mergeObserved(current, desired)
	if reflect.DeepEqual(current, desired) {
		return nil
	}
	ops, err := t.UpdateOps(current, desired, fields...)
	tx.ops = append(tx.ops, ops...)
	return err
}

func rowIdentity(row model.Model) (*string, map[string]string) {
	switch r := row.(type) {
	case *ovnnb.LogicalSwitch:
		return &r.UUID, r.ExternalIDs
	case *ovnnb.LogicalSwitchPort:
		return &r.UUID, r.ExternalIDs
	case *ovnnb.LogicalRouterPort:
		return &r.UUID, r.ExternalIDs
	case *ovnnb.PortGroup:
		return &r.UUID, r.ExternalIDs
	default:
		panic("unsupported OpenPERouter row")
	}
}

func setRowIDs(row model.Model, ids map[string]string) {
	switch r := row.(type) {
	case *ovnnb.LogicalSwitch:
		r.ExternalIDs = ids
	case *ovnnb.LogicalSwitchPort:
		r.ExternalIDs = ids
	case *ovnnb.LogicalRouterPort:
		r.ExternalIDs = ids
	case *ovnnb.PortGroup:
		r.ExternalIDs = ids
	}
}

// Preserve status and columns outside this reconciler's ownership. This also
// avoids writes on a steady-state reconciliation.
func mergeObserved(current, desired model.Model) []any {
	switch d := desired.(type) {
	case *ovnnb.PortGroup:
		c := current.(*ovnnb.PortGroup)
		ports, ids := d.Ports, d.ExternalIDs
		if sameSet(ports, c.Ports) {
			ports = c.Ports
		}
		*d = *c
		d.Ports, d.ExternalIDs = ports, ids
		return []any{&d.Ports, &d.ExternalIDs}
	case *ovnnb.LogicalSwitch:
		c := current.(*ovnnb.LogicalSwitch)
		ports, ids, acls := d.Ports, d.ExternalIDs, d.ACLs
		if sameSet(ports, c.Ports) {
			ports = c.Ports
		}
		if sameSet(acls, c.ACLs) {
			acls = c.ACLs
		}
		*d = *c
		d.Ports, d.ExternalIDs, d.ACLs = ports, ids, acls
		return []any{&d.Ports, &d.ExternalIDs, &d.ACLs}
	case *ovnnb.LogicalSwitchPort:
		c := current.(*ovnnb.LogicalSwitchPort)
		typ, addr, options, ids := d.Type, d.Addresses, d.Options, d.ExternalIDs
		if sameSet(addr, c.Addresses) {
			addr = c.Addresses
		}
		if maps.Equal(options, c.Options) {
			options = c.Options
		}
		*d = *c
		d.Type, d.Addresses, d.Options, d.ExternalIDs = typ, addr, options, ids
		return []any{&d.Type, &d.Addresses, &d.Options, &d.ExternalIDs}
	case *ovnnb.LogicalRouterPort:
		c := current.(*ovnnb.LogicalRouterPort)
		mac, networks, ids := d.MAC, d.Networks, d.ExternalIDs
		if sameSet(networks, c.Networks) {
			networks = c.Networks
		}
		*d = *c
		d.MAC, d.Networks, d.ExternalIDs = mac, networks, ids
		return []any{&d.MAC, &d.Networks, &d.ExternalIDs}
	}
	return nil
}

func sameSet(a, b []string) bool {
	left, right := slices.Clone(a), slices.Clone(b)
	slices.Sort(left)
	slices.Sort(right)
	return slices.Equal(left, right)
}

func addresses(mac string, prefixes []string) string {
	values := []string{mac}
	for _, prefix := range prefixes {
		values = append(values, strings.SplitN(prefix, "/", 2)[0])
	}
	return strings.Join(values, " ")
}
