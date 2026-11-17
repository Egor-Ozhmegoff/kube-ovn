package northbound

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"
	"time"

	uuidpkg "github.com/google/uuid"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/openperouter"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/ovnnb"
	"github.com/kubeovn/kube-ovn/pkg/ovsdb/table"
)

// EnsureL2 uses a localport, not localnet: OVN retains its internal tunnels.
// localport multicast output is executed on the originating chassis only;
// fabric ingress is local-only. The source-MAC fence rejects fabric copies of
// traffic which OVN already delivered through its internal overlay.
func EnsureL2(ctx context.Context, provider table.Provider, vpc *kubeovnv1.Vpc, subnet *kubeovnv1.Subnet, guard *openperouter.MACGuard, preparingMACs ...string) error {
	if err := openperouter.ValidateL2(subnet.Spec.OpenPERouterL2); err != nil {
		return err
	}
	if subnet.Spec.OpenPERouterL2 == nil || vpc.Spec.OpenPERouter == nil {
		return errors.New("native L2VNI and OpenPERouter VPC are required")
	}
	ls := &ovnnb.LogicalSwitch{Name: subnet.Name}
	if err := readRow(ctx, provider, ls); err != nil {
		return err
	}
	var ports []ovnnb.LogicalSwitchPort
	if err := provider.Table(&ovnnb.LogicalSwitchPort{}).FilterByUUIDs(ctx, func(*ovnnb.LogicalSwitchPort) bool { return true }, &ports, ls.Ports...); err != nil {
		return err
	}
	observed := slices.Clone(preparingMACs)
	for _, port := range ports {
		if port.Type != "" {
			continue
		}
		for _, address := range append(slices.Clone(port.Addresses), port.PortSecurity...) {
			if address == "unknown" {
				return fmt.Errorf("OpenPERouter L2 requires declared source MACs; port %s permits unknown sources", port.Name)
			}
			fields := strings.Fields(address)
			if len(fields) > 0 {
				if mac, err := net.ParseMAC(fields[0]); err == nil {
					observed = append(observed, mac.String())
				}
			}
		}
	}
	ids := map[string]string{openperouter.OwnerKey: vpc.Name, openperouter.OwnerKey + "-uid": string(vpc.UID), "subnet": subnet.Name, "subnet-uid": string(subnet.UID)}
	tx := nbTransaction{ctx: ctx, provider: provider, ids: ids}
	hash := sha256.Sum256([]byte(string(subnet.UID) + "/" + subnet.Name))
	as := &ovnnb.AddressSet{Name: fmt.Sprintf("oprm_%x", hash[:12])}
	existing := &ovnnb.AddressSet{Name: as.Name}
	err := provider.Table(as).Get(ctx, existing)
	if err != nil && !errors.Is(err, table.ErrNotFound) {
		return err
	}
	if err == nil && (existing.ExternalIDs["subnet-uid"] != string(subnet.UID) || existing.ExternalIDs[openperouter.OwnerKey+"-uid"] != string(vpc.UID)) {
		return fmt.Errorf("refuse to adopt MAC reflection address set %s", as.Name)
	}
	as.ExternalIDs = maps.Clone(ids)
	as.Addresses = guard.Update(string(subnet.UID), existing.Addresses, observed, time.Now())
	if errors.Is(err, table.ErrNotFound) {
		as.UUID = "op_" + strings.ReplaceAll(uuidpkg.NewString(), "-", "")
		ops, err := provider.Table(as).CreateOps(as)
		if err != nil {
			return err
		}
		tx.ops = append(tx.ops, ops...)
	} else if !sameSet(as.Addresses, existing.Addresses) {
		as.UUID = existing.UUID
		ops, err := provider.Table(as).UpdateOps(existing, as, &as.Addresses)
		if err != nil {
			return err
		}
		tx.ops = append(tx.ops, ops...)
	}
	port := &ovnnb.LogicalSwitchPort{Name: openperouter.L2Port(subnet.Name), Type: "localport", Addresses: []string{"unknown"}}
	if err := tx.row(port); err != nil {
		return err
	}
	aclID, err := tx.fence(fmt.Sprintf("oprl2_%x", hash[:12]), fmt.Sprintf("inport == %q && eth.src == $%s", port.Name, as.Name))
	if err != nil {
		return err
	}
	if !slices.Contains(ls.Ports, port.UUID) {
		ops, err := provider.Table(ls).MutateOps(ls, model.Mutation{Field: &ls.Ports, Mutator: ovsdb.MutateOperationInsert, Value: []string{port.UUID}})
		if err != nil {
			return err
		}
		tx.ops = append(tx.ops, ops...)
	}
	if !slices.Contains(ls.ACLs, aclID) {
		ops, err := provider.Table(ls).MutateOps(ls, model.Mutation{Field: &ls.ACLs, Mutator: ovsdb.MutateOperationInsert, Value: []string{aclID}})
		if err != nil {
			return err
		}
		tx.ops = append(tx.ops, ops...)
	}
	return provider.Table(ls).Transact(ctx, "openperouter-l2-fence", tx.ops...)
}

func (tx *nbTransaction) fence(name, match string) (string, error) {
	var existing []ovnnb.ACL
	if err := tx.provider.Table(&ovnnb.ACL{}).Filter(tx.ctx, func(a *ovnnb.ACL) bool { return a.Name != nil && *a.Name == name }, &existing); err != nil {
		return "", err
	}
	if len(existing) > 1 {
		return "", fmt.Errorf("duplicate ACL %s", name)
	}
	desired := &ovnnb.ACL{Name: &name, Match: match, Direction: ovnnb.ACLDirectionFromLport, Priority: 32767, Tier: 0, Action: ovnnb.ACLActionDrop, ExternalIDs: maps.Clone(tx.ids)}
	if len(existing) == 0 {
		desired.UUID = "op_" + strings.ReplaceAll(uuidpkg.NewString(), "-", "")
		ops, err := tx.provider.Table(desired).CreateOps(desired)
		tx.ops = append(tx.ops, ops...)
		return desired.UUID, err
	}
	current := &existing[0]
	if current.ExternalIDs[openperouter.OwnerKey+"-uid"] != tx.ids[openperouter.OwnerKey+"-uid"] {
		return "", fmt.Errorf("refuse to adopt ACL %s", name)
	}
	desired.UUID = current.UUID
	desired.ExternalIDs = maps.Clone(current.ExternalIDs)
	maps.Copy(desired.ExternalIDs, tx.ids)
	if current.Match == desired.Match && current.Direction == desired.Direction &&
		current.Priority == desired.Priority && current.Tier == desired.Tier &&
		current.Action == desired.Action && maps.Equal(current.ExternalIDs, desired.ExternalIDs) && maps.Equal(current.Options, desired.Options) {
		return desired.UUID, nil
	}
	ops, err := tx.provider.Table(desired).UpdateOps(current, desired, &desired.Match, &desired.Direction, &desired.Priority, &desired.Tier, &desired.Action, &desired.ExternalIDs, &desired.Options)
	tx.ops = append(tx.ops, ops...)
	return desired.UUID, err
}
