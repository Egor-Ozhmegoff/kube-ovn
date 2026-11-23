package v1

// OpenPERouterIntegration binds a VPC to a systemd-managed OpenPERouter routing
// domain. OpenPERouter remains the owner of its namespace, FRR and VXLAN state.
type OpenPERouterIntegration struct {
	// +kubebuilder:validation:Enum=OpenPERouter
	Mode string `json:"mode"`
	// Linux VRF in the OpenPERouter namespace and on the host.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z][a-zA-Z0-9_-]*$`
	// +kubebuilder:validation:MaxLength=15
	VRF string `json:"vrf"`
	// Name of the native OpenPERouter L3VNI definition, managed by bootstrap.
	// +kubebuilder:validation:MinLength=1
	L3VNIName string `json:"l3vniName"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=16777215
	L3VNI int32 `json:"l3vni"`
	// Dedicated OVN-to-host transit pools, separate from workload/hostSession pools.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	TransitCIDRs []string `json:"transitCIDRs"`
	// Must match hostSession.localCIDRs in the native L3VNI bootstrap.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	HostSessionCIDRs []string `json:"hostSessionCIDRs"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=4294967295
	// +kubebuilder:validation:Format=int64
	RouterASN uint32 `json:"routerASN"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=4294967295
	// +kubebuilder:validation:Format=int64
	HostASN uint32 `json:"hostASN"`
}

// OpenPERouterL2Attachment attaches an OVN switch to a native L2VNI using a
// chassis-local port, while retaining OVN tunnels between workload ports.
type OpenPERouterL2Attachment struct {
	// +kubebuilder:validation:MinLength=1
	L2VNIName string `json:"l2vniName"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=16777215
	VNI int32 `json:"vni"`
	// Must match both the native IRB gateway and the OVN router port MAC.
	// +kubebuilder:validation:Pattern=`^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$`
	GatewayMAC string `json:"gatewayMAC"`
}
