package northbound

import (
	"context"
	"fmt"

	"github.com/ovn-kubernetes/libovsdb/model"

	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnnb"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/table"
)

// Router and switch names are not unique schema indexes. Use the repository's
// named-row lookup so duplicates are rejected rather than accidentally adopted.
func readRow(ctx context.Context, provider table.Provider, row model.Model) error {
	switch r := row.(type) {
	case *ovnnb.LogicalRouter:
		return readNamed(ctx, provider, r, r.Name, func(r *ovnnb.LogicalRouter) string { return r.Name })
	case *ovnnb.LogicalSwitch:
		return readNamed(ctx, provider, r, r.Name, func(r *ovnnb.LogicalSwitch) string { return r.Name })
	case *ovnnb.LogicalSwitchPort:
		return readNamed(ctx, provider, r, r.Name, func(r *ovnnb.LogicalSwitchPort) string { return r.Name })
	case *ovnnb.LogicalRouterPort:
		return readNamed(ctx, provider, r, r.Name, func(r *ovnnb.LogicalRouterPort) string { return r.Name })
	case *ovnnb.PortGroup:
		return readNamed(ctx, provider, r, r.Name, func(r *ovnnb.PortGroup) string { return r.Name })
	default:
		return fmt.Errorf("unsupported named OpenPERouter row %T", row)
	}
}

func readNamed[T any](ctx context.Context, provider table.Provider, row *T, name string, nameOf func(*T) string) error {
	result, err := table.GetByName(ctx, provider, row, name, "OpenPERouter row", true, nameOf)
	if err != nil {
		return err
	}
	if result == nil {
		return table.ErrNotFound
	}
	*row = *result
	return nil
}
