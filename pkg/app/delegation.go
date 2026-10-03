package app

import (
	"context"

	"github.com/docker/docker-agent/pkg/runtime"
)

// CanSetDelegationPolicy reports whether this view's owner holds a canonical
// "Use subagents" policy for its session tree (shared by every client).
func (a *App) CanSetDelegationPolicy() bool {
	if a == nil {
		return false
	}
	h := a.SessionHandle()
	if h == nil {
		return false
	}
	_, ok := h.(runtime.SessionDelegationController)
	return ok && h.Metadata().Capabilities.DelegationPolicy
}

// DelegationPolicy reads the effective policy of this view's session tree.
func (a *App) DelegationPolicy(ctx context.Context) (bool, error) {
	h := a.SessionHandle()
	controller, ok := h.(runtime.SessionDelegationController)
	if !ok || !a.CanSetDelegationPolicy() {
		return false, a.delegationUnsupported()
	}
	return controller.DelegationPolicy(ctx)
}

// SetDelegationPolicy changes only new delegation for this view's session
// tree; accepted work continues. It never touches saved local defaults.
func (a *App) SetDelegationPolicy(ctx context.Context, enabled bool) error {
	h := a.SessionHandle()
	controller, ok := h.(runtime.SessionDelegationController)
	if !ok || !a.CanSetDelegationPolicy() {
		return a.delegationUnsupported()
	}
	return controller.SetDelegationPolicy(ctx, enabled)
}

func (a *App) delegationUnsupported() error {
	sessionID := ""
	if h := a.SessionHandle(); h != nil {
		sessionID = h.ID()
	}
	return runtime.UnsupportedSessionOperation(sessionID, "delegation_policy")
}
