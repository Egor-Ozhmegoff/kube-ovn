// Package openperouter describes the OVN/host boundary. It never owns devices
// or FRR configuration inside the OpenPERouter namespace.
package openperouter

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
)

const (
	Mode                    = "OpenPERouter"
	NodeIndexAnnotation     = "openperouter.kubeovn.io/node-index"
	NodeRouterMACAnnotation = "openperouter.kubeovn.io/router-mac"
	OwnerKey                = "openperouter.kubeovn.io/vpc"
	MaxNodeIndex            = 4091
)

var vrfName = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,14}$`)

// Transit uses a replicated localport only for egress. Return traffic uses a
// separate VIF: a localport cannot deliver to another chassis after ingress.
type Transit struct {
	Switch, RouterPort, SwitchPort, EgressPort string
	RouterMAC, EgressMAC                       string
	RouterNetworks, EgressAddresses            []string
	Defaults                                   []Route
}

type Route struct {
	Prefix, NextHop string
}

type NodeAttachment struct {
	ReturnPort, ReturnMAC, EgressInterface, ReturnInterface, HostInterface string
	ReturnAddresses, HostAddresses, PeerAddresses                          []string
}

func BuildTransit(vpc string, spec *kubeovnv1.OpenPERouterIntegration) (Transit, error) {
	if err := Validate(spec); err != nil {
		return Transit{}, err
	}
	t := Transit{
		Switch: "oprt-" + vpc, RouterPort: "oprr-" + vpc,
		SwitchPort: "oprs-" + vpc, EgressPort: "opre-" + vpc,
		RouterMAC: portMAC(spec.L3VNI, 0), EgressMAC: portMAC(spec.L3VNI, 1),
	}
	for _, cidr := range spec.TransitCIDRs {
		prefix := netip.MustParsePrefix(cidr).Masked()
		router, err := poolAddress(prefix, 1)
		if err != nil {
			return Transit{}, err
		}
		egress, err := poolAddress(prefix, 2)
		if err != nil {
			return Transit{}, err
		}
		t.RouterNetworks = append(t.RouterNetworks, netip.PrefixFrom(router, prefix.Bits()).String())
		t.EgressAddresses = append(t.EgressAddresses, netip.PrefixFrom(egress, prefix.Bits()).String())
		defaultPrefix := "0.0.0.0/0"
		if prefix.Addr().Is6() {
			defaultPrefix = "::/0"
		}
		t.Defaults = append(t.Defaults, Route{Prefix: defaultPrefix, NextHop: egress.String()})
	}
	return t, nil
}

func BuildNode(vpc, node string, index int, spec *kubeovnv1.OpenPERouterIntegration) (NodeAttachment, error) {
	if err := Validate(spec); err != nil {
		return NodeAttachment{}, err
	}
	if index < 0 || index > MaxNodeIndex {
		return NodeAttachment{}, fmt.Errorf("node index %d is outside 0..%d", index, MaxNodeIndex)
	}
	n := NodeAttachment{
		ReturnPort: "oprb-" + vpc + "-" + node, ReturnMAC: portMAC(spec.L3VNI, index+2),
		EgressInterface: fmt.Sprintf("opre%x", spec.L3VNI), ReturnInterface: fmt.Sprintf("oprr%x", spec.L3VNI),
		HostInterface: fmt.Sprintf("host-e-%d", spec.L3VNI),
	}
	for _, cidr := range spec.TransitCIDRs {
		prefix := netip.MustParsePrefix(cidr).Masked()
		addr, err := poolAddress(prefix, index+3)
		if err != nil {
			return NodeAttachment{}, err
		}
		n.ReturnAddresses = append(n.ReturnAddresses, netip.PrefixFrom(addr, prefix.Bits()).String())
	}
	for _, cidr := range spec.HostSessionCIDRs {
		prefix := netip.MustParsePrefix(cidr)
		// Mirrors the pinned OpenPERouter VethIPsFromPool allocation. Its PE
		// address is shared across isolated router namespaces, not across nodes.
		peerIndex := 0
		if prefix.Masked().Addr().AsSlice()[len(prefix.Addr().AsSlice())-1] == 0 {
			peerIndex = 1
		}
		peer, err := poolAddress(prefix.Masked(), peerIndex)
		if err != nil {
			return NodeAttachment{}, err
		}
		host, err := poolAddress(prefix.Masked(), index+peerIndex+1)
		if err != nil {
			return NodeAttachment{}, err
		}
		n.PeerAddresses = append(n.PeerAddresses, peer.String())
		n.HostAddresses = append(n.HostAddresses, netip.PrefixFrom(host, prefix.Bits()).String())
	}
	return n, nil
}

func Validate(spec *kubeovnv1.OpenPERouterIntegration) error {
	if spec == nil || spec.Mode != Mode {
		return fmt.Errorf("integration mode must be %s", Mode)
	}
	if !vrfName.MatchString(spec.VRF) || spec.L3VNIName == "" {
		return errors.New("VRF (1..15 characters) and native L3VNI name are required")
	}
	if spec.L3VNI < 1 || spec.L3VNI > 16777215 || spec.RouterASN == 0 || spec.HostASN == 0 || spec.RouterASN == spec.HostASN {
		return errors.New("valid VNI and distinct nonzero router/host ASNs are required")
	}
	transit, err := validatePools(spec.TransitCIDRs)
	if err != nil {
		return fmt.Errorf("transit: %w", err)
	}
	host, err := validatePools(spec.HostSessionCIDRs)
	if err != nil {
		return fmt.Errorf("host session: %w", err)
	}
	for _, p := range transit {
		for _, q := range host {
			if p.Overlaps(q) {
				return fmt.Errorf("transit %s overlaps native host session %s", p, q)
			}
		}
		if !slices.ContainsFunc(host, func(q netip.Prefix) bool { return p.Addr().BitLen() == q.Addr().BitLen() }) {
			return fmt.Errorf("transit family %s has no native host session", p)
		}
	}
	return nil
}

func ValidateL2(spec *kubeovnv1.OpenPERouterL2Attachment) error {
	if spec == nil {
		return nil
	}
	mac, err := net.ParseMAC(spec.GatewayMAC)
	if err != nil || len(mac) != 6 || mac[0]&1 != 0 || strings.EqualFold(spec.GatewayMAC, "00:00:00:00:00:00") {
		return errors.New("L2 gateway MAC must be a nonzero 48-bit unicast address")
	}
	if spec.L2VNIName == "" || spec.VNI < 1 || spec.VNI > 16777215 {
		return errors.New("native L2VNI name and VNI 1..16777215 are required")
	}
	return nil
}

func L2Port(subnet string) string { return "oprl2-" + subnet }

func portMAC(vni int32, id int) string {
	b := []byte{2, byte((vni >> 16) & 255), byte((vni >> 8) & 255), byte(vni & 255), 0, 0}
	binary.BigEndian.PutUint16(b[4:], uint16(id&65535))
	return net.HardwareAddr(b).String()
}

func validatePools(cidrs []string) ([]netip.Prefix, error) {
	if len(cidrs) < 1 || len(cidrs) > 2 {
		return nil, errors.New("one IPv4 and/or one IPv6 pool is required")
	}
	pools := make([]netip.Prefix, 0, len(cidrs))
	for _, cidr := range cidrs {
		p, err := netip.ParsePrefix(cidr)
		if err != nil || p.Addr().Is4In6() || p.Addr().IsMulticast() || p != p.Masked() {
			return nil, fmt.Errorf("invalid canonical unicast pool %q", cidr)
		}
		if slices.ContainsFunc(pools, func(q netip.Prefix) bool { return p.Addr().BitLen() == q.Addr().BitLen() }) {
			return nil, fmt.Errorf("duplicate address family in %q", cidrs)
		}
		if _, err := poolAddress(p, MaxNodeIndex+3); err != nil {
			return nil, fmt.Errorf("pool must accommodate node indices through %d: %w", MaxNodeIndex, err)
		}
		pools = append(pools, p)
	}
	return pools, nil
}

func poolAddress(prefix netip.Prefix, offset int) (netip.Addr, error) {
	addr := prefix.Addr()
	for range offset {
		addr = addr.Next()
	}
	if !addr.IsValid() || !prefix.Contains(addr) || (addr.Is4() && !prefix.Contains(addr.Next())) {
		return netip.Addr{}, fmt.Errorf("offset %d exceeds usable pool %s", offset, prefix)
	}
	return addr, nil
}
