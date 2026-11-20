package daemon

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kubeovn/kube-ovn/pkg/openperouter/barrier"
)

func TestOpenPERouterNodeUIDChangeClosesOldIdentityFirst(t *testing.T) {
	ctx := t.Context()
	data, err := json.Marshal(barrier.State{SubnetUID: "subnet", Revision: "r1", Members: map[string]barrier.Member{
		"old-node": {Name: "node", Session: "session", Phase: barrier.Active, Revision: "r1"},
	}})
	require.NoError(t, err)
	cm := &corev1.ConfigMap{Name: barrier.Name("subnet"), Namespace: "kube-system", UID: "barrier", ResourceVersion: "1", OwnerReferences: []metav1.OwnerReference{{APIVersion: "kubeovn.io/v1", Kind: "Subnet", Name: "blue", UID: "subnet"}}, Data: map[string]string{"state": string(data)}}
	client := fake.NewClientset(cm)
	store := barrier.Store{Maps: client.CoreV1().ConfigMaps(cm.Namespace), SubnetName: "blue", SubnetUID: "subnet"}
	guards := &openPERouterGuards{session: "session", nodeUID: "old-node", joined: map[types.UID]guardRegistration{
		"subnet": {store: store, uid: cm.UID, vpcUID: "vpc"},
	}}
	failure := errors.New("fence failed")
	require.ErrorIs(t, guards.bindNode(ctx, "new-node", func() error { return failure }), failure)
	require.Equal(t, types.UID("old-node"), guards.nodeUID)
	_, state, err := store.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, barrier.Active, state.Members["old-node"].Phase, "failed closure must not publish Closed")
	require.NoError(t, guards.bindNode(ctx, "new-node", func() error {
		require.Equal(t, types.UID("old-node"), guards.nodeUID)
		return nil
	}))
	require.Empty(t, guards.joined)
	require.Equal(t, types.UID("new-node"), guards.nodeUID)
	_, state, err = store.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, barrier.Closed, state.Members["old-node"].Phase)
	require.NoError(t, store.JoinAfterClose(ctx, cm.UID, "new-node", "node", "session", func() error { return nil }))
	_, state, err = store.Read(ctx)
	require.NoError(t, err)
	require.Len(t, state.Members, 2)
}
