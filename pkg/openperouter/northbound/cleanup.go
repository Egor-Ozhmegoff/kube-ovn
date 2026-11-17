package northbound

import (
	"context"
	"slices"

	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/kubeovn/kube-ovn/pkg/openperouter"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnnb"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/table"
)

func Cleanup(ctx context.Context, provider table.Provider, vpcUIDs, subnetUIDs map[string]bool, guard *openperouter.MACGuard) error {
	stale := func(ids map[string]string) bool {
		uid := ids[openperouter.OwnerKey+"-uid"]
		return uid != "" && (!vpcUIDs[uid] || (ids["subnet-uid"] != "" && !subnetUIDs[ids["subnet-uid"]]))
	}
	var ports []ovnnb.LogicalSwitchPort
	if err := provider.Table(&ovnnb.LogicalSwitchPort{}).Filter(ctx, func(p *ovnnb.LogicalSwitchPort) bool { return stale(p.ExternalIDs) }, &ports); err != nil {
		return err
	}
	var switches []ovnnb.LogicalSwitch
	if err := provider.Table(&ovnnb.LogicalSwitch{}).List(ctx, &switches); err != nil {
		return err
	}
	var ops []ovsdb.Operation
	for _, port := range ports {
		for _, ls := range switches {
			if !slices.Contains(ls.Ports, port.UUID) {
				continue
			}
			mutations, err := provider.Table(&ls).MutateOps(&ls, model.Mutation{Field: &ls.Ports, Mutator: ovsdb.MutateOperationDelete, Value: []string{port.UUID}})
			if err != nil {
				return err
			}
			ops = append(ops, mutations...)
		}
	}
	var lrps []ovnnb.LogicalRouterPort
	if err := provider.Table(&ovnnb.LogicalRouterPort{}).Filter(ctx, func(p *ovnnb.LogicalRouterPort) bool { return stale(p.ExternalIDs) }, &lrps); err != nil {
		return err
	}
	var routers []ovnnb.LogicalRouter
	if err := provider.Table(&ovnnb.LogicalRouter{}).List(ctx, &routers); err != nil {
		return err
	}
	for _, lrp := range lrps {
		for _, lr := range routers {
			if !slices.Contains(lr.Ports, lrp.UUID) {
				continue
			}
			mutations, err := provider.Table(&lr).MutateOps(&lr, model.Mutation{Field: &lr.Ports, Mutator: ovsdb.MutateOperationDelete, Value: []string{lrp.UUID}})
			if err != nil {
				return err
			}
			ops = append(ops, mutations...)
		}
	}
	var acls []ovnnb.ACL
	if err := provider.Table(&ovnnb.ACL{}).Filter(ctx, func(a *ovnnb.ACL) bool { return stale(a.ExternalIDs) }, &acls); err != nil {
		return err
	}
	for _, acl := range acls {
		for _, ls := range switches {
			if !slices.Contains(ls.ACLs, acl.UUID) {
				continue
			}
			mutations, err := provider.Table(&ls).MutateOps(&ls, model.Mutation{Field: &ls.ACLs, Mutator: ovsdb.MutateOperationDelete, Value: []string{acl.UUID}})
			if err != nil {
				return err
			}
			ops = append(ops, mutations...)
		}
	}
	for _, ls := range switches {
		if stale(ls.ExternalIDs) {
			deletions, err := provider.Table(&ls).DeleteOps(&ls)
			if err != nil {
				return err
			}
			ops = append(ops, deletions...)
		}
	}
	var groups []ovnnb.PortGroup
	if err := provider.Table(&ovnnb.PortGroup{}).Filter(ctx, func(group *ovnnb.PortGroup) bool { return stale(group.ExternalIDs) }, &groups); err != nil {
		return err
	}
	for _, group := range groups {
		deletions, err := provider.Table(&group).DeleteOps(&group)
		if err != nil {
			return err
		}
		ops = append(ops, deletions...)
	}
	var sets []ovnnb.AddressSet
	if err := provider.Table(&ovnnb.AddressSet{}).Filter(ctx, func(a *ovnnb.AddressSet) bool { return stale(a.ExternalIDs) }, &sets); err != nil {
		return err
	}
	for _, set := range sets {
		deletions, err := provider.Table(&set).DeleteOps(&set)
		if err != nil {
			return err
		}
		ops = append(ops, deletions...)
		guard.Forget(set.ExternalIDs["subnet-uid"])
	}
	return provider.Table(&ovnnb.LogicalSwitch{}).Transact(ctx, "openperouter-gc", ops...)
}
