package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/openperouter"
	"github.com/kubeovn/kube-ovn/pkg/openperouter/barrier"
	opnb "github.com/kubeovn/kube-ovn/pkg/openperouter/northbound"
	"github.com/kubeovn/kube-ovn/pkg/util"
)

func (c *Controller) reconcileOpenPERouterVpc(vpc *kubeovnv1.Vpc) ([]*kubeovnv1.StaticRoute, error) {
	if vpc.Spec.OpenPERouter == nil {
		return nil, nil
	}
	if !c.config.EnableOpenPERouter {
		return nil, errors.New("OpenPERouter integration requires --enable-openperouter")
	}
	if c.config.NetworkType == util.NetworkTypeVlan {
		return nil, errors.New("OpenPERouter requires OVN overlay networking")
	}
	if vpc.Name == c.config.ClusterRouter {
		return nil, errors.New("OpenPERouter tenant integration cannot use the management/default VPC")
	}
	if err := util.ValidateVpc(vpc); err != nil {
		return nil, err
	}
	vpcs, err := c.vpcsLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	if err := openperouter.ValidateVpcConflicts(vpc, vpcs); err != nil {
		return nil, err
	}
	nodes, err := c.nodesLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	subnets, err := c.subnetsLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	var returnCIDRs []string
	for _, subnet := range subnets {
		if subnet.Spec.Vpc == vpc.Name {
			if err := openperouter.ValidateSubnetBinding(subnet, vpc, nil); err != nil {
				return nil, err
			}
			returnCIDRs = append(returnCIDRs, strings.Split(subnet.Spec.CIDRBlock, ",")...)
		}
	}
	routes, err := opnb.EnsureTransit(context.Background(), c.OVNNbTables, vpc, nodes, returnCIDRs...)
	if err != nil {
		return nil, fmt.Errorf("reconcile OpenPERouter VPC %s: %w", vpc.Name, err)
	}
	for _, subnet := range subnets {
		if subnet.Spec.Vpc == vpc.Name && subnet.Spec.OpenPERouterL2 != nil && subnet.Status.IsReady() {
			if err := c.prepareOpenPERouterL2(subnet); err != nil {
				return nil, err
			}
		}
	}
	result := make([]*kubeovnv1.StaticRoute, 0, len(routes))
	for _, route := range routes {
		result = append(result, &kubeovnv1.StaticRoute{Policy: kubeovnv1.PolicyDst, CIDR: route.Prefix, NextHopIP: route.NextHop})
	}
	return result, nil
}

func (c *Controller) prepareOpenPERouterL2(subnet *kubeovnv1.Subnet, preparingMACs ...string) error {
	if subnet.Spec.OpenPERouterL2 == nil {
		return nil
	}
	if !c.config.EnableOpenPERouter {
		return errors.New("OpenPERouter L2 requires --enable-openperouter")
	}
	vpc, err := c.vpcsLister.Get(subnet.Spec.Vpc)
	if err != nil {
		return err
	}
	c.openPERouterL2KeyMutex.LockKey(subnet.Name)
	defer func() { _ = c.openPERouterL2KeyMutex.UnlockKey(subnet.Name) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := opnb.EnsureL2(ctx, c.OVNNbTables, vpc, subnet, &c.openPERouterMACGuard, preparingMACs...); err != nil {
		return err
	}
	store := barrier.Store{Maps: c.config.KubeClient.CoreV1().ConfigMaps(c.config.PodNamespace), SubnetName: subnet.Name, SubnetUID: subnet.UID}
	return opnb.AdmitMACs(ctx, c.OVNNbTables, subnet, store, preparingMACs...)
}

// Node attachment changes do not require an external-route copy in OVN or a
// write to a Kubernetes status object for every BGP update.
func (c *Controller) reconcileOpenPERouterAttachments() {
	if !c.config.EnableOpenPERouter {
		return
	}
	vpcs, err := c.vpcsLister.List(labels.Everything())
	if err != nil {
		klog.ErrorS(err, "List OpenPERouter VPCs")
		return
	}
	subnets, err := c.subnetsLister.List(labels.Everything())
	if err != nil {
		klog.ErrorS(err, "List OpenPERouter subnets")
		return
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
	if err := opnb.Cleanup(context.Background(), c.OVNNbTables, liveVpcs, liveSubnets, &c.openPERouterMACGuard); err != nil {
		klog.ErrorS(err, "Garbage collect owned OpenPERouter objects")
		return
	}
	for _, vpc := range vpcs {
		if vpc.Spec.OpenPERouter == nil || !vpc.DeletionTimestamp.IsZero() {
			continue
		}
		c.vpcKeyMutex.LockKey(vpc.Name)
		_, err := c.reconcileOpenPERouterVpc(vpc)
		_ = c.vpcKeyMutex.UnlockKey(vpc.Name)
		if err != nil {
			klog.ErrorS(err, "Reconcile OpenPERouter attachments", "vpc", vpc.Name)
		}
	}
}

func (c *Controller) keepOpenPERouterReturnPort(name, vpcName, uid string) bool {
	if vpcName == "" || uid == "" || c.vpcsLister == nil {
		return false
	}
	vpc, err := c.vpcsLister.Get(vpcName)
	if err != nil || vpc.Spec.OpenPERouter == nil || vpc.Spec.OpenPERouter.Mode != openperouter.Mode || string(vpc.UID) != uid {
		return false
	}
	prefix := "oprb-" + vpc.Name + "-"
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	_, err = c.nodesLister.Get(strings.TrimPrefix(name, prefix))
	return err == nil
}

func (c *Controller) keepOpenPERouterSwitch(name string) bool {
	if !strings.HasPrefix(name, "oprt-") || c.vpcsLister == nil {
		return false
	}
	vpc, err := c.vpcsLister.Get(strings.TrimPrefix(name, "oprt-"))
	if err != nil || vpc.Spec.OpenPERouter == nil {
		return false
	}
	ls, err := c.getLogicalSwitch(name, true)
	return err == nil && ls != nil && ls.ExternalIDs[openperouter.OwnerKey+"-uid"] == string(vpc.UID)
}
