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

func (r *LocalRuntime) delegationRoot(sess *session.Session) (*session.Session, error) {
	seen := map[string]bool{}
	for sess != nil {
		if seen[sess.ID] {
			return nil, &SessionError{Kind: SessionErrorWrongSession, SessionID: sess.ID, Operation: "delegation_policy"}
		}
		seen[sess.ID] = true
		if driver, ok := r.sessionDrivers.Lookup(sess.ID); ok {
			sess = driver.session()
		}
		if sess.ParentID == "" {
			return sess, nil
		}
		parent, ok := r.sessionDrivers.Lookup(sess.ParentID)
		if ok {
			sess = parent.session()
			continue
		}
		if r.subagents != nil {
			r.subagents.mu.Lock()
			tracked := r.subagents.sessions[sess.ParentID]
			var liveParent *session.Session
			if tracked != nil {
				liveParent = tracked.sess
			}
			r.subagents.mu.Unlock()
			if liveParent != nil {
				sess = liveParent
				continue
			}
		}
		if r.sessionStore != nil {
			stored, err := r.sessionStore.GetSession(r.lifetime(), sess.ParentID)
			if err == nil {
				sess = stored
				continue
			}
		}
		return nil, &SessionError{Kind: SessionErrorNotFound, SessionID: sess.ParentID, Operation: "delegation_policy"}
	}
	return nil, &SessionError{Kind: SessionErrorNotFound, Operation: "delegation_policy"}
}

func (r *LocalRuntime) sessionDelegationEnabled(sess *session.Session) bool {
	if sess == nil {
		return r.UseSubagents()
	}
	root, err := r.delegationRoot(sess)
	if err != nil {
		return false
	}
	value, exists := root.AttributesSnapshot()[SessionDelegationAttribute]
	if !exists {
		return r.UseSubagents()
	}
	enabled, err := strconv.ParseBool(value)
	return err == nil && enabled
}

func (h *sessionHandle) DelegationPolicy(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	h.runtime.subagentAdmissionMu.RLock()
	defer h.runtime.subagentAdmissionMu.RUnlock()
	root, err := h.runtime.delegationRoot(h.driver.session())
	if err != nil {
		return false, err
	}
	return h.runtime.sessionDelegationEnabled(root), nil
}

func (h *sessionHandle) SetDelegationPolicy(ctx context.Context, enabled bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r := h.runtime
	r.subagentAdmissionMu.Lock()
	defer r.subagentAdmissionMu.Unlock()
	root, err := r.delegationRoot(h.driver.session())
	if err != nil {
		return err
	}
	driver, ok := r.sessionDrivers.Lookup(root.ID)
	if !ok {
		return &SessionError{Kind: SessionErrorNotFound, SessionID: root.ID, Operation: "delegation_policy"}
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if driver.stopped {
		return &SessionError{Kind: SessionErrorStopped, SessionID: root.ID, Operation: "delegation_policy"}
	}
	unlockMetadata := root.LockMetadata()
	defer unlockMetadata()
	next := root.Clone()
	next.SetAttribute(SessionDelegationAttribute, strconv.FormatBool(enabled))
	if r.sessionStore == nil {
		return sessionUnsupported(root.ID, "delegation_policy")
	}
	if err := r.sessionStore.UpdateSession(ctx, next); err != nil {
		return err
	}
	root.SetAttribute(SessionDelegationAttribute, strconv.FormatBool(enabled))
	return nil
}
