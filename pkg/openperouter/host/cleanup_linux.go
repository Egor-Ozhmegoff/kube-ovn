package host

import (
	"context"
	"fmt"
	"strings"

	"github.com/vishvananda/netlink"

	"github.com/kubeovn/kube-ovn/pkg/openperouter"
	"github.com/kubeovn/kube-ovn/pkg/ovs"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/table"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/vswitch"
)

// Cleanup removes only objects bearing this integration's owner UID. Native
// veths are detached, never deleted. Names alone do not confer ownership.
func Cleanup(ctx context.Context, provider table.Provider, vpcUIDs, subnetUIDs map[string]bool) error {
	var ifaces []vswitch.Interface
	if err := provider.Table(&vswitch.Interface{}).Filter(ctx, func(i *vswitch.Interface) bool {
		return i.ExternalIDs[openperouter.OwnerKey+"-uid"] != ""
	}, &ifaces); err != nil {
		return err
	}
	owned := make(map[string]bool)
	for _, iface := range ifaces {
		uid := iface.ExternalIDs[openperouter.OwnerKey+"-uid"]
		owned[iface.Name] = true
		if vpcUIDs[uid] && (iface.ExternalIDs["subnet-uid"] == "" || subnetUIDs[iface.ExternalIDs["subnet-uid"]]) {
			continue
		}
		if err := ovs.DeleteVswitchPort(ctx, provider, iface.Name); err != nil {
			return err
		}
	}
	links, err := netlink.LinkList()
	if err != nil {
		return err
	}
	for _, link := range links {
		vrf, ok := link.(*netlink.Vrf)
		if !ok || !strings.HasPrefix(vrf.Alias, "kube-ovn/openperouter/") {
			continue
		}
		uid := strings.TrimPrefix(vrf.Alias, "kube-ovn/openperouter/")
		if vpcUIDs[uid] {
			continue
		}
		for _, slave := range links {
			if slave.Attrs().MasterIndex != vrf.Index {
				continue
			}
			if !owned[slave.Attrs().Name] && (slave.Type() != "veth" || !strings.HasPrefix(slave.Attrs().Name, "host-e-")) {
				return fmt.Errorf("refuse to remove VRF %s with foreign slave %s", vrf.Name, slave.Attrs().Name)
			}
			if err := netlink.LinkSetNoMaster(slave); err != nil {
				return err
			}
		}
		if err := netlink.LinkDel(vrf); err != nil {
			return err
		}
	}
	return nil
}
