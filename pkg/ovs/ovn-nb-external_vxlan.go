package ovs

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	ovsclient "github.com/kubeovn/kube-ovn/pkg/ovsdb/client"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnnb"
	"github.com/kubeovn/kube-ovn/pkg/util"
)

var externalVXLANOptions = []string{
	"dynamic-routing-vni", "dynamic-routing-bridge-ifname", "dynamic-routing-vxlan-ifname",
	"dynamic-routing-advertise-ifname", "dynamic-routing-fdb-prefer-local",
	"dynamic-routing-redistribute",
}

// ReconcileExternalVXLANSwitch manages only this feature's LS options. The
// cluster tunnel key and unrelated switch configuration are never changed.
func (c *OVNNbClient) ReconcileExternalVXLANSwitch(lsName string, config *kubeovnv1.ExternalVXLAN) error {
	return c.reconcileExternalVXLANSwitch(lsName, config, false)
}

func (c *OVNNbClient) reconcileExternalVXLANSwitch(lsName string, config *kubeovnv1.ExternalVXLAN, retainOwner bool) error {
	ls, err := c.GetLogicalSwitch(lsName, config == nil)
	if err != nil || ls == nil {
		return err
	}
	owned := ls.ExternalIDs[util.ExternalVXLANOwner] == lsName
	if config == nil && !owned {
		return nil
	}
	if config != nil && !owned && ls.OtherConfig["dynamic-routing-vni"] != "" {
		return fmt.Errorf("logical switch %s already has externally managed EVPN configuration", lsName)
	}
	otherConfig, externalIDs := maps.Clone(ls.OtherConfig), maps.Clone(ls.ExternalIDs)
	if otherConfig == nil {
		otherConfig = map[string]string{}
	}
	if externalIDs == nil {
		externalIDs = map[string]string{}
	}
	for _, key := range externalVXLANOptions {
		delete(otherConfig, key)
	}
	if !retainOwner {
		delete(externalIDs, util.ExternalVXLANOwner)
	}
	if config != nil {
		if config.FRR == nil {
			return fmt.Errorf("logical switch %s requires a host FRR interface contract", lsName)
		}
		bridge, vxlan, advertise := config.FRR.Bridge, config.FRR.VXLAN, config.FRR.Advertise
		otherConfig["dynamic-routing-redistribute"] = "fdb,ip"
		otherConfig["dynamic-routing-vni"] = strconv.FormatUint(uint64(config.VNI), 10)
		otherConfig["dynamic-routing-bridge-ifname"] = bridge
		otherConfig["dynamic-routing-vxlan-ifname"] = vxlan
		otherConfig["dynamic-routing-advertise-ifname"] = advertise
		otherConfig["dynamic-routing-fdb-prefer-local"] = "true"
		externalIDs[util.ExternalVXLANOwner] = lsName
	}
	if maps.Equal(ls.OtherConfig, otherConfig) && maps.Equal(ls.ExternalIDs, externalIDs) {
		return nil
	}
	ls.OtherConfig, ls.ExternalIDs = otherConfig, externalIDs
	ops, err := c.Where(ls).Update(ls, &ls.OtherConfig, &ls.ExternalIDs)
	if err != nil {
		return err
	}
	return c.Transact("external-vxlan-switch", ops)
}

// DeactivateExternalVXLANRouter stops native exchange without destroying
// ingress identities needed to drain VM type-2 host routes. Keep the transit
// switch ownership marker so normal reconciliation can safely resume.
func (c *OVNNbClient) DeactivateExternalVXLANRouter(router string) error {
	if err := c.reconcileExternalVXLANSwitch(util.ExternalVXLANTransitSwitch(router), nil, true); err != nil {
		return err
	}
	return c.reconcileExternalVXLANRouting(router, nil)
}

// ReconcileExternalVXLANRouter supports the legacy single-gateway identity with host FRR.
func (c *OVNNbClient) ReconcileExternalVXLANRouter(lrName, routerMAC, chassis string, config *kubeovnv1.ExternalVXLANGateway) error {
	var gateways []ExternalVXLANRouterGateway
	if config != nil {
		gateways = []ExternalVXLANRouterGateway{{Host: config.GatewayNode, MAC: routerMAC, Chassis: chassis, TransitCIDR: config.TransitCIDR, RoutingPort: config.RoutingPort, Active: true}}
	}
	return c.ReconcileExternalVXLANRouterGateways(lrName, gateways, config)
}

