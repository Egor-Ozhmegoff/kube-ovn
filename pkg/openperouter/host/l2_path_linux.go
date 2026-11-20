package host

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/vishvananda/netlink"

	"github.com/kubeovn/kube-ovn/pkg/openperouter"
	"github.com/kubeovn/kube-ovn/pkg/ovs"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/table"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/vswitch"
)

const l2AliasPrefix = "kube-ovn/openperouter/l2/"

type L2Attachment struct{ Guard, Peer, Native netlink.Link }

func L2Names(vni int32) (string, string, string) {
	return fmt.Sprintf("oplb%x", vni), fmt.Sprintf("oplf%x", vni), fmt.Sprintf("oplp%x", vni)
}

// Open is called only after the session/revision acknowledgement commits.
// Only Kube-OVN creates or raises Guard; native veth recreation cannot bypass it.
func (a *L2Attachment) Open() error {
	if err := finishIsolation(a.Native.Attrs().Name); err != nil {
		return err
	}
	for _, link := range []netlink.Link{a.Peer, a.Native, a.Guard} {
		if err := netlink.LinkSetUp(link); err != nil {
			return err
		}
	}
	return nil
}

func CloseL2(vni int32, vpcUID, subnetUID string) error {
	_, guard, _ := L2Names(vni)
	link, err := netlink.LinkByName(guard)
	if errors.As(err, &netlink.LinkNotFoundError{}) {
		return nil
	}
	if err != nil {
		return err
	}
	if link.Attrs().Alias != l2AliasPrefix+vpcUID+"/"+subnetUID {
		return fmt.Errorf("foreign L2 guard %s", guard)
	}
	return netlink.LinkSetDown(link)
}

func prepareL2Path(ctx context.Context, provider table.Provider, vni int32, vpcName, vpcUID, subnetName, subnetUID string, native netlink.Link, tableID uint32, macs []string) (*L2Attachment, error) {
	bridgeName, guardName, peerName := L2Names(vni)
	alias := l2AliasPrefix + vpcUID + "/" + subnetUID
	bridge, err := ensureL2Bridge(bridgeName, alias)
	if err != nil {
		return nil, err
	}
	guard, peer, err := ensureL2Pair(guardName, peerName, alias, native.Attrs().MTU)
	if err != nil {
		return nil, err
	}
	for _, link := range []netlink.Link{native, peer} {
		if link.Attrs().MasterIndex != 0 && link.Attrs().MasterIndex != bridge.Attrs().Index {
			return nil, fmt.Errorf("L2 interface %s belongs to another master", link.Attrs().Name)
		}
	}
	for _, name := range []string{native.Attrs().Name, guardName, bridgeName} {
		if err := EnsureIsolation(name, tableID); err != nil {
			return nil, err
		}
	}
	if err := EnsureReflection(ctx, subnetUID, guardName, macs); err != nil {
		return nil, err
	}
	for _, link := range []netlink.Link{native, peer} {
		if err := netlink.LinkSetMaster(link, bridge); err != nil {
			return nil, err
		}
	}
	if err := netlink.LinkSetUp(bridge); err != nil {
		return nil, err
	}
	ids := map[string]string{openperouter.OwnerKey: vpcName, openperouter.OwnerKey + "-uid": vpcUID, "subnet-uid": subnetUID, "native-interface": native.Attrs().Name}
	iface := &vswitch.Interface{Name: guardName, ExternalIDs: map[string]string{"iface-id": openperouter.L2Port(subnetName), openperouter.OwnerKey: vpcName, openperouter.OwnerKey + "-uid": vpcUID, "subnet-uid": subnetUID, "native-interface": native.Attrs().Name}}
	if err := ovs.EnsureVswitchPort(ctx, provider, ovs.VswitchPortConfig{BridgeName: "br-int", Port: &vswitch.Port{Name: guardName, ExternalIDs: ids}, Interface: iface, InterfaceFields: []any{&iface.ExternalIDs}}); err != nil {
		return nil, err
	}
	return &L2Attachment{Guard: guard, Peer: peer, Native: native}, nil
}

