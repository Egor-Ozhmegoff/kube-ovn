package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/openperouter"
	"github.com/kubeovn/kube-ovn/pkg/openperouter/advertiser"
	ophost "github.com/kubeovn/kube-ovn/pkg/openperouter/host"
	"github.com/kubeovn/kube-ovn/pkg/ovs"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnsb"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/vswitch"
	"github.com/kubeovn/kube-ovn/pkg/util"
)

type openPERouterRuntime struct {
	attachment    *ophost.Attachment
	speaker       *advertiser.Speaker
	lease         advertiser.Lease
	uid           string
	configuration string
}

func (c *Controller) runOpenPERouter(stop <-chan struct{}) {
	// Fence persisted attachments before trusting any API/configuration cache.
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := ophost.Fence(ctx, c.vswitchTables, "")
		cancel()
		if err == nil {
			break
		}
		klog.ErrorS(err, "Fence persisted OpenPERouter attachments")
		select {
		case <-stop:
			return
		case <-time.After(5 * time.Second):
		}
	}
	if !cache.WaitForCacheSync(stop, c.openPERouterVpcsSynced) {
		return
	}
	active := make(map[string]*openPERouterRuntime)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := ophost.Fence(ctx, c.vswitchTables, ""); err != nil {
			klog.ErrorS(err, "Fence OpenPERouter attachments on shutdown")
		}
		cancel()
		for _, state := range active {
			state.speaker.Close()
		}
	}()
	var bindings *ovs.LocalBindingClient
	defer func() {
		if bindings != nil {
			bindings.Close()
		}
	}()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		err := c.syncOpenPERouter(ctx, active, &bindings)
		cancel()
		if err != nil {
			klog.ErrorS(err, "Reconcile local OpenPERouter integration")
			fenceCtx, fenceCancel := context.WithTimeout(context.Background(), 3*time.Second)
			if fenceErr := ophost.Fence(fenceCtx, c.vswitchTables, ""); fenceErr != nil {
				klog.ErrorS(fenceErr, "Fence OpenPERouter ingress")
			}
			fenceCancel()
			for _, state := range active {
				if err := state.speaker.Sync(nil); err != nil {
					klog.ErrorS(err, "Withdraw OpenPERouter workload routes")
				}
			}
		}
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
	}
}

