package host

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/kubeovn/kube-ovn/pkg/openperouter"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/table"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/vswitch"
)

// Cleanup removes only objects bearing this integration's owner UID. Native
// veths are detached, never deleted. Names alone do not confer ownership.
func Cleanup(ctx context.Context, provider table.Provider, vpcUIDs, subnetUIDs map[string]bool) error {
	defer openperouter.Measure(ctx, "host.cleanup")()
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
		if err := removeAttachment(ctx, provider, iface); err != nil {
			return fmt.Errorf("remove host attachment %s: %w", iface.Name, err)
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
			if err := EnsureIsolation(slave.Attrs().Name, vrf.Table); err != nil {
				return err
			}
			if err := closeAndDetachVRFSlave(
				func() error { return netlink.LinkSetDown(slave) },
				func() error { return netlink.LinkSetNoMaster(slave) },
			); err != nil {
				return fmt.Errorf("close/detach VRF slave %s: %w", slave.Attrs().Name, err)
			}
		}
		if err := netlink.LinkDel(vrf); err != nil {
			return fmt.Errorf("delete host VRF %s: %w", vrf.Name, err)
		}
	}
	if err := cleanupL2Paths(vpcUIDs, subnetUIDs); err != nil {
		return err
	}
	return cleanupReflection(ctx, subnetUIDs)
}

// OVS removes internal devices asynchronously after the Port transaction. A
// slave from LinkList can already be gone; its terminal rules were installed
// before this call. Absence proves closure, other errors do not authorize detach.
func closeAndDetachVRFSlave(closePath, detachPath func() error) error {
	if err := closePath(); err != nil {
		if errors.Is(err, unix.ENODEV) {
			return nil
		}
		return err
	}
	if err := detachPath(); err != nil && !errors.Is(err, unix.ENODEV) {
		return err
	}
	return nil
}
