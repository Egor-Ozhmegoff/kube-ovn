package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	clientset "github.com/kubeovn/kube-ovn/pkg/client/clientset/versioned"
	"github.com/kubeovn/kube-ovn/pkg/util"
)

// Borrow host-owned interfaces and OVS settings only after checking their
// explicit contract. This path intentionally performs no Netlink mutations:
// FRR and OVN own the entries, and host setup owns interface lifecycle.
func (c *Controller) validateExternalVXLANFRR(plan map[uint32]*kubeovnv1.ExternalVXLAN, vpcs []kubeovnv1.Vpc, localIP, routerMAC string) error {
	if err := c.checkExternalVXLANRuntime(localIP); err != nil {
		return err
	}
	for key, want := range map[string]string{"ovn-evpn-local-ip": localIP, "ovn-evpn-vxlan-ports": "4789"} {
		actual, err := c.config.getOpenVSwitchExternalID(key)
		if err != nil {
			return err
		}
		if actual != want {
			return fmt.Errorf("host FRR requires preprovisioned OVS %s=%s, got %q", key, want, actual)
		}
	}
	for vni, config := range plan {
		if err := validateExternalVXLANFRRLinks(vni, config.FRR, localIP); err != nil {
			return err
		}
	}
	mappings, err := c.config.getOpenVSwitchExternalID("dynamic-routing-port-mapping")
	if err != nil {
		return err
	}
	for _, vpc := range vpcs {
		config := vpc.Spec.ExternalVXLAN
		if config == nil || config.FRR == nil {
			continue
		}
		bridge, err := netlink.LinkByName(config.FRR.Bridge)
		if err != nil {
			return err
		}
		if !strings.EqualFold(bridge.Attrs().HardwareAddr.String(), routerMAC) {
			return fmt.Errorf("L3 bridge %s MAC must match node Router MAC %s", config.FRR.Bridge, routerMAC)
		}
		for _, gateway := range util.ExternalVXLANGatewayNodes(config) {
			if gateway.Node != c.config.NodeName {
				continue
			}
			if err := validateExternalVXLANFRRMapping(mappings, gateway.RoutingPort, config.FRR.Bridge); err != nil {
				return err
			}
		}
	}
	return nil
}

func externalVXLANResources(ctx context.Context, client clientset.Interface) ([]kubeovnv1.Subnet, []kubeovnv1.Vpc, error) {
	subnets, err := client.KubeovnV1().Subnets().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, err
	}
	vpcs, err := client.KubeovnV1().Vpcs().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, err
	}
	return subnets.Items, vpcs.Items, nil
}

func (c *Controller) publishExternalVXLANValidation(node *corev1.Node, validated string) error {
	if node.Annotations[util.ExternalVXLANValidatedAnnotation] == validated {
		return nil
	}
	var value any
	if validated != "" {
		value = validated
	}
	return util.PatchAnnotations(c.config.KubeClient.CoreV1().Nodes(), node.Name, util.KVPatch{util.ExternalVXLANValidatedAnnotation: value})
}

func (c *Controller) finishExternalVXLANValidation(node *corev1.Node, validated string, validationErr error) error {
	if validationErr != nil {
		validated = ""
	}
	return errors.Join(validationErr, c.publishExternalVXLANValidation(node, validated))
}

func validateExternalVXLANFRRMapping(mappings, port, bridge string) error {
	count := 0
	for entry := range strings.SplitSeq(mappings, ",") {
		key, value, _ := strings.Cut(entry, "=")
		if key == port {
			if value != bridge {
				return fmt.Errorf("FRR routingPort %s maps to %s, expected %s", port, value, bridge)
			}
			count++
		}
	}
	if count != 1 {
		return fmt.Errorf("FRR routingPort %s requires exactly one host mapping to %s", port, bridge)
	}
	return nil
}

