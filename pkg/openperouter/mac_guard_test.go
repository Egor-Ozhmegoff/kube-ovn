package openperouter

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMACGuardRetainsReflectionFenceAcrossAllocationDeletionAndRestart(t *testing.T) {
	var guard MACGuard
	now := time.Now()
	oldMAC, newMAC := "02:00:00:00:00:11", "02:00:00:00:00:12"
	set := guard.Update("network", []string{oldMAC}, []string{newMAC}, now)
	require.ElementsMatch(t, []string{oldMAC, newMAC}, set)
	// The preparing MAC is protected before the LSP exists. A concurrent
	// empty snapshot cannot reopen a reflection window during allocation.
	require.ElementsMatch(t, set, guard.Update("network", set, nil, now.Add(time.Minute)))
	set = guard.Update("network", set, []string{newMAC}, now.Add(9*time.Minute))
	require.ElementsMatch(t, []string{oldMAC, newMAC}, set)
	set = guard.Update("network", set, []string{newMAC}, now.Add(10*time.Minute))
	require.Equal(t, []string{newMAC}, set)
	var restarted MACGuard
	require.Equal(t, set, restarted.Update("network", set, nil, now.Add(11*time.Minute)))
	require.Empty(t, restarted.Update("network", set, nil, now.Add(21*time.Minute)))
}
