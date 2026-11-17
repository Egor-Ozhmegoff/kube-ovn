package webhook

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"

	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	ovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/util"
)

func (v *ValidatingHook) VpcCreateHook(ctx context.Context, req admission.Request) admission.Response {
	vpc := ovnv1.Vpc{}
	if err := v.decoder.DecodeRaw(req.Object, &vpc); err != nil {
		return ctrlwebhook.Errored(http.StatusBadRequest, err)
	}

	subnetList := &ovnv1.SubnetList{}
	if err := v.cache.List(ctx, subnetList); err != nil {
		return ctrlwebhook.Errored(http.StatusBadRequest, err)
	}
	for _, item := range subnetList.Items {
		if item.Name == vpc.Name {
			err := errors.New("vpc and subnet cannot have the same name")
			return ctrlwebhook.Errored(http.StatusBadRequest, err)
		}
	}

	if err := util.ValidateVpc(&vpc); err != nil {
		return ctrlwebhook.Errored(http.StatusBadRequest, err)
	}
	if err := v.validateOpenPERouterVpc(ctx, &vpc); err != nil {
		return ctrlwebhook.Denied(err.Error())
	}

	return ctrlwebhook.Allowed("bypass")
}

func (v *ValidatingHook) VpcUpdateHook(ctx context.Context, req admission.Request) admission.Response {
	vpc := ovnv1.Vpc{}
	if err := v.decoder.DecodeRaw(req.Object, &vpc); err != nil {
		return ctrlwebhook.Errored(http.StatusBadRequest, err)
	}

	if err := util.ValidateVpc(&vpc); err != nil {
		return ctrlwebhook.Errored(http.StatusBadRequest, err)
	}
	var old ovnv1.Vpc
	if len(req.OldObject.Raw) != 0 {
		if err := v.decoder.DecodeRaw(req.OldObject, &old); err != nil {
			return ctrlwebhook.Errored(http.StatusBadRequest, err)
		}
	}
	if !reflect.DeepEqual(old.Spec.OpenPERouter, vpc.Spec.OpenPERouter) &&
		(old.Spec.OpenPERouter != nil || len(old.Status.Subnets) != 0) {
		return ctrlwebhook.Denied("OpenPERouter settings are immutable; configure a new VPC before creating its subnets")
	}
	if err := v.validateOpenPERouterVpc(ctx, &vpc); err != nil {
		return ctrlwebhook.Denied(err.Error())
	}

	return ctrlwebhook.Allowed("bypass")
}

func (v *ValidatingHook) VpcDeleteHook(ctx context.Context, req admission.Request) admission.Response {
	vpc := ovnv1.Vpc{}
	if err := v.decoder.DecodeRaw(req.OldObject, &vpc); err != nil {
		return ctrlwebhook.Errored(http.StatusBadRequest, err)
	}
	if len(vpc.Status.Subnets) != 0 {
		return ctrlwebhook.Denied("can't delete vpc when any subnet in the vpc")
	}

	dnatList := &ovnv1.OvnDnatRuleList{}
	if err := v.cache.List(ctx, dnatList); err != nil {
		return ctrlwebhook.Errored(http.StatusInternalServerError, err)
	}
	// Use Status.Vpc first as vpc may be inferred from spec.ipName if not in spec.
	for _, item := range dnatList.Items {
		referencedVpc := item.Status.Vpc
		if referencedVpc == "" {
			referencedVpc = item.Spec.Vpc
		}
		if referencedVpc == vpc.Name {
			return ctrlwebhook.Denied(fmt.Sprintf("can't delete vpc %q: still referenced by OvnDnatRule %q", vpc.Name, item.Name))
		}
	}

	snatList := &ovnv1.OvnSnatRuleList{}
	if err := v.cache.List(ctx, snatList); err != nil {
		return ctrlwebhook.Errored(http.StatusInternalServerError, err)
	}
	// Use Status.Vpc first as vpc may be inferred from spec.ipName if not in spec.
	for _, item := range snatList.Items {
		referencedVpc := item.Status.Vpc
		if referencedVpc == "" {
			referencedVpc = item.Spec.Vpc
		}
		if referencedVpc == vpc.Name {
			return ctrlwebhook.Denied(fmt.Sprintf("can't delete vpc %q: still referenced by OvnSnatRule %q", vpc.Name, item.Name))
		}
	}

	fipList := &ovnv1.OvnFipList{}
	if err := v.cache.List(ctx, fipList); err != nil {
		return ctrlwebhook.Errored(http.StatusInternalServerError, err)
	}
	// Use Status.Vpc first as vpc may be inferred from spec.ipName if not in spec.
	for _, item := range fipList.Items {
		referencedVpc := item.Status.Vpc
		if referencedVpc == "" {
			referencedVpc = item.Spec.Vpc
		}
		if referencedVpc == vpc.Name {
			return ctrlwebhook.Denied(fmt.Sprintf("can't delete vpc %q: still referenced by OvnFip %q", vpc.Name, item.Name))
		}
	}

	return ctrlwebhook.Allowed("bypass")
}
