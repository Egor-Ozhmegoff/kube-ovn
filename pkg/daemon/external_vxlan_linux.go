package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/klog/v2"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/ovs"
	"github.com/kubeovn/kube-ovn/pkg/util"
)

var externalVXLANVersion = regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+`)

// syncExternalVXLAN validates the host FRR contract without mutating host state.
func (c *Controller) syncExternalVXLAN() {
	if err := c.reconcileExternalVXLAN(); err != nil {
		klog.ErrorS(err, "Reconcile external VXLAN")
	}
}

func (c *Controller) externalVXLANPlan(subnets []kubeovnv1.Subnet, vpcs []kubeovnv1.Vpc) (map[uint32]*kubeovnv1.ExternalVXLAN, error) {
	plan := map[uint32]*kubeovnv1.ExternalVXLAN{}
	seen := map[uint32]bool{}
	for _, subnet := range subnets {
		if subnet.Spec.ExternalVXLAN == nil || !subnet.DeletionTimestamp.IsZero() {
			continue
		}
		if err := util.ValidateExternalVXLANSubnet(subnet); err != nil {
			return nil, err
		}
		if seen[subnet.Spec.ExternalVXLAN.VNI] {
			return nil, fmt.Errorf("duplicate external VXLAN VNI %d", subnet.Spec.ExternalVXLAN.VNI)
		}
		plan[subnet.Spec.ExternalVXLAN.VNI] = subnet.Spec.ExternalVXLAN
		seen[subnet.Spec.ExternalVXLAN.VNI] = true
	}
	for _, vpc := range vpcs {
		config := vpc.Spec.ExternalVXLAN
		if config == nil || !vpc.DeletionTimestamp.IsZero() {
			continue
		}
		if err := util.ValidateExternalVXLANGateway(config); err != nil {
			return nil, err
		}
		if seen[config.VNI] {
			return nil, fmt.Errorf("duplicate external VXLAN VNI %d", config.VNI)
		}
		seen[config.VNI] = true
		// EVPN LS options apply on every workload chassis, including ingress-only members.
		plan[config.VNI] = &config.ExternalVXLAN
	}
	return plan, nil
}

func (c *Controller) reconcileExternalVXLAN() (retErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	node, err := c.config.KubeClient.CoreV1().Nodes().Get(ctx, c.config.NodeName, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	validated := ""
	defer func() {
		retErr = c.finishExternalVXLANValidation(node, validated, retErr)
	}()
	localIP := node.Annotations[util.ExternalVXLANIPAnnotation]
	owner, err := c.config.getOpenVSwitchExternalID(util.ExternalVXLANOwner)
	if err != nil {
		return err
	}
	if localIP == "" && owner == "" {
		return nil
	}
	if owner != "" {
		return errors.New("remove legacy Kube-OVN-owned external VXLAN state before attaching host FRR")
	}
	plan := map[uint32]*kubeovnv1.ExternalVXLAN{}
	var subnets []kubeovnv1.Subnet
	var vpcs []kubeovnv1.Vpc
	if localIP != "" {
		ip := net.ParseIP(localIP)
		if ip == nil || ip.To4() == nil || !ip.IsGlobalUnicast() {
			return fmt.Errorf("invalid external VXLAN local IPv4 %q", localIP)
		}
		subnets, vpcs, err = externalVXLANResources(ctx, c.config.KubeOvnClient)
		if err != nil {
			return err
		}
		plan, err = c.externalVXLANPlan(subnets, vpcs)
		if err != nil {
			return err
		}
	}
	if len(plan) == 0 {
		return nil
	}
	if err := c.validateExternalVXLANFRR(plan, vpcs, localIP, node.Annotations[util.ExternalVXLANMACAnnotation]); err != nil {
		return err
	}
	validated, err = util.ExternalVXLANHostContract(node, subnets, vpcs)
	return err
}

func (c *Controller) checkExternalVXLANRuntime(localIP string) error {
	lease, err := c.config.getOpenVSwitchExternalID("ovn-ipsec-protection-lease")
	if err != nil {
		return err
	}
	if lease != "" {
		return errors.New("external VXLAN cannot be combined with IPsec protection")
	}
	for component, minimum := range map[string]string{ovs.OvnController: "26.3.3", ovs.OvsVswitchd: "3.7.1"} {
		output, err := ovs.Appctl(component, "version")
		if err != nil {
			return err
		}
		actual, err := version.ParseGeneric(externalVXLANVersion.FindString(output))
		if err != nil || actual.LessThan(version.MustParseGeneric(minimum)) {
			return fmt.Errorf("external VXLAN requires %s >= %s, got %q", component, minimum, output)
		}
	}
	bridges, err := c.listVswitchBridges(false, nil)
	if err != nil {
		return err
	}
	for _, bridge := range bridges {
		if bridge.Name == "br-int" && bridge.DatapathType != "" && bridge.DatapathType != "system" {
			return errors.New("external VXLAN requires the kernel OVS datapath")
		}
	}
	encapIP, err := c.config.getOpenVSwitchExternalID("ovn-encap-ip")
	if err != nil {
		return err
	}
	if slices.Contains(strings.Split(encapIP, ","), localIP) {
		return errors.New("external VXLAN VTEP must differ from the cluster encapsulation IP")
	}
	addresses, err := netlink.AddrList(nil, unix.AF_INET)
	if err != nil {
		return err
	}
	found := false
	for _, address := range addresses {
		found = found || address.IP.Equal(net.ParseIP(localIP))
	}
	if !found {
		return fmt.Errorf("external VTEP %s must already be configured on the node", localIP)
	}

	return nil
}
