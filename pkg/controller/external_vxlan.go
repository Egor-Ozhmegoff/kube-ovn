package controller

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/ovs"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnnb"
	"github.com/kubeovn/kube-ovn/pkg/util"
)

func (c *Controller) validateExternalVXLANVNI(vni uint32, subnetName, vpcName string) error {
	subnets, err := c.subnetsLister.List(labels.Everything())
	if err != nil {
		return err
	}
	for _, subnet := range subnets {
		if subnet.Name != subnetName && subnet.Spec.ExternalVXLAN != nil && subnet.Spec.ExternalVXLAN.VNI == vni {
			return fmt.Errorf("external VXLAN VNI %d is already used by Subnet %s", vni, subnet.Name)
		}
	}
	vpcs, err := c.vpcsLister.List(labels.Everything())
	if err != nil {
		return err
	}
	for _, vpc := range vpcs {
		if vpc.Name != vpcName && vpc.Spec.ExternalVXLAN != nil && vpc.Spec.ExternalVXLAN.VNI == vni {
			return fmt.Errorf("external VXLAN VNI %d is already used by Vpc %s", vni, vpc.Name)
		}
	}
	return nil
}

func (c *Controller) reconcileExternalVXLANVpc(vpc *kubeovnv1.Vpc) error {
	config := vpc.Spec.ExternalVXLAN
	if config == nil && !vpc.Status.ExternalVXLAN {
		return nil
	}
	if config == nil {
		vpc.Status.ExternalVXLANReadyNodes = nil
		return c.OVNNbClient.ReconcileExternalVXLANRouter(vpc.Name, "", "", nil)
	}
	if err := util.ValidateVpc(vpc); err != nil {
		return err
	}
	if err := c.validateExternalVXLANVNI(config.VNI, "", vpc.Name); err != nil {
		return err
	}
	if err := c.validateExternalVXLANFRRIsolation(vpc); err != nil {
		return err
	}
	if err := c.ensureExternalVXLANHostContracts(vpc); err != nil {
		return err
	}
	var gateways []ovs.ExternalVXLANRouterGateway
	var ready []string
	for _, spec := range util.ExternalVXLANGatewayNodes(config) {
		node, err := c.nodesLister.Get(spec.Node)
		if k8serrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		gateway, err := c.externalVXLANNodeGateway(vpc, spec, node)
		if err != nil {
			// An unconfigured/rejoining candidate must not freeze healthy members.
			klog.ErrorS(err, "External VXLAN gateway identity unavailable", "node", node.Name)
			continue
		}
		chassis, err := c.OVNSbClient.GetChassisByHost(node.Name)
		if err != nil {
			// A deleted/rejoining chassis cannot carry either direction yet.
			klog.InfoS("External VXLAN gateway chassis unavailable", "node", node.Name, "error", err)
			continue
		}
		gateway.Chassis = chassis.Name
		gateways = append(gateways, gateway)
		if gateway.Active {
			ready = append(ready, node.Name)
		}
	}
	if config.FRR != nil {
		retained, err := c.externalVXLANIngressGateways(vpc, gateways)
		if err != nil {
			return err
		}
		gateways = append(gateways, retained...)
	}
	if err := c.OVNNbClient.ReconcileExternalVXLANRouterGateways(vpc.Name, gateways, config); err != nil {
		return err
	}
	slices.Sort(ready)
	vpc.Status.ExternalVXLANReadyNodes = ready
	return nil
}

// LS EVPN options apply on every chassis hosting the datapath. Do not activate
// them until all declared gateways and externally annotated hosts have checked
// the exact current interface contract. Node readiness is handled separately.
func (c *Controller) ensureExternalVXLANHostContracts(vpc *kubeovnv1.Vpc) error {
	if vpc.Spec.ExternalVXLAN == nil || vpc.Spec.ExternalVXLAN.FRR == nil {
		return nil
	}
	err := c.validateExternalVXLANHostContracts(vpc)
	if err == nil {
		return nil
	}
	// Remove only owned native exchange state; keep logical VM gateways and
	// host-owned Linux devices, FRR entries and OVS settings intact.
	if withdrawErr := c.OVNNbClient.DeactivateExternalVXLANRouter(vpc.Name); withdrawErr != nil {
		return errors.Join(err, withdrawErr)
	}
	subnets, listErr := c.subnetsLister.List(labels.Everything())
	if listErr != nil {
		return errors.Join(err, listErr)
	}
	for _, subnet := range subnets {
		if subnet.Spec.Vpc == vpc.Name && subnet.Spec.ExternalVXLAN != nil && subnet.Spec.ExternalVXLAN.FRR != nil {
			if withdrawErr := c.OVNNbClient.ReconcileExternalVXLANSwitch(subnet.Name, nil); withdrawErr != nil {
				return errors.Join(err, withdrawErr)
			}
		}
	}
	if len(vpc.Status.ExternalVXLANReadyNodes) != 0 {
		updated := vpc.DeepCopy()
		updated.Status.ExternalVXLANReadyNodes = nil
		if _, patchErr := c.patchVpcStatus(updated, &vpc.Status); patchErr != nil {
			return errors.Join(err, patchErr)
		}
	}
	return err
}

