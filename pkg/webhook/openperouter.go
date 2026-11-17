package webhook

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/openperouter"
)

func (v *ValidatingHook) validateOpenPERouterVpc(ctx context.Context, vpc *kubeovnv1.Vpc) error {
	if vpc.Spec.OpenPERouter == nil {
		return nil
	}
	var all kubeovnv1.VpcList
	if err := v.cache.List(ctx, &all); err != nil {
		return err
	}
	others := make([]*kubeovnv1.Vpc, len(all.Items))
	for i := range all.Items {
		others[i] = &all.Items[i]
	}
	if err := openperouter.ValidateVpcConflicts(vpc, others); err != nil {
		return err
	}
	var subnets kubeovnv1.SubnetList
	if err := v.cache.List(ctx, &subnets); err != nil {
		return err
	}
	for _, subnet := range subnets.Items {
		if subnet.Spec.OpenPERouterL2 != nil && subnet.Spec.OpenPERouterL2.VNI == vpc.Spec.OpenPERouter.L3VNI {
			return fmt.Errorf("L3VNI conflicts with L2 attachment of subnet %s", subnet.Name)
		}
		if subnet.Spec.Vpc == vpc.Name {
			if err := openperouter.ValidateSubnetBinding(&subnet, vpc, subnets.Items); err != nil {
				return err
			}
		}
	}
	return nil
}

func (v *ValidatingHook) validateOpenPERouterSubnet(ctx context.Context, subnet *kubeovnv1.Subnet, others []kubeovnv1.Subnet) error {
	if subnet.Spec.Vpc == "" && subnet.Spec.OpenPERouterL2 == nil {
		return nil
	}
	vpc := &kubeovnv1.Vpc{}
	if err := v.cache.Get(ctx, types.NamespacedName{Name: subnet.Spec.Vpc}, vpc); err != nil {
		if apierrors.IsNotFound(err) && subnet.Spec.OpenPERouterL2 == nil {
			return nil
		}
		return err
	}
	if err := openperouter.ValidateSubnetBinding(subnet, vpc, others); err != nil {
		return err
	}
	if subnet.Spec.OpenPERouterL2 != nil {
		var all kubeovnv1.VpcList
		if err := v.cache.List(ctx, &all); err != nil {
			return err
		}
		for _, current := range all.Items {
			if current.Spec.OpenPERouter != nil && current.Spec.OpenPERouter.L3VNI == subnet.Spec.OpenPERouterL2.VNI {
				return fmt.Errorf("L2VNI conflicts with VPC %s L3VNI", current.Name)
			}
		}
	}
	return nil
}