// A new VIF needs an explicitly declared RMAC/transit identity on its host.
// Removed members may drain existing IPs through retained ingress ports, but
// new attachments require gatewayNodes membership (egress:false is allowed).
func (csh cniServerHandler) validateExternalVXLANAttachment(subnet *kubeovnv1.Subnet) error {
	if subnet == nil || subnet.Spec.Vpc == "" || subnet.Spec.Vpc == util.DefaultVpc {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	vpc, err := csh.KubeOvnClient.KubeovnV1().Vpcs().Get(ctx, subnet.Spec.Vpc, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if vpc.Spec.ExternalVXLAN != nil && vpc.Spec.ExternalVXLAN.FRR == nil {
		return errors.New("native EVPN VPC requires a host FRR interface contract")
	}
	if vpc.Spec.ExternalVXLAN == nil {
		if subnet.Spec.ExternalVXLAN != nil && subnet.Spec.ExternalVXLAN.FRR != nil {
			return errors.New("native EVPN subnet requires a host FRR VPC")
		}
		return nil
	}
	if vpc.Spec.ExternalVXLAN != nil && vpc.Spec.ExternalVXLAN.FRR != nil {
		for _, gateway := range util.ExternalVXLANGatewayNodes(vpc.Spec.ExternalVXLAN) {
			if gateway.Node == csh.Config.NodeName {
				node, err := csh.KubeClient.CoreV1().Nodes().Get(ctx, csh.Config.NodeName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				subnets, vpcs, err := externalVXLANResources(ctx, csh.KubeOvnClient)
				if err != nil {
					return err
				}
				return util.ValidateExternalVXLANHostContract(node, subnets, vpcs)
			}
		}
	}
	return fmt.Errorf("native EVPN attachment on node %s requires an ingress identity in Vpc %s gatewayNodes (egress:false is allowed)", csh.Config.NodeName, subnet.Spec.Vpc)
}

func validateExternalVXLANFRRLinks(vni uint32, config *kubeovnv1.ExternalVXLANFRR, localIP string) error {
	if config == nil {
		return fmt.Errorf("VNI %d has no host FRR interface contract", vni)
	}
	bridge, err := netlink.LinkByName(config.Bridge)
	if err != nil {
		return err
	}
	vrfLink, err := netlink.LinkByName(config.VRF)
	if err != nil {
		return err
	}
	vrf, ok := vrfLink.(*netlink.Vrf)
	if !ok || vrf.Table != config.TableID || bridge.Type() != "bridge" || bridge.Attrs().MasterIndex != vrfLink.Attrs().Index {
		return fmt.Errorf("VNI %d bridge/VRF/table do not match the explicit FRR contract", vni)
	}
	vxlanLink, err := netlink.LinkByName(config.VXLAN)
	if err != nil {
		return err
	}
	vxlan, ok := vxlanLink.(*netlink.Vxlan)
	if !ok || vxlan.VxlanId != int(vni) || vxlan.Port != config.ControlPort || !vxlan.SrcAddr.Equal(net.ParseIP(localIP)) || vxlan.Learning || vxlan.Attrs().MasterIndex != bridge.Attrs().Index {
		return fmt.Errorf("VNI %d shadow VXLAN parameters do not match the explicit FRR contract", vni)
	}
	advertise, err := netlink.LinkByName(config.Advertise)
	if err != nil {
		return err
	}
	if advertise.Type() != "dummy" || advertise.Attrs().MasterIndex != bridge.Attrs().Index {
		return fmt.Errorf("VNI %d requires a dedicated advertise dummy on its control-plane bridge", vni)
	}
	links, err := netlink.LinkList()
	if err != nil {
		return err
	}
	for _, link := range links {
		if link.Attrs().MasterIndex == bridge.Attrs().Index && link.Attrs().Index != vxlan.Attrs().Index && link.Attrs().Index != advertise.Attrs().Index {
			return fmt.Errorf("FRR bridge %s contains unexpected port %s: workload/physical ports are forbidden", config.Bridge, link.Attrs().Name)
		}
	}
	for _, link := range []netlink.Link{bridge, vrf, vxlan, advertise} {
		if strings.HasPrefix(link.Attrs().Alias, util.ExternalVXLANOwner+":") {
			return fmt.Errorf("FRR interface %s still belongs to static external VXLAN; complete static cleanup before provisioning host interfaces", link.Attrs().Name)
		}
		if link.Attrs().Flags&net.FlagUp == 0 {
			return fmt.Errorf("host FRR interface %s must already be up", link.Attrs().Name)
		}
	}
	return nil
}
