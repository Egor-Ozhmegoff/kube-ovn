package routes

import (
	"fmt"
	"net/netip"
	"strings"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/openperouter"
	"github.com/kubeovn/kube-ovn/pkg/util"
)

// RoutedSubnetPrefixes describes reachability behind the OVN router. Native
// L2VNI segments already have PE connected routes and EVPN Type-2 ownership.
// Endpoint location, readiness and migration are deliberately not inputs.
func RoutedSubnetPrefixes(vpc *kubeovnv1.Vpc, subnets []*kubeovnv1.Subnet) (map[netip.Prefix]struct{}, error) {
	prefixes := make(map[netip.Prefix]struct{})
	for _, subnet := range subnets {
		if subnet.Spec.Vpc != vpc.Name || subnet.Spec.OpenPERouterL2 != nil || !subnet.DeletionTimestamp.IsZero() || !subnet.Status.IsReady() || !util.IsOvnProvider(subnet.Spec.Provider) {
			continue
		}
		if err := openperouter.ValidateSubnetBinding(subnet, vpc, nil); err != nil {
			return nil, err
		}
		for cidr := range strings.SplitSeq(subnet.Spec.CIDRBlock, ",") {
			p, err := netip.ParsePrefix(strings.TrimSpace(cidr))
			if err != nil {
				return nil, fmt.Errorf("subnet %s: %w", subnet.Name, err)
			}
			prefixes[p.Masked()] = struct{}{}
		}
	}
	return prefixes, nil
}
