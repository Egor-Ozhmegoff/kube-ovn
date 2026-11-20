// Package barrier orders endpoint admission after host reflection protection.
package barrier

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/util/retry"
)

var ErrPending = errors.New("reflection protection is not acknowledged")

const (
	Closed  = "closed"
	Pending = "pending"
	Active  = "active"
)

type Member struct {
	Name     string `json:"name"`
	Session  string `json:"session"`
	Phase    string `json:"phase"`
	Revision string `json:"revision,omitempty"`
}

type State struct {
	SubnetUID string            `json:"subnetUID"`
	Revision  string            `json:"revision"`
	Members   map[string]Member `json:"members"`
}

type Store struct {
	Maps       typedcore.ConfigMapInterface
	SubnetName string
	SubnetUID  types.UID
}

func Digest(macs []string) string {
	values := slices.Clone(macs)
	slices.Sort(values)
	sum := sha256.Sum256([]byte(strings.Join(slices.Compact(values), ",")))
	return hex.EncodeToString(sum[:])
}

func Name(uid types.UID) string { return "kube-ovn-opr-guard-" + Digest([]string{string(uid)})[:24] }

func (s Store) Read(ctx context.Context) (*corev1.ConfigMap, State, error) {
	cm, err := s.Maps.Get(ctx, Name(s.SubnetUID), metav1.GetOptions{})
	if err != nil {
		return nil, State{}, err
	}
	var state State
	if err := json.Unmarshal([]byte(cm.Data["state"]), &state); err != nil {
		return nil, State{}, err
	}
	owned := slices.ContainsFunc(cm.OwnerReferences, func(owner metav1.OwnerReference) bool {
		return owner.APIVersion == "kubeovn.io/v1" && owner.Kind == "Subnet" && owner.Name == s.SubnetName && owner.UID == s.SubnetUID
	})
	if !owned || state.SubnetUID != string(s.SubnetUID) || state.Members == nil {
		return nil, State{}, errors.New("foreign or invalid reflection barrier")
	}
	return cm, state, nil
}

// Publish retains members, including disconnected ones. Neither timeouts nor a
// Node deletion prove that an old fabric attachment has stopped forwarding.
func (s Store) Publish(ctx context.Context, revision string) error {
	return s.update(ctx, true, func(_ types.UID, state *State) error {
		state.Revision = revision
		// Earlier versions inserted all Nodes as sessionless Pending placeholders.
		// They never authorized an edge to open. Registered Pending members remain.
		for uid, member := range state.Members {
			if member.Session == "" && member.Phase == Pending {
				delete(state.Members, uid)
			}
		}
		return nil
	})
}

// JoinClosed replaces a prior daemon session only after the local edge is
// closed. A cached acknowledgement cannot authorize a new process to open it.
func (s Store) JoinClosed(ctx context.Context, barrierUID, nodeUID types.UID, name, session string) error {
	if barrierUID == "" || nodeUID == "" || name == "" || session == "" {
		return ErrPending
	}
	return s.update(ctx, false, func(uid types.UID, state *State) error {
		if uid != barrierUID {
			return ErrPending
		}
		state.Members[string(nodeUID)] = Member{Name: name, Session: session, Phase: Closed}
		return nil
	})
}

func (s Store) InstallIntent(ctx context.Context, uid, nodeUID types.UID, session, revision string) error {
	return s.memberUpdate(ctx, uid, nodeUID, session, revision, Pending)
}

func (s Store) Acknowledge(ctx context.Context, uid, nodeUID types.UID, session, revision string) error {
	return s.memberUpdate(ctx, uid, nodeUID, session, revision, Active)
}

func (s Store) memberUpdate(ctx context.Context, uid, nodeUID types.UID, session, revision, phase string) error {
	return s.update(ctx, false, func(currentUID types.UID, state *State) error {
		member, exists := state.Members[string(nodeUID)]
		if currentUID != uid || !exists || session == "" || member.Session != session || state.Revision != revision {
			return ErrPending
		}
		member.Phase, member.Revision = phase, revision
		state.Members[string(nodeUID)] = member
		return nil
	})
}

func (s Store) Ready(ctx context.Context, revision string) error {
	_, state, err := s.Read(ctx)
	if err != nil {
		return err
	}
	if state.Revision != revision {
		return ErrPending
	}
	for _, member := range state.Members {
		if member.Session == "" || (member.Phase != Closed && (member.Phase != Active || member.Revision != revision)) {
			return fmt.Errorf("%w: node %s", ErrPending, member.Name)
		}
	}
	return nil
}

func (s Store) update(ctx context.Context, create bool, apply func(types.UID, *State) error) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm, state, err := s.Read(ctx)
		if apierrors.IsNotFound(err) && create {
			cm = &corev1.ConfigMap{Name: Name(s.SubnetUID), OwnerReferences: []metav1.OwnerReference{{APIVersion: "kubeovn.io/v1", Kind: "Subnet", Name: s.SubnetName, UID: s.SubnetUID}}}
			state = State{SubnetUID: string(s.SubnetUID), Members: map[string]Member{}}
		} else if err != nil {
			return err
		}
		before, err := json.Marshal(state)
		if err != nil {
			return err
		}
		if err := apply(cm.UID, &state); err != nil {
			return err
		}
		data, err := json.Marshal(state)
		if err != nil {
			return err
		}
		if cm.ResourceVersion != "" && string(before) == string(data) {
			return nil
		}
		cm.Data = map[string]string{"state": string(data)}
		if cm.ResourceVersion == "" {
			_, err = s.Maps.Create(ctx, cm, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				return apierrors.NewConflict(corev1.Resource("configmaps"), cm.Name, err)
			}
			return err
		}
		_, err = s.Maps.Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
}

func (s Store) Close(ctx context.Context, uid, nodeUID types.UID, session string) error {
	return s.update(ctx, false, func(currentUID types.UID, state *State) error {
		member, exists := state.Members[string(nodeUID)]
		if currentUID != uid || !exists || session == "" || member.Session != session {
			return ErrPending
		}
		member.Phase, member.Revision = Closed, ""
		state.Members[string(nodeUID)] = member
		return nil
	})
}
