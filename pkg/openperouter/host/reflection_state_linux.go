package host

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"slices"
)

// Inspect the kernel on every ensure. In particular a surviving chain without
// its device is not protection: unregistering a veth removes the ingress hook.
func reflectionCurrent(data []byte, uid, device string, macs []string) (bool, error) {
	var dump struct {
		Nftables []map[string]map[string]any `json:"nftables"`
	}
	if err := json.Unmarshal(data, &dump); err != nil {
		return false, err
	}
	objects := make(map[string]map[string]any)
	matches := true
	for _, entry := range dump.Nftables {
		for kind, fields := range entry {
			if kind == "metainfo" {
				continue
			}
			if kind == "table" && (fields["name"] != reflectionTable(uid) || fields["comment"] != reflectionOwner+uid) {
				return false, fmt.Errorf("refuse foreign reflection table %s", reflectionTable(uid))
			}
			if _, duplicate := objects[kind]; duplicate {
				matches = false
				continue
			}
			delete(fields, "handle")
			if kind == "rule" {
				if expressions, ok := fields["expr"].([]any); ok {
					for _, expression := range expressions {
						if expr, ok := expression.(map[string]any); ok && expr["counter"] != nil {
							expr["counter"] = map[string]any{}
						}
					}
				}
			}
			if kind == "set" {
				values, ok := reflectionElements(fields["elem"])
				if !ok {
					matches = false
					continue
				}
				fields["elem"] = values
			}
			objects[kind] = fields
		}
	}
	// Do not repair an unowned/incomplete dump by deleting a table whose owner
	// could not be established. A missing rule/set/chain in an owned table is
	// instead recoverable by replacing that table in one nft transaction.
	if objects["table"] == nil {
		return false, fmt.Errorf("missing reflection table ownership for %s", reflectionTable(uid))
	}
	if !matches {
		return false, nil
	}
	values := make([]string, 0, len(macs))
	for _, value := range macs {
		mac, err := net.ParseMAC(value)
		if err != nil {
			return false, err
		}
		values = append(values, mac.String())
	}
	slices.Sort(values)
	actual, err := json.Marshal(objects)
	if err != nil {
		return false, err
	}
	wanted, err := json.Marshal(reflectionObjects(uid, device, slices.Compact(values)))
	return bytes.Equal(actual, wanted), err
}

func reflectionElements(raw any) ([]string, bool) {
	values := make([]string, 0)
	if raw == nil {
		return values, true
	}
	elements, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	for _, element := range elements {
		value, ok := element.(string)
		if !ok {
			return nil, false
		}
		values = append(values, value)
	}
	slices.Sort(values)
	return values, true
}

func reflectionObjects(uid, device string, macs []string) map[string]map[string]any {
	name := reflectionTable(uid)
	return map[string]map[string]any{
		"table": {"family": "netdev", "name": name, "comment": reflectionOwner + uid},
		"chain": {
			"family": "netdev", "table": name, "name": "ingress", "dev": device,
			"type": "filter", "hook": "ingress", "prio": -500, "policy": "accept",
		},
		"set": {"family": "netdev", "table": name, "name": "macs", "type": "ether_addr", "elem": macs},
		"rule": {"family": "netdev", "table": name, "chain": "ingress", "expr": []any{
			map[string]any{"match": map[string]any{"op": "==", "left": map[string]any{
				"payload": map[string]any{"protocol": "ether", "field": "saddr"},
			}, "right": "@macs"}},
			map[string]any{"counter": map[string]any{}},
			map[string]any{"drop": nil},
		}},
	}
}
