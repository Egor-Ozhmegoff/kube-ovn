package routes

import (
	"context"
	"errors"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func (s *Session) actual(ctx context.Context) ([]Route, error) {
	if err := s.budget(ctx); err != nil {
		return nil, err
	}
	var result []Route
	wanted := make(map[routeKey]bool, len(s.desired))
	for _, r := range s.desired {
		wanted[routeKey{r.Table, r.Prefix}] = true
	}
	err := s.handle.RouteListFilteredIter(netlink.FAMILY_ALL, &netlink.Route{Table: unix.RT_TABLE_UNSPEC}, netlink.RT_FILTER_TABLE, func(r netlink.Route) bool {
		if ctx.Err() != nil {
			return false
		}
		route := fromNetlink(r)
		if s.removing != nil && s.removing(route) || s.removing == nil && ((Owned(route) && route.Metric == s.owner) || wanted[routeKey{route.Table, route.Prefix}]) {
			result = append(result, route)
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Session) apply(ctx context.Context, diff Diff) error {
	return applyDiff(ctx, diff, func(add bool, r Route) error {
		if err := s.budget(ctx); err != nil {
			return err
		}
		actual := toNetlink(r)
		if add {
			return s.handle.RouteAdd(&actual)
		} // EXCL, never REPLACE.
		err := s.handle.RouteDel(&actual)
		if errors.Is(err, unix.ESRCH) || errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	})
}

func fromNetlink(r netlink.Route) Route {
	p := netip.Prefix{}
	if r.Dst != nil {
		if addr, ok := netip.AddrFromSlice(r.Dst.IP); ok {
			ones, _ := r.Dst.Mask.Size()
			p = netip.PrefixFrom(addr.Unmap(), ones).Masked()
		}
	}
	gw, _ := netip.AddrFromSlice(r.Gw)
	table := uint32(r.Table) // #nosec G115 -- Linux routing table IDs are unsigned 32-bit values.
	return Route{Table: table, Prefix: p, Gateway: gw.Unmap(), LinkIndex: r.LinkIndex, Protocol: int(r.Protocol), Metric: r.Priority, Realm: r.Realm, Type: r.Type, Scope: int(r.Scope), Multipath: len(r.MultiPath) != 0}
}

func toNetlink(r Route) netlink.Route {
	// Only Build/Plan-validated managed routes reach mutations: universe scope (0).
	return netlink.Route{Table: int(r.Table), Dst: &net.IPNet{IP: net.ParseIP(r.Prefix.Addr().String()), Mask: net.CIDRMask(r.Prefix.Bits(), r.Prefix.Addr().BitLen())}, Gw: net.ParseIP(r.Gateway.String()), LinkIndex: r.LinkIndex, Protocol: netlink.RouteProtocol(r.Protocol), Priority: r.Metric, Realm: r.Realm, Type: r.Type, Scope: netlink.SCOPE_UNIVERSE}
}
