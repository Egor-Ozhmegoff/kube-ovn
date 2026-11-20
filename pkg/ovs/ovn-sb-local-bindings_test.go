package ovs

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnsb"
)

func TestActiveBindingFollowsRARP(t *testing.T) {
	source := "source-uuid"
	binding := ovnsb.PortBinding{Chassis: &source, AdditionalChassis: []string{"target-uuid"}, Options: map[string]string{"activation-strategy": "rarp"}}
	require.True(t, activeBindingOwner(binding, source, "source"))
	require.False(t, activeBindingOwner(binding, "target-uuid", "target"))
	binding.Options["additional-chassis-activated"] = "target"
	require.False(t, activeBindingOwner(binding, source, "source"))
	require.True(t, activeBindingOwner(binding, "target-uuid", "target"))
	// Cancellation restores the source; stale activation text cannot elect a
	// chassis which is no longer an additional binding.
	binding.AdditionalChassis = nil
	delete(binding.Options, "activation-strategy")
	require.True(t, activeBindingOwner(binding, source, "source"))
	require.False(t, activeBindingOwner(binding, "target-uuid", "target"))
	binding.Chassis = new("target-uuid")
	require.True(t, activeBindingOwner(binding, "target-uuid", "target"))
	require.False(t, activeBindingOwner(binding, source, "source"))
}
