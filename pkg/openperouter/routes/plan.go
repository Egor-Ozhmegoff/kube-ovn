// Package routes owns only explicitly marked OVN subnet routes in PE tenant
// tables. It never manages FRR, devices, connected routes or host protocol 202.
package routes

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"net/netip"
	"slices"
)

const (
	Protocol  = 203
	ownerMark = 0x4c000000
)

type Route struct {
	Table                                           uint32
	Prefix                                          netip.Prefix
	Gateway                                         netip.Addr
	LinkIndex, Protocol, Metric, Realm, Type, Scope int
	Multipath                                       bool
}

type Diff struct{ Delete, Add []Route }

// OwnerID persists the VPC scope in the metric for both IP families. IPv6
// kernels do not retain route realms. The namespace reserves this signature;
// hash collisions among live VPCs are rejected by Sweep before reconciliation.
func OwnerID(uid string) int { return ownerMark | int(crc32.ChecksumIEEE([]byte(uid))&0xffffff) }

func Owned(r Route) bool {
	return r.Protocol == Protocol && r.Metric&0xff000000 == ownerMark &&
		((r.Prefix.Addr().Is4() && r.Realm == r.Metric) || (r.Prefix.Addr().Is6() && r.Realm == 0)) &&
		routedPrefix(r.Prefix) && r.Type == 1 && r.Scope == 0 && !r.Multipath && tenantTable(r.Table)
}

// legacyOwned is only a migration cleanup predicate. It cannot create or renew
// endpoint routes. Protocol 201/metric 42760 was reserved by the retired owner.
func legacyOwned(r Route) bool {
	return r.Protocol == 201 && r.Metric == 42760 &&
		((r.Prefix.Addr().Is4() && r.Realm&0xff000000 == 0x4b000000) || (r.Prefix.Addr().Is6() && r.Realm == 0)) &&
		routedPrefix(r.Prefix) && r.Prefix.Bits() == r.Prefix.Addr().BitLen() &&
		r.Type == 1 && r.Scope == 0 && !r.Multipath && tenantTable(r.Table) && r.LinkIndex > 0 &&
		r.Gateway.IsValid() && r.Gateway.Is4() == r.Prefix.Addr().Is4()
}

func Build(table uint32, linkIndex int, uid string, gateways []netip.Addr, prefixes map[netip.Prefix]struct{}) ([]Route, error) {
	if !tenantTable(table) || linkIndex <= 0 || uid == "" {
		return nil, errors.New("real tenant table, interface and VPC UID are required")
	}
	var result []Route
	for prefix := range prefixes {
		if !routedPrefix(prefix) {
			return nil, fmt.Errorf("only masked non-default unicast subnet prefixes are allowed: %s", prefix)
		}
		gateway := netip.Addr{}
		for _, candidate := range gateways {
			if candidate.IsValid() && candidate.Is4() == prefix.Addr().Is4() {
				gateway = candidate
				break
			}
		}
		if !gateway.IsValid() || gateway.IsUnspecified() || gateway.IsMulticast() {
			return nil, fmt.Errorf("no local attachment next hop for %s", prefix)
		}
		realm := 0
		if prefix.Addr().Is4() {
			realm = OwnerID(uid)
		} // IPv6 FIB does not retain RTA_FLOW.
		result = append(result, Route{Table: table, Prefix: prefix, Gateway: gateway, LinkIndex: linkIndex, Protocol: Protocol, Metric: OwnerID(uid), Realm: realm, Type: 1})
	}
	slices.SortFunc(result, func(a, b Route) int { return a.Prefix.Addr().Compare(b.Prefix.Addr()) })
	return result, nil
}

// Plan validates the whole desired set before allowing mutations. Exact-prefix
// conflicts with foreign routes are rejected even at a different metric.
func Plan(actual, desired []Route, owner int) (Diff, error) {
	var diff Diff
	wanted, present := make(map[Route]bool, len(desired)), make(map[Route]bool, len(actual))
	prefixes := make(map[routeKey]bool, len(desired))
	for _, want := range desired {
		if !Owned(want) || want.Metric != owner || !want.Gateway.IsValid() || want.Gateway.IsUnspecified() || want.Gateway.IsMulticast() || want.Gateway.Is4() != want.Prefix.Addr().Is4() || want.LinkIndex <= 0 {
			return Diff{}, errors.New("invalid managed subnet route")
		}
		key := routeKey{want.Table, want.Prefix}
		if prefixes[key] {
			return Diff{}, fmt.Errorf("duplicate desired route %s in table %d", want.Prefix, want.Table)
		}
		prefixes[key], wanted[want] = true, true
	}
	for _, existing := range actual {
		if prefixes[routeKey{existing.Table, existing.Prefix}] && (!Owned(existing) || existing.Metric != owner) {
			return Diff{}, fmt.Errorf("foreign route conflicts with %s in table %d (protocol %d)", existing.Prefix, existing.Table, existing.Protocol)
		}
		present[existing] = true
		if Owned(existing) && existing.Metric == owner && !wanted[existing] {
			diff.Delete = append(diff.Delete, existing)
		}
	}
	for _, want := range desired {
		if !present[want] {
			diff.Add = append(diff.Add, want)
		}
	}
	return diff, nil
}

type routeKey struct {
	table  uint32
	prefix netip.Prefix
}

func tenantTable(table uint32) bool {
	return table != 0 && table != 253 && table != 254 && table != 255
}

func routedPrefix(p netip.Prefix) bool {
	return p.IsValid() && !p.Addr().Is4In6() && p == p.Masked() && p.Bits() > 0 && p.Addr().IsGlobalUnicast()
}

// applyDiff stops on the first error; the next reconcile reads actual kernel
// state again, rather than assuming the rest of a partial batch succeeded.
func applyDiff(ctx context.Context, diff Diff, mutate func(bool, Route) error) error {
	for _, batch := range []struct {
		add    bool
		routes []Route
	}{{false, diff.Delete}, {true, diff.Add}} {
		for _, r := range batch.routes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := mutate(batch.add, r); err != nil {
				return fmt.Errorf("PE subnet route %s (add=%t): %w", r.Prefix, batch.add, err)
			}
		}
	}
	return nil
}
