package host

import (
	"errors"
	"fmt"
	"math"
	"syscall"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// EnsureIsolation keeps ingress in its tenant table even if the interface loses
// its VRF master. Rules refer to names so they survive native veth recreation.
// The terminal rule must exist before the lookup or the attachment is opened.
func EnsureIsolation(name string, tableID uint32) error {
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		for _, rule := range isolationRules(name, tableID, family) {
			existing, err := netlink.RuleList(family)
			if err != nil {
				return err
			}
			found := false
			for _, current := range existing {
				if current.Priority != rule.Priority || current.IifName != name {
					continue
				}
				// This netlink version does not decode the rule action/type.
				// Protocol, iif, priority and table identify our persisted rule.
				if current.Table != rule.Table || current.Protocol != rule.Protocol {
					return fmt.Errorf("refuse conflicting ingress rule for %s at priority %d", name, rule.Priority)
				}
				found = true
			}
			if !found {
				if err := netlink.RuleAdd(rule); err != nil && !errors.Is(err, syscall.EEXIST) {
					return err
				}
			}
		}
	}
	return nil
}

func isolationRules(name string, tableID uint32, family int) []*netlink.Rule {
	terminal := netlink.NewRule()
	terminal.Family, terminal.Priority, terminal.IifName = family, 901, name
	terminal.Type, terminal.Protocol = unix.RTN_UNREACHABLE, routeProtocol
	lookup := netlink.NewRule()
	lookup.Family, lookup.Priority, lookup.IifName = family, 900, name
	lookup.Type, lookup.Protocol, lookup.Table = unix.RTN_UNICAST, routeProtocol, int(tableID)
	return []*netlink.Rule{terminal, lookup}
}

// ReleaseIsolation removes only this integration's lookup rules. The terminal
// rule remains for a native link which OpenPERouter may subsequently raise.
// Callers must first close the owned attachment, while its OVS owner is known.
func ReleaseIsolation(name string) error {
	// Also recover old/partially created attachments whose terminal rule was
	// missing. Protect both families before retiring the first lookup.
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		if err := ensureTerminalRule(isolationRules(name, 0, family)[0]); err != nil {
			return err
		}
	}
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		rules, err := netlink.RuleList(family)
		if err != nil {
			return err
		}
		for _, rule := range rules {
			if rule.IifName != name || rule.Priority != 900 {
				continue
			}
			if rule.Protocol != routeProtocol {
				return fmt.Errorf("foreign lookup rule for %s", name)
			}
			if rule.Table < 0 || uint64(rule.Table) > math.MaxUint32 {
				return fmt.Errorf("invalid table %d", rule.Table)
			}
			want := isolationRules(name, uint32(rule.Table), family)[1]
			if err := netlink.RuleDel(want); err != nil && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.ENOENT) {
				return err
			}
		}
	}
	return nil
}

// RebindIsolation is permitted only for a new, closed L2 attachment. A higher
// priority terminal rule protects both families during an interrupted change.
func RebindIsolation(name string, tableID uint32) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	if err := netlink.LinkSetDown(link); err != nil {
		return err
	}
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		rule := isolationRules(name, tableID, family)[0]
		rule.Priority = 899
		if err := ensureTerminalRule(rule); err != nil {
			return err
		}
	}
	if err := ReleaseIsolation(name); err != nil {
		return err
	}
	return EnsureIsolation(name, tableID)
}

func finishIsolation(name string) error {
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		rule := isolationRules(name, 0, family)[0]
		rule.Priority = 899
		if err := netlink.RuleDel(rule); err != nil && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.ENOENT) {
			return err
		}
	}
	return nil
}

func ensureTerminalRule(rule *netlink.Rule) error {
	rules, err := netlink.RuleList(rule.Family)
	if err != nil {
		return err
	}
	for _, current := range rules {
		if current.IifName != rule.IifName || current.Priority != rule.Priority {
			continue
		}
		if current.Protocol != routeProtocol || current.Table != 0 {
			return fmt.Errorf("foreign terminal rule for %s", rule.IifName)
		}
		return nil
	}
	return netlink.RuleAdd(rule)
}
