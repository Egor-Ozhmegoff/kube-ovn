package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	uuidpkg "github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/openperouter"
	ophost "github.com/kubeovn/kube-ovn/pkg/openperouter/host"
	"github.com/kubeovn/kube-ovn/pkg/openperouter/routes"
	"github.com/kubeovn/kube-ovn/pkg/ovs"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnsb"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/vswitch"
	"github.com/kubeovn/kube-ovn/pkg/util"
)

func (c *Controller) runOpenPERouter(stop <-chan struct{}) {
	for {
		lock, err := ophost.Lock()
		if err == nil {
			defer func() {
				if err := lock.Close(); err != nil {
					klog.ErrorS(err, "Release OpenPERouter owner lock")
				}
			}()
			break
		}
		klog.ErrorS(err, "Wait for OpenPERouter attachment owner")
		select {
		case <-stop:
			return
		case <-time.After(time.Second):
		}
	}

	// Fence and withdraw persisted state before trusting any API/configuration cache.
	manager := routes.Manager{Namespace: c.config.OpenPERouterNamespace}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := ophost.Fence(ctx, c.vswitchTables, "")
		if err == nil {
			err = manager.Sweep(ctx, nil)
		}
		if err == nil {
			err = manager.PurgeLegacy(ctx)
		}
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
	guards := &openPERouterGuards{session: uuidpkg.NewString(), joined: make(map[types.UID]guardRegistration)}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := ophost.Fence(ctx, c.vswitchTables, ""); err != nil {
			klog.ErrorS(err, "Fence OpenPERouter attachments on shutdown")
		} else if err := guards.close(ctx); err != nil {
			klog.ErrorS(err, "Acknowledge closed guards on shutdown")
		}
		if err := manager.Sweep(ctx, nil); err != nil {
			klog.ErrorS(err, "Withdraw PE subnet routes on shutdown")
		}
		cancel()
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
		ctx, timing := openperouter.WithReconcileTiming(ctx)
		started := time.Now()
		err := c.syncOpenPERouter(ctx, manager, &bindings, guards)
		klog.InfoS("OpenPERouter reconciliation timing", "duration", time.Since(started), "stages", timing.Snapshot(), "error", err)
		cancel()
		if err != nil {
			klog.ErrorS(err, "Reconcile local OpenPERouter integration")
			fenceCtx, fenceCancel := context.WithTimeout(context.Background(), 3*time.Second)
			if fenceErr := ophost.Fence(fenceCtx, c.vswitchTables, ""); fenceErr != nil {
				klog.ErrorS(fenceErr, "Fence OpenPERouter ingress")
			} else if err := guards.close(fenceCtx); err != nil {
				klog.ErrorS(err, "Acknowledge closed OpenPERouter guards")
			}
			if err := manager.Sweep(fenceCtx, nil); err != nil {
				klog.ErrorS(err, "Withdraw PE subnet routes")
			}
			fenceCancel()
		}
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
	}
}

