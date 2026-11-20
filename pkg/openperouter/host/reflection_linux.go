package host

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"slices"
	"strings"
)

const reflectionOwner = "kube-ovn/openperouter/"

// EnsureReflection filters only PE-to-OVN ingress. It acts before reserved OVN
// ND allow flows; outbound RARP and legitimate external sources are unchanged.
func EnsureReflection(ctx context.Context, uid, device string, macs []string) error {
	name := reflectionTable(uid)
	output, err := exec.CommandContext(ctx, "nft", "-j", "list", "tables").CombinedOutput()
	if err != nil {
		return fmt.Errorf("list reflection tables: %w: %s", err, output)
	}
	tables, err := reflectionTables(output)
	if err != nil {
		return err
	}
	existing := false
	for _, table := range tables {
		if table.Name == name && table.Family == "netdev" {
			if table.Comment != reflectionOwner+uid {
				return fmt.Errorf("refuse foreign reflection table %s", name)
			}
			existing = true
		}
	}
	rules, err := reflectionRules(uid, device, macs, existing)
	if err != nil {
		return err
	}
	return applyReflection(ctx, rules)
}

func cleanupReflection(ctx context.Context, live map[string]bool) error {
	output, err := exec.CommandContext(ctx, "nft", "-j", "list", "tables").CombinedOutput()
	if err != nil {
		return fmt.Errorf("list reflection tables: %w: %s", err, output)
	}
	tables, err := reflectionTables(output)
	if err != nil {
		return err
	}
	for _, table := range tables {
		uid, owned := strings.CutPrefix(table.Comment, reflectionOwner)
		if table.Family != "netdev" || !owned || live[uid] || table.Name != reflectionTable(uid) {
			continue
		}
		if err := applyReflection(ctx, fmt.Sprintf("delete table netdev %s\n", reflectionTable(uid))); err != nil {
			return err
		}
	}
	return nil
}

type reflectionTableInfo struct{ Name, Family, Comment string }

func reflectionTables(data []byte) ([]reflectionTableInfo, error) {
	var result struct {
		Nftables []struct{ Table *reflectionTableInfo }
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	var tables []reflectionTableInfo
	for _, entry := range result.Nftables {
		if entry.Table != nil {
			tables = append(tables, *entry.Table)
		}
	}
	return tables, nil
}

func reflectionTable(uid string) string {
	hash := sha256.Sum256([]byte(uid))
	return fmt.Sprintf("kopr_%x", hash[:12])
}

func reflectionRules(uid, device string, macs []string, existing bool) (string, error) {
	if uid == "" || strings.ContainsAny(uid, "\"\\\n\r") || (!strings.HasPrefix(device, "host-e-") && !strings.HasPrefix(device, "oplf")) {
		return "", errors.New("invalid reflection owner or device")
	}
	if len(device) > 15 || strings.ContainsAny(device, "\"\\\n\r") {
		return "", errors.New("invalid reflection device")
	}
	values := make([]string, 0, len(macs))
	for _, value := range macs {
		mac, err := net.ParseMAC(value)
		if err != nil || len(mac) != 6 || mac[0]&1 != 0 {
			return "", fmt.Errorf("invalid reflection MAC %q", value)
		}
		values = append(values, mac.String())
	}
	slices.Sort(values)
	values = slices.Compact(values)
	name := reflectionTable(uid)
	var script strings.Builder
	if existing {
		fmt.Fprintf(&script, "delete table netdev %s\n", name)
	}
	fmt.Fprintf(&script, "add table netdev %s { comment %q; }\n", name, reflectionOwner+uid)
	fmt.Fprintf(&script, "add set netdev %s macs { type ether_addr; }\n", name)
	if len(values) != 0 {
		fmt.Fprintf(&script, "add element netdev %s macs { %s }\n", name, strings.Join(values, ", "))
	}
	fmt.Fprintf(&script, "add chain netdev %s ingress { type filter hook ingress device %q priority -500; policy accept; }\n", name, device)
	fmt.Fprintf(&script, "add rule netdev %s ingress ether saddr @macs counter drop\n", name)
	return script.String(), nil
}

func applyReflection(ctx context.Context, rules string) error {
	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("apply reflection filter: %w: %s", err, output)
	}
	return nil
}
