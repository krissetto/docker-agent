package runtime

import (
	"context"
	"math"
	"os"
	"path/filepath"

	"github.com/docker/docker-agent/pkg/session"
)

type SessionEditKind string

const (
	SessionEditResume         SessionEditKind = "resume"
	SessionEditOpenView       SessionEditKind = "open_view"
	SessionEditPendingMessage SessionEditKind = "pending_message"
	SessionEditAttachment     SessionEditKind = "attachment"
	SessionEditPolicy         SessionEditKind = "policy"
	SessionEditPermissions    SessionEditKind = "permissions"
	SessionEditTitle          SessionEditKind = "title"
	SessionEditMessage        SessionEditKind = "message"
	SessionEditSummary        SessionEditKind = "summary"
	SessionEditTokens         SessionEditKind = "tokens"
)

type PendingMessageEdit struct {
	TurnID          string  `json:"turn_id"`
	Content         string  `json:"content"`
	ExpectedContent *string `json:"expected_content,omitempty"`
}

type SessionEdit struct {
	PendingMessage      *PendingMessageEdit        `json:"pending_message,omitempty"`
	AttachmentPath      string                     `json:"attachment_path,omitempty"`
	Kind                SessionEditKind            `json:"kind"`
	SafetyPolicy        *session.SafetyPolicy      `json:"safety_policy,omitempty"`
	ToolsApproved       *bool                      `json:"tools_approved,omitempty"`
	ToggleToolsApproved bool                       `json:"toggle_tools_approved,omitempty"`
	Permissions         *session.PermissionsConfig `json:"permissions,omitempty"`
	Title               string                     `json:"title,omitempty"`
	Message             *session.Message           `json:"message,omitempty"`
	MessageIndex        int64                      `json:"message_index,omitempty"`
	Summary             *session.Item              `json:"summary,omitempty"`
	InputTokens         int64                      `json:"input_tokens,omitempty"`
	OutputTokens        int64                      `json:"output_tokens,omitempty"`
	Cost                float64                    `json:"cost,omitempty"`
}

