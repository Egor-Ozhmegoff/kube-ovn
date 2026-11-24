package daemon

import (
	"context"
	"errors"
	"reflect"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/openperouter"
	ophost "github.com/kubeovn/kube-ovn/pkg/openperouter/host"
	"github.com/kubeovn/kube-ovn/pkg/openperouter/routes"
	"github.com/kubeovn/kube-ovn/pkg/ovs"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnsb"
	"github.com/kubeovn/kube-ovn/pkg/util"
)

type openPERouterSnapshot struct {
	vpcs                  []*kubeovnv1.Vpc
	subnets               []*kubeovnv1.Subnet
	liveVpcs, liveSubnets map[string]bool
	keep, guardNames      []string
	bootstrap             *openperouter.Bootstrap
	nodeUID               types.UID
	reader                *ovs.LocalBindingClient
	bound                 map[string]ovnsb.PortBinding
	installed             map[string]bool
}

func (s *openPERouterSnapshot) vpc(uid types.UID) *kubeovnv1.Vpc {
	for _, vpc := range s.vpcs {
		if vpc.UID == uid {
			return vpc
		}
	}
	return nil
}

func (c *Controller) readOpenPERouterNetworks(ctx context.Context) (*openPERouterSnapshot, error) {
	if c.config.NetworkType != util.NetworkTypeVxlan {
		return nil, errors.New("OpenPERouter integration requires internal OVN VXLAN")
	}
	vpcs, err := c.config.KubeOvnClient.KubeovnV1().Vpcs().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	subnets, err := c.config.KubeOvnClient.KubeovnV1().Subnets().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	s := &openPERouterSnapshot{liveVpcs: make(map[string]bool), liveSubnets: make(map[string]bool)}
	names := make(map[string]bool)
	for i := range vpcs.Items {
		vpc := &vpcs.Items[i]
		if vpc.Spec.OpenPERouter != nil && vpc.DeletionTimestamp.IsZero() {
			s.vpcs = append(s.vpcs, vpc)
			s.liveVpcs[string(vpc.UID)] = true
			s.keep = append(s.keep, string(vpc.UID))
			names[vpc.Name] = true
		}
	}
	for i := range subnets.Items {
		subnet := &subnets.Items[i]
		s.subnets = append(s.subnets, subnet)
		if subnet.Spec.OpenPERouterL2 != nil && subnet.DeletionTimestamp.IsZero() && names[subnet.Spec.Vpc] {
			s.liveSubnets[string(subnet.UID)] = true
			s.guardNames = append(s.guardNames, openperouter.ReflectionAddressSet(string(subnet.UID), subnet.Name))
		}
	}
	slices.Sort(s.guardNames)
	return s, nil
}