func (c *Controller) syncOpenPERouter(ctx context.Context, manager routes.Manager, reader **ovs.LocalBindingClient, guards *openPERouterGuards) error {
	if c.config.NetworkType != util.NetworkTypeVxlan {
		return errors.New("OpenPERouter integration requires internal OVN VXLAN")
	}
	vpcList, err := c.config.KubeOvnClient.KubeovnV1().Vpcs().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	subnetList, err := c.config.KubeOvnClient.KubeovnV1().Subnets().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	var vpcs []*kubeovnv1.Vpc
	var subnets []*kubeovnv1.Subnet
	var keep []string
	liveVpcs, liveSubnets := make(map[string]bool), make(map[string]bool)
	for i := range vpcList.Items {
		vpc := &vpcList.Items[i]
		if vpc.Spec.OpenPERouter != nil && vpc.DeletionTimestamp.IsZero() {
			vpcs = append(vpcs, vpc)
			liveVpcs[string(vpc.UID)] = true
			keep = append(keep, string(vpc.UID))
		}
	}
	var guardNames []string
	for i := range subnetList.Items {
		subnet := &subnetList.Items[i]
		subnets = append(subnets, subnet)
		if subnet.Spec.OpenPERouterL2 != nil && subnet.DeletionTimestamp.IsZero() && liveVpcName(vpcs, subnet.Spec.Vpc) {
			liveSubnets[string(subnet.UID)] = true
			guardNames = append(guardNames, openperouter.ReflectionAddressSet(string(subnet.UID), subnet.Name))
		}
	}
	if err := manager.Sweep(ctx, keep); err != nil {
		return err
	}
	if err := ophost.Cleanup(ctx, c.vswitchTables, liveVpcs, liveSubnets); err != nil {
		return err
	}
	if err := guards.forgetRemoved(ctx, liveSubnets); err != nil {
		// Cleanup already proved these removed paths closed. Retain failed
		// registrations for retry without disrupting live VPCs.
		klog.ErrorS(err, "Retry Closed acknowledgement for removed OpenPERouter paths")
	}
	if len(vpcs) == 0 {
		return nil
	}
	if err := c.prepareOpenPERouterGuards(ctx, guards); err != nil {
		return err
	}
	bootstrap, err := c.readOpenPERouterBootstrap(ctx)
	if err != nil {
		return err
	}
	slices.Sort(guardNames)
	bound, interfaces, err := c.openPERouterBindings(ctx, reader, guardNames...)
	if err != nil {
		return err
	}
	return reconcileOpenPERouterVPCs(ctx, vpcs, func(vpc *kubeovnv1.Vpc) error {
		return c.syncOpenPERouterVPC(ctx, manager, vpc, subnets, bootstrap, guards, *reader, bound, interfaces)
	}, func(closeCtx context.Context, uid string) error {
		return c.closeOpenPERouterVPC(closeCtx, uid, guards, manager)
	})
}

func liveVpcName(vpcs []*kubeovnv1.Vpc, name string) bool {
	return slices.ContainsFunc(vpcs, func(vpc *kubeovnv1.Vpc) bool { return vpc.Name == name })
}

// A VPC-local failure must not become the global fencing trigger. Closure gets
// its own budget so a failed preparation still attempts fencing and withdrawal.
func reconcileOpenPERouterVPCs(ctx context.Context, vpcs []*kubeovnv1.Vpc, syncVpc func(*kubeovnv1.Vpc) error, closeVpc func(context.Context, string) error) error {
	for _, vpc := range vpcs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := syncVpc(vpc); err != nil {
			klog.ErrorS(err, "Reconcile OpenPERouter VPC; close only its local paths", "vpc", vpc.Name)
			closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			if err := closeVpc(closeCtx, string(vpc.UID)); err != nil {
				klog.ErrorS(err, "Close OpenPERouter VPC paths/guards/routes", "vpc", vpc.Name)
			}
			cancel()
		}
	}
	return ctx.Err()
}

func (c *Controller) closeOpenPERouterVPC(ctx context.Context, uid string, guards *openPERouterGuards, manager routes.Manager) error {
	fenceErr := ophost.Fence(ctx, c.vswitchTables, uid)
	var guardErr error
	if fenceErr == nil {
		guardErr = guards.close(ctx, uid)
	}
	// Always attempt route withdrawal, even if fencing or guard API failed.
	return errors.Join(fenceErr, guardErr, manager.Withdraw(ctx, uid))
}

