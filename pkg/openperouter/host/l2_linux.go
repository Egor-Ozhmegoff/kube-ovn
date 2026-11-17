package host

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/vishvananda/netlink"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/openperouter"
	"github.com/kubeovn/kube-ovn/pkg/ovs"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/table"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/vswitch"
)

func EnsureL2(ctx context.Context, provider table.Provider, vpc *kubeovnv1.Vpc, subnet *kubeovnv1.Subnet, bootstrap *openperouter.Bootstrap, defaultMTU int) error {
	if err := bootstrap.ValidateL2(vpc.Spec.OpenPERouter, subnet); err != nil {
		return err
	}
	name := fmt.Sprintf("host-e-%d", subnet.Spec.OpenPERouterL2.VNI)
	link, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	mtu := defaultMTU
	if subnet.Spec.Mtu != 0 {
		mtu = int(subnet.Spec.Mtu)
	}
	if link.Type() != "veth" || link.Attrs().Flags&net.FlagUp == 0 || mtu > link.Attrs().MTU {
		return fmt.Errorf("native L2 interface %s is not ready or workload MTU %d exceeds %d", name, mtu, link.Attrs().MTU)
	}
	iface := &vswitch.Interface{Name: name}
	err = provider.Table(iface).Get(ctx, iface)
	if err != nil && !errors.Is(err, table.ErrNotFound) {
		return err
	}
	if err == nil && iface.ExternalIDs[openperouter.OwnerKey+"-uid"] != string(vpc.UID) {
		return fmt.Errorf("refuse to adopt OVS L2 interface %s", name)
	}
	port := &vswitch.Port{Name: name}
	portErr := provider.Table(port).Get(ctx, port)
	if portErr != nil && !errors.Is(portErr, table.ErrNotFound) {
		return portErr
	}
	if portErr == nil && port.ExternalIDs[openperouter.OwnerKey+"-uid"] != string(vpc.UID) {
		return fmt.Errorf("refuse to adopt OVS L2 port %s", name)
	}
	if errors.Is(err, table.ErrNotFound) && link.Attrs().MasterIndex != 0 {
		return fmt.Errorf("native L2 host interface %s is already attached", name)
	}
	ids := map[string]string{openperouter.OwnerKey: vpc.Name, openperouter.OwnerKey + "-uid": string(vpc.UID), "subnet-uid": string(subnet.UID)}
	desired := &vswitch.Interface{Name: name, ExternalIDs: map[string]string{
		"iface-id": openperouter.L2Port(subnet.Name), openperouter.OwnerKey: vpc.Name,
		openperouter.OwnerKey + "-uid": string(vpc.UID), "subnet-uid": string(subnet.UID),
	}}
	return ovs.EnsureVswitchPort(ctx, provider, ovs.VswitchPortConfig{
		BridgeName: "br-int", Port: &vswitch.Port{Name: name, ExternalIDs: ids}, Interface: desired,
		InterfaceFields: []any{&desired.ExternalIDs},
	})
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
		if iface.Type == "" {
			if err := ovs.DeleteVswitchPort(ctx, provider, iface.Name); err != nil {
				return err
			}
			continue
		}
		link, err := netlink.LinkByName(iface.Name)
		if errors.As(err, &netlink.LinkNotFoundError{}) {
			continue
		}
		if err != nil {
			return err
		}
		if err := netlink.LinkSetDown(link); err != nil {
			return err
		}
	}
	return nil
}
