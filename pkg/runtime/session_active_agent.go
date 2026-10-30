package runtime

import (
	"context"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// sessionOperationSwitchActiveAgent names the in-place switch in typed errors.
// SessionOperationSwitchAgent is the clone-based switch and keeps its meaning.
const sessionOperationSwitchActiveAgent SessionOperation = "switch_active_agent"

// ActiveAgentSwitcher is an optional capability of local root session handles.
// It changes the session's active agent in place: the same session ID and
// history continue with the target agent from the next turn on, and no session
// is cloned. Remote handles do not implement it; [SessionCapabilities] reports
// ActiveAgentSwitching for every handle.
//
// The switch is idle-only. It fails with a SessionError (Kind
// SessionErrorCapacity, Reason SessionErrorReasonBusy) while a turn runs, while
// messages are queued or interactions are pending, or while another durable
// session operation holds the session. Accepted messages therefore keep the
// agent they were routed to, and the switch applies to the next user message.
// Child (subagent) sessions keep their creation agent and return
// SessionErrorUnsupported.
type ActiveAgentSwitcher interface {
	SwitchActiveAgent(ctx context.Context, agentName string) error
}

var _ ActiveAgentSwitcher = (*sessionHandle)(nil)

// SwitchActiveAgent makes agentName the session's active agent for subsequent
// turns. Switching to the agent that is already active is a no-op. Validation
// rejects agents missing from the session's team before anything is persisted.
func (h *sessionHandle) SwitchActiveAgent(ctx context.Context, agentName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if h.driver.identityParent != "" {
		return &SessionError{Kind: SessionErrorUnsupported, SessionID: h.sessionID, Operation: sessionOperationSwitchActiveAgent, Detail: "child sessions keep their creation agent"}
	}
	if agentName == "" {
		return &SessionError{Kind: SessionErrorInvalid, SessionID: h.sessionID, Operation: sessionOperationSwitchActiveAgent, Detail: "agent name is required"}
	}
	opCtx, release, err := h.runtime.captureEnvironmentForOperation(ctx, h.driver.session())
	if err != nil {
		return err
	}
	defer release()
	tm := h.runtime.teamForContext(opCtx, h.driver.session())
	if tm == nil {
		return ErrSessionClosed
	}
	if _, err := tm.Agent(agentName); err != nil {
		return &SessionError{Kind: SessionErrorNotFound, SessionID: h.sessionID, Operation: sessionOperationSwitchActiveAgent, Detail: "agent " + agentName + " is not in this session's team"}
	}
	return h.driver.switchActiveAgent(opCtx, agentName, tm)
}

// switchActiveAgent commits an idle, user-initiated switch through the same
// reserve/write/commit pipeline as the model override. Model providers for the
// target agent are resolved before the write. AgentInfo and TeamInfo for the
// target are published only when the write commits, and agent-switch hooks run
// after the commit, so observers never see a switch that did not persist.
func (d *sessionDriver) switchActiveAgent(ctx context.Context, name string, tm *team.Team) error {
	binding := d.activeBinding()
	if binding.agentName == name {
		return nil
	}
	var next *session.Session
	var generation uint64
	if err := d.ownerCall(ctx, func() error {
		if d.stopped || d.sess == nil {
			return ErrSessionStopped
		}
		if err := d.validateBindingLocked(binding); err != nil {
			return err
		}
		next = d.sess.OwnSnapshot()
		generation = d.generation
		return nil
	}); err != nil {
		return err
	}
	next.AgentName = name
	next.AgentID = configuredAgentID(tm, name)
	ref, models, err := d.r.resolveSessionModelBinding(ctx, next, next.AgentModelOverrides[name])
	if err != nil {
		return err
	}
	target, err := tm.Agent(name)
	if err != nil {
		return err
	}
	var infoEvents []Event
	d.r.emitAgentAndTeamInfo(agent.WithContextModels(ctx, name, models), target, func(event Event) bool {
		infoEvents = append(infoEvents, event)
		return true
	})
	if err := d.durableIO(ctx, func() (sessionIOReservation, error) {
		if serr := d.admitLocked(SessionOperationSwitchAgent); serr != nil {
			serr.Operation = sessionOperationSwitchActiveAgent
			return sessionIOReservation{}, serr
		}
		if d.generation != generation {
			return sessionIOReservation{}, ErrSessionStopped
		}
		if err := d.validateBindingLocked(binding); err != nil {
			return sessionIOReservation{}, err
		}
		committed := d.sess.OwnSnapshot()
		committed.AgentName = name
		committed.AgentID = configuredAgentID(tm, name)
		d.switchReserved = true
		return sessionIOReservation{
			write: func(ctx context.Context) error {
				if d.r.sessionStore != nil {
					return d.r.sessionStore.UpdateSession(ctx, committed)
				}
				return nil
			},
			commit: func(err error) error {
				d.switchReserved = false
				if err != nil {
					return err
				}
				if d.stopped || d.generation != generation {
					return ErrSessionStopped
				}
				if err := d.validateBindingLocked(binding); err != nil {
					return err
				}
				d.sess.AgentName = name
				d.sess.AgentID = configuredAgentID(tm, name)
				d.modelRef = ref
				d.modelProviders = models
				d.bindingVersion++
				for _, event := range infoEvents {
					d.events.Publish(d.identityID, event)
				}
				return nil
			},
		}, nil
	}); err != nil {
		return err
	}
	if from, err := tm.Agent(binding.agentName); err == nil {
		d.r.executeOnAgentSwitchHooks(ctx, from, d.identityID, binding.agentName, name, agentSwitchKindUserSwitch)
	}
	return nil
}