// ExternalVXLANRouterGateway is a chassis-specific DGP on one shared EVPN LS.
// An inactive member retains ingress identity but contributes no egress route.
type ExternalVXLANRouterGateway struct {
	Node, Host, MAC, Chassis, TransitCIDR string
	RoutingPort                           string
	Active                                bool
}

func (c *OVNNbClient) ReconcileExternalVXLANRouterGateways(lrName string, gateways []ExternalVXLANRouterGateway, config *kubeovnv1.ExternalVXLANGateway) error {
	lsName := util.ExternalVXLANTransitSwitch(lrName)
	ls, err := c.GetLogicalSwitch(lsName, true)
	if err != nil {
		return err
	}
	if ls != nil && ls.ExternalIDs[util.ExternalVXLANOwner] != lsName {
		return fmt.Errorf("external VXLAN transit switch %s is not owned by Kube-OVN external VXLAN", lsName)
	}
	if config == nil {
		if err := c.reconcileExternalVXLANRouting(lrName, nil); err != nil {
			return err
		}
		if err := c.cleanupExternalVXLANGateways(lrName, nil); err != nil {
			return err
		}
		if ls == nil {
			return nil
		}
		return c.DeleteLogicalSwitch(lsName)
	}
	if ls == nil {
		row := &ovnnb.LogicalSwitch{
			UUID: ovsclient.NamedUUID(), Name: lsName,
			ExternalIDs: map[string]string{"vendor": util.CniTypeName, util.ExternalVXLANOwner: lsName},
		}
		creation, err := c.Create(row)
		if err != nil {
			return err
		}
		ops := append([]ovsdb.Operation{ConstructWaitForNameNotExistsOperation(lsName, "Logical_Switch")}, creation...)
		created, err := c.TransactConditional("external-vxlan-transit-create", ops)
		if err != nil {
			return err
		}
		if !created {
			return fmt.Errorf("transit switch %s was created concurrently; retry reconciliation", lsName)
		}
	}
	if err := c.ReconcileExternalVXLANSwitch(lsName, &config.ExternalVXLAN); err != nil {
		return err
	}
	if err := c.reconcileExternalVXLANRouting(lrName, config); err != nil {
		return err
	}
	desired := map[string]bool{}
	for _, gateway := range gateways {
		port := externalVXLANRouterPort(lrName, gateway.Node)
		desired[port] = true
		if err := c.reconcileExternalVXLANPort(lrName, gateway); err != nil {
			return err
		}
		if err := c.cleanupExternalVXLANRoutes(lrName, port); err != nil {
			return err
		}
	}
	return c.cleanupExternalVXLANGateways(lrName, desired)
}

func externalVXLANRouterPort(router, node string) string {
	port := LogicalRouterPortName(router, util.ExternalVXLANTransitSwitch(router))
	if node != "" {
		port += "-" + node
	}
	return port
}

func externalVXLANPeerPort(router, port string) string {
	return LogicalSwitchPortName(router, util.ExternalVXLANTransitSwitch(router)) + strings.TrimPrefix(port, externalVXLANRouterPort(router, ""))
}

