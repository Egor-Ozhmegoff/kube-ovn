// Package host owns the host-side attachment only. It never changes the
// OpenPERouter namespace or creates/deletes OpenPERouter veth interfaces.
package host

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"syscall"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/openperouter"
	"github.com/kubeovn/kube-ovn/pkg/ovs"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/table"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/vswitch"
)

const routeProtocol = 202

type Attachment struct {
	VRF    *netlink.Vrf
	Node   openperouter.NodeAttachment
	Egress netlink.Link
	Return netlink.Link
	Peer   netlink.Link
}

func Ensure(ctx context.Context, provider table.Provider, vpc *kubeovnv1.Vpc, nodeName string, bootstrap *openperouter.Bootstrap, mtu int, workloadCIDRs []string) (*Attachment, error) {
	defer openperouter.Measure(ctx, "host.l3")()
	if mtu <= 0 {
		return nil, errors.New("workload MTU must be initialized before attaching OpenPERouter")
	}
	spec := vpc.Spec.OpenPERouter
	if err := bootstrap.Validate(spec); err != nil {
		return nil, err
	}
	node, err := openperouter.BuildNode(vpc.Name, nodeName, bootstrap.Index, spec)
	if err != nil {
		return nil, err
	}
	peer, err := netlink.LinkByName(node.HostInterface)
	if err != nil {
		return nil, fmt.Errorf("native OpenPERouter host interface is unavailable: %w", err)
	}
	if peer.Type() != "veth" || peer.Attrs().Flags&net.FlagUp == 0 {
		return nil, fmt.Errorf("native interface %s is not a ready veth", node.HostInterface)
	}
	if err := checkNativeAddresses(peer, node.HostAddresses); err != nil {
		return nil, err
	}
	vrf, err := ensureVRF(spec.VRF, spec.L3VNI, string(vpc.UID))
	if err != nil {
		return nil, err
	}
	// Install terminal defaults before attaching any ingress. They remain in
	// this host VRF even when the native veth disappears or loses its routes.
	for _, cidr := range []string{"0.0.0.0/0", "::/0"} {
		_, dst, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, err
		}
		if err := netlink.RouteReplace(&netlink.Route{Dst: dst, Table: int(vrf.Table), Type: unix.RTN_UNREACHABLE, Priority: 32767, Protocol: routeProtocol}); err != nil {
			return nil, err
		}
	}
	if err := EnsureIsolation(node.HostInterface, vrf.Table); err != nil {
		return nil, err
	}
	if peer.Attrs().MasterIndex != 0 && peer.Attrs().MasterIndex != vrf.Index {
		return nil, fmt.Errorf("native host interface %s is attached to another master", node.HostInterface)
	}
	if peer.Attrs().MasterIndex != vrf.Index {
		if err := netlink.LinkSetMaster(peer, vrf); err != nil {
			return nil, err
		}
	}
	transit, err := openperouter.BuildTransit(vpc.Name, spec)
	if err != nil {
		return nil, err
	}
	mtu = min(mtu, peer.Attrs().MTU)
	ret, err := ensureInternal(ctx, provider, node.ReturnInterface, node.ReturnPort, node.ReturnMAC, node.ReturnAddresses, vpc, vrf, mtu, true)
	if err != nil {
		return nil, err
	}
	a := &Attachment{VRF: vrf, Node: node, Return: ret, Peer: peer}
	if err := a.routes(transit, workloadCIDRs); err != nil {
		return nil, err
	}
	// Egress becomes usable only after the return interface and routes exist.
	egress, err := ensureInternal(ctx, provider, node.EgressInterface, transit.EgressPort, transit.EgressMAC, transit.EgressAddresses, vpc, vrf, mtu, false)
	if err != nil {
		return nil, err
	}
	a.Egress = egress
	return a, nil
}