func (c *Controller) syncOpenPERouterVPC(ctx context.Context, manager routes.Manager, vpc *kubeovnv1.Vpc, subnets []*kubeovnv1.Subnet, bootstrap *openperouter.Bootstrap, guards *openPERouterGuards, reader *ovs.LocalBindingClient, bound map[string]ovnsb.PortBinding, installed map[string]bool) error {
	if err := c.registerOpenPERouterGuards(ctx, guards, vpc, subnets); err != nil {
		return err
	}
	if err := bootstrap.Validate(vpc.Spec.OpenPERouter); err != nil {
		return err
	}
	var cidrs []string
	for _, subnet := range subnets {
		if subnet.Spec.Vpc != vpc.Name || !subnet.DeletionTimestamp.IsZero() || !subnet.Status.IsReady() || !util.IsOvnProvider(subnet.Spec.Provider) {
			continue
		}
		if err := bootstrap.ValidateL2(vpc.Spec.OpenPERouter, subnet); err != nil {
			return err
		}
		cidrs = append(cidrs, strings.Split(subnet.Spec.CIDRBlock, ",")...)
	}
	prefixes, err := routes.RoutedSubnetPrefixes(vpc, subnets)
	if err != nil {
		return err
	}
	attachment, err := ophost.Ensure(ctx, c.vswitchTables, vpc, c.config.NodeName, bootstrap, c.config.MTU, cidrs)
	if err != nil {
		return err
	}
	if err := openPERouterReturnReady(attachment.Node.ReturnPort, bound, installed); err != nil {
		return err
	}
	config := routes.Attachment{
		UID: string(vpc.UID), VRF: vpc.Spec.OpenPERouter.VRF,
		Device: fmt.Sprintf("pe-e-%d", vpc.Spec.OpenPERouter.L3VNI), HostIndex: attachment.Peer.Attrs().Index,
		HostAddresses: attachment.Node.HostAddresses, PeerAddresses: attachment.Node.PeerAddresses, Prefixes: prefixes,
	}
	return manager.Reconcile(ctx, config, func(session *routes.Session) error {
		if err := session.Check(ctx, string(vpc.UID)); err != nil {
			return err
		}
		if err := attachment.Open(); err != nil {
			return err
		}
		for _, subnet := range subnets {
			if subnet.Spec.Vpc != vpc.Name || subnet.Spec.OpenPERouterL2 == nil || !subnet.DeletionTimestamp.IsZero() || !subnet.Status.IsReady() {
				continue
			}
			macs, err := reader.ReflectionMACs(ctx, openperouter.ReflectionAddressSet(string(subnet.UID), subnet.Name))
			if err != nil {
				return err
			}
			if err := c.ensureAcknowledgedL2(ctx, vpc, subnet, bootstrap, guards, macs); err != nil {
				return err
			}
		}
		return nil
	})
}

func (c *Controller) readOpenPERouterBootstrap(ctx context.Context) (*openperouter.Bootstrap, error) {
	bootstrap, err := openperouter.ReadBootstrap(c.config.OpenPERouterBootstrap)
	if err != nil {
		return nil, err
	}
	node, err := c.config.KubeClient.CoreV1().Nodes().Get(ctx, c.config.NodeName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	index, err := strconv.Atoi(node.Annotations[openperouter.NodeIndexAnnotation])
	if err != nil || index != bootstrap.Index {
		return nil, errors.New("node annotation and local OpenPERouter bootstrap index disagree")
	}
	if !strings.EqualFold(node.Annotations[openperouter.NodeRouterMACAnnotation], bootstrap.RouterMAC) {
		return nil, errors.New("node router MAC annotation disagrees with local OpenPERouter bootstrap")
	}
	return bootstrap, nil
}

func (c *Controller) openPERouterBindings(ctx context.Context, reader **ovs.LocalBindingClient, guardNames ...string) (map[string]ovnsb.PortBinding, map[string]bool, error) {
	defer openperouter.Measure(ctx, "ovs.bindings")()
	var roots []vswitch.OpenvSwitch
	if err := c.vswitchTables.Table(&vswitch.OpenvSwitch{}).List(ctx, &roots); err != nil {
		return nil, nil, err
	}
	if len(roots) != 1 {
		return nil, nil, errors.New("expected one local Open_vSwitch row")
	}
	if *reader != nil && !(*reader).Matches(roots[0].ExternalIDs["ovn-remote"], roots[0].ExternalIDs["system-id"], guardNames...) {
		(*reader).Close()
		*reader = nil
	}
	if *reader == nil {
		client, err := ovs.NewLocalBindingClient(ctx, roots[0].ExternalIDs["ovn-remote"], roots[0].ExternalIDs["system-id"], guardNames...)
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

func openPERouterReturnReady(port string, bound map[string]ovnsb.PortBinding, installed map[string]bool) error {
	if _, ready := bound[port]; !ready || !installed[port] {
		return fmt.Errorf("local OVN return port %s is not bound and installed", port)
	}
	return nil
}
