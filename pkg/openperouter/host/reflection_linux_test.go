package host

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kubeovn/kube-ovn/pkg/openperouter"
)

func TestReflectionScope(t *testing.T) {
	rules, err := reflectionRules("subnet-uid", "host-e-10101", []string{"02:00:00:00:01:11"}, false)
	require.NoError(t, err)
	require.Contains(t, rules, `hook ingress device "host-e-10101"`)
	require.Contains(t, rules, "ether saddr @macs counter drop")
	require.NotContains(t, rules, "ether type", "all EtherTypes, including ND and RARP, share the ingress guard")
	require.NotContains(t, rules, "hook egress", "migration RARP must leave OVN")
	_, err = reflectionRules("subnet-uid", "fabric0", nil, false)
	require.Error(t, err, "underlay is outside this owner's scope")
}

func TestReflectionKernelLifecycle(t *testing.T) {
	if os.Getenv("KUBE_OVN_DISPOSABLE_LAB") != "true" {
		t.Skip("requires disposable privileged container")
	}
	_, err := os.Stat("/.dockerenv")
	require.NoError(t, err)
	require.Zero(t, os.Geteuid())
	ctx := t.Context()
	ip := func(args ...string) {
		t.Helper()
		out, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput()
		require.NoError(t, err, string(out))
	}
	ip("link", "add", "host-e-10101", "type", "veth", "peer", "name", "fabric")
	ip("link", "set", "host-e-10101", "up")
	require.NoError(t, EnsureReflection(ctx, "lifecycle", "host-e-10101", []string{"02:00:00:00:01:11"}))
	auditCtx, timing := openperouter.WithReconcileTiming(ctx)
	require.NoError(t, EnsureReflection(auditCtx, "lifecycle", "host-e-10101", []string{"02:00:00:00:01:11"}))
	require.Zero(t, timing.Snapshot()["nft.apply"].Calls, "an unchanged live kernel filter must not be replaced")
	ip("link", "del", "host-e-10101")
	ip("link", "add", "host-e-10101", "type", "veth", "peer", "name", "fabric")
	ip("link", "set", "host-e-10101", "up")
	repairCtx, repairTiming := openperouter.WithReconcileTiming(ctx)
	require.NoError(t, EnsureReflection(repairCtx, "lifecycle", "host-e-10101", []string{"02:00:00:00:01:11"}))
	require.Equal(t, 1, repairTiming.Snapshot()["nft.apply"].Calls, "same MACs still need repair after device replacement")
	require.NoError(t, EnsureReflection(ctx, "lifecycle", "host-e-10101", []string{"02:00:00:00:01:12"}))
	require.NoError(t, exec.CommandContext(ctx, "nft", "flush", "chain", "netdev", reflectionTable("lifecycle"), "ingress").Run())
	require.NoError(t, EnsureReflection(ctx, "lifecycle", "host-e-10101", []string{"02:00:00:00:01:12"}))
	dump, err := inspectReflection(ctx, runReflectionCommand, reflectionTable("lifecycle"))
	require.NoError(t, err)
	current, err := reflectionCurrent(dump, "lifecycle", "host-e-10101", []string{"02:00:00:00:01:12"})
	require.NoError(t, err)
	require.True(t, current, "lost drop rule must be repaired")
	require.NoError(t, cleanupReflection(ctx, map[string]bool{"lifecycle": true}))
	require.NoError(t, cleanupReflection(ctx, nil))
	require.Error(t, exec.CommandContext(ctx, "nft", "list", "table", "netdev", reflectionTable("lifecycle")).Run())
	ip("link", "del", "host-e-10101")
}
