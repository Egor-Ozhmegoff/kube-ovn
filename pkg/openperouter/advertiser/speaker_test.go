package advertiser

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAvailabilityIsNotRestoredAtBootOrRetainedWithoutAPI(t *testing.T) {
	var lease Lease
	now := time.Now()
	require.Empty(t, lease.Desired(now, 30*time.Second, true))
	prefix := netip.MustParsePrefix("10.101.3.11/32")
	observed := map[netip.Prefix]struct{}{prefix: {}}
	lease.Refresh(now, observed)
	delete(observed, prefix)
	require.Len(t, lease.Desired(now.Add(29*time.Second), 30*time.Second, true), 1)
	require.Empty(t, lease.Desired(now.Add(30*time.Second), 30*time.Second, true))
	require.Empty(t, lease.Desired(now, 30*time.Second, false), "lost attachment withdraws without waiting for API timeout")
	require.Empty(t, lease.Desired(now.Add(-time.Second), 30*time.Second, true))
	lease.Refresh(now.Add(time.Minute), nil)
	require.Empty(t, lease.Desired(now.Add(time.Minute), 30*time.Second, true))
}

func TestHostPathsDoNotAdvertiseCachedAggregatesOrDefault(t *testing.T) {
	for _, pair := range [][2]string{{"10.101.3.11/32", "169.254.16.13"}, {"fd00:3::11/128", "fd00:16::d"}} {
		path, err := makePath(netip.MustParsePrefix(pair[0]), netip.MustParseAddr(pair[1]))
		require.NoError(t, err)
		require.NotNil(t, path.Nlri)
	}
	for _, prefix := range []string{"10.101.3.0/24", "0.0.0.0/0", "fd00:3::/64"} {
		_, err := makePath(netip.MustParsePrefix(prefix), netip.MustParseAddr("169.254.16.13"))
		require.Error(t, err)
	}
}
