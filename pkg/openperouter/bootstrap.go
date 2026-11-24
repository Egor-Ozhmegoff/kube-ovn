package openperouter

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
)

// Bootstrap describes attachments, not proof of local dataplane readiness.
// Only fields needed to verify the host-side contract are decoded here.
type Bootstrap struct {
	Index       int
	RouterMAC   string
	L3VNIs      []NativeL3VNI
	L2VNIs      []NativeL2VNI
	Attachments []LocalL3Attachment
}

// NativeHostSession detects an incompatible legacy contract, never configures BGP.
type NativeHostSession struct{}

// LocalL3Attachment is Kube-OVN bootstrap metadata, not an OpenPERouter API.
// Configuration management must supply it only after a native non-BGP veth
// lifecycle is available. Runtime verifies the actual peer and VRF separately.
type LocalL3Attachment struct {
	L3VNIName  string
	LocalCIDRs []string
}

type NativeL3VNI struct {
	Name, VRF   string
	VNI         int32
	HostSession *NativeHostSession
}

type NativeL2VNI struct {
	Name, GatewayMAC string
	VNI              int32
	GatewayIPs       []string
	HostMaster       *struct{ Type string }
	RoutingDomain    struct {
		Type  string
		L3VNI struct{ Name string }
	}
}

func (b *Bootstrap) ValidateL2(parent *kubeovnv1.OpenPERouterIntegration, subnet *kubeovnv1.Subnet) error {
	spec := subnet.Spec.OpenPERouterL2
	if err := ValidateL2(spec); err != nil {
		return err
	}
	if spec == nil {
		return nil
	}
	var desiredGateways []string
	for ip := range strings.SplitSeq(subnet.Spec.Gateway, ",") {
		addr, err := netip.ParseAddr(ip)
		if err != nil {
			return err
		}
		for cidr := range strings.SplitSeq(subnet.Spec.CIDRBlock, ",") {
			prefix, err := netip.ParsePrefix(cidr)
			if err != nil {
				return err
			}
			if prefix.Contains(addr) {
				desiredGateways = append(desiredGateways, netip.PrefixFrom(addr, prefix.Bits()).String())
			}
		}
	}
	slices.Sort(desiredGateways)
	matches := 0
	for _, native := range b.L2VNIs {
		if native.Name != spec.L2VNIName {
			continue
		}
		matches++
		actual := slices.Clone(native.GatewayIPs)
		slices.Sort(actual)
		if native.VNI != spec.VNI || native.VNI == parent.L3VNI || native.HostMaster != nil ||
			!strings.EqualFold(native.GatewayMAC, spec.GatewayMAC) || !slices.Equal(actual, desiredGateways) ||
			native.RoutingDomain.Type != "L3VNI" || native.RoutingDomain.L3VNI.Name != parent.L3VNIName {
			return fmt.Errorf("subnet %s disagrees with native L2VNI %s or has a competing hostMaster", subnet.Name, native.Name)
		}
	}
	if matches != 1 {
		return fmt.Errorf("expected one native L2VNI %s, found %d", spec.L2VNIName, matches)
	}
	return nil
}

func ReadBootstrap(directory string) (*Bootstrap, error) {
	data, err := os.ReadFile(filepath.Join(directory, "node-config.yaml"))
	if err != nil {
		return nil, err
	}
	var node struct {
		RouterMAC string
		NodeIndex struct{ Index *int }
	}
	if err := yaml.Unmarshal(data, &node); err != nil {
		return nil, err
	}
	if node.NodeIndex.Index == nil || *node.NodeIndex.Index < 0 || *node.NodeIndex.Index > MaxNodeIndex {
		return nil, fmt.Errorf("OpenPERouter node-config requires an explicit nodeIndex.index in 0..%d", MaxNodeIndex)
	}
	mac, err := net.ParseMAC(node.RouterMAC)
	if err != nil || len(mac) != 6 || mac[0]&1 != 0 || mac.String() == "00:00:00:00:00:00" {
		return nil, errors.New("OpenPERouter node-config requires a nonzero unicast routerMAC")
	}
	b := &Bootstrap{Index: *node.NodeIndex.Index, RouterMAC: mac.String()}
	files, err := filepath.Glob(filepath.Join(directory, "openpe_*.yaml"))
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		var config struct {
			L3VNIs []NativeL3VNI
			L2VNIs []NativeL2VNI
		}
		if err := yaml.Unmarshal(data, &config); err != nil {
			return nil, err
		}
		b.L3VNIs = append(b.L3VNIs, config.L3VNIs...)
		b.L2VNIs = append(b.L2VNIs, config.L2VNIs...)
	}
	data, err = os.ReadFile(filepath.Join(directory, "kube-ovn-attachments.yaml"))
	if err != nil {
		return nil, fmt.Errorf("netlink mode requires kube-ovn-attachments.yaml and native L3 attachments without BGP: %w", err)
	}
	var contract struct {
		Version       string
		L3Attachments []LocalL3Attachment
	}
	if err := yaml.UnmarshalStrict(data, &contract); err != nil {
		return nil, err
	}
	if contract.Version != "netlink-v1" {
		return nil, errors.New("unsupported Kube-OVN attachment contract; expected netlink-v1")
	}
	b.Attachments = contract.L3Attachments
	return b, nil
}

func (b *Bootstrap) Validate(spec *kubeovnv1.OpenPERouterIntegration) error {
	if err := Validate(spec); err != nil {
		return err
	}
	matches := 0
	for _, l3 := range b.L3VNIs {
		if l3.Name != spec.L3VNIName {
			continue
		}
		matches++
		if l3.HostSession != nil {
			return fmt.Errorf("L3VNI %s uses legacy hostSession: it creates local BGP and also owns the L3 veth; migrate the external owner to a non-BGP attachment contract before enabling netlink mode", l3.Name)
		}
		if l3.VRF != spec.VRF || l3.VNI != spec.L3VNI {
			return fmt.Errorf("VPC disagrees with native L3VNI %s", l3.Name)
		}
	}
	if matches != 1 {
		return fmt.Errorf("expected one native L3VNI %s, found %d", spec.L3VNIName, matches)
	}
	matches = 0
	for _, attachment := range b.Attachments {
		if attachment.L3VNIName != spec.L3VNIName {
			continue
		}
		matches++
		actual, wanted := slices.Clone(attachment.LocalCIDRs), slices.Clone(spec.HostSessionCIDRs)
		slices.Sort(actual)
		slices.Sort(wanted)
		if !slices.Equal(actual, wanted) {
			return fmt.Errorf("VPC attachment pools disagree with local contract for %s", attachment.L3VNIName)
		}
	}
	if matches != 1 {
		return fmt.Errorf("expected one non-BGP local attachment contract for %s, found %d", spec.L3VNIName, matches)
	}
	return nil
}
