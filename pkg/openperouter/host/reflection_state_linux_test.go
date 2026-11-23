package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReflectionScaleSteadyAndChurn(t *testing.T) {
	nft := &reflectionKernel{tables: make(map[string][]byte)}
	macs := []string{"02:00:00:00:01:11"}
	for cycle := range 3 {
		for subnet := range 26 {
			uid := fmt.Sprintf("vrf-%d/subnet-%d", subnet%8, subnet)
			require.NoError(t, ensureReflection(t.Context(), uid, fmt.Sprintf("oplf%x", subnet), macs, nft.run))
		}
		require.Equal(t, 26, nft.applies, "steady cycle %d must inspect, without rewriting any filter", cycle)
	}
	require.Equal(t, 78, nft.inspections, "every attachment must be freshly inspected on every cycle")
	uid, device := "vrf-0/subnet-0", "oplf0"
	require.NoError(t, ensureReflection(t.Context(), uid, device, append(macs, "02:00:00:00:01:12"), nft.run))
	require.Equal(t, 27, nft.applies)
	delete(nft.tables, reflectionTable(uid))
	require.NoError(t, ensureReflection(t.Context(), uid, device, macs, nft.run))
	require.Equal(t, 28, nft.applies, "loss of kernel state must not be hidden by successful prior ensures")
}

func TestReflectionPartialFailureAndTimeoutRetry(t *testing.T) {
	nft := &reflectionKernel{tables: make(map[string][]byte), failApply: 6}
	for i := range 26 {
		err := ensureReflection(t.Context(), fmt.Sprintf("subnet-%d", i), fmt.Sprintf("oplf%x", i), nil, nft.run)
		if i == 5 {
			require.ErrorContains(t, err, "injected apply failure")
			break
		}
		require.NoError(t, err)
	}
	for i := range 26 {
		require.NoError(t, ensureReflection(t.Context(), fmt.Sprintf("subnet-%d", i), fmt.Sprintf("oplf%x", i), nil, nft.run))
	}
	require.Equal(t, 27, nft.applies, "successful earlier filters must not be rewritten after a partial failure")
	ctx, cancel := context.WithCancel(t.Context())
	nft.beforeApply = cancel
	err := ensureReflection(ctx, "subnet-0", "oplf0", []string{"02:00:00:00:01:11"}, nft.run)
	require.ErrorIs(t, err, context.Canceled)
	nft.beforeApply = nil
	require.NoError(t, ensureReflection(t.Context(), "subnet-0", "oplf0", []string{"02:00:00:00:01:11"}, nft.run))
	ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	before := nft.applies
	require.ErrorIs(t, ensureReflection(ctx, "subnet-0", "oplf0", nil, nft.run), context.DeadlineExceeded)
	require.Equal(t, before, nft.applies)
}

func TestReflectionRepairsOnlyOwnedKernelState(t *testing.T) {
	for _, damaged := range []string{"device", "rule", "set", "foreign", "inspect"} {
		t.Run(damaged, func(t *testing.T) {
			nft := &reflectionKernel{tables: make(map[string][]byte)}
			require.NoError(t, ensureReflection(t.Context(), "owner", "oplf1", nil, nft.run))
			name := reflectionTable("owner")
			switch damaged {
			case "device":
				nft.tables[name] = []byte(strings.ReplaceAll(string(nft.tables[name]), `"dev":"oplf1",`, ""))
			case "rule", "set":
				var dump struct{ Nftables []map[string]json.RawMessage }
				require.NoError(t, json.Unmarshal(nft.tables[name], &dump))
				dump.Nftables = slices.DeleteFunc(dump.Nftables, func(entry map[string]json.RawMessage) bool { return entry[damaged] != nil })
				var err error
				nft.tables[name], err = json.Marshal(dump)
				require.NoError(t, err)
			case "foreign":
				nft.tables[name] = []byte(strings.ReplaceAll(string(nft.tables[name]), reflectionOwner+"owner", "someone-else"))
			case "inspect":
				nft.failInspect = true
			}
			err := ensureReflection(t.Context(), "owner", "oplf1", nil, nft.run)
			if damaged == "foreign" || damaged == "inspect" {
				require.Error(t, err)
				require.Equal(t, 1, nft.applies, "a failed read must not authorize destructive replacement")
				return
			}
			require.NoError(t, err)
			require.Equal(t, 2, nft.applies)
			require.NoError(t, ensureReflection(t.Context(), "owner", "oplf1", nil, nft.run))
			require.Equal(t, 2, nft.applies)
		})
	}
}

type reflectionKernel struct {
	tables               map[string][]byte
	applies, inspections int
	failApply            int
	failInspect          bool
	beforeApply          func()
}

func (k *reflectionKernel) run(ctx context.Context, input string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if args[0] == "-f" {
		k.applies++
		if k.beforeApply != nil {
			k.beforeApply()
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if k.applies == k.failApply {
			return nil, errors.New("injected apply failure")
		}
		name, dump, err := reflectionScriptFixture(input)
		if err != nil {
			return nil, err
		}
		k.tables[name] = dump
		return nil, nil
	}
	if slices.Contains(args, "table") {
		k.inspections++
		if data, ok := k.tables[args[len(args)-1]]; ok && !k.failInspect {
			return data, nil
		}
		return nil, errors.New("injected inspection failure or absent table")
	}
	var tables []map[string]any
	for name := range k.tables {
		tables = append(tables, map[string]any{"table": map[string]any{"family": "netdev", "name": name}})
	}
	return json.Marshal(map[string]any{"nftables": tables})
}

// These fixtures follow nft's JSON output, independently of the state matcher.
func reflectionScriptFixture(script string) (string, []byte, error) {
	table := regexp.MustCompile(`add table netdev (\w+) \{ comment "([^"]+)"`).FindStringSubmatch(script)
	device := regexp.MustCompile(`hook ingress device "([^"]+)"`).FindStringSubmatch(script)
	if len(table) != 3 || len(device) != 2 {
		return "", nil, errors.New("invalid nft apply script")
	}
	var elements []string
	if match := regexp.MustCompile(`add element[^\n]+\{ ([^}]+) \}`).FindStringSubmatch(script); match != nil {
		elements = strings.Split(match[1], ", ")
	}
	values, err := json.Marshal(elements)
	if err != nil {
		return "", nil, err
	}
	return table[1], fmt.Appendf(nil, `{"nftables":[
{"table":{"family":"netdev","name":%q,"handle":1,"comment":%q}},
{"chain":{"family":"netdev","table":%q,"name":"ingress","handle":2,"dev":%q,"type":"filter","hook":"ingress","prio":-500,"policy":"accept"}},
{"set":{"family":"netdev","table":%q,"name":"macs","handle":3,"type":"ether_addr","elem":%s}},
{"rule":{"family":"netdev","table":%q,"chain":"ingress","handle":4,"expr":[{"match":{"op":"==","left":{"payload":{"protocol":"ether","field":"saddr"}},"right":"@macs"}},{"counter":{"packets":123,"bytes":456}},{"drop":null}]}}]}`,
		table[1], table[2], table[1], device[1], table[1], values, table[1]), nil
}
