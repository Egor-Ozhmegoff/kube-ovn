package barrier

import (
	"context"

	"k8s.io/apimachinery/pkg/types"
)

// JoinAfterClose requires the host owner lock to be held until the daemon
// exits. A failed close must never replace a prior session with Closed.
func (s Store) JoinAfterClose(ctx context.Context, uid, nodeUID types.UID, name, session string, closePath func() error) error {
	if err := closePath(); err != nil {
		return err
	}
	return s.JoinClosed(ctx, uid, nodeUID, name, session)
}

// ProtectAndOpen requires the host owner lock. Install must retain all MACs
// previously admitted for this subnet, including when the edge is already up.
func (s Store) ProtectAndOpen(ctx context.Context, uid, nodeUID types.UID, session, revision string, install, openPath func() error) error {
	if uid == "" || nodeUID == "" || session == "" {
		return ErrPending
	}
	cm, state, err := s.Read(ctx)
	if err != nil {
		return err
	}
	member, exists := state.Members[string(nodeUID)]
	if cm.UID != uid || !exists || member.Session != session || state.Revision != revision {
		return ErrPending
	}
	stable := member.Phase == Active && member.Revision == revision
	if !stable {
		if err := s.InstallIntent(ctx, uid, nodeUID, session, revision); err != nil {
			return err
		}
	}
	if err := install(); err != nil {
		return err
	}
	if !stable {
		if err := s.Acknowledge(ctx, uid, nodeUID, session, revision); err != nil {
			return err
		}
	}
	if err := s.canOpen(ctx, uid, nodeUID, session, revision); err != nil {
		return err
	}
	return openPath()
}

// This is the final API authorization, not an atomic API/netlink transaction.
// A Publish after this read leaves this member Active at the older revision:
// admission of any newly added MAC is blocked until its filter is installed.
func (s Store) canOpen(ctx context.Context, uid, nodeUID types.UID, session, revision string) error {
	cm, state, err := s.Read(ctx)
	if err != nil {
		return err
	}
	member, exists := state.Members[string(nodeUID)]
	if cm.UID != uid || !exists || member.Session != session || state.Revision != revision || member.Phase != Active || member.Revision != revision {
		return ErrPending
	}
	return nil
}
