package barrier

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestJoinRequiresSuccessfulClosureAndPreservesOldNodeUID(t *testing.T) {
	ctx := t.Context()
	store := Store{Maps: &memoryMaps{}, SubnetName: "blue", SubnetUID: "subnet"}
	require.NoError(t, store.Publish(ctx, "r1"))
	cm, _, err := store.Read(ctx)
	require.NoError(t, err)
	closed := func() error { return nil }
	require.NoError(t, store.JoinAfterClose(ctx, cm.UID, "old-node", "node", "old", closed))
	require.NoError(t, store.ProtectAndOpen(ctx, cm.UID, "old-node", "old", "r1", closed, closed))
	failure := errors.New("close failed")
	require.ErrorIs(t, store.JoinAfterClose(ctx, cm.UID, "old-node", "node", "new", func() error { return failure }), failure)
	_, state, err := store.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, Member{Name: "node", Session: "old", Phase: Active, Revision: "r1"}, state.Members["old-node"])
	require.NoError(t, store.JoinAfterClose(ctx, cm.UID, "old-node", "node", "new", closed))
	never := func() error { t.Fatal("stale session reached local dataplane"); return nil }
	require.ErrorIs(t, store.ProtectAndOpen(ctx, cm.UID, "old-node", "old", "r1", never, never), ErrPending)
	require.NoError(t, store.JoinAfterClose(ctx, cm.UID, "new-node", "node", "third", closed))
	_, state, err = store.Read(ctx)
	require.NoError(t, err)
	require.Len(t, state.Members, 2, "recreated Node UID must not erase the old member")
}

func TestRevisionChangesDuringInstallPreventOpen(t *testing.T) {
	ctx := t.Context()
	store := Store{Maps: &memoryMaps{}, SubnetName: "blue", SubnetUID: "subnet"}
	require.NoError(t, store.Publish(ctx, "r1"))
	cm, _, err := store.Read(ctx)
	require.NoError(t, err)
	require.NoError(t, store.JoinAfterClose(ctx, cm.UID, "node", "node", "session", func() error { return nil }))
	err = store.ProtectAndOpen(ctx, cm.UID, "node", "session", "r1", func() error {
		require.ErrorIs(t, store.Ready(ctx, "r1"), ErrPending, "install intent is visible before local preparation")
		return store.Publish(ctx, "r2")
	}, func() error { t.Fatal("old revision opened"); return nil })
	require.ErrorIs(t, err, ErrPending)
	require.ErrorIs(t, store.Ready(ctx, "r2"), ErrPending)
	require.NoError(t, store.ProtectAndOpen(ctx, cm.UID, "node", "session", "r2", func() error { return nil }, func() error { return nil }))
	require.NoError(t, store.Ready(ctx, "r2"))
}

func TestInstallFailureAndLostAPIDoNotOpen(t *testing.T) {
	ctx := t.Context()
	maps := &memoryMaps{}
	store := Store{Maps: maps, SubnetName: "blue", SubnetUID: "subnet"}
	require.NoError(t, store.Publish(ctx, "r1"))
	cm, _, err := store.Read(ctx)
	require.NoError(t, err)
	require.NoError(t, store.JoinAfterClose(ctx, cm.UID, "node", "node", "session", func() error { return nil }))
	failure := errors.New("install failed")
	never := func() error { t.Fatal("failed operation opened the attachment"); return nil }
	require.ErrorIs(t, store.ProtectAndOpen(ctx, cm.UID, "node", "session", "r1", func() error { return failure }, never), failure)
	require.ErrorIs(t, store.Ready(ctx, "r1"), ErrPending)
	require.Error(t, store.ProtectAndOpen(ctx, cm.UID, "node", "session", "r1", func() error {
		maps.mu.Lock()
		maps.cm = nil
		maps.mu.Unlock()
		return nil
	}, never))
}

func TestPublishAfterAcknowledgementBeforeOpen(t *testing.T) {
	ctx := t.Context()
	maps := &afterUpdateMaps{memoryMaps: &memoryMaps{}}
	store := Store{Maps: maps, SubnetName: "blue", SubnetUID: "subnet"}
	require.NoError(t, store.Publish(ctx, "r1"))
	cm, _, err := store.Read(ctx)
	require.NoError(t, err)
	require.NoError(t, store.JoinClosed(ctx, cm.UID, "node", "node", "session"))
	maps.afterUpdate = func(cm *corev1.ConfigMap) {
		var state State
		require.NoError(t, json.Unmarshal([]byte(cm.Data["state"]), &state))
		if state.Members["node"].Phase == Active {
			maps.afterUpdate = nil
			require.NoError(t, store.Publish(ctx, "r2"))
		}
	}
	require.ErrorIs(t, store.ProtectAndOpen(ctx, cm.UID, "node", "session", "r1", func() error { return nil }, func() error { t.Fatal("stale ACK opened the attachment"); return nil }), ErrPending)
	require.ErrorIs(t, store.Ready(ctx, "r2"), ErrPending)
}

type afterUpdateMaps struct {
	*memoryMaps
	beforeUpdate func()
	afterUpdate  func(*corev1.ConfigMap)
}

func (m *afterUpdateMaps) Update(ctx context.Context, cm *corev1.ConfigMap, options metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	if m.beforeUpdate != nil {
		hook := m.beforeUpdate
		m.beforeUpdate = nil
		hook()
	}
	result, err := m.memoryMaps.Update(ctx, cm, options)
	if err == nil && m.afterUpdate != nil {
		m.afterUpdate(result)
	}
	return result, err
}

func TestCASRetriesPreserveJoinAndRejectStaleIntent(t *testing.T) {
	ctx := t.Context()
	maps := &afterUpdateMaps{memoryMaps: &memoryMaps{}}
	store := Store{Maps: maps, SubnetName: "blue", SubnetUID: "subnet"}
	require.NoError(t, store.Publish(ctx, "r1"))
	cm, _, err := store.Read(ctx)
	require.NoError(t, err)
	maps.beforeUpdate = func() { require.NoError(t, store.JoinClosed(ctx, cm.UID, "joining", "joining", "session")) }
	require.NoError(t, store.Publish(ctx, "r2"))
	_, state, err := store.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, "r2", state.Revision)
	require.Equal(t, Closed, state.Members["joining"].Phase, "Publish CAS retry must retain the concurrent join")
	maps.beforeUpdate = func() { require.NoError(t, store.Publish(ctx, "r3")) }
	require.ErrorIs(t, store.InstallIntent(ctx, cm.UID, "joining", "session", "r2"), ErrPending)
	_, state, err = store.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, Closed, state.Members["joining"].Phase, "stale intent must not commit after its conflict retry")
}