func (c *Controller) syncOpenPERouter(ctx context.Context, active map[string]*openPERouterRuntime, reader **ovs.LocalBindingClient) error {
	if c.config.NetworkType != util.NetworkTypeVxlan {
		return errors.New("OpenPERouter integration requires internal OVN VXLAN")
	}
	vpcs, err := c.openPERouterVpcs.List(labels.Everything())
	if err != nil {
		return err
	}
	subnets, err := c.subnetsLister.List(labels.Everything())
	if err != nil {
		return err
	}
	liveVpcs, liveSubnets := make(map[string]bool), make(map[string]bool)
	for _, vpc := range vpcs {
		if vpc.Spec.OpenPERouter != nil {
			liveVpcs[string(vpc.UID)] = true
		}
	}
	for _, subnet := range subnets {
		if subnet.Spec.OpenPERouterL2 != nil {
			liveSubnets[string(subnet.UID)] = true
		}
	}
	for name, state := range active {
		if !liveVpcs[state.uid] {
			state.speaker.Close()
			delete(active, name)
		}
	}
	if err := ophost.Cleanup(ctx, c.vswitchTables, liveVpcs, liveSubnets); err != nil {
		return err
	}
	if len(liveVpcs) == 0 {
		return nil
	}
	bootstrap, err := openperouter.ReadBootstrap(c.config.OpenPERouterBootstrap)
	if err != nil {
		return err
	}
	node, err := c.nodesLister.Get(c.config.NodeName)
	if err != nil {
		return err
	}
	index, err := strconv.Atoi(node.Annotations[openperouter.NodeIndexAnnotation])
	if err != nil || index != bootstrap.Index {
		return errors.New("node annotation and local OpenPERouter bootstrap index disagree")
	}
	if !strings.EqualFold(node.Annotations[openperouter.NodeRouterMACAnnotation], bootstrap.RouterMAC) {
		return errors.New("node router MAC annotation disagrees with local OpenPERouter bootstrap")
	}
	// Validate the complete native contract before opening either L2 or L3.
	for _, vpc := range vpcs {
		if vpc.Spec.OpenPERouter == nil {
			continue
		}
		if err := bootstrap.Validate(vpc.Spec.OpenPERouter); err != nil {
			return err
		}
		for _, subnet := range subnets {
			if subnet.Spec.Vpc == vpc.Name && subnet.Spec.OpenPERouterL2 != nil {
				if err := bootstrap.ValidateL2(vpc.Spec.OpenPERouter, subnet); err != nil {
					return err
				}
			}
		}
	}
	bound, interfaces, err := c.openPERouterBindings(ctx, reader)
	if err != nil {
		return err
	}
	// A direct local-pod snapshot is the availability lease. Informer/config
	// caches alone must not renew announcements across an API outage or reboot.
	pods, podErr := c.config.KubeClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("spec.nodeName", c.config.NodeName).String()})
	if podErr != nil {
		klog.ErrorS(podErr, "Fresh local Pod snapshot unavailable; withdrawing OpenPERouter workload routes")
	}
	now := time.Now()
	keep := make(map[string]bool)
	for _, vpc := range vpcs {
		if vpc.Spec.OpenPERouter == nil || !vpc.DeletionTimestamp.IsZero() {
			continue
		}
		keep[vpc.Name] = true
		state := active[vpc.Name]
		if state != nil && state.uid != string(vpc.UID) {
			state.speaker.Close()
			delete(active, vpc.Name)
			state = nil
		}
		var cidrs []string
		for _, subnet := range subnets {
			if subnet.Spec.Vpc == vpc.Name {
				cidrs = append(cidrs, strings.Split(subnet.Spec.CIDRBlock, ",")...)
				if subnet.Spec.OpenPERouterL2 != nil && subnet.Status.IsReady() {
					if err := ophost.EnsureL2(ctx, c.vswitchTables, vpc, subnet, bootstrap, c.config.MTU); err != nil {
						return err
					}
				}
			}
		}
		attachment, err := ophost.Ensure(ctx, c.vswitchTables, vpc, c.config.NodeName, bootstrap, c.config.MTU, cidrs)
		if err != nil {
			if state != nil {
				if withdrawErr := state.speaker.Sync(nil); withdrawErr != nil {
					return withdrawErr
				}
			}
			return err
		}
		configuration := fmt.Sprintf("%s/%d/%d/%d/%d/%s", vpc.Spec.OpenPERouter.VRF, vpc.Spec.OpenPERouter.HostASN,
			vpc.Spec.OpenPERouter.RouterASN, attachment.VRF.Index, attachment.Peer.Attrs().Index, strings.Join(attachment.Node.HostAddresses, ","))
		if state != nil && state.configuration != configuration {
			state.speaker.Close()
			delete(active, vpc.Name)
			state = nil
		}
		if _, ready := bound[attachment.Node.ReturnPort]; !ready {
			if state != nil {
				if err := state.speaker.Sync(nil); err != nil {
					return err
				}
			}
			continue
		}
		if state == nil {
			var sessions []advertiser.Session
			for i, peer := range attachment.Node.PeerAddresses {
				sessions = append(sessions, advertiser.Session{Local: netip.MustParsePrefix(attachment.Node.HostAddresses[i]).Addr(), Peer: netip.MustParseAddr(peer)})
			}
			routerID := c.config.NodeIPv4
			if routerID == "" {
				routerID = fmt.Sprintf("10.255.%d.%d", bootstrap.Index/256, bootstrap.Index%256)
			}
			speaker, err := advertiser.New(ctx, vpc.Spec.OpenPERouter.VRF, vpc.Spec.OpenPERouter.HostASN, vpc.Spec.OpenPERouter.RouterASN, routerID, sessions)
			if err != nil {
				return err
			}
			state = &openPERouterRuntime{speaker: speaker, uid: string(vpc.UID), configuration: configuration}
			active[vpc.Name] = state
		}
		state.attachment = attachment
		if podErr == nil {
			state.lease.Refresh(now, localOpenPERouterPrefixes(pods.Items, subnets, vpc.Name, bound, interfaces))
		}
		// Fail closed on API loss as well as missing local primary bindings.
		// A VRF/configuration cache must never renew workload availability.
		desired := state.lease.Desired(now, 30*time.Second, podErr == nil)
		if err := state.speaker.Sync(desired); err != nil {
			return err
		}
	}
	for name, state := range active {
		if !keep[name] {
			state.speaker.Close()
			delete(active, name)
		}
	}
	return nil
}

