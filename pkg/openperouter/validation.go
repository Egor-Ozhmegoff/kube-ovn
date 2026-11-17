package openperouter

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
)

func ValidateVpcConflicts(vpc *kubeovnv1.Vpc, others []*kubeovnv1.Vpc) error {
	spec := vpc.Spec.OpenPERouter
	if spec == nil {
		return nil
	}
	if err := Validate(spec); err != nil {
		return err
	}
	for _, other := range others {
		x := other.Spec.OpenPERouter
		if other.Name == vpc.Name || x == nil {
			continue
		}
		if x.L3VNI == spec.L3VNI || x.VRF == spec.VRF || x.L3VNIName == spec.L3VNIName {
			return fmt.Errorf("OpenPERouter routing domain conflicts with VPC %s", other.Name)
		}
		for _, cidr := range spec.HostSessionCIDRs {
			prefix := netip.MustParsePrefix(cidr)
			for _, existing := range x.HostSessionCIDRs {
				if otherPrefix, err := netip.ParsePrefix(existing); err == nil && prefix.Overlaps(otherPrefix) {
					return fmt.Errorf("native host session %s overlaps VPC %s", prefix, other.Name)
				}
			}
		}
	}
	return nil
}

func ValidateSubnetBinding(subnet *kubeovnv1.Subnet, vpc *kubeovnv1.Vpc, others []kubeovnv1.Subnet) error {
	if vpc == nil || vpc.Spec.OpenPERouter == nil {
		if subnet.Spec.OpenPERouterL2 != nil {
			return fmt.Errorf("subnet %s requires an OpenPERouter VPC", subnet.Name)
		}
		return nil
	}
	if subnet.Spec.NatOutgoing || subnet.Spec.Vlan != "" || subnet.Spec.U2OInterconnection || subnet.Spec.ExternalEgressGateway != "" ||
		subnet.Spec.GatewayType == kubeovnv1.GWCentralizedType || subnet.Spec.GatewayNode != "" || len(subnet.Spec.GatewayNodeSelectors) != 0 || subnet.Spec.RouteTable != "" {
		return fmt.Errorf("subnet %s cannot combine OpenPERouter with NAT, VLAN, centralized/alternate gateways or route tables", subnet.Name)
	}
	for cidr := range strings.SplitSeq(subnet.Spec.CIDRBlock, ",") {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			return err
		}
		for _, pool := range append(append([]string(nil), vpc.Spec.OpenPERouter.TransitCIDRs...), vpc.Spec.OpenPERouter.HostSessionCIDRs...) {
			transit, err := netip.ParsePrefix(pool)
			if err != nil {
				return err
			}
			if transit.Overlaps(prefix) {
				return fmt.Errorf("workload prefix %s overlaps OpenPERouter transit/host session %s", prefix, transit)
			}
		}
	}
	spec := subnet.Spec.OpenPERouterL2
	if err := ValidateL2(spec); err != nil {
		return err
	}
	if spec == nil {
		return nil
	}
	if spec.VNI == vpc.Spec.OpenPERouter.L3VNI {
		return errors.New("L2VNI and L3VNI must be distinct")
	}
	for _, other := range others {
		if x := other.Spec.OpenPERouterL2; other.Name != subnet.Name && x != nil && (x.VNI == spec.VNI || x.L2VNIName == spec.L2VNIName) {
			return fmt.Errorf("native L2VNI is already attached to subnet %s", other.Name)
		}
	}
	return nil
}