func (c *OVNNbClient) reconcileExternalVXLANPort(router string, gateway ExternalVXLANRouterGateway) error {
	lsName, port := util.ExternalVXLANTransitSwitch(router), externalVXLANRouterPort(router, gateway.Node)
	lr, err := c.GetLogicalRouter(router, false)
	if err != nil {
		return err
	}
	ls, err := c.GetLogicalSwitch(lsName, false)
	if err != nil {
		return err
	}
	lrp, err := c.GetLogicalRouterPort(port, true)
	if err != nil {
		return err
	}
	lsp, err := c.GetLogicalSwitchPort(externalVXLANPeerPort(router, port), true)
	if err != nil {
		return err
	}
	var ops []ovsdb.Operation
	create := lrp == nil
	if lrp == nil {
		lrp = &ovnnb.LogicalRouterPort{UUID: ovsclient.NamedUUID(), Name: port}
	} else if !slices.Contains(lr.Ports, lrp.UUID) || (lrp.ExternalIDs[util.ExternalVXLANOwner] != lsName && gateway.Node != "") {
		return fmt.Errorf("external VXLAN router port %s has a different owner", port)
	}
	lrp.MAC, lrp.Networks = strings.ToLower(gateway.MAC), []string{gateway.TransitCIDR}
	lrp.Options = maps.Clone(lrp.Options)
	if lrp.Options == nil {
		lrp.Options = map[string]string{}
	}
	delete(lrp.Options, "dynamic-routing-port-name")
	delete(lrp.Options, "dynamic-routing-maintain-vrf")
	delete(lrp.Options, "dynamic-routing-no-learning")
	if gateway.RoutingPort != "" {
		lrp.Options["dynamic-routing-port-name"] = gateway.RoutingPort
		lrp.Options["dynamic-routing-maintain-vrf"] = "false"
	}
	if !gateway.Active {
		lrp.Options["dynamic-routing-no-learning"] = "true"
	}
	lrp.ExternalIDs = maps.Clone(lrp.ExternalIDs)
	if lrp.ExternalIDs == nil {
		lrp.ExternalIDs = map[string]string{}
	}
	lrp.ExternalIDs["vendor"], lrp.ExternalIDs[util.ExternalVXLANOwner] = util.CniTypeName, lsName
	host := gateway.Host
	if host == "" {
		host = gateway.Node
	}
	if host != "" {
		lrp.ExternalIDs[util.ExternalVXLANNode] = host
	}
	if create {
		ops, err = c.CreateLogicalRouterPortOp(lrp, router)
	} else {
		ops, err = c.Where(lrp).Update(lrp, &lrp.MAC, &lrp.Networks, &lrp.ExternalIDs, &lrp.Options)
	}
	if err != nil {
		return err
	}
	if lsp == nil {
		lsp = &ovnnb.LogicalSwitchPort{UUID: ovsclient.NamedUUID(), Name: externalVXLANPeerPort(router, port), Type: "router", Addresses: []string{"router"}, Options: map[string]string{"router-port": port}, ExternalIDs: map[string]string{"vendor": util.CniTypeName, util.ExternalVXLANOwner: lsName}}
		creation, err := c.CreateLogicalSwitchPortOp(lsp, lsName)
		if err != nil {
			return err
		}
		ops = append(ops, creation...)
	} else if !slices.Contains(ls.Ports, lsp.UUID) || lsp.Type != "router" || lsp.Options["router-port"] != port {
		return fmt.Errorf("external VXLAN switch port %s has a different owner or type", lsp.Name)
	}
	if err := c.Transact("external-vxlan-port", ops); err != nil {
		return err
	}
	chassises := []string{gateway.Chassis}
	if !gateway.Active {
		// A regular distributed LRP accepts the VM host's type-2 RMAC but
		// performs no chassis-bound IP Route Exchange. Deleting the LRP
		// would blackhole host routes still advertised by that node's FRR.
		chassises = nil
	}
	return c.ReconcileGatewayChassises(port, chassises)
}

func (c *OVNNbClient) reconcileExternalVXLANRouting(router string, config *kubeovnv1.ExternalVXLANGateway) error {
	lr, err := c.GetLogicalRouter(router, true)
	if err != nil || lr == nil {
		return err
	}
	const ownerKey = "kube-ovn-external-vxlan-routing"
	desired := util.ExternalVXLANRouterOptions(config)
	if lr.ExternalIDs[ownerKey] == "" && len(desired) == 0 {
		return nil
	}
	options, ids := maps.Clone(lr.Options), maps.Clone(lr.ExternalIDs)
	if options == nil {
		options = map[string]string{}
	}
	if ids == nil {
		ids = map[string]string{}
	}
	// Delete only the keys owned by this integration, not other route options.
	for _, key := range []string{"dynamic-routing", "dynamic-routing-vrf-name", "dynamic-routing-vrf-id", "dynamic-routing-redistribute"} {
		delete(options, key)
	}
	delete(ids, ownerKey)
	if len(desired) != 0 {
		maps.Copy(options, desired)
		ids[ownerKey] = router
	}
	if maps.Equal(options, lr.Options) && maps.Equal(ids, lr.ExternalIDs) {
		return nil
	}
	lr.Options, lr.ExternalIDs = options, ids
	ops, err := c.Where(lr).Update(lr, &lr.Options, &lr.ExternalIDs)
	if err != nil {
		return err
	}
	return c.Transact("external-vxlan-routing", ops)
}

