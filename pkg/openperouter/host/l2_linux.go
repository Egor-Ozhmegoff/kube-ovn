package host

import (
	"context"
	"errors"
	"fmt"

	"github.com/vishvananda/netlink"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/openperouter"
	"github.com/kubeovn/kube-ovn/pkg/ovs"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/table"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/vswitch"
)

func EnsureL2(ctx context.Context, provider table.Provider, vpc *kubeovnv1.Vpc, subnet *kubeovnv1.Subnet, bootstrap *openperouter.Bootstrap, defaultMTU int, reflectionMACs []string) (*L2Attachment, error) {
	if err := bootstrap.ValidateL2(vpc.Spec.OpenPERouter, subnet); err != nil {
		return nil, err
	}
	vni := subnet.Spec.OpenPERouterL2.VNI
	name := fmt.Sprintf("host-e-%d", vni)
	native, err := netlink.LinkByName(name)
	if err != nil {
		return nil, err
	}
	mtu := defaultMTU
	if subnet.Spec.Mtu != 0 {
		mtu = int(subnet.Spec.Mtu)
	}
	if native.Type() != "veth" || mtu > native.Attrs().MTU {
		return nil, fmt.Errorf("native L2 interface %s is not ready", name)
	}
	// Migrate only an old attachment of this exact subnet, never a live binding
	// of another subnet/VPC that happens to reuse the same native name.
	legacy := &vswitch.Interface{Name: name}
	err = provider.Table(legacy).Get(ctx, legacy)
	if err != nil && !errors.Is(err, table.ErrNotFound) {
		return nil, err
	}
	if err == nil {
		if legacy.ExternalIDs[openperouter.OwnerKey+"-uid"] != string(vpc.UID) || legacy.ExternalIDs["subnet-uid"] != string(subnet.UID) {
			return nil, fmt.Errorf("foreign native L2 binding %s", name)
		}
		if err := removeAttachment(ctx, provider, *legacy); err != nil {
			return nil, err
		}
	}
	_, guardName, _ := L2Names(vni)
	guard := &vswitch.Interface{Name: guardName}
	err = provider.Table(guard).Get(ctx, guard)
	if err != nil && !errors.Is(err, table.ErrNotFound) {
		return nil, err
	}
	if err == nil && (guard.ExternalIDs[openperouter.OwnerKey+"-uid"] != string(vpc.UID) || guard.ExternalIDs["subnet-uid"] != string(subnet.UID)) {
		return nil, fmt.Errorf("foreign L2 binding %s", guardName)
	}
	fresh := errors.Is(err, table.ErrNotFound)
	bridgeName, _, peerName := L2Names(vni)
	alias := l2AliasPrefix + string(vpc.UID) + "/" + string(subnet.UID)
	for _, linkName := range []string{bridgeName, guardName, peerName} {
		link, err := netlink.LinkByName(linkName)
		if errors.As(err, &netlink.LinkNotFoundError{}) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if link.Attrs().Alias != alias && (linkName != peerName || link.Attrs().Alias != "") {
			return nil, fmt.Errorf("foreign L2 path %s", linkName)
		}
	}
	port := &vswitch.Port{Name: guardName}
	portErr := provider.Table(port).Get(ctx, port)
	if portErr != nil && !errors.Is(portErr, table.ErrNotFound) {
		return nil, portErr
	}
	if portErr == nil && (port.ExternalIDs[openperouter.OwnerKey+"-uid"] != string(vpc.UID) || port.ExternalIDs["subnet-uid"] != string(subnet.UID)) {
		return nil, fmt.Errorf("foreign L2 port %s", guardName)
	}
	vrf, err := ensureVRF(vpc.Spec.OpenPERouter.VRF, vpc.Spec.OpenPERouter.L3VNI, string(vpc.UID))
	if err != nil {
		return nil, err
	}
	if fresh && native.Attrs().MasterIndex == 0 {
		if err := RebindIsolation(name, vrf.Table); err != nil {
			return nil, err
		}
	}
	return prepareL2Path(ctx, provider, vni, vpc.Name, string(vpc.UID), subnet.Name, string(subnet.UID), native, vrf.Table, reflectionMACs)
}

// Fence closes owned ingress without deleting or reconfiguring native veths.
func Fence(ctx context.Context, provider table.Provider, uid string) error {
	var interfaces []vswitch.Interface
	if err := provider.Table(&vswitch.Interface{}).Filter(ctx, func(i *vswitch.Interface) bool {
		owner := i.ExternalIDs[openperouter.OwnerKey+"-uid"]
		return owner != "" && (uid == "" || owner == uid)
	}, &interfaces); err != nil {
		return err
	}
	for _, iface := range interfaces {
		if err := closeAttachment(ctx, provider, iface); err != nil {
			return err
		}
	}
	return nil
}

func closeAttachment(ctx context.Context, provider table.Provider, iface vswitch.Interface) error {
	link, err := netlink.LinkByName(iface.Name)
	if err != nil && !errors.As(err, &netlink.LinkNotFoundError{}) {
		return err
	}
	if err == nil {
		if err := netlink.LinkSetDown(link); err != nil {
			return err
		}
	}
	if iface.Type == "" {
		port := &vswitch.Port{Name: iface.Name}
		err := provider.Table(port).Get(ctx, port)
		if err != nil && !errors.Is(err, table.ErrNotFound) {
			return err
		}
		if err == nil && port.ExternalIDs[openperouter.OwnerKey+"-uid"] != iface.ExternalIDs[openperouter.OwnerKey+"-uid"] {
			return fmt.Errorf("refuse to close foreign OVS port %s", iface.Name)
		}
		return ovs.DeleteVswitchPort(ctx, provider, iface.Name)
	}
	return nil
}

// removeAttachment is final deletion, unlike Fence which keeps internal ports
// available for an ordinary retry. Both OVS rows must bear the same owner UID.
func removeAttachment(ctx context.Context, provider table.Provider, iface vswitch.Interface) error {
	port := &vswitch.Port{Name: iface.Name}
	err := provider.Table(port).Get(ctx, port)
	if err != nil && !errors.Is(err, table.ErrNotFound) {
		return err
	}
	if err == nil && port.ExternalIDs[openperouter.OwnerKey+"-uid"] != iface.ExternalIDs[openperouter.OwnerKey+"-uid"] {
		return fmt.Errorf("refuse to delete foreign OVS port %s", iface.Name)
	}
	if err := closeLink(iface.Name); err != nil {
		return err
	}
	if err := ReleaseIsolation(iface.Name); err != nil {
		return err
	}
	if err := ovs.DeleteVswitchPort(ctx, provider, iface.Name); err != nil {
		return err
	}
	// Recover an interrupted deletion which already removed the Port row.
	return provider.Table(&iface).Delete(ctx, "openperouter-interface-cleanup", &iface)
}

func closeLink(name string) error {
	link, err := netlink.LinkByName(name)
	if errors.As(err, &netlink.LinkNotFoundError{}) {
		return nil
	}
	if err != nil {
		return err
	}
	return netlink.LinkSetDown(link)
}