func ensureVRF(name string, vni int32, uid string) (*netlink.Vrf, error) {
	if vni < 1 || vni > 16777215 {
		return nil, fmt.Errorf("invalid L3VNI %d", vni)
	}
	alias := "kube-ovn/openperouter/" + uid
	link, err := netlink.LinkByName(name)
	if err == nil {
		vrf, ok := link.(*netlink.Vrf)
		if !ok || vrf.Alias != alias {
			return nil, fmt.Errorf("refuse to adopt host interface %s", name)
		}
		if vrf.Flags&net.FlagUp == 0 {
			if err := netlink.LinkSetUp(vrf); err != nil {
				return nil, err
			}
		}
		return vrf, nil
	}
	if !errors.As(err, &netlink.LinkNotFoundError{}) {
		return nil, err
	}
	tableID := uint32(vni) + 100000 // validated VNI is positive and at most 24 bits
	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	for _, existing := range links {
		if vrf, ok := existing.(*netlink.Vrf); ok && vrf.Table == tableID {
			return nil, fmt.Errorf("host routing table %d is already in use", tableID)
		}
	}
	vrf := &netlink.Vrf{Name: name, Alias: alias, Table: tableID}
	if err := netlink.LinkAdd(vrf); err != nil {
		return nil, err
	}
	if err := netlink.LinkSetAlias(vrf, alias); err != nil {
		return nil, err
	}
	if err := netlink.LinkSetUp(vrf); err != nil {
		return nil, err
	}
	return vrf, nil
}

func ensureInternal(ctx context.Context, provider table.Provider, name, logicalPort, mac string, addresses []string, vpc *kubeovnv1.Vpc, vrf *netlink.Vrf, mtu int, open bool) (netlink.Link, error) {
	iface := &vswitch.Interface{Name: name}
	err := provider.Table(iface).Get(ctx, iface)
	if err != nil && !errors.Is(err, table.ErrNotFound) {
		return nil, err
	}
	if err == nil && iface.ExternalIDs[openperouter.OwnerKey+"-uid"] != string(vpc.UID) {
		return nil, fmt.Errorf("refuse to adopt OVS interface %s", name)
	}
	port := &vswitch.Port{Name: name}
	portErr := provider.Table(port).Get(ctx, port)
	if portErr != nil && !errors.Is(portErr, table.ErrNotFound) {
		return nil, portErr
	}
	if portErr == nil && port.ExternalIDs[openperouter.OwnerKey+"-uid"] != string(vpc.UID) {
		return nil, fmt.Errorf("refuse to adopt OVS port %s", name)
	}
	if err := EnsureIsolation(name, vrf.Table); err != nil {
		return nil, err
	}
	fresh := errors.Is(err, table.ErrNotFound)
	ids := map[string]string{openperouter.OwnerKey: vpc.Name, openperouter.OwnerKey + "-uid": string(vpc.UID)}
	desired := &vswitch.Interface{Name: name, Type: "internal", ExternalIDs: map[string]string{"iface-id": logicalPort, openperouter.OwnerKey: vpc.Name, openperouter.OwnerKey + "-uid": string(vpc.UID)}}
	desired.MTURequest = &mtu
	if err := ensureVswitchPort(ctx, provider, ovs.VswitchPortConfig{
		BridgeName: "br-int", Port: &vswitch.Port{Name: name, ExternalIDs: ids}, Interface: desired,
		InterfaceFields: []any{&desired.Type, &desired.ExternalIDs, &desired.MTURequest},
	}); err != nil {
		return nil, err
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		return nil, err
	}
	if !open && (fresh || link.Attrs().MasterIndex != vrf.Index || link.Attrs().HardwareAddr.String() != mac || link.Attrs().MTU != mtu || checkNativeAddresses(link, addresses) != nil) {
		if err := netlink.LinkSetDown(link); err != nil {
			return nil, err
		}
	}
	if link.Attrs().MasterIndex != 0 && link.Attrs().MasterIndex != vrf.Index {
		return nil, fmt.Errorf("interface %s is attached to another master", name)
	}
	if link.Attrs().MasterIndex != vrf.Index {
		if err := netlink.LinkSetMaster(link, vrf); err != nil {
			return nil, err
		}
	}
	hardware, err := net.ParseMAC(mac)
	if err != nil {
		return nil, err
	}
	if link.Attrs().HardwareAddr.String() != hardware.String() {
		if err := netlink.LinkSetHardwareAddr(link, hardware); err != nil {
			return nil, err
		}
	}
	currentAddresses, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return nil, err
	}
	for _, address := range addresses {
		addr, err := netlink.ParseAddr(address)
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(currentAddresses, func(current netlink.Addr) bool { return current.Equal(*addr) }) {
			if err := netlink.AddrReplace(link, addr); err != nil {
				return nil, err
			}
		}
	}
	if !open {
		return netlink.LinkByName(name)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return nil, err
	}
	return netlink.LinkByName(name)
}

