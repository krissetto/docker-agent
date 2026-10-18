package app

import (
	"context"
	"errors"
	"reflect"
	"strings"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/subagent"
)

var ErrReviewSubmitTargetChanged = errors.New("review send target changed after confirmation")
var ErrReviewSubmitTargetInvalid = errors.New("review send target is invalid")

// ReviewSubmitTarget is captured by the host before confirmation and checked
// again here. The host also fences its route/source generation before calling.
type ReviewSubmitTarget struct {
	SessionID       string
	Handle          runtime.SessionHandle
	AttachedNodeID  subagent.NodeID
	ParentSessionID string
	Source          string
	WorkingDir      string
	AgentName       string
}

// ReviewSubmitReceipt distinguishes admitted input from eventual model work.
type ReviewSubmitReceipt struct {
	Submission runtime.Submission
	Durable    bool
}

// CaptureReviewSubmitTarget captures the canonical human-input destination.
// For an attached agent view, submission remains user-origin input to that
// already-authorized attached handle; it does not call model-to-child send_message.
func (a *App) CaptureReviewSubmitTarget() ReviewSubmitTarget {
	state := a.state()
	if state.session == nil || state.handle == nil {
		return ReviewSubmitTarget{}
	}
	t := ReviewSubmitTarget{SessionID: state.session.ID, Handle: state.handle, Source: state.session.AttributesSnapshot()["docker-agent.actor.source"], WorkingDir: state.session.WorkingDir, AgentName: state.handle.AgentName()}
	if attached := a.AttachedSubagent(); attached != nil {
		t.AttachedNodeID = attached.NodeID
		t.ParentSessionID = attached.ParentSessionID
	}
	return t
}

// SubmitReviewInput admits the serialized review body as one normal literal user
// input. Stable requestID supports idempotent retries after a lost response.
func (a *App) SubmitReviewInput(ctx context.Context, target ReviewSubmitTarget, requestID, body string) (ReviewSubmitReceipt, error) {
	if err := ctx.Err(); err != nil {
		return ReviewSubmitReceipt{}, err
	}
	if strings.TrimSpace(body) == "" || requestID == "" || target.SessionID == "" || target.Handle == nil {
		return ReviewSubmitReceipt{}, ErrReviewSubmitTargetInvalid
	}
	state := a.state()
	if state.session == nil || state.handle == nil || state.session.ID != target.SessionID || state.handle.ID() != target.SessionID || !sameReviewHandle(state.handle, target.Handle) || state.session.WorkingDir != target.WorkingDir || state.session.AttributesSnapshot()["docker-agent.actor.source"] != target.Source || state.handle.AgentName() != target.AgentName {
		return ReviewSubmitReceipt{}, ErrReviewSubmitTargetChanged
	}
	if a.IsReadOnly() || target.Handle.Metadata().Historical || !target.Handle.Metadata().ExecutionAvailable {
		return ReviewSubmitReceipt{}, runtime.UnsupportedSessionOperation(target.SessionID, "submit_review")
	}
	attached := a.AttachedSubagent()
	if attached == nil {
		if target.AttachedNodeID != "" || target.ParentSessionID != "" {
			return ReviewSubmitReceipt{}, ErrReviewSubmitTargetChanged
		}
	} else if attached.NodeID != target.AttachedNodeID || attached.ParentSessionID != target.ParentSessionID {
		return ReviewSubmitReceipt{}, ErrReviewSubmitTargetChanged
	}
	submission, err := state.handle.Submit(ctx, runtime.TurnInput{Content: body, RequestID: requestID})
	if err != nil {
		return ReviewSubmitReceipt{}, err
	}
	if submission.SessionID != target.SessionID || submission.TurnID == "" {
		return ReviewSubmitReceipt{}, ErrReviewSubmitTargetChanged
	}
	a.recordSubmission(submission.TurnID)
	return ReviewSubmitReceipt{Submission: submission, Durable: state.handle.Metadata().Capabilities.Durability == subagent.DurabilityDurable}, nil
}

// Avoid interface equality/panic for third-party Handle implementations: only
// immutable target ID and creation-agent binding are comparable guarantees.
func sameReviewHandle(a, b runtime.SessionHandle) bool {
	if a == nil || b == nil {
		return false
	}
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	if ta != tb || !ta.Comparable() {
		return false
	}
	return a == b
}