func (c *Controller) prepareOpenPERouter(ctx context.Context, manager routes.Manager, reader **ovs.LocalBindingClient, guards *openPERouterGuards) (*openPERouterSnapshot, error) {
	s, err := c.readOpenPERouterNetworks(ctx)
	if err != nil {
		return nil, err
	}
	if err := manager.Sweep(ctx, s.keep); err != nil {
		return nil, err
	}
	if err := ophost.Cleanup(ctx, c.vswitchTables, s.liveVpcs, s.liveSubnets); err != nil {
		return nil, err
	}
	if err := guards.forgetRemoved(ctx, s.liveSubnets); err != nil {
		klog.ErrorS(err, "Retry Closed acknowledgement for removed OpenPERouter paths")
	}
	if len(s.vpcs) != 0 {
		if err := c.prepareOpenPERouterGuards(ctx, guards); err != nil {
			return nil, err
		}
		if err := c.completeOpenPERouterSnapshot(ctx, s, reader, guards); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (c *Controller) completeOpenPERouterSnapshot(ctx context.Context, s *openPERouterSnapshot, reader **ovs.LocalBindingClient, guards *openPERouterGuards) error {
	var err error
	s.bootstrap, s.nodeUID, err = c.readOpenPERouterBootstrap(ctx)
	if err != nil {
		return err
	}
	if s.nodeUID != guards.nodeUID {
		return errors.New("OpenPERouter node identity changed during snapshot")
	}
	s.bound, s.installed, err = c.openPERouterBindings(ctx, reader, s.guardNames...)
	s.reader = *reader
	return err
}

func (c *Controller) readOpenPERouterSnapshot(ctx context.Context, reader **ovs.LocalBindingClient, guards *openPERouterGuards) (*openPERouterSnapshot, error) {
	s, err := c.readOpenPERouterNetworks(ctx)
	if err == nil && len(s.vpcs) != 0 {
		err = c.completeOpenPERouterSnapshot(ctx, s, reader, guards)
	}
	return s, err
}

// Only network-relevant state is compared. Pod-driven IP usage/status counters
// and unrelated resourceVersion updates must not flap a configured subnet.
func sameOpenPERouterNetworks(a, b *openPERouterSnapshot, uid types.UID) bool {
	av, bv := a.vpc(uid), b.vpc(uid)
	if av == nil || bv == nil || !reflect.DeepEqual(av.Spec, bv.Spec) {
		return false
	}
	type subnetState struct {
		name           string
		spec           kubeovnv1.SubnetSpec
		ready, deleted bool
	}
	selectSubnets := func(s *openPERouterSnapshot, name string) map[types.UID]subnetState {
		result := make(map[types.UID]subnetState)
		for _, subnet := range s.subnets {
			if subnet.Spec.Vpc == name {
				result[subnet.UID] = subnetState{subnet.Name, subnet.Spec, subnet.Status.IsReady(), !subnet.DeletionTimestamp.IsZero()}
			}
		}
		return result
	}
	return reflect.DeepEqual(selectSubnets(a, av.Name), selectSubnets(b, bv.Name))
}

func (c *Controller) checkOpenPERouterSnapshot(ctx context.Context, expected *openPERouterSnapshot, vpc *kubeovnv1.Vpc, reader **ovs.LocalBindingClient, guards *openPERouterGuards) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := c.readOpenPERouterSnapshot(ctx, reader, guards)
	if err != nil {
		return &openPERouterSharedError{err: err}
	}
	return validateOpenPERouterSnapshot(expected, current, vpc, c.config.NodeName)
}

func validateOpenPERouterSnapshot(expected, current *openPERouterSnapshot, vpc *kubeovnv1.Vpc, nodeName string) error {
	if !sameOpenPERouterNetworks(expected, current, vpc.UID) {
		return errors.New("OpenPERouter VPC/subnet configuration changed before Open")
	}
	if expected.nodeUID != current.nodeUID || expected.reader != current.reader || expected.bootstrap.Index != current.bootstrap.Index || expected.bootstrap.RouterMAC != current.bootstrap.RouterMAC {
		return &openPERouterSharedError{err: errors.New("OpenPERouter node/OVN connection generation changed before Open")}
	}
	if err := current.bootstrap.Validate(vpc.Spec.OpenPERouter); err != nil {
		return err
	}
	for _, subnet := range current.subnets {
		if subnet.Spec.Vpc == vpc.Name && subnet.DeletionTimestamp.IsZero() {
			if err := current.bootstrap.ValidateL2(vpc.Spec.OpenPERouter, subnet); err != nil {
				return err
			}
		}
	}
	node, err := openperouter.BuildNode(vpc.Name, nodeName, current.bootstrap.Index, vpc.Spec.OpenPERouter)
	if err != nil {
		return err
	}
	return openPERouterReturnReady(node.ReturnPort, current.bound, current.installed)
}

// Configuration was refreshed immediately before L3 Open in this same bounded
// VPC attempt. Each L2 edge still checks live SB/OVS and native generations;
// avoid cluster-wide API Lists once per L2VNI just to reuse that configuration.
func (c *Controller) checkOpenPERouterDataplane(ctx context.Context, expected *openPERouterSnapshot, vpc *kubeovnv1.Vpc, reader **ovs.LocalBindingClient) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	bound, installed, err := c.openPERouterBindings(ctx, reader, expected.guardNames...)
	if err != nil {
		return &openPERouterSharedError{err: err}
	}
	if *reader != expected.reader {
		return &openPERouterSharedError{err: errors.New("OVN connection generation changed before L2 Open")}
	}
	node, err := openperouter.BuildNode(vpc.Name, c.config.NodeName, expected.bootstrap.Index, vpc.Spec.OpenPERouter)
	if err != nil {
		return err
	}
	return openPERouterReturnReady(node.ReturnPort, bound, installed)
}
