package runtime

import (
	"context"
	"errors"
)

// ResidentAttention is a constant-size, read-only projection of outstanding
// resident interactions. Unknown means the session has no verified resident
// executable owner; it is never inferred from history or a restore attempt.
type ResidentAttention struct {
	Known              bool
	ExecutionAvailable bool
	AgentID            string
	AgentName          string
	ParentID           string
	Source             string
	WorkingDir         string
	ApprovalCount      int
	ElicitationCount   int
}

type ResidentAttentionReader interface {
	ReadResidentAttention(context.Context) (ResidentAttention, error)
}

func (h *sessionHandle) ReadResidentAttention(ctx context.Context) (ResidentAttention, error) {
	if err := ctx.Err(); err != nil {
		return ResidentAttention{}, err
	}
	state := h.driver.registrySnapshot()
	if state.stopped || state.stoppedView || state.reclaiming || h.driver.identityID != h.sessionID {
		return ResidentAttention{}, nil
	}
	var result ResidentAttention
	h.driver.mu.Lock()
	if h.driver.sess != nil {
		snapshot := h.driver.sess.MetadataSnapshot()
		if snapshot != nil {
			attributes := snapshot.AttributesSnapshot()
			result.AgentID = attributes[SessionAgentIDAttribute]
			result.AgentName = attributes[SessionAgentAttribute]
			result.ParentID = snapshot.ParentID
			result.Source = attributes["docker-agent.actor.source"]
			result.WorkingDir = snapshot.WorkingDir
		}
		if snapshot != nil && h.driver.identityParent == "" && !h.driver.viewDormant {
			tm := h.runtime.team
			if environment := h.driver.teamEnvironment; environment != nil && environment.Environment() != nil {
				tm = environment.Environment().Team()
			}
			if tm != nil {
				creation, _, creationOK := identityForBinding(tm, snapshot, SessionAgentAttribute, SessionAgentIDAttribute)
				active, _, activeOK := resolveAgentIdentity(tm, snapshot.AgentID, snapshot.AgentName)
				result.ExecutionAvailable = creationOK && activeOK && creation != nil && active != nil
			}
		}
	}
	h.driver.mu.Unlock()
	err := h.driver.ownerCall(ctx, func() error {
		state := h.driver.registrySnapshot()
		if state.stopped || state.stoppedView || state.reclaiming || h.driver.sess == nil || h.driver.sess.ID != h.sessionID || h.driver.identityID != h.sessionID {
			return nil
		}
		result.Known = true
		for _, interaction := range h.driver.interactions {
			switch interaction.kind {
			case InteractionConfirmation, InteractionMaxIterations:
				result.ApprovalCount++
			case InteractionElicitation:
				result.ElicitationCount++
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrSessionStopped) || errors.Is(err, ErrSessionClosed) {
			return ResidentAttention{}, nil
		}
		return ResidentAttention{}, err
	}
	return result, nil
}
