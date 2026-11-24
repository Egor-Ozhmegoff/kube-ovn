package routes

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
)

// Namespace loss/replacement affects every tenant, unlike a missing VRF/veth.
var (
	ErrNamespaceUnavailable = errors.New("PE network namespace unavailable")
	ErrNamespaceChanged     = errors.New("PE namespace generation changed")
)

// CheckNamespace probes the common namespace without changing routes or links.
func (m Manager) CheckNamespace(ctx context.Context) error {
	session, err := m.connect(ctx)
	if err != nil {
		return err
	}
	session.handle.Close()
	return ctx.Err()
}

type Attachment struct {
	UID, VRF, Device             string
	HostIndex                    int
	HostAddresses, PeerAddresses []string
	Prefixes                     map[netip.Prefix]struct{}
}

type attachmentState struct {
	config              Attachment
	vrfIndex, linkIndex int
	table               uint32
}

type Manager struct{ Namespace string }

type Session struct {
	path     string
	identity os.FileInfo
	handle   *netlink.Handle
	states   map[string]attachmentState
	desired  []Route
	owner    int
	removing func(Route) bool
}

// Reconcile operates on one VPC, keeping every other owner's routes intact.
// It pins a netlink socket without changing the daemon thread's namespace.
func (m Manager) Reconcile(ctx context.Context, a Attachment, open func(*Session) error) error {
	s, err := m.connect(ctx)
	if err != nil {
		return err
	}
	defer s.handle.Close()
	s.owner = OwnerID(a.UID)
	state, err := s.inspect(ctx, a)
	if err != nil {
		return err
	}
	s.states[a.UID] = state
	var gateways []netip.Addr
	for _, address := range a.HostAddresses {
		prefix, err := netip.ParsePrefix(address)
		if err != nil {
			return err
		}
		gateways = append(gateways, prefix.Addr())
	}
	s.desired, err = Build(state.table, state.linkIndex, a.UID, gateways, a.Prefixes)
	if err != nil {
		return err
	}
	actual, err := s.actual(ctx)
	if err != nil {
		return err
	}
	diff, err := Plan(actual, s.desired, s.owner)
	if err != nil {
		return err
	}
	if err := s.apply(ctx, diff); err != nil {
		return err
	}
	if open != nil {
		return open(s)
	}
	return nil
}

// Sweep removes routes of deleted VPCs, without renewing configured subnets.
// A nil keep set is a complete withdrawal (startup/global failure/shutdown).
func (m Manager) Sweep(ctx context.Context, keepUIDs []string) error {
	keep := make(map[int]string)
	for _, uid := range keepUIDs {
		owner := OwnerID(uid)
		if other, exists := keep[owner]; exists && other != uid {
			return fmt.Errorf("subnet route owner collision between VPC UIDs %s and %s", uid, other)
		}
		keep[owner] = uid
	}
	return m.remove(ctx, func(r Route) bool { _, live := keep[r.Metric]; return Owned(r) && !live })
}

func (m Manager) Withdraw(ctx context.Context, uid string) error {
	return m.remove(ctx, func(r Route) bool { return Owned(r) && r.Metric == OwnerID(uid) })
}

// PurgeLegacy runs only after startup fencing. The retired signature must have
// remained reserved; see the documented offline transition if it was shared.
func (m Manager) PurgeLegacy(ctx context.Context) error { return m.remove(ctx, legacyOwned) }

func (m Manager) remove(ctx context.Context, match func(Route) bool) error {
	s, err := m.connect(ctx)
	if err != nil {
		return err
	}
	defer s.handle.Close()
	s.removing = match
	actual, err := s.actual(ctx)
	if err != nil {
		return err
	}
	return s.apply(ctx, Diff{Delete: actual})
}

func (m Manager) connect(ctx context.Context) (*Session, error) {
	ns, err := netns.GetFromPath(m.Namespace)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNamespaceUnavailable, err)
	}
	defer func() {
		if err := ns.Close(); err != nil {
			klog.ErrorS(err, "Close PE namespace descriptor")
		}
	}()
	identity, err := os.Stat(fmt.Sprintf("/proc/self/fd/%d", int(ns)))
	if err != nil {
		return nil, err
	}
	handle, err := netlink.NewHandleAt(ns, unix.NETLINK_ROUTE)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNamespaceUnavailable, err)
	}
	s := &Session{path: m.Namespace, identity: identity, handle: handle, states: make(map[string]attachmentState)}
	if err := s.budget(ctx); err != nil {
		handle.Close()
		return nil, err
	}
	return s, nil
}

