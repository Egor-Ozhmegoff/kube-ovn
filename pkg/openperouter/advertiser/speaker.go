// Package advertiser owns only host-side unicast BGP sessions. External EVPN
// and the router namespace remain entirely under OpenPERouter control.
package advertiser

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"time"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	gobgp "github.com/osrg/gobgp/v4/pkg/server"

	"github.com/kubeovn/kube-ovn/pkg/openperouter"
)

type Session struct {
	Local, Peer netip.Addr
}

type Speaker struct {
	server   *gobgp.BgpServer
	nextHops map[bool]netip.Addr
	paths    map[netip.Prefix]*apiutil.Path
}

func New(ctx context.Context, vrf string, localASN, peerASN uint32, routerID string, sessions []Session) (*Speaker, error) {
	s := &Speaker{server: gobgp.NewBgpServer(), nextHops: make(map[bool]netip.Addr), paths: make(map[netip.Prefix]*apiutil.Path)}
	go s.server.Serve()
	if err := s.server.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{Asn: localASN, RouterId: routerID, ListenPort: -1, BindToDevice: vrf}}); err != nil {
		s.server.Stop()
		return nil, err
	}
	for _, session := range sessions {
		if !session.Local.IsValid() || !session.Peer.IsValid() || session.Local.Is4() != session.Peer.Is4() {
			s.server.Stop()
			return nil, fmt.Errorf("invalid host BGP session %v", session)
		}
		afi := api.Family_AFI_IP
		if session.Local.Is6() {
			afi = api.Family_AFI_IP6
		}
		peer := &api.Peer{
			Conf:        &api.PeerConf{NeighborAddress: session.Peer.String(), PeerAsn: peerASN},
			Transport:   &api.Transport{LocalAddress: session.Local.String(), BindInterface: vrf},
			Timers:      &api.Timers{Config: &api.TimersConfig{HoldTime: 9, KeepaliveInterval: 3}},
			AfiSafis:    []*api.AfiSafi{{Config: &api.AfiSafiConfig{Family: &api.Family{Afi: afi, Safi: api.Family_SAFI_UNICAST}, Enabled: true}}},
			ApplyPolicy: &api.ApplyPolicy{ImportPolicy: &api.PolicyAssignment{DefaultAction: api.RouteAction_ROUTE_ACTION_REJECT}},
		}
		if err := s.server.AddPeer(ctx, &api.AddPeerRequest{Peer: peer}); err != nil {
			s.server.Stop()
			return nil, err
		}
		s.nextHops[session.Local.Is4()] = session.Local
	}
	return s, nil
}

func (s *Speaker) Close() { s.server.Stop() }

// Ready proves host-to-PE control-plane connectivity, not the presence of an
// external fabric route. Fabric withdrawal remains local to the native PE.
func (s *Speaker) Ready(ctx context.Context) (bool, error) {
	defer openperouter.Measure(ctx, "bgp.ready")()
	established := 0
	err := s.server.ListPeer(ctx, &api.ListPeerRequest{}, func(peer *api.Peer) {
		if peer.GetState().GetSessionState() == api.PeerState_SESSION_STATE_ESTABLISHED {
			established++
		}
	})
	return len(s.nextHops) != 0 && established == len(s.nextHops), err
}

// Sync withdraws before announcing, so loss of availability never waits behind
// a batch of additions. No routes are restored from a configuration cache.
func (s *Speaker) Sync(prefixes map[netip.Prefix]struct{}) error {
	for prefix, path := range s.paths {
		if _, keep := prefixes[prefix]; keep {
			continue
		}
		if err := s.server.DeletePath(apiutil.DeletePathRequest{Paths: []*apiutil.Path{path}}); err != nil {
			return err
		}
		delete(s.paths, prefix)
	}
	for prefix := range prefixes {
		if _, exists := s.paths[prefix]; exists {
			continue
		}
		path, err := makePath(prefix, s.nextHops[prefix.Addr().Is4()])
		if err != nil {
			return err
		}
		results, err := s.server.AddPath(apiutil.AddPathRequest{Paths: []*apiutil.Path{path}})
		if err != nil {
			return err
		}
		for _, result := range results {
			if result.Error != nil {
				return result.Error
			}
		}
		s.paths[prefix] = path
	}
	return nil
}

// Lease contains availability observations from this process lifetime only.
// Successful API reads renew it; stored VRF/VNI definitions never do.
type Lease struct {
	refreshed time.Time
	prefixes  map[netip.Prefix]struct{}
}

func (l *Lease) Refresh(now time.Time, prefixes map[netip.Prefix]struct{}) {
	l.refreshed, l.prefixes = now, maps.Clone(prefixes)
}

func (l *Lease) Desired(now time.Time, maxAge time.Duration, attached bool) map[netip.Prefix]struct{} {
	if !attached || l.refreshed.IsZero() || now.Before(l.refreshed) || now.Sub(l.refreshed) >= maxAge {
		return nil
	}
	return maps.Clone(l.prefixes)
}

func makePath(prefix netip.Prefix, nextHop netip.Addr) (*apiutil.Path, error) {
	if !prefix.IsValid() || !nextHop.IsValid() || prefix.Addr().Is4() != nextHop.Is4() || prefix.Bits() != prefix.Addr().BitLen() {
		return nil, fmt.Errorf("only host routes with a matching local next hop may be advertised: %s via %s", prefix, nextHop)
	}
	nlri, err := bgp.NewIPAddrPrefix(prefix)
	if err != nil {
		return nil, err
	}
	family := bgp.RF_IPv4_UC
	attrs := []bgp.PathAttributeInterface{bgp.NewPathAttributeOrigin(0)}
	if prefix.Addr().Is4() {
		nh, err := bgp.NewPathAttributeNextHop(nextHop)
		if err != nil {
			return nil, err
		}
		attrs = append(attrs, nh)
	} else {
		family = bgp.RF_IPv6_UC
		nh, err := bgp.NewPathAttributeMpReachNLRI(family, []bgp.PathNLRI{{NLRI: nlri}}, nextHop)
		if err != nil {
			return nil, err
		}
		attrs = append(attrs, nh)
	}
	return &apiutil.Path{Family: family, Nlri: nlri, Attrs: attrs}, nil
}