// Cleanup removes only the former attachment's owned NB state. It never
// synthesizes routes or MAC bindings; remote state belongs to OVN/FRR.
func (c *OVNNbClient) cleanupExternalVXLANGateways(router string, desired map[string]bool) error {
	base := externalVXLANRouterPort(router, "")
	owned := func(port string) bool { return port == base || strings.HasPrefix(port, base+"-") }
	ports, err := c.ListLogicalRouterPorts(nil, func(row *ovnnb.LogicalRouterPort) bool { return owned(row.Name) })
	if err != nil {
		return err
	}
	stale := map[string]bool{}
	for _, port := range ports {
		if owner := port.ExternalIDs[util.ExternalVXLANOwner]; owner != util.ExternalVXLANTransitSwitch(router) && (port.Name != base || owner != "") {
			return fmt.Errorf("refuse to clean foreign external VXLAN port %s", port.Name)
		}
		stale[port.Name] = true
	}
	bindings, err := filterTimeout(c.Database, &ovnnb.StaticMACBinding{}, func(row *ovnnb.StaticMACBinding) bool { return owned(row.LogicalPort) })
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		stale[binding.LogicalPort] = true
	}
	lr, err := c.GetLogicalRouter(router, true)
	if err != nil {
		return err
	}
	if lr != nil {
		routes, err := c.ListLogicalRouterStaticRoutes(router, nil, nil, "", nil)
		if err != nil {
			return err
		}
		for _, route := range routes {
			if port := route.ExternalIDs[util.ExternalVXLANOwner]; owned(port) {
				stale[port] = true
			}
		}
	}
	for port := range stale {
		if desired[port] {
			continue
		}
		if err := c.cleanupExternalVXLANRoutes(router, port); err != nil {
			return err
		}
		if err := c.RemoveLogicalPatchPort(externalVXLANPeerPort(router, port), port); err != nil {
			return err
		}
	}
	return nil
}

func (c *OVNNbClient) cleanupExternalVXLANRoutes(router, port string) error {
	lr, err := c.GetLogicalRouter(router, true)
	if err != nil {
		return err
	}
	var existing []*ovnnb.LogicalRouterStaticRoute
	if lr != nil {
		existing, err = c.ListLogicalRouterStaticRoutes(router, nil, nil, "", map[string]string{util.ExternalVXLANOwner: port})
		if err != nil {
			return err
		}
	}
	bindings, err := filterTimeout(c.Database, &ovnnb.StaticMACBinding{}, func(row *ovnnb.StaticMACBinding) bool { return row.LogicalPort == port })
	if err != nil {
		return err
	}
	if len(existing) == 0 && len(bindings) == 0 {
		return nil
	}
	var ops []ovsdb.Operation
	var oldIDs []string
	for _, row := range existing {
		oldIDs = append(oldIDs, row.UUID)
		deletion, err := c.Where(row).Delete()
		if err != nil {
			return err
		}
		ops = append(ops, deletion...)
	}
	for _, binding := range bindings {
		deletion, err := c.Where(&binding).Delete()
		if err != nil {
			return err
		}
		ops = append(ops, deletion...)
	}
	if lr != nil && len(oldIDs) != 0 {
		detach, err := c.LogicalRouterUpdateStaticRouteOp(router, oldIDs, ovsdb.MutateOperationDelete)
		if err != nil {
			return err
		}
		ops = append(ops, detach...)
	}
	return c.Transact("external-vxlan-legacy-routes-cleanup", ops)
}
