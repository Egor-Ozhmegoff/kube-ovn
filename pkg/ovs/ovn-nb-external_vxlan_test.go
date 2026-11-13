package ovs

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	ovsclient "github.com/kubeovn/kube-ovn/pkg/ovsdb/client"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnnb"
	"github.com/kubeovn/kube-ovn/pkg/util"
)

func TestExternalVXLANFRRRouterLifecycle(t *testing.T) {
	client, _ := newTransactionRecordingNbClient(t)
	const router = "frr-tenant"
	require.NoError(t, client.CreateLogicalRouter(router))
	config := &kubeovnv1.ExternalVXLANGateway{
		VNI: 220,
		FRR: &kubeovnv1.ExternalVXLANFRR{Bridge: "cp220", VXLAN: "cpvx220", Advertise: "adv220", ControlPort: 60001, VRF: "vrf220", TableID: 220},
	}
	gateways := []ExternalVXLANRouterGateway{
		{Node: "worker-1", MAC: "02:ee:0b:00:00:00", Chassis: "hv1", TransitCIDR: "169.254.220.11/24", RoutingPort: "native-220-worker-1", Active: true},
		{Node: "worker-2", MAC: "02:ee:0c:00:00:00", Chassis: "hv2", TransitCIDR: "169.254.220.12/24", RoutingPort: "native-220-worker-2", Active: true},
	}
	for range 3 {
		require.NoError(t, client.ReconcileExternalVXLANRouterGateways(router, gateways, config))
	}
	lr, err := client.GetLogicalRouter(router, false)
	require.NoError(t, err)
	require.Equal(t, "true", lr.Options["dynamic-routing"])
	require.Equal(t, "220", lr.Options["dynamic-routing-vrf-id"])
	ls, err := client.GetLogicalSwitch(util.ExternalVXLANTransitSwitch(router), false)
	require.NoError(t, err)
	require.Equal(t, "fdb,ip", ls.OtherConfig["dynamic-routing-redistribute"])
	require.Equal(t, "cpvx220", ls.OtherConfig["dynamic-routing-vxlan-ifname"])
	for _, gateway := range gateways {
		port, err := client.GetLogicalRouterPort(externalVXLANRouterPort(router, gateway.Node), false)
		require.NoError(t, err)
		require.Equal(t, gateway.RoutingPort, port.Options["dynamic-routing-port-name"])
		require.Equal(t, "false", port.Options["dynamic-routing-maintain-vrf"])
	}
	routes, err := client.ListLogicalRouterStaticRoutes(router, nil, nil, "", nil)
	require.NoError(t, err)
	require.Empty(t, routes, "FRR mode must not synthesize static external routes")
	for range 3 {
		require.NoError(t, client.DeactivateExternalVXLANRouter(router))
	}
	ls, err = client.GetLogicalSwitch(util.ExternalVXLANTransitSwitch(router), false)
	require.NoError(t, err)
	require.NotContains(t, ls.OtherConfig, "dynamic-routing-vni")
	require.Equal(t, ls.Name, ls.ExternalIDs[util.ExternalVXLANOwner], "paused transit switch must retain ownership for recovery")
	lr, err = client.GetLogicalRouter(router, false)
	require.NoError(t, err)
	require.NotContains(t, lr.Options, "dynamic-routing")
	for _, gateway := range gateways {
		port, err := client.GetLogicalRouterPort(externalVXLANRouterPort(router, gateway.Node), false)
		require.NoError(t, err)
		require.Equal(t, gateway.MAC, port.MAC, "pending revalidation must not lose retained ingress identities")
	}
	require.NoError(t, client.ReconcileExternalVXLANRouterGateways(router, gateways, config))
	gateways[0].Active = false
	require.NoError(t, client.ReconcileExternalVXLANRouterGateways(router, gateways, config))
	ingress, err := client.GetLogicalRouterPort(externalVXLANRouterPort(router, "worker-1"), false)
	require.NoError(t, err)
	require.Equal(t, "02:ee:0b:00:00:00", ingress.MAC)
	require.Empty(t, ingress.GatewayChassis, "ordinary ingress LRP must not run chassis-bound Route Exchange")
	require.Equal(t, "true", ingress.Options["dynamic-routing-no-learning"])
	gateways[0].Active = true
	require.NoError(t, client.ReconcileExternalVXLANRouterGateways(router, gateways, config))
	ingress, err = client.GetLogicalRouterPort(externalVXLANRouterPort(router, "worker-1"), false)
	require.NoError(t, err)
	require.Len(t, ingress.GatewayChassis, 1)
	require.NotContains(t, ingress.Options, "dynamic-routing-no-learning")
	require.NoError(t, client.ReconcileExternalVXLANRouterGateways(router, gateways[1:], config))
	port, err := client.GetLogicalRouterPort(externalVXLANRouterPort(router, "worker-1"), true)
	require.NoError(t, err)
	require.Nil(t, port, "withdraw DGP to stop local advertisements/learning")
	require.NoError(t, client.ReconcileExternalVXLANRouterGateways(router, gateways, config))
	require.NoError(t, client.ReconcileExternalVXLANRouterGateways(router, nil, nil))
	lr, err = client.GetLogicalRouter(router, false)
	require.NoError(t, err)
	require.NotContains(t, lr.Options, "dynamic-routing")
}