func (c *Controller) openPERouterBindings(ctx context.Context, reader **ovs.LocalBindingClient) (map[string]ovnsb.PortBinding, map[string]bool, error) {
	var roots []vswitch.OpenvSwitch
	if err := c.vswitchTables.Table(&vswitch.OpenvSwitch{}).List(ctx, &roots); err != nil {
		return nil, nil, err
	}
	if len(roots) != 1 {
		return nil, nil, errors.New("expected one local Open_vSwitch row")
	}
	if *reader != nil && !(*reader).Matches(roots[0].ExternalIDs["ovn-remote"], roots[0].ExternalIDs["system-id"]) {
		(*reader).Close()
		*reader = nil
	}
	if *reader == nil {
		client, err := ovs.NewLocalBindingClient(ctx, roots[0].ExternalIDs["ovn-remote"], roots[0].ExternalIDs["system-id"])
		if err != nil {
			return nil, nil, err
		}
		*reader = client
	}
	bound, err := (*reader).BoundPorts(ctx)
	if err != nil {
		(*reader).Close()
		*reader = nil
		return nil, nil, err
	}
	output, err := ovs.Appctl(ovs.OvnController, "connection-status")
	if err != nil {
		return nil, nil, fmt.Errorf("local ovn-controller connection status: %w", err)
	}
	if strings.TrimSpace(output) != "connected" {
		return nil, nil, fmt.Errorf("local ovn-controller is not connected: %s", output)
	}
	var ifaces []vswitch.Interface
	if err := c.vswitchTables.Table(&vswitch.Interface{}).List(ctx, &ifaces); err != nil {
		return nil, nil, err
	}
	installed := make(map[string]bool)
	for _, iface := range ifaces {
		if iface.Ofport != nil && *iface.Ofport > 0 && (iface.Error == nil || *iface.Error == "") && iface.ExternalIDs["ovn-installed"] == "true" {
			installed[iface.ExternalIDs["iface-id"]] = true
		}
	}
	return bound, installed, nil
}

func localOpenPERouterPrefixes(pods []corev1.Pod, subnets []*kubeovnv1.Subnet, vpc string, bound map[string]ovnsb.PortBinding, installed map[string]bool) map[netip.Prefix]struct{} {
	byName := make(map[string]*kubeovnv1.Subnet)
	for _, subnet := range subnets {
		if subnet.Spec.Vpc == vpc && subnet.Status.IsReady() && !subnet.Spec.NatOutgoing {
			byName[subnet.Name] = subnet
		}
	}
	prefixes := make(map[netip.Prefix]struct{})
	for _, pod := range pods {
		if pod.Spec.HostNetwork || pod.Status.Phase != corev1.PodRunning || !pod.DeletionTimestamp.IsZero() || !slicesPodReady(pod.Status.Conditions) {
			continue
		}
		for annotation, value := range pod.Annotations {
			provider, ok := strings.CutSuffix(annotation, ".kubernetes.io/ip_address")
			if !ok || provider == "" || pod.Annotations[provider+".kubernetes.io/allocated"] != "true" {
				continue
			}
			subnet := byName[pod.Annotations[provider+".kubernetes.io/logical_switch"]]
			if subnet == nil {
				continue
			}
			candidates := []string{ovs.PodNameToPortName(pod.Name, pod.Namespace, provider)}
			for _, owner := range pod.OwnerReferences {
				if owner.Kind == util.KindVirtualMachineInstance && strings.HasPrefix(owner.APIVersion, "kubevirt.io/") {
					candidates = append(candidates, ovs.PodNameToPortName(owner.Name, pod.Namespace, provider))
				}
			}
			for _, port := range candidates {
				binding, exists := bound[port]
				if !exists || !installed[port] {
					continue
				}
				for ip := range strings.SplitSeq(value, ",") {
					addr, err := netip.ParseAddr(strings.TrimSpace(ip))
					if err != nil || !bindingHasAddress(binding, addr) || !subnetHasAddress(subnet.Spec.CIDRBlock, addr) {
						continue
					}
					prefixes[netip.PrefixFrom(addr, addr.BitLen())] = struct{}{}
				}
			}
		}
	}
	return prefixes
}

func slicesPodReady(conditions []corev1.PodCondition) bool {
	for _, condition := range conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func bindingHasAddress(binding ovnsb.PortBinding, address netip.Addr) bool {
	for _, entry := range binding.MAC {
		for field := range strings.FieldsSeq(entry) {
			if ip, err := netip.ParseAddr(field); err == nil && ip == address {
				return true
			}
		}
	}
	return false
}

func subnetHasAddress(cidrs string, address netip.Addr) bool {
	for cidr := range strings.SplitSeq(cidrs, ",") {
		if prefix, err := netip.ParsePrefix(cidr); err == nil && prefix.Contains(address) {
			return true
		}
	}
	return false
}
