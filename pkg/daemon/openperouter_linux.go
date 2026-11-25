package daemon

import (
	"context"
	"errors"
	"fmt"
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	for {
		if ctx.Err() != nil {
			return
		}
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
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	manager := routes.Manager{Namespace: c.config.OpenPERouterNamespace}
	guards := &openPERouterGuards{session: uuidpkg.NewString(), joined: make(map[types.UID]guardRegistration)}
	var reader *ovs.LocalBindingClient
	defer func() {
		if reader != nil {
			reader.Close()
		}
	}()
	runner := c.openPERouterReconciler(manager, &reader, guards)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	runner.run(ctx, ticker.C, func(ctx context.Context) bool {
		for ctx.Err() == nil {
			startCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.closeAllOpenPERouter(startCtx, guards, manager)
			if err == nil {
				err = manager.PurgeLegacy(startCtx)
			}
			cancel()
			if err == nil {
				return cache.WaitForCacheSync(ctx.Done(), c.openPERouterVpcsSynced)
			}
			klog.ErrorS(err, "Fence persisted OpenPERouter attachments")
			select {
			case <-ctx.Done():
				return false
			case <-time.After(5 * time.Second):
			}
		}
		return false
	})
}

func (c *Controller) closeAllOpenPERouter(ctx context.Context, guards *openPERouterGuards, manager routes.Manager) error {
	fenceCtx, cancel := openPERouterFenceBudget(ctx)
	fenceErr := ophost.Fence(fenceCtx, c.vswitchTables, "")
	var guardErr error
	if fenceErr == nil {
		guardErr = guards.close(fenceCtx)
	}
	cancel()
	return errors.Join(fenceErr, guardErr, manager.Sweep(ctx, nil))
}

func (c *Controller) openPERouterReconciler(manager routes.Manager, reader **ovs.LocalBindingClient, guards *openPERouterGuards) *openPERouterReconciler {
	var snapshot *openPERouterSnapshot
	return &openPERouterReconciler{
		budgets: defaultOpenPERouterBudgets,
		prepare: func(ctx context.Context) ([]*kubeovnv1.Vpc, error) {
			ctx, timing := openperouter.WithReconcileTiming(ctx)
			started := time.Now()
			prepared, err := c.prepareOpenPERouter(ctx, manager, reader, guards)
			klog.InfoS("OpenPERouter common preparation timing", "duration", time.Since(started), "stages", timing.Snapshot(), "error", err)
			if err != nil {
				return nil, err
			}
			return prepared.vpcs, nil
		},
		refresh: func(ctx context.Context) error {
			if err := manager.CheckNamespace(ctx); err != nil {
				return err
			}
			var err error
			snapshot, err = c.readOpenPERouterSnapshot(ctx, reader, guards)
			return err
		},
		syncVpc: func(ctx context.Context, vpc *kubeovnv1.Vpc) error {
			ctx, timing := openperouter.WithReconcileTiming(ctx)
			started := time.Now()
			current := snapshot.vpc(vpc.UID)
			if current == nil {
				return errors.New("VPC removed or replaced since common preparation")
			}
			check := func() error { return c.checkOpenPERouterSnapshot(ctx, snapshot, current, reader, guards) }
			checkL2 := func() error { return c.checkOpenPERouterDataplane(ctx, snapshot, current, reader) }
			err := c.syncOpenPERouterVPC(ctx, manager, current, snapshot.subnets, snapshot.bootstrap, guards, reader, snapshot.bound, snapshot.installed, check, checkL2)
			klog.InfoS("OpenPERouter VPC reconciliation timing", "vpc", vpc.Name, "duration", time.Since(started), "stages", timing.Snapshot(), "error", err)
			return err
		},
		closeVpc: func(ctx context.Context, uid string) error { return c.closeOpenPERouterVPC(ctx, uid, guards, manager) },
		closeAll: func(ctx context.Context) error { return c.closeAllOpenPERouter(ctx, guards, manager) },
	}
}

func (c *Controller) closeOpenPERouterVPC(ctx context.Context, uid string, guards *openPERouterGuards, manager routes.Manager) error {
	fenceCtx, cancel := openPERouterFenceBudget(ctx)
	fenceErr := ophost.Fence(fenceCtx, c.vswitchTables, uid)
	var guardErr error
	if fenceErr == nil {
		guardErr = guards.close(fenceCtx, uid)
	}
	cancel()
	// The original closure budget still has time reserved for kernel withdrawal.
	return errors.Join(fenceErr, guardErr, manager.Withdraw(ctx, uid))
}

func (c *Controller) syncOpenPERouterVPC(ctx context.Context, manager routes.Manager, vpc *kubeovnv1.Vpc, subnets []*kubeovnv1.Subnet, bootstrap *openperouter.Bootstrap, guards *openPERouterGuards, reader **ovs.LocalBindingClient, bound map[string]ovnsb.PortBinding, installed map[string]bool, fresh, freshL2 func() error) error {
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
	err = manager.Reconcile(ctx, config, func(session *routes.Session) error {
		if err := fresh(); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
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
			macs, err := openPERouterReflectionMACs(ctx, *reader, openperouter.ReflectionAddressSet(string(subnet.UID), subnet.Name))
			if err != nil {
				return err
			}
			if err := c.ensureAcknowledgedL2(ctx, vpc, subnet, bootstrap, guards, macs, func() error {
				if err := freshL2(); err != nil {
					return err
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := session.Check(ctx, string(vpc.UID)); err != nil {
					return err
				}
				return attachment.Check()
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, routes.ErrNamespaceUnavailable) || errors.Is(err, routes.ErrNamespaceChanged) {
		return &openPERouterSharedError{err: err}
	}
	return err
}

func (c *Controller) readOpenPERouterBootstrap(ctx context.Context) (*openperouter.Bootstrap, types.UID, error) {
	bootstrap, err := openperouter.ReadBootstrap(c.config.OpenPERouterBootstrap)
	if err != nil {
		return nil, "", err
	}
	node, err := c.config.KubeClient.CoreV1().Nodes().Get(ctx, c.config.NodeName, metav1.GetOptions{})
	if err != nil {
		return nil, "", err
	}
	index, err := strconv.Atoi(node.Annotations[openperouter.NodeIndexAnnotation])
	if err != nil || index != bootstrap.Index {
		return nil, "", errors.New("node annotation and local OpenPERouter bootstrap index disagree")
	}
	if !strings.EqualFold(node.Annotations[openperouter.NodeRouterMACAnnotation], bootstrap.RouterMAC) {
		return nil, "", errors.New("node router MAC annotation disagrees with local OpenPERouter bootstrap")
	}
	return bootstrap, node.UID, nil
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
	if err := ovs.RefreshLocalBindingClient(ctx, reader, roots[0].ExternalIDs["ovn-remote"], roots[0].ExternalIDs["system-id"], guardNames...); err != nil {
		return nil, nil, err
	}
	bound, err := (*reader).BoundPorts(ctx)
	if err != nil {
		(*reader).Close()
		*reader = nil
		return nil, nil, err
	}
	output, err := ovs.AppctlContext(ctx, ovs.OvnController, "connection-status")
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

// Reserve half the remaining closure budget for netlink withdrawal even when
// the OVS/API fencing/ack phase consumes its entire allowance.
func openPERouterFenceBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	allowance := defaultOpenPERouterBudgets.close / 2
	if deadline, ok := ctx.Deadline(); ok {
		allowance = min(allowance, time.Until(deadline)/2)
	}
	return context.WithTimeout(ctx, allowance)
}
