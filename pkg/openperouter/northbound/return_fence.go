package northbound

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// Keep family-specific field prerequisites inside their own branch. Negating
// a mixed IPv4/IPv6 allow expression can require incompatible prerequisites.
func returnFenceMatch(group string, cidrs []string) (string, error) {
	prefixes := map[bool][]string{}
	for _, cidr := range cidrs {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			return "", fmt.Errorf("invalid return destination %q: %w", cidr, err)
		}
		family := prefix.Addr().Is4()
		prefixes[family] = append(prefixes[family], prefix.Masked().String())
	}
	branches := make([]string, 0, 2)
	for _, ipv4 := range []bool{true, false} {
		family := "ip6"
		if ipv4 {
			family = "ip4"
		}
		values := prefixes[ipv4]
		slices.Sort(values)
		values = slices.Compact(values)
		match := family
		if len(values) != 0 {
			match += " && " + family + ".dst != {" + strings.Join(values, ", ") + "}"
		}
		branches = append(branches, "("+match+")")
	}
	return "inport == @" + group + " && (" + strings.Join(branches, " || ") + ")", nil
}
