package routes

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func subnetRoutes(t *testing.T, table uint32, uid string, prefixes ...string) []Route {
	t.Helper()
	set := make(map[netip.Prefix]struct{})
	for _, p := range prefixes {
		set[netip.MustParsePrefix(p)] = struct{}{}
	}
	result, err := Build(table, 19, uid, []netip.Addr{netip.MustParseAddr("169.254.16.13"), netip.MustParseAddr("fd00:16::d")}, set)
	require.NoError(t, err)
	return result
}

func TestSubnetPlanOwnershipRecreationAndDefaults(t *testing.T) {
	old := subnetRoutes(t, 23001, "a", "10.1.1.0/24", "fd00:1::/64")
	desired := subnetRoutes(t, 23009, "a", "10.1.1.0/24", "fd00:1::/64")
	other := subnetRoutes(t, 23002, "b", "10.2.0.0/16", "fd00:2::/64")
	foreign := append(other, Route{Table: 23001, Prefix: netip.MustParsePrefix("0.0.0.0/0"), Protocol: 203, Type: 7}, Route{Table: 23001, Prefix: netip.MustParsePrefix("::/0"), Protocol: 186, Type: 7}, Route{Table: 23009, Prefix: netip.MustParsePrefix("169.254.16.0/20"), Protocol: 2, Type: 1})
	diff, err := Plan(append(old, foreign...), desired, OwnerID("a"))
	require.NoError(t, err)
	require.ElementsMatch(t, old, diff.Delete)
	require.ElementsMatch(t, desired, diff.Add)
	stable, err := Plan(append(desired, foreign...), desired, OwnerID("a"))
	require.NoError(t, err)
	require.Empty(t, stable)
	for _, protocol := range []int{2, 186, 202, 201, 203} {
		conflict := desired[0]
		conflict.Protocol = protocol
		conflict.Realm = 0
		_, err = Plan([]Route{conflict}, desired, OwnerID("a"))
		require.ErrorContains(t, err, "foreign route conflicts")
	}
	conflict := desired[0]
	conflict.Metric = OwnerID("b")
	conflict.Realm = conflict.Metric
	_, err = Plan([]Route{conflict}, desired, OwnerID("a"))
	require.ErrorContains(t, err, "foreign route conflicts")
}

func TestSubnetBuildRejectsInvalidIntent(t *testing.T) {
	for _, table := range []uint32{0, 253, 254, 255} {
		_, err := Build(table, 19, "a", nil, nil)
		require.Error(t, err)
	}
	for _, prefix := range []string{"0.0.0.0/0", "::/0", "10.1.1.3/24", "fe80::/64", "ff02::/16"} {
		_, err := Build(23001, 19, "a", []netip.Addr{netip.MustParseAddr("169.254.1.1")}, map[netip.Prefix]struct{}{netip.MustParsePrefix(prefix): {}})
		require.Error(t, err)
	}
	_, err := Build(23001, 19, "a", nil, map[netip.Prefix]struct{}{netip.MustParsePrefix("10.1.1.0/24"): {}})
	require.Error(t, err)
}

func TestSubnetSteadyStatePartialFailureAndCancellation(t *testing.T) {
	var desired []Route
	for subnet := range 26 {
		desired = append(desired, subnetRoutes(t, 23001, "a", fmt.Sprintf("10.1.%d.0/24", subnet))...)
	}
	var actual []Route
	diff, err := Plan(actual, desired, OwnerID("a"))
	require.NoError(t, err)
	calls := 0
	failure := errors.New("injected netlink failure")
	err = applyDiff(t.Context(), diff, func(add bool, r Route) error {
		calls++
		if calls == 17 {
			return failure
		}
		require.True(t, add)
		actual = append(actual, r)
		return nil
	})
	require.ErrorIs(t, err, failure)
	require.Len(t, actual, 16)
	diff, err = Plan(actual, desired, OwnerID("a"))
	require.NoError(t, err)
	require.Len(t, diff.Add, 10)
	require.NoError(t, applyDiff(t.Context(), diff, func(_ bool, r Route) error { actual = append(actual, r); return nil }))
	diff, err = Plan(actual, desired, OwnerID("a"))
	require.NoError(t, err)
	require.Empty(t, diff)
	diff, err = Plan(actual, desired[1:], OwnerID("a"))
	require.NoError(t, err)
	require.Len(t, diff.Delete, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, applyDiff(ctx, diff, func(bool, Route) error { t.Fatal("must not mutate after cancellation"); return nil }), context.Canceled)
}

func TestLegacyCleanupSignature(t *testing.T) {
	for _, prefix := range []string{"10.1.1.11/32", "fd00:1::b/128"} {
		p := netip.MustParsePrefix(prefix)
		gw := netip.MustParseAddr("169.254.16.13")
		realm := 0x4b000001
		if p.Addr().Is6() {
			gw = netip.MustParseAddr("fd00:16::d")
			realm = 0
		}
		old := Route{Table: 23001, Prefix: p, Gateway: gw, LinkIndex: 19, Protocol: 201, Metric: 42760, Realm: realm, Type: 1}
		require.True(t, legacyOwned(old))
		require.False(t, Owned(old))
		for _, mutate := range []func(*Route){func(r *Route) { r.Metric++ }, func(r *Route) { r.Protocol = 202 }, func(r *Route) { r.Type = 7 }, func(r *Route) { r.Table = 254 }, func(r *Route) { r.Multipath = true }} {
			foreign := old
			mutate(&foreign)
			require.False(t, legacyOwned(foreign))
		}
	}
}