func TestExternalVXLANMultiGatewayLifecycle(t *testing.T) {
	client, _ := newTransactionRecordingNbClient(t)
	const router = "external-multi"
	require.NoError(t, client.CreateLogicalRouter(router))
	config := &kubeovnv1.ExternalVXLANGateway{VNI: 220, FRR: &kubeovnv1.ExternalVXLANFRR{Bridge: "cp220", VXLAN: "cpvx220", Advertise: "adv220", VRF: "vrf220", TableID: 220, ControlPort: 60001}}
	gateways := make([]ExternalVXLANRouterGateway, 4)
	for i := range gateways {
		gateways[i] = ExternalVXLANRouterGateway{Node: fmt.Sprintf("worker-%d", i+1), MAC: fmt.Sprintf("02:ee:%02x:00:00:00", i+11), Chassis: fmt.Sprintf("chassis%d", i+1), TransitCIDR: fmt.Sprintf("169.254.220.%d/24", i+11), RoutingPort: fmt.Sprintf("native-220-worker-%d", i+1), Active: true}
	}
	check := func() {
		t.Helper()
		routes, err := client.ListLogicalRouterStaticRoutes(router, nil, nil, "", nil)
		require.NoError(t, err)
		require.Empty(t, routes, "remote routes belong exclusively to native IP Route Exchange")
		for _, gateway := range gateways {
			port, err := client.GetLogicalRouterPort(externalVXLANRouterPort(router, gateway.Node), false)
			require.NoError(t, err)
			require.Equal(t, gateway.MAC, port.MAC)
			if gateway.Active {
				require.Len(t, port.GatewayChassis, 1)
			} else {
				require.Empty(t, port.GatewayChassis)
			}
		}
	}
	for range 3 {
		require.NoError(t, client.ReconcileExternalVXLANRouterGateways(router, gateways, config))
	}
	check()
	gateways[0].Active = false
	require.NoError(t, client.ReconcileExternalVXLANRouterGateways(router, gateways, config))
	check()
	removed := gateways[0].Node
	gateways = gateways[1:]
	require.NoError(t, client.ReconcileExternalVXLANRouterGateways(router, gateways, config))
	check()
	port, err := client.GetLogicalRouterPort(externalVXLANRouterPort(router, removed), true)
	require.NoError(t, err)
	require.Nil(t, port)
	for i := range gateways {
		gateways[i].Active = false
	}
	require.NoError(t, client.ReconcileExternalVXLANRouterGateways(router, gateways, config))
	check()
	gateways[0].Active = true
	require.NoError(t, client.ReconcileExternalVXLANRouterGateways(router, gateways, config))
	check()
	for range 2 {
		require.NoError(t, client.ReconcileExternalVXLANRouter(router, "", "", nil))
	}
	ls, err := client.GetLogicalSwitch(util.ExternalVXLANTransitSwitch(router), true)
	require.NoError(t, err)
	require.Nil(t, ls)
}

func TestExternalVXLANSwitchOwnership(t *testing.T) {
	client, _ := newTransactionRecordingNbClient(t)
	require.NoError(t, client.CreateBareLogicalSwitch("external-switch"))
	require.NoError(t, client.LogicalSwitchUpdateOtherConfig("external-switch", "insert", map[string]string{"unrelated": "keep"}))
	require.NoError(t, client.ReconcileExternalVXLANSwitch("external-switch", &kubeovnv1.ExternalVXLAN{VNI: 504231, FRR: &kubeovnv1.ExternalVXLANFRR{Bridge: "cp504231", VXLAN: "cpvx504231", Advertise: "adv504231"}}))
	require.NoError(t, client.ReconcileExternalVXLANSwitch("external-switch", nil))
	ls, err := client.GetLogicalSwitch("external-switch", false)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"unrelated": "keep"}, ls.OtherConfig)
	require.NoError(t, client.LogicalSwitchUpdateOtherConfig("external-switch", "insert", map[string]string{"dynamic-routing-vni": "10"}))
	require.ErrorContains(t, client.ReconcileExternalVXLANSwitch("external-switch", &kubeovnv1.ExternalVXLAN{VNI: 504231, FRR: &kubeovnv1.ExternalVXLANFRR{Bridge: "cp504231", VXLAN: "cpvx504231", Advertise: "adv504231"}}), "externally managed")
}

// Migration cleanup must not remove ordinary routes configured before this fork.
func TestExternalVXLANLegacyRouteCleanupPreservesForeignState(t *testing.T) {
	client, _ := newTransactionRecordingNbClient(t)
	const router = "legacy-cleanup"
	require.NoError(t, client.CreateLogicalRouter(router))
	port := externalVXLANRouterPort(router, "worker-1")
	owned := &ovnnb.LogicalRouterStaticRoute{UUID: ovsclient.NamedUUID(), IPPrefix: "172.20.220.0/24", Nexthop: "169.254.220.100", OutputPort: &port, ExternalIDs: map[string]string{util.ExternalVXLANOwner: port}}
	foreign := &ovnnb.LogicalRouterStaticRoute{UUID: ovsclient.NamedUUID(), IPPrefix: "192.0.2.0/24", Nexthop: "169.254.220.100"}
	require.NoError(t, client.CreateLogicalRouterStaticRoutes(router, owned, foreign))
	for range 3 {
		require.NoError(t, client.cleanupExternalVXLANRoutes(router, port))
	}
	routes, err := client.ListLogicalRouterStaticRoutes(router, nil, nil, "", nil)
	require.NoError(t, err)
	require.Len(t, routes, 1)
	require.Equal(t, foreign.IPPrefix, routes[0].IPPrefix)
}