func checkNativeAddresses(link netlink.Link, expected []string) error {
	actual, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return err
	}
	for _, address := range expected {
		if !slices.ContainsFunc(actual, func(addr netlink.Addr) bool { return addr.IPNet != nil && addr.IPNet.String() == address }) {
			return fmt.Errorf("native host interface %s lacks bootstrap address %s", link.Attrs().Name, address)
		}
	}
	return nil
}

func (a *Attachment) routes(transit openperouter.Transit, workloadCIDRs []string) error {
	var desired []netlink.Route
	for _, peer := range a.Node.PeerAddresses {
		gateway := net.ParseIP(peer)
		prefix := "0.0.0.0/0"
		if gateway.To4() == nil {
			prefix = "::/0"
		}
		_, dst, err := net.ParseCIDR(prefix)
		if err != nil {
			return err
		}
		desired = append(desired, netlink.Route{Dst: dst, Gw: gateway, LinkIndex: a.Peer.Attrs().Index, Priority: 100})
	}
	for _, cidr := range workloadCIDRs {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			return err
		}
		for _, network := range transit.RouterNetworks {
			router := netip.MustParsePrefix(network).Addr()
			if router.Is4() == prefix.Addr().Is4() {
				_, dst, err := net.ParseCIDR(cidr)
				if err != nil {
					return err
				}
				desired = append(desired, netlink.Route{Dst: dst, Gw: net.ParseIP(router.String()), LinkIndex: a.Return.Attrs().Index})
			}
		}
	}
	for i := range desired {
		desired[i].Table, desired[i].Protocol = int(a.VRF.Table), routeProtocol
		if err := netlink.RouteReplace(&desired[i]); err != nil {
			return err
		}
	}
	current, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: int(a.VRF.Table), Protocol: routeProtocol}, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_PROTOCOL)
	if err != nil {
		return err
	}
	for _, route := range current {
		if route.Type == unix.RTN_UNREACHABLE || slices.ContainsFunc(desired, func(want netlink.Route) bool {
			return routeDestination(want) == routeDestination(route) && want.Gw.Equal(route.Gw) && want.LinkIndex == route.LinkIndex
		}) {
			continue
		}
		if err := netlink.RouteDel(&route); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	return nil
}

func routeDestination(route netlink.Route) string {
	if route.Dst != nil {
		return route.Dst.String()
	}
	if route.Family == netlink.FAMILY_V6 || (route.Gw != nil && route.Gw.To4() == nil) {
		return "::/0"
	}
	return "0.0.0.0/0"
}

func ensureVswitchPort(ctx context.Context, provider table.Provider, config ovs.VswitchPortConfig) error {
	defer openperouter.Measure(ctx, "ovs.ensure-port")()
	return ovs.EnsureVswitchPort(ctx, provider, config)
}

// Check revalidates the local host generation without opening ingress.
func (a *Attachment) Check() error {
	for _, expected := range []netlink.Link{a.VRF, a.Peer, a.Return, a.Egress} {
		actual, err := netlink.LinkByName(expected.Attrs().Name)
		if err != nil {
			return err
		}
		if actual.Attrs().Index != expected.Attrs().Index || actual.Attrs().Alias != expected.Attrs().Alias || (expected != a.VRF && actual.Attrs().MasterIndex != a.VRF.Index) {
			return fmt.Errorf("attachment generation changed: %s", expected.Attrs().Name)
		}
		if expected != a.Egress && actual.Attrs().Flags&net.FlagUp == 0 {
			return fmt.Errorf("attachment is no longer up: %s", expected.Attrs().Name)
		}
		if vrf, ok := actual.(*netlink.Vrf); ok && vrf.Table != a.VRF.Table {
			return errors.New("host attachment VRF table changed")
		}
	}
	return nil
}

// Open requires the fresh OVN/PE checks performed by the caller as well.
func (a *Attachment) Open() error {
	if err := a.Check(); err != nil {
		return err
	}
	return netlink.LinkSetUp(a.Egress)
}
