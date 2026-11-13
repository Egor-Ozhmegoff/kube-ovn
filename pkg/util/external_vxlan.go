package util

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
)

const (
	ExternalVXLANIPAnnotation  = "ovn.kubernetes.io/external-vxlan-ip"
	ExternalVXLANMACAnnotation = "ovn.kubernetes.io/external-vxlan-router-mac"
	ExternalVXLANOwner         = "kube-ovn-external-vxlan"
	ExternalVXLANNode          = "kube-ovn-external-vxlan-node"
)

func ExternalVXLANTransitSwitch(vpc string) string { return "evx-transit-" + vpc }

func ValidateExternalVXLANMAC(value string) error {
	mac, err := net.ParseMAC(value)
	if err != nil || len(mac) != 6 || len(value) != 17 || strings.Count(value, ":") != 5 || mac[0]&1 != 0 || mac.String() == "00:00:00:00:00:00" {
		return fmt.Errorf("external VXLAN requires a nonzero unicast Ethernet MAC, got %q", value)
	}
	return nil
}

func ValidateExternalVXLAN(config *kubeovnv1.ExternalVXLAN) error {
	if config == nil {
		return nil
	}
	if config.VNI == 0 || config.VNI > 16777215 {
		return fmt.Errorf("external VXLAN VNI %d is outside 1..16777215", config.VNI)
	}
	if config.FRR == nil {
		return errors.New("external VXLAN requires a host FRR interface contract; static inputs are unsupported")
	}
	return ValidateExternalVXLANFRR(config.FRR)
}

func ValidateExternalVXLANFRR(config *kubeovnv1.ExternalVXLANFRR) error {
	seen := map[string]bool{}
	for _, name := range []string{config.Bridge, config.VXLAN, config.Advertise, config.VRF} {
		if name == "" || len(name) > 15 || strings.ContainsAny(name, "/ \t\n,=:") || seen[name] {
			return errors.New("FRR requires distinct valid explicitly named host interfaces")
		}
		seen[name] = true
	}
	if config.TableID == 0 || config.TableID >= 252 && config.TableID <= 255 || config.TableID == 4294967295 || config.ControlPort < 1 || config.ControlPort > 65535 || config.ControlPort == 4789 {
		return errors.New("FRR requires a nonreserved VRF table and a shadow UDP port distinct from 4789")
	}
	return nil
}

func ValidateExternalVXLANSubnet(subnet kubeovnv1.Subnet) error {
	config := subnet.Spec.ExternalVXLAN
	if config == nil {
		return nil
	}
	if strings.HasPrefix(subnet.Name, "evx-transit-") {
		return errors.New("external VXLAN subnet name uses the reserved transit switch prefix")
	}
	if err := ValidateExternalVXLAN(config); err != nil {
		return err
	}
	if err := ValidateExternalVXLANMAC(config.GatewayMAC); err != nil {
		return err
	}
	if subnet.Spec.Vlan != "" || subnet.Spec.U2OInterconnection || subnet.Spec.NatOutgoing ||
		subnet.Spec.ExternalEgressGateway != "" || subnet.Spec.Vpc == "" || subnet.Spec.Vpc == DefaultVpc ||
		!IsOvnProvider(subnet.Spec.Provider) ||
		(subnet.Spec.GatewayType != "" && subnet.Spec.GatewayType != kubeovnv1.GWDistributedType) {
		return errors.New("external VXLAN subnet requires a custom VPC, OVN overlay and distributed gateway without host egress or NAT")
	}
	if name, namespace, secondary := GetNadBySubnetProvider(subnet.Spec.Provider); secondary {
		if len(validation.IsDNS1123Label(name)) != 0 || len(validation.IsDNS1123Label(namespace)) != 0 {
			return errors.New("external VXLAN secondary provider must be <nad>.<namespace>.ovn")
		}
	}
	if prefix, err := netip.ParsePrefix(subnet.Spec.CIDRBlock); err != nil || !prefix.Addr().Is4() {
		return errors.New("external VXLAN currently supports IPv4 subnets only")
	}
	return nil
}

func ValidateExternalVXLANGateway(config *kubeovnv1.ExternalVXLANGateway) error {
	if config == nil {
		return nil
	}
	if err := ValidateExternalVXLAN(&config.ExternalVXLAN); err != nil {
		return err
	}
	if config.GatewayMAC != "" || (config.GatewayNode != "") == (len(config.GatewayNodes) != 0) {
		return errors.New("external VXLAN VPC requires gatewayNodes or legacy gatewayNode, exclusively, and uses node Router MACs")
	}
	if len(config.GatewayNodes) != 0 && (config.TransitCIDR != "" || config.RoutingPort != "") {
		return errors.New("transitCIDR and routingPort must be configured per gatewayNodes entry")
	}
	nodes, addresses := map[string]bool{}, map[netip.Addr]bool{}
	for _, gateway := range ExternalVXLANGatewayNodes(config) {
		if gateway.RoutingPort == "" || strings.ContainsAny(gateway.RoutingPort, ",= \t\n") {
			return errors.New("FRR requires each gateway routingPort to reference an existing host port mapping")
		}
		prefix, err := netip.ParsePrefix(gateway.TransitCIDR)
		if err != nil || !prefix.Addr().Is4() || prefix.Addr().IsUnspecified() || prefix.Addr().IsMulticast() {
			return errors.New("external VXLAN transitCIDR must be an IPv4 address/prefix")
		}
		if len(validation.IsDNS1123Subdomain(gateway.Node)) != 0 || nodes[gateway.Node] || addresses[prefix.Addr()] {
			return errors.New("external VXLAN gateways require valid unique node names and transit addresses")
		}
		nodes[gateway.Node], addresses[prefix.Addr()] = true, true
	}

	return nil
}

func ExternalVXLANGatewayNodes(config *kubeovnv1.ExternalVXLANGateway) []kubeovnv1.ExternalVXLANGatewayNode {
	if len(config.GatewayNodes) != 0 {
		return config.GatewayNodes
	}
	return []kubeovnv1.ExternalVXLANGatewayNode{{Node: config.GatewayNode, TransitCIDR: config.TransitCIDR, RoutingPort: config.RoutingPort}}
}

// ExternalVXLANRouterOptions is desired configuration, not a blanket preservation
// of arbitrary externally written dynamic-routing options.
func ExternalVXLANRouterOptions(config *kubeovnv1.ExternalVXLANGateway) map[string]string {
	if config == nil || config.FRR == nil {
		return nil
	}
	return map[string]string{
		"dynamic-routing":              "true",
		"dynamic-routing-vrf-name":     config.FRR.VRF,
		"dynamic-routing-vrf-id":       strconv.FormatUint(uint64(config.FRR.TableID), 10),
		"dynamic-routing-redistribute": "connected",
	}
}

func ExternalVXLANNodeReady(node *corev1.Node) bool {
	if node == nil || !node.DeletionTimestamp.IsZero() {
		return false
	}
	ready := false
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeNetworkUnavailable && condition.Status == corev1.ConditionTrue {
			return false
		}
		if condition.Type == corev1.NodeReady {
			ready = condition.Status == corev1.ConditionTrue
		}
	}
	return ready
}
