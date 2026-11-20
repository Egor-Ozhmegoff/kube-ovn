package barrier

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

func TestDelayedAcknowledgementAndRestart(t *testing.T) {
	ctx := t.Context()
	maps := &memoryMaps{}
	store := Store{Maps: maps, SubnetName: "blue", SubnetUID: "subnet-1"}
	nodes := []corev1.Node{{Name: "a", UID: "node-a"}, {Name: "b", UID: "node-b"}}
	r1 := Digest([]string{"02:00:00:00:00:01"})
	require.NoError(t, store.Publish(ctx, r1))
	cm, _, err := store.Read(ctx)
	require.NoError(t, err)
	require.NoError(t, store.Ready(ctx, r1), "no registered attachments yet")
	for _, node := range nodes {
		require.NoError(t, store.JoinClosed(ctx, cm.UID, node.UID, node.Name, "session-1"))
		require.NoError(t, store.InstallIntent(ctx, cm.UID, node.UID, "session-1", r1))
		require.NoError(t, store.Acknowledge(ctx, cm.UID, node.UID, "session-1", r1))
	}
	require.NoError(t, store.Ready(ctx, r1))
	r2 := Digest([]string{"02:00:00:00:00:01", "02:00:00:00:00:02"})
	require.NoError(t, store.Publish(ctx, r2))
	require.NoError(t, store.Acknowledge(ctx, cm.UID, "node-a", "session-1", r2))
	require.ErrorIs(t, store.Ready(ctx, r2), ErrPending, "B's old filter does not authorize endpoint activation")
	require.ErrorIs(t, store.Acknowledge(ctx, cm.UID, "node-b", "session-1", r1), ErrPending, "delayed old-revision ACK is rejected")
	require.NoError(t, store.Acknowledge(ctx, cm.UID, "node-b", "session-1", r2))
	require.NoError(t, store.Ready(ctx, r2))
	require.NoError(t, store.JoinClosed(ctx, cm.UID, "node-b", "b", "session-2"))
	require.ErrorIs(t, store.Acknowledge(ctx, cm.UID, "node-b", "session-1", r2), ErrPending, "old process cannot acknowledge after restart")
	require.NoError(t, store.Ready(ctx, r2), "the restarted member is physically closed")
	require.NoError(t, store.InstallIntent(ctx, cm.UID, "node-b", "session-2", r2))
	require.ErrorIs(t, store.Ready(ctx, r2), ErrPending, "opening intent must stop admission until the filter is installed")
	require.NoError(t, store.Acknowledge(ctx, cm.UID, "node-b", "session-2", r2))
	require.NoError(t, store.Ready(ctx, r2))
}

func TestJoinRaceAndBarrierRecreation(t *testing.T) {
	ctx := t.Context()
	maps := &memoryMaps{}
	store := Store{Maps: maps, SubnetName: "blue", SubnetUID: "subnet-1"}
	r1 := Digest([]string{"02:00:00:00:00:01"})
	require.NoError(t, store.Publish(ctx, r1))
	cm, _, err := store.Read(ctx)
	require.NoError(t, err)
	r2 := Digest([]string{"02:00:00:00:00:01", "02:00:00:00:00:02"})
	require.NoError(t, store.Publish(ctx, r2))
	require.NoError(t, store.JoinClosed(ctx, cm.UID, "late-node", "late", "new-session"))
	require.ErrorIs(t, store.InstallIntent(ctx, cm.UID, "late-node", "new-session", r1), ErrPending, "joining with a stale snapshot cannot open the edge")
	require.NoError(t, store.InstallIntent(ctx, cm.UID, "late-node", "new-session", r2))
	require.ErrorIs(t, store.Ready(ctx, r2), ErrPending)
	maps.mu.Lock()
	maps.cm = nil
	maps.mu.Unlock()
	require.NoError(t, store.Publish(ctx, r2))
	require.ErrorIs(t, store.Acknowledge(ctx, cm.UID, "late-node", "new-session", r2), ErrPending, "old ConfigMap UID cannot authorize a new barrier")
}

// This fake enforces resourceVersion CAS and API-assigned UIDs, unlike the
// client-go object tracker. It exercises the production retry protocol.
type memoryMaps struct {
	typedcore.ConfigMapInterface
	mu      sync.Mutex
	cm      *corev1.ConfigMap
	version int
}

func (m *memoryMaps) Get(_ context.Context, name string, _ metav1.GetOptions) (*corev1.ConfigMap, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cm == nil {
		return nil, apierrors.NewNotFound(corev1.Resource("configmaps"), name)
	}
	return m.cm.DeepCopy(), nil
}

func (m *memoryMaps) Create(_ context.Context, cm *corev1.ConfigMap, _ metav1.CreateOptions) (*corev1.ConfigMap, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cm != nil {
		return nil, apierrors.NewAlreadyExists(corev1.Resource("configmaps"), cm.Name)
	}
	m.version++
	m.cm = cm.DeepCopy()
	m.cm.UID = types.UID(fmt.Sprintf("barrier-%d", m.version))
	m.cm.ResourceVersion = strconv.Itoa(m.version)
	return m.cm.DeepCopy(), nil
}

func (m *memoryMaps) Update(_ context.Context, cm *corev1.ConfigMap, _ metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cm == nil || cm.ResourceVersion != m.cm.ResourceVersion {
		return nil, apierrors.NewConflict(corev1.Resource("configmaps"), cm.Name, errors.New("stale resourceVersion"))
	}
	m.version++
	m.cm = cm.DeepCopy()
	m.cm.ResourceVersion = strconv.Itoa(m.version)
	return m.cm.DeepCopy(), nil
}

func TestDisconnectedMemberIsNotEvicted(t *testing.T) {
	ctx := t.Context()
	maps := &memoryMaps{}
	store := Store{Maps: maps, SubnetName: "blue", SubnetUID: "subnet-1"}
	node := corev1.Node{Name: "deleted-node", UID: "old-uid"}
	require.NoError(t, store.Publish(ctx, "old"))
	cm, _, err := store.Read(ctx)
	require.NoError(t, err)
	require.NoError(t, store.JoinClosed(ctx, cm.UID, node.UID, node.Name, "old-session"))
	require.NoError(t, store.Acknowledge(ctx, cm.UID, node.UID, "old-session", "old"))
	require.NoError(t, store.Publish(ctx, "new"))
	require.ErrorIs(t, store.Ready(ctx, "new"), ErrPending, "Node deletion cannot prove that a fabric edge stopped forwarding")
	require.NoError(t, store.Close(ctx, cm.UID, node.UID, "old-session"))
	require.NoError(t, store.Ready(ctx, "new"))
}