func cleanupL2Paths(vpcs, subnets map[string]bool) error {
	links, err := netlink.LinkList()
	if err != nil {
		return err
	}
	for _, bridge := range links {
		if bridge.Type() != "bridge" || !strings.HasPrefix(bridge.Attrs().Alias, l2AliasPrefix) {
			continue
		}
		ids := strings.Split(strings.TrimPrefix(bridge.Attrs().Alias, l2AliasPrefix), "/")
		if len(ids) != 2 {
			return fmt.Errorf("invalid L2 bridge owner %s", bridge.Attrs().Name)
		}
		if vpcs[ids[0]] && subnets[ids[1]] {
			continue
		}
		for _, link := range links {
			if link.Attrs().Alias == bridge.Attrs().Alias {
				if err := netlink.LinkSetDown(link); err != nil {
					return err
				}
				if err := ReleaseIsolation(link.Attrs().Name); err != nil {
					return err
				}
			}
		}
		for _, link := range links {
			if link.Attrs().MasterIndex != bridge.Attrs().Index {
				continue
			}
			owned := link.Attrs().Alias == bridge.Attrs().Alias
			if !owned && (link.Type() != "veth" || !strings.HasPrefix(link.Attrs().Name, "host-e-")) {
				return fmt.Errorf("foreign L2 bridge member %s", link.Attrs().Name)
			}
			if err := netlink.LinkSetDown(link); err != nil {
				return err
			}
			if err := ReleaseIsolation(link.Attrs().Name); err != nil {
				return err
			}
			if err := netlink.LinkSetNoMaster(link); err != nil {
				return err
			}
		}
		if err := ReleaseIsolation(bridge.Attrs().Name); err != nil {
			return err
		}
		if err := netlink.LinkDel(bridge); err != nil {
			return err
		}
	}

	links, err = netlink.LinkList()
	if err != nil {
		return err
	}
	for _, link := range links {
		if link.Type() != "veth" || !strings.HasPrefix(link.Attrs().Alias, l2AliasPrefix) {
			continue
		}
		ids := strings.Split(strings.TrimPrefix(link.Attrs().Alias, l2AliasPrefix), "/")
		if len(ids) != 2 {
			return errors.New("invalid L2 pair owner")
		}
		if vpcs[ids[0]] && subnets[ids[1]] {
			continue
		}
		current, err := netlink.LinkByName(link.Attrs().Name)
		if errors.As(err, &netlink.LinkNotFoundError{}) {
			continue
		}
		if err != nil {
			return err
		}
		if err := netlink.LinkSetDown(current); err != nil {
			return err
		}
		if err := ReleaseIsolation(current.Attrs().Name); err != nil {
			return err
		}
		if err := netlink.LinkDel(current); err != nil {
			return err
		}
	}
	return nil
}

func ensureL2Bridge(bridgeName, alias string) (netlink.Link, error) {
	bridge, err := netlink.LinkByName(bridgeName)
	if errors.As(err, &netlink.LinkNotFoundError{}) {
		bridge = &netlink.Bridge{Name: bridgeName, Alias: alias}
		if err := netlink.LinkAdd(bridge); err != nil {
			return nil, err
		}
		if err := netlink.LinkSetAlias(bridge, alias); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if bridge.Type() != "bridge" || bridge.Attrs().Alias != alias {
		return nil, fmt.Errorf("foreign L2 bridge %s", bridgeName)
	}
	return bridge, nil
}

func ensureL2Pair(guardName, peerName, alias string, mtu int) (netlink.Link, netlink.Link, error) {
	guard, err := netlink.LinkByName(guardName)
	if errors.As(err, &netlink.LinkNotFoundError{}) {
		guard = &netlink.Veth{Name: guardName, Alias: alias, MTU: mtu, PeerName: peerName}
		if err := netlink.LinkAdd(guard); err != nil {
			return nil, nil, err
		}
		if err := netlink.LinkSetAlias(guard, alias); err != nil {
			return nil, nil, err
		}
		peer, err := netlink.LinkByName(peerName)
		if err != nil {
			return nil, nil, err
		}
		if err := netlink.LinkSetAlias(peer, alias); err != nil {
			return nil, nil, err
		}
	} else if err != nil {
		return nil, nil, err
	}
	if guard.Type() != "veth" || guard.Attrs().Alias != alias {
		return nil, nil, fmt.Errorf("foreign L2 guard %s", guardName)
	}
	peer, err := netlink.LinkByName(peerName)
	if err != nil {
		return nil, nil, err
	}
	if peer.Type() == "veth" && peer.Attrs().Alias == "" {
		index, err := netlink.VethPeerIndex(guard.(*netlink.Veth))
		if err != nil {
			return nil, nil, err
		}
		if index == peer.Attrs().Index {
			if err := netlink.LinkSetAlias(peer, alias); err != nil {
				return nil, nil, err
			}
			peer.Attrs().Alias = alias
		}
	}
	if peer.Type() != "veth" || peer.Attrs().Alias != alias {
		return nil, nil, fmt.Errorf("foreign L2 peer %s", peerName)
	}
	return guard, peer, nil
}