func (h *sessionHandle) Edit(ctx context.Context, edit SessionEdit) (*session.Session, error) {
	d := h.driver
	if edit.Kind == SessionEditResume || edit.Kind == SessionEditOpenView {
		d.mu.Lock()
		if err := ctx.Err(); err != nil {
			d.mu.Unlock()
			return nil, err
		}
		if d.r.lifetime().Err() != nil {
			d.mu.Unlock()
			return nil, ErrSessionClosed
		}
		if err := d.admitLocked(SessionOperationPost); err != nil {
			d.mu.Unlock()
			return nil, err
		}
		changed := edit.Kind == SessionEditResume && d.authorizeViewLocked()
		snapshot := d.sess.Clone()
		d.mu.Unlock()
		if changed {
			d.r.sessionDrivers.signalWork()
		}
		return snapshot, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if d.stopped {
		return nil, ErrSessionStopped
	}
	if edit.Kind == SessionEditPendingMessage {
		return h.editPendingMessageLocked(ctx, edit.PendingMessage)
	}
	transcript := edit.Kind == SessionEditMessage || edit.Kind == SessionEditSummary || edit.Kind == SessionEditTokens
	if transcript && (d.running() || d.starting() || d.settling() || len(d.pending) != 0 || len(d.steering) != 0 || d.compactReserved) {
		return nil, ErrSessionCapacity
	}
	unlockMetadata := d.sess.LockMetadata()
	defer unlockMetadata()
	next := d.sess.Clone()
	store := d.r.sessionStore
	var err error
	switch edit.Kind {
	case SessionEditAttachment:
		abs, pathErr := filepath.Abs(edit.AttachmentPath)
		if pathErr != nil {
			return nil, pathErr
		}
		info, statErr := os.Stat(abs)
		if statErr != nil {
			return nil, statErr
		}
		if !info.Mode().IsRegular() {
			return nil, &SessionError{Kind: SessionErrorInvalid, Operation: "edit_attachment"}
		}
		next.AddAttachedFile(abs)
		edit.AttachmentPath = abs
		if store != nil {
			err = store.UpdateSession(ctx, next)
		}
	case SessionEditPolicy:
		if edit.SafetyPolicy != nil {
			if !edit.SafetyPolicy.IsValid() {
				return nil, &SessionError{Kind: SessionErrorInvalid, Operation: "edit_policy"}
			}
			next.SetSafetyPolicy(*edit.SafetyPolicy)
		}
		if edit.ToolsApproved != nil {
			next.SetToolsApproved(*edit.ToolsApproved)
		}
		if edit.ToggleToolsApproved {
			next.ToggleYolo()
		}
		if store != nil {
			err = store.UpdateSession(ctx, next)
		}
	case SessionEditPermissions:
		next.SetPermissions(edit.Permissions)
		if store != nil {
			err = store.UpdateSession(ctx, next)
		}
	case SessionEditTitle:
		next.SetTitle(edit.Title)
		if store != nil {
			err = store.UpdateSessionTitle(ctx, h.sessionID, edit.Title)
		}
	case SessionEditMessage:
		if edit.Message == nil {
			return nil, &SessionError{Kind: SessionErrorInvalid, Operation: "edit_message"}
		}
		if edit.MessageIndex < 0 {
			next.AddMessage(edit.Message)
			if store != nil {
				_, err = store.AddMessage(ctx, h.sessionID, edit.Message)
			}
		} else {
			if int(edit.MessageIndex) >= len(next.Messages) {
				return nil, &SessionError{Kind: SessionErrorInvalid, Operation: "edit_message"}
			}
			next.Messages[edit.MessageIndex].Message = edit.Message
			if store != nil {
				err = store.UpdateMessage(ctx, h.sessionID, edit.MessageIndex, edit.Message)
			}
		}
	case SessionEditSummary:
		if edit.Summary != nil && (edit.Summary.Cost < 0 || math.IsNaN(edit.Summary.Cost) || math.IsInf(edit.Summary.Cost, 0) || len(edit.Summary.Model) > 256) {
			return nil, &SessionError{Kind: SessionErrorInvalid, Operation: "edit_summary"}
		}
		if edit.Summary == nil {
			return nil, &SessionError{Kind: SessionErrorInvalid, Operation: "edit_summary"}
		}
		next.Messages = append(next.Messages, *edit.Summary)
		if store != nil {
			err = store.AddSummary(ctx, h.sessionID, *edit.Summary)
		}
	case SessionEditTokens:
		if edit.InputTokens < 0 || edit.OutputTokens < 0 || edit.Cost < 0 || math.IsNaN(edit.Cost) || math.IsInf(edit.Cost, 0) {
			return nil, &SessionError{Kind: SessionErrorInvalid, Operation: "edit_tokens"}
		}
		next.SetTokensAndCost(edit.InputTokens, edit.OutputTokens, edit.Cost)
		if store != nil {
			err = store.UpdateSessionTokens(ctx, h.sessionID, edit.InputTokens, edit.OutputTokens, edit.Cost)
		}
	default:
		return nil, &SessionError{Kind: SessionErrorInvalid, Operation: "edit"}
	}
	if err != nil {
		return nil, err
	}
	// Policy changes apply to the live session even while a provider call runs.
	if !transcript {
		switch edit.Kind {
		case SessionEditAttachment:
			d.sess.AddAttachedFile(edit.AttachmentPath)
		case SessionEditPolicy:
			if edit.SafetyPolicy != nil {
				d.sess.SetSafetyPolicy(*edit.SafetyPolicy)
			}
			if edit.ToolsApproved != nil {
				d.sess.SetToolsApproved(*edit.ToolsApproved)
			}
			if edit.ToggleToolsApproved {
				d.sess.ToggleYolo()
			}
		case SessionEditPermissions:
			d.sess.SetPermissions(edit.Permissions)
		case SessionEditTitle:
			d.sess.SetTitle(edit.Title)
			d.events.Publish(h.sessionID, SessionTitle(h.sessionID, edit.Title))
		}
	} else {
		d.sess = next
	}
	return d.sess.Clone(), nil
}
