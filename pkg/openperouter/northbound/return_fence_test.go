package northbound

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReturnFenceSeparatesAddressFamilies(t *testing.T) {
	match, err := returnFenceMatch("returns", []string{"fd00:101:3::1/64", "10.101.3.11/24", "10.101.3.0/24"})
	require.NoError(t, err)
	require.Equal(t, "inport == @returns && ((ip4 && ip4.dst != {10.101.3.0/24}) || (ip6 && ip6.dst != {fd00:101:3::/64}))", match)
	match, err = returnFenceMatch("returns", nil)
	require.NoError(t, err)
	require.Equal(t, "inport == @returns && ((ip4) || (ip6))", match)
	_, err = returnFenceMatch("returns", []string{"invalid"})
	require.ErrorContains(t, err, "invalid return destination")
}
