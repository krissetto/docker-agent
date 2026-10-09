package runtime

import (
	"context"
	"strconv"

	"github.com/docker/docker-agent/pkg/session"
)

const SessionDelegationAttribute = "docker-agent.actor.use_subagents"

// SessionDelegationController controls only new autonomous work in this tree.
// Descendants inherit the canonical root policy; accepted work is unaffected.
type SessionDelegationController interface {
	DelegationPolicy(ctx context.Context) (bool, error)
	SetDelegationPolicy(ctx context.Context, enabled bool) error
}

type delegationPolicyState struct {
	id, parentID, value string
	hasValue, present   bool
}

func delegationState(sess *session.Session) delegationPolicyState {
	if sess == nil {
		return delegationPolicyState{}
	}
	value, exists := sess.Attribute(SessionDelegationAttribute)
	return delegationPolicyState{id: sess.ID, parentID: sess.ParentID, value: value, hasValue: exists, present: true}
}

func (d *sessionDriver) delegationState() delegationPolicyState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return delegationState(d.sess)
}

func (r *LocalRuntime) delegationRoot(state delegationPolicyState) (delegationPolicyState, error) {
	seen := map[string]bool{}
	for state.present {
		if seen[state.id] {
			return delegationPolicyState{}, &SessionError{Kind: SessionErrorWrongSession, SessionID: state.id, Operation: "delegation_policy"}
		}
		seen[state.id] = true
		if driver, ok := r.sessionDrivers.Lookup(state.id); ok {
			state = driver.delegationState()
		}
		if !state.present {
			break
		}
		if state.parentID == "" {
			return state, nil
		}
		parent, ok := r.sessionDrivers.Lookup(state.parentID)
		if ok {
			state = parent.delegationState()
			continue
		}
		if r.subagents != nil {
			r.subagents.mu.Lock()
			tracked := r.subagents.sessions[state.parentID]
			var liveParent *session.Session
			if tracked != nil {
				liveParent = tracked.sess
			}
			r.subagents.mu.Unlock()
			if liveParent != nil {
				state = delegationState(liveParent)
				continue
			}
		}
		if r.sessionStore != nil {
			stored, err := r.sessionStore.GetSession(r.lifetime(), state.parentID)
			if err == nil {
				state = delegationState(stored)
				continue
			}
		}
		return delegationPolicyState{}, &SessionError{Kind: SessionErrorNotFound, SessionID: state.parentID, Operation: "delegation_policy"}
	}
	return delegationPolicyState{}, &SessionError{Kind: SessionErrorNotFound, Operation: "delegation_policy"}
}

func (r *LocalRuntime) delegationEnabled(root delegationPolicyState) bool {
	if !root.hasValue {
		return r.UseSubagents()
	}
	enabled, err := strconv.ParseBool(root.value)
	return err == nil && enabled
}

func (r *LocalRuntime) sessionDelegationEnabled(sess *session.Session) bool {
	return r.sessionDelegationEnabledState(delegationState(sess))
}

func (r *LocalRuntime) sessionDelegationEnabledState(state delegationPolicyState) bool {
	if !state.present {
		return r.UseSubagents()
	}
	root, err := r.delegationRoot(state)
	if err != nil {
		return false
	}
	return r.delegationEnabled(root)
}

func (h *sessionHandle) DelegationPolicy(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	root, err := h.runtime.delegationRoot(h.driver.delegationState())
	if err != nil {
		return false, err
	}
	return h.runtime.delegationEnabled(root), nil
}

func (h *sessionHandle) SetDelegationPolicy(ctx context.Context, enabled bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r := h.runtime
	root, err := r.delegationRoot(h.driver.delegationState())
	if err != nil {
		return err
	}
	driver, ok := r.sessionDrivers.Lookup(root.id)
	if !ok {
		return &SessionError{Kind: SessionErrorNotFound, SessionID: root.id, Operation: "delegation_policy"}
	}
	transition := r.subagents.transition(root.id)
	transition.Lock()
	defer transition.Unlock()
	if r.sessionStore == nil {
		return sessionUnsupported(root.id, "delegation_policy")
	}
	value := strconv.FormatBool(enabled)
	return driver.durableIO(ctx, func() (sessionIOReservation, error) {
		if driver.stopped {
			return sessionIOReservation{}, &SessionError{Kind: SessionErrorStopped, SessionID: root.id, Operation: "delegation_policy"}
		}
		next := driver.sess.OwnSnapshot()
		next.SetAttribute(SessionDelegationAttribute, value)
		return sessionIOReservation{
			write: func(ctx context.Context) error { return r.sessionStore.UpdateSession(ctx, next) },
			commit: func(err error) error {
				if err != nil {
					return err
				}
				driver.sess.SetAttribute(SessionDelegationAttribute, value)
				return nil
			},
		}, nil
	})
}