func (s *Session) budget(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timeout := 2 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, time.Until(deadline))
	}
	if timeout < time.Microsecond {
		return context.DeadlineExceeded
	}
	return s.handle.SetSocketTimeout(timeout)
}

func (s *Session) inspect(ctx context.Context, a Attachment) (attachmentState, error) {
	if err := s.budget(ctx); err != nil {
		return attachmentState{}, err
	}
	vrfLink, err := s.handle.LinkByName(a.VRF)
	if err != nil {
		return attachmentState{}, fmt.Errorf("PE VRF %s: %w", a.VRF, err)
	}
	vrf, ok := vrfLink.(*netlink.Vrf)
	if !ok || !tenantTable(vrf.Table) || vrf.Flags&net.FlagUp == 0 {
		return attachmentState{}, fmt.Errorf("PE %s is not a ready tenant VRF", a.VRF)
	}
	links, err := s.handle.LinkList()
	if err != nil {
		return attachmentState{}, err
	}
	for _, other := range links {
		if x, ok := other.(*netlink.Vrf); ok && x.Index != vrf.Index && x.Table == vrf.Table {
			return attachmentState{}, fmt.Errorf("PE VRFs %s and %s share table %d", a.VRF, x.Name, vrf.Table)
		}
	}
	link, err := s.handle.LinkByName(a.Device)
	if err != nil {
		return attachmentState{}, err
	}
	if link.Type() != "veth" || link.Attrs().MasterIndex != vrf.Index || link.Attrs().Flags&net.FlagUp == 0 || link.Attrs().ParentIndex != a.HostIndex {
		return attachmentState{}, fmt.Errorf("PE attachment %s is not the ready local peer in VRF %s", a.Device, a.VRF)
	}
	addresses, err := s.handle.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return attachmentState{}, err
	}
	for _, peer := range a.PeerAddresses {
		ip, err := netip.ParseAddr(peer)
		if err != nil {
			return attachmentState{}, err
		}
		found := false
		for _, address := range addresses {
			if address.IPNet != nil && address.IP.Equal(net.ParseIP(ip.String())) {
				for _, host := range a.HostAddresses {
					p, e := netip.ParsePrefix(host)
					if e == nil && p.Contains(ip) && prefixLength(address.Mask) == p.Bits() {
						found = true
					}
				}
			}
		}
		if !found {
			return attachmentState{}, fmt.Errorf("PE attachment %s lacks expected connected peer %s", a.Device, peer)
		}
	}
	return attachmentState{config: a, vrfIndex: vrf.Index, linkIndex: link.Attrs().Index, table: vrf.Table}, nil
}

// Check rejects namespace replacement and changed table/ifindex. It reads back
// routes from the kernel; a successful earlier netlink call is not a cache lease.
func (s *Session) Check(ctx context.Context, uid string) error {
	info, err := os.Stat(s.path)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNamespaceUnavailable, err)
	}
	if !os.SameFile(info, s.identity) {
		return ErrNamespaceChanged
	}
	state, ok := s.states[uid]
	if !ok {
		return errors.New("PE attachment was not prepared")
	}
	actualState, err := s.inspect(ctx, state.config)
	if err != nil {
		return err
	}
	if state.vrfIndex != actualState.vrfIndex || state.linkIndex != actualState.linkIndex || state.table != actualState.table {
		return errors.New("PE attachment generation changed")
	}
	actual, err := s.actual(ctx)
	if err != nil {
		return err
	}
	diff, err := Plan(actual, s.desired, s.owner)
	if err != nil {
		return err
	}
	if len(diff.Add) != 0 || len(diff.Delete) != 0 {
		return errors.New("PE subnet routes changed before ingress open")
	}
	return nil
}

func prefixLength(mask net.IPMask) int { ones, _ := mask.Size(); return ones }
