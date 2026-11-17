package openperouter

import (
	"net"
	"slices"
	"sync"
	"time"
)

// MACGuard remembers recently allocated identities long enough to reject BUM
// reflected by the fabric after deletion. This configuration cache is never
// used as workload availability or as a source of BGP advertisements.
type MACGuard struct {
	mu       sync.Mutex
	networks map[string]map[string]time.Time
}

func (g *MACGuard) Update(network string, existing, observed []string, now time.Time) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.networks == nil {
		g.networks = make(map[string]map[string]time.Time)
	}
	entries, initialized := g.networks[network]
	if !initialized {
		entries = make(map[string]time.Time)
		g.networks[network] = entries
		// Seed once from OVN on controller restart; an empty local process
		// cache must not remove the existing reflection fence.
		observed = append(slices.Clone(observed), existing...)
	}
	for _, value := range observed {
		if mac, err := net.ParseMAC(value); err == nil && len(mac) == 6 && mac[0]&1 == 0 {
			entries[mac.String()] = now.Add(10 * time.Minute)
		}
	}
	result := make([]string, 0, len(entries))
	for mac, deadline := range entries {
		if !now.Before(deadline) {
			delete(entries, mac)
			continue
		}
		result = append(result, mac)
	}
	slices.Sort(result)
	return result
}

func (g *MACGuard) Forget(network string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.networks, network)
}
