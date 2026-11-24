package daemon

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/openperouter"
	"github.com/kubeovn/kube-ovn/pkg/openperouter/barrier"
	ophost "github.com/kubeovn/kube-ovn/pkg/openperouter/host"
)

type guardRegistration struct {
	store  barrier.Store
	uid    types.UID
	vpcUID string
}
type openPERouterGuards struct {
	session string
	nodeUID types.UID
	joined  map[types.UID]guardRegistration
}

func (c *Controller) prepareOpenPERouterGuards(ctx context.Context, guards *openPERouterGuards) error {
	defer openperouter.Measure(ctx, "guard.register")()
	node, err := c.config.KubeClient.CoreV1().Nodes().Get(ctx, c.config.NodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if err := guards.bindNode(ctx, node.UID, func() error {
		return ophost.Fence(ctx, c.vswitchTables, "")
	}); err != nil {
		return err
	}
	return nil
}

func (c *Controller) registerOpenPERouterGuards(ctx context.Context, guards *openPERouterGuards, vpc *kubeovnv1.Vpc, subnets []*kubeovnv1.Subnet) error {
	for _, subnet := range subnets {
		if subnet.Spec.Vpc != vpc.Name || subnet.Spec.OpenPERouterL2 == nil || !subnet.DeletionTimestamp.IsZero() {
			continue
		}
		store := barrier.Store{Maps: c.config.KubeClient.CoreV1().ConfigMaps(c.config.PodNamespace), SubnetName: subnet.Name, SubnetUID: subnet.UID}
		cm, state, err := store.Read(ctx)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if previous, ok := guards.joined[subnet.UID]; ok && previous.uid == cm.UID {
			if state.Members[string(guards.nodeUID)].Session != guards.session {
				return fmt.Errorf("OpenPERouter guard session superseded on %s", c.config.NodeName)
			}
			continue
		}
		// Fence the old local edge before replacing an acknowledgement/session.
		if err := store.JoinAfterClose(ctx, cm.UID, guards.nodeUID, c.config.NodeName, guards.session, func() error {
			return ophost.CloseL2(subnet.Spec.OpenPERouterL2.VNI, string(vpc.UID), string(subnet.UID))
		}); err != nil {
			return err
		}
		guards.joined[subnet.UID] = guardRegistration{store: store, uid: cm.UID, vpcUID: string(vpc.UID)}
	}
	return nil
}

// Changing the Node UID in a running process must close/report its old identity
// before re-enrolling. Never use a new UID to overwrite the old member.
func (g *openPERouterGuards) bindNode(ctx context.Context, uid types.UID, closePaths func() error) error {
	if g.nodeUID != "" && g.nodeUID != uid {
		if err := closePaths(); err != nil {
			return err
		}
		if err := g.close(ctx); err != nil {
			return err
		}
		clear(g.joined)
	}
	g.nodeUID = uid
	return nil
}

func (g *openPERouterGuards) close(ctx context.Context, vpcUID ...string) error {
	var result error
	for _, registration := range g.joined {
		if len(vpcUID) > 0 && registration.vpcUID != vpcUID[0] {
			continue
		}
		if err := registration.store.Close(ctx, registration.uid, g.nodeUID, g.session); err != nil && !apierrors.IsNotFound(err) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func (c *Controller) ensureAcknowledgedL2(ctx context.Context, vpc *kubeovnv1.Vpc, subnet *kubeovnv1.Subnet, bootstrap *openperouter.Bootstrap, guards *openPERouterGuards, macs []string, fresh func() error) error {
	defer openperouter.Measure(ctx, "guard.protect-open")()
	registration, ok := guards.joined[subnet.UID]
	if !ok {
		return barrier.ErrPending
	}
	var attachment *ophost.L2Attachment
	return registration.store.ProtectAndOpen(ctx, registration.uid, guards.nodeUID, guards.session, barrier.Digest(macs), func() error {
		var err error
		attachment, err = ophost.EnsureL2(ctx, c.vswitchTables, vpc, subnet, bootstrap, c.config.MTU, macs)
		return err
	}, func() error {
		if err := fresh(); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return attachment.Open()
	})
}

// Called only after successful host cleanup proves removed subnet paths closed.
func (g *openPERouterGuards) forgetRemoved(ctx context.Context, live map[string]bool) error {
	var result error
	for subnetUID, registration := range g.joined {
		if live[string(subnetUID)] {
			continue
		}
		err := registration.store.Close(ctx, registration.uid, g.nodeUID, g.session)
		if err != nil && !apierrors.IsNotFound(err) {
			result = errors.Join(result, err)
			continue
		}
		delete(g.joined, subnetUID)
	}
	return result
}
