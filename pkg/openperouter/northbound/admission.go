package northbound

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/openperouter"
	"github.com/kubeovn/kube-ovn/pkg/openperouter/barrier"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnnb"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/table"
)

// ReflectionMACs waits for this connection to observe its own monotonic insert.
// This is a DB observation, not the host-filter readiness acknowledgement.
func ReflectionMACs(ctx context.Context, provider table.Provider, subnet *kubeovnv1.Subnet, required ...string) ([]string, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		row := &ovnnb.AddressSet{Name: openperouter.ReflectionAddressSet(string(subnet.UID), subnet.Name)}
		if err := provider.Table(row).Get(ctx, row); err != nil {
			return nil, err
		}
		ready := true
		for _, mac := range required {
			ready = ready && slices.Contains(row.Addresses, mac)
		}
		if ready {
			return row.Addresses, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func ProtectedMACs(ctx context.Context, provider table.Provider, subnet *kubeovnv1.Subnet, macs []string) (bool, error) {
	row := &ovnnb.AddressSet{Name: openperouter.ReflectionAddressSet(string(subnet.UID), subnet.Name) + "_ready"}
	if err := provider.Table(row).Get(ctx, row); err != nil {
		if errors.Is(err, table.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if row.ExternalIDs["subnet-uid"] != string(subnet.UID) {
		return false, fmt.Errorf("foreign admitted MAC set %s", row.Name)
	}
	for _, mac := range macs {
		if !slices.Contains(row.Addresses, mac) {
			return false, nil
		}
	}
	return true, nil
}

// ApproveMACs is called only after the distributed barrier. Keeping admitted
// identities separately lets constant-MAC migration bypass unrelated new MACs.
func ApproveMACs(ctx context.Context, provider table.Provider, subnet *kubeovnv1.Subnet, macs []string) error {
	row := &ovnnb.AddressSet{Name: openperouter.ReflectionAddressSet(string(subnet.UID), subnet.Name) + "_ready"}
	err := provider.Table(row).Get(ctx, row)
	if errors.Is(err, table.ErrNotFound) {
		row.Addresses = macs
		source := &ovnnb.AddressSet{Name: openperouter.ReflectionAddressSet(string(subnet.UID), subnet.Name)}
		if err := provider.Table(source).Get(ctx, source); err != nil {
			return err
		}
		row.ExternalIDs = maps.Clone(source.ExternalIDs)
		return provider.Table(row).Create(ctx, "openperouter-admitted-macs", row)
	}
	if err != nil {
		return err
	}
	if row.ExternalIDs["subnet-uid"] != string(subnet.UID) {
		return fmt.Errorf("foreign admitted MAC set %s", row.Name)
	}
	return provider.Table(row).Mutate(ctx, "openperouter-admitted-macs", row, model.Mutation{Field: &row.Addresses, Mutator: ovsdb.MutateOperationInsert, Value: macs})
}

// AdmitMACs must succeed before creating a workload port. Required membership
// is registered by daemons; disconnected members remain until proven closed.
func AdmitMACs(ctx context.Context, provider table.Provider, subnet *kubeovnv1.Subnet, store barrier.Store, required ...string) error {
	macs, err := ReflectionMACs(ctx, provider, subnet, required...)
	if err != nil {
		return err
	}
	revision := barrier.Digest(macs)
	if err := publishProtection(ctx, provider, subnet, store, revision); err != nil {
		return err
	}
	if len(required) == 0 {
		return nil
	}
	protected, err := ProtectedMACs(ctx, provider, subnet, required)
	if err != nil || protected {
		return err
	}
	if err := store.Ready(ctx, revision); err != nil {
		return err
	}
	return ApproveMACs(ctx, provider, subnet, macs)
}

// Pin the barrier generation in NB as well. Losing the ConfigMap must not
// forget a disconnected member which could still forward with an old filter.
func publishProtection(ctx context.Context, provider table.Provider, subnet *kubeovnv1.Subnet, store barrier.Store, revision string) error {
	row := &ovnnb.AddressSet{Name: openperouter.ReflectionAddressSet(string(subnet.UID), subnet.Name)}
	if err := provider.Table(row).Get(ctx, row); err != nil {
		return err
	}
	previous := row.ExternalIDs["barrier-uid"]
	cm, _, err := store.Read(ctx)
	if previous != "" && (apierrors.IsNotFound(err) || (err == nil && string(cm.UID) != previous)) {
		return fmt.Errorf("%w: reflection barrier generation lost; close all old attachments before operator recovery", barrier.ErrPending)
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err := store.Publish(ctx, revision); err != nil {
		return err
	}
	if previous != "" {
		return nil
	}
	cm, _, err = store.Read(ctx)
	if err != nil {
		return err
	}
	return provider.Table(row).Mutate(ctx, "openperouter-barrier-generation", row, model.Mutation{Field: &row.ExternalIDs, Mutator: ovsdb.MutateOperationInsert, Value: map[string]string{"barrier-uid": string(cm.UID)}})
}