func (c *Controller) validateExternalVXLANHostContracts(vpc *kubeovnv1.Vpc) error {
	subnetObjects, err := c.subnetsLister.List(labels.Everything())
	if err != nil {
		return err
	}
	vpcObjects, err := c.vpcsLister.List(labels.Everything())
	if err != nil {
		return err
	}
	nodes, err := c.nodesLister.List(labels.Everything())
	if err != nil {
		return err
	}
	subnets := make([]kubeovnv1.Subnet, 0, len(subnetObjects))
	for _, subnet := range subnetObjects {
		subnets = append(subnets, *subnet)
	}
	vpcs := make([]kubeovnv1.Vpc, 0, len(vpcObjects))
	for _, object := range vpcObjects {
		vpcs = append(vpcs, *object)
	}
	required := map[string]bool{}
	for _, gateway := range util.ExternalVXLANGatewayNodes(vpc.Spec.ExternalVXLAN) {
		required[gateway.Node] = true
	}
	// The transit LS may become local through an ordinary Pod subnet too.
	// Existing allocations must not bypass validation merely because their
	// host lacks the external VTEP annotation.
	vpcSubnets := map[string]bool{}
	for _, subnet := range subnets {
		if subnet.Spec.Vpc == vpc.Name {
			vpcSubnets[subnet.Name] = true
		}
	}
	ips, err := c.ipsLister.List(labels.Everything())
	if err != nil {
		return err
	}
	for _, ip := range ips {
		if vpcSubnets[ip.Spec.Subnet] {
			required[ip.Spec.NodeName] = true
		}
	}
	for _, node := range nodes {
		if !required[node.Name] && node.Annotations[util.ExternalVXLANIPAnnotation] == "" {
			continue
		}
		if err := util.ValidateExternalVXLANHostContract(node, subnets, vpcs); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) validateExternalVXLANFRRIsolation(vpc *kubeovnv1.Vpc) error {
	if vpc.Spec.ExternalVXLAN.FRR == nil {
		return nil
	}
	vpcs, err := c.vpcsLister.List(labels.Everything())
	if err != nil {
		return err
	}
	want := vpc.Spec.ExternalVXLAN.FRR
	for _, other := range vpcs {
		if other.Name == vpc.Name || other.Spec.ExternalVXLAN == nil || other.Spec.ExternalVXLAN.FRR == nil {
			continue
		}
		have := other.Spec.ExternalVXLAN.FRR
		if have.VRF == want.VRF || have.TableID == want.TableID {
			return fmt.Errorf("FRR VRF/table must be unique between VPCs %s and %s", vpc.Name, other.Name)
		}
	}
	return nil
}

// Type-2 host routes continue to carry the VM host's RMAC after egress moves.
// Keep an owned ingress identity while VPC workload IPs remain allocated there.
// This reads only CMS-owned NB ports and Kubernetes IPAM; it writes no SB state.
func (c *Controller) externalVXLANIngressGateways(vpc *kubeovnv1.Vpc, current []ovs.ExternalVXLANRouterGateway) ([]ovs.ExternalVXLANRouterGateway, error) {
	subnets, err := c.subnetsLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	native := map[string]bool{}
	for _, subnet := range subnets {
		if subnet.Spec.Vpc == vpc.Name {
			native[subnet.Name] = true
		}
	}
	ips, err := c.ipsLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	allocated := map[string]bool{}
	for _, ip := range ips {
		if native[ip.Spec.Subnet] && ip.Spec.NodeName != "" {
			allocated[ip.Spec.NodeName] = true
		}
	}
	for _, gateway := range current {
		delete(allocated, gateway.Host)
	}
	if len(allocated) == 0 {
		return nil, nil
	}
	lsName := util.ExternalVXLANTransitSwitch(vpc.Name)
	ports, err := c.OVNNbClient.ListLogicalRouterPorts(map[string]string{util.ExternalVXLANOwner: lsName}, nil)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(ports, func(a, b ovnnb.LogicalRouterPort) int { return strings.Compare(a.Name, b.Name) })
	base := ovs.LogicalRouterPortName(vpc.Name, lsName)
	var retained []ovs.ExternalVXLANRouterGateway
	for _, port := range ports {
		host := port.ExternalIDs[util.ExternalVXLANNode]
		suffix, multi := strings.CutPrefix(port.Name, base+"-")
		if host == "" && multi {
			host = suffix // Upgrade from ports created before explicit node tagging.
		}
		if !allocated[host] || len(port.Networks) != 1 {
			continue
		}
		node, err := c.nodesLister.Get(host)
		if k8serrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		spec := kubeovnv1.ExternalVXLANGatewayNode{Node: host, TransitCIDR: port.Networks[0], RoutingPort: port.Options["dynamic-routing-port-name"]}
		gateway, err := c.externalVXLANNodeGateway(vpc, spec, node)
		if err != nil {
			klog.ErrorS(err, "External VXLAN retained ingress identity unavailable", "node", host)
			continue
		}
		chassis, err := c.OVNSbClient.GetChassisByHost(host)
		if err != nil {
			continue
		}
		gateway.Chassis, gateway.Active = chassis.Name, false
		gateway.Node = ""
		if multi {
			gateway.Node = suffix
		}
		retained = append(retained, gateway)
		delete(allocated, host)
	}
	return retained, nil
}

func (c *Controller) externalVXLANNodeGateway(vpc *kubeovnv1.Vpc, spec kubeovnv1.ExternalVXLANGatewayNode, node *corev1.Node) (ovs.ExternalVXLANRouterGateway, error) {
	var gateway ovs.ExternalVXLANRouterGateway
	ip := node.Annotations[util.ExternalVXLANIPAnnotation]
	address, err := netip.ParseAddr(ip)
	if err != nil || !address.Is4() || !address.IsGlobalUnicast() {
		return gateway, fmt.Errorf("node %s needs a valid %s", node.Name, util.ExternalVXLANIPAnnotation)
	}
	mac := node.Annotations[util.ExternalVXLANMACAnnotation]
	if err := util.ValidateExternalVXLANMAC(mac); err != nil {
		return gateway, err
	}
	nodes, err := c.nodesLister.List(labels.Everything())
	if err != nil {
		return gateway, err
	}
	for _, other := range nodes {
		if other.Name == node.Name {
			continue
		}
		if other.Annotations[util.ExternalVXLANIPAnnotation] == ip || strings.EqualFold(other.Annotations[util.ExternalVXLANMACAnnotation], mac) {
			return gateway, fmt.Errorf("nodes %s and %s have duplicate external VTEP IP or Router MAC", node.Name, other.Name)
		}
	}
	gateway = ovs.ExternalVXLANRouterGateway{
		Node: spec.Node, Host: node.Name, MAC: strings.ToLower(mac), TransitCIDR: spec.TransitCIDR, RoutingPort: spec.RoutingPort,
		Active: util.ExternalVXLANNodeReady(node) && (spec.Egress == nil || *spec.Egress),
	}
	if len(vpc.Spec.ExternalVXLAN.GatewayNodes) == 0 {
		gateway.Node = ""
	}
	return gateway, nil
}

// Periodic reconciliation also handles gateway Node/chassis changes and NB drift.
func (c *Controller) resyncExternalVXLAN() {
	vpcs, err := c.vpcsLister.List(labels.Everything())
	if err != nil {
		klog.ErrorS(err, "List VPCs for external VXLAN reconciliation")
		return
	}
	for _, vpc := range vpcs {
		if vpc.Spec.ExternalVXLAN != nil {
			c.addOrUpdateVpcQueue.Add(vpc.Name)
		}
	}
	subnets, err := c.subnetsLister.List(labels.Everything())
	if err != nil {
		klog.ErrorS(err, "List subnets for external VXLAN reconciliation")
		return
	}
	for _, subnet := range subnets {
		if subnet.Spec.ExternalVXLAN != nil {
			c.addOrUpdateSubnetQueue.Add(subnet.Name)
		}
	}
	if err := c.cleanupExternalVXLANOrphans(); err != nil {
		klog.ErrorS(err, "Clean up external VXLAN configuration without an API owner")
	}
}

// Recover even if a process stopped after writing NB but before recording status.
func (c *Controller) cleanupExternalVXLANOrphans() error {
	switches, err := c.OVNNbClient.ListLogicalSwitch(true, func(row *ovnnb.LogicalSwitch) bool {
		return row.ExternalIDs[util.ExternalVXLANOwner] == row.Name
	})
	if err != nil {
		return err
	}
	for _, ls := range switches {
		if err := c.cleanupExternalVXLANOrphan(ls.Name); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) cleanupExternalVXLANOrphan(lsName string) error {
	// Re-read desired state under the same key lock as ordinary reconciliation.
	// Otherwise a queued cleanup can remove a freshly recreated attachment.
	if name, transit := strings.CutPrefix(lsName, "evx-transit-"); transit {
		c.vpcKeyMutex.LockKey(name)
		defer func() { _ = c.vpcKeyMutex.UnlockKey(name) }()
		vpc, err := c.vpcsLister.Get(name)
		if err != nil && !k8serrors.IsNotFound(err) {
			return err
		}
		if vpc != nil && vpc.Spec.ExternalVXLAN != nil {
			return nil
		}
		return c.OVNNbClient.ReconcileExternalVXLANRouter(name, "", "", nil)
	}
	c.subnetKeyMutex.LockKey(lsName)
	defer func() { _ = c.subnetKeyMutex.UnlockKey(lsName) }()
	subnet, err := c.subnetsLister.Get(lsName)
	if err != nil && !k8serrors.IsNotFound(err) {
		return err
	}
	if subnet != nil && subnet.Spec.ExternalVXLAN != nil {
		return nil
	}
	return c.OVNNbClient.ReconcileExternalVXLANSwitch(lsName, nil)
}
