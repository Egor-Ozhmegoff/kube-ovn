package v1

// ExternalVXLAN configures native OVN EVPN with host FRR.
// Remote VTEPs and MACs reach OVN through its kernel Netlink control-plane API.
type ExternalVXLAN struct {
	// External VXLAN network identifier, independent of OVN's cluster tunnel keys.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=16777215
	// +kubebuilder:validation:Required
	VNI uint32 `json:"vni"`
	// GatewayMAC is the VM-facing distributed anycast gateway MAC for a Subnet.
	// It must be unset for a Vpc attachment, which uses the gateway Node's Router MAC.
	GatewayMAC string `json:"gatewayMAC,omitempty"`
	// FRR attaches to explicitly named, host-owned control-plane interfaces.
	// Remote VTEPs, MACs and routes are learned exclusively from FRR.
	// +kubebuilder:validation:Required
	FRR *ExternalVXLANFRR `json:"frr"`
}

// ExternalVXLANFRR references existing host interfaces; Kube-OVN never creates,
// deletes, flushes or changes their FDB/neighbors/routes.
type ExternalVXLANFRR struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=15
	Bridge string `json:"bridge"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=15
	VXLAN string `json:"vxlan"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=15
	Advertise string `json:"advertise"`
	// Shadow control-plane socket, distinct from OVS workload UDP 4789.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	ControlPort int `json:"controlPort"`
	// Host-owned VRF shared by the L2 and L3 control-plane bridges of a tenant.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=15
	VRF string `json:"vrf"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Format=int64
	// +kubebuilder:validation:Maximum=4294967294
	// +kubebuilder:validation:XValidation:rule="self < 252 || self > 255",message="VRF table IDs 252 through 255 are reserved in OVN 26.03"
	TableID uint32 `json:"tableID"`
}

// +kubebuilder:validation:XValidation:rule="(has(self.gatewayNodes) && size(self.gatewayNodes) > 0) != (has(self.gatewayNode) && size(self.gatewayNode) > 0)",message="specify gatewayNodes or legacy gatewayNode, exclusively"
// +kubebuilder:validation:XValidation:rule="!has(self.gatewayNodes) || !has(self.transitCIDR)",message="transitCIDR belongs in each gatewayNodes entry"
// +kubebuilder:validation:XValidation:rule="!has(self.gatewayNode) || has(self.transitCIDR)",message="legacy gatewayNode requires transitCIDR"
type ExternalVXLANGateway struct {
	ExternalVXLAN `json:",inline"`
	// GatewayNode is the legacy single-node attachment. Do not combine with gatewayNodes.
	GatewayNode string `json:"gatewayNode,omitempty"`
	// TransitCIDR is an IPv4 address/prefix for the fabric-facing logical router port.
	TransitCIDR string `json:"transitCIDR,omitempty"`
	// Host's existing dynamic-routing-port-mapping key for legacy gatewayNode.
	RoutingPort string `json:"routingPort,omitempty"`
	// GatewayNodes configures active-active DGPs with per-node transit IP and Router MAC.
	// Inactive nodes retain ingress identity; in FRR mode the controller clears their DGP chassis binding.
	// +listType=map
	// +listMapKey=node
	// +kubebuilder:validation:MinItems=1
	GatewayNodes []ExternalVXLANGatewayNode `json:"gatewayNodes,omitempty"`
}

type ExternalVXLANGatewayNode struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Node string `json:"node"`
	// Unique IPv4 address/prefix for this node's logical router port.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	TransitCIDR string `json:"transitCIDR"`
	// Existing OVN dynamic-routing-port-mapping key resolving to the FRR bridge.
	RoutingPort string `json:"routingPort,omitempty"`
	// Egress defaults to true. False retains an ingress identity without egress routes.
	Egress *bool `json:"egress,omitempty"`
}
