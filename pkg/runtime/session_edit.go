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
	if edit.Kind == SessionEditPendingMessage {
		return h.editPendingMessage(ctx, edit.PendingMessage)
	}
	if edit.Kind == SessionEditAttachment {
		abs, err := filepath.Abs(edit.AttachmentPath)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, &SessionError{Kind: SessionErrorInvalid, Operation: "edit_attachment"}
		}
		edit.AttachmentPath = abs
	}
	var snapshot *session.Session
	if edit.Kind == SessionEditResume || edit.Kind == SessionEditOpenView {
		changed := false
		err := d.ownerCall(ctx, func() error {
			if d.r.lifetime().Err() != nil {
				return ErrSessionClosed
			}
			if err := d.admitLocked(SessionOperationPost); err != nil {
				return err
			}
			changed = edit.Kind == SessionEditResume && d.authorizeViewLocked()
			snapshot = d.sess.Clone()
			return nil
		})
		if changed {
			d.r.sessionDrivers.signalWork()
		}
		if err != nil {
			return nil, err
		}
		return snapshot, nil
	}
	err := d.durableIO(ctx, func() (sessionIOReservation, error) {
		if d.stopped {
			return sessionIOReservation{}, ErrSessionStopped
		}
		transcript := edit.Kind == SessionEditMessage || edit.Kind == SessionEditSummary || edit.Kind == SessionEditTokens
		if transcript && (d.running() || d.starting() || d.settling() || len(d.pending) != 0 || len(d.steering) != 0 || d.compactReserved) {
			return sessionIOReservation{}, ErrSessionCapacity
		}
		next := d.sess.OwnSnapshot()
		store := d.r.sessionStore
		var write func(context.Context) error
		position := -1
		invalid := func() (sessionIOReservation, error) {
			return sessionIOReservation{}, &SessionError{Kind: SessionErrorInvalid, Operation: "edit"}
		}
		switch edit.Kind {
		case SessionEditAttachment:
			next.AddAttachedFile(edit.AttachmentPath)
			if store != nil {
				write = func(ctx context.Context) error { return store.UpdateSession(ctx, next) }
			}
		case SessionEditPolicy:
			if edit.SafetyPolicy != nil {
				if !edit.SafetyPolicy.IsValid() {
					return invalid()
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
				write = func(ctx context.Context) error { return store.UpdateSession(ctx, next) }
			}
		case SessionEditPermissions:
			next.SetPermissions(edit.Permissions)
			if store != nil {
				write = func(ctx context.Context) error { return store.UpdateSession(ctx, next) }
			}
		case SessionEditTitle:
			next.SetTitle(edit.Title)
			if store != nil {
				write = func(ctx context.Context) error { return store.UpdateSessionTitle(ctx, h.sessionID, edit.Title) }
			}
		case SessionEditMessage:
			if edit.Message == nil {
				return invalid()
			}
			// MessageIndex is the durable message identity, never a transcript position.
			detached := session.New()
			detached.AddMessage(edit.Message)
			edit.Message = detached.OwnSnapshot().Messages[0].Message
			if edit.MessageIndex < 0 {
				edit.Message.ID = 0
				if store != nil {
					write = func(ctx context.Context) error {
						id, err := store.AddMessage(ctx, h.sessionID, edit.Message)
						if err == nil {
							edit.Message.ID = id
						}
						return err
					}
				}
			} else {
				for i, item := range next.Messages {
					if item.Message != nil && item.Message.ID == edit.MessageIndex {
						position = i
						break
					}
				}
				if position < 0 {
					return invalid()
				}
				edit.Message.ID = next.Messages[position].Message.ID
				if store != nil {
					write = func(ctx context.Context) error {
						return store.UpdateMessage(ctx, h.sessionID, edit.Message.ID, edit.Message)
					}
				}
			}
		case SessionEditSummary:
			if edit.Summary == nil || edit.Summary.Message != nil || edit.Summary.SubSession != nil || edit.Summary.Error != nil || edit.Summary.Termination != nil || edit.Summary.Cost < 0 || math.IsNaN(edit.Summary.Cost) || math.IsInf(edit.Summary.Cost, 0) || len(edit.Summary.Model) > 256 {
				return invalid()
			}
			detached := session.New()
			detached.Messages = []session.Item{*edit.Summary}
			item := detached.OwnSnapshot().Messages[0]
			edit.Summary = &item
			if store != nil {
				write = func(ctx context.Context) error { return store.AddSummary(ctx, h.sessionID, item) }
			}
		case SessionEditTokens:
			if edit.InputTokens < 0 || edit.OutputTokens < 0 || edit.Cost < 0 || math.IsNaN(edit.Cost) || math.IsInf(edit.Cost, 0) {
				return invalid()
			}
			if store != nil {
				write = func(ctx context.Context) error {
					return store.UpdateSessionTokens(ctx, h.sessionID, edit.InputTokens, edit.OutputTokens, edit.Cost)
				}
			}
		default:
			return invalid()
		}
		d.editReserved = true
		return sessionIOReservation{write: write, commit: func(err error) error {
			d.editReserved = false
			if err != nil {
				return err
			}
			switch edit.Kind {
			case SessionEditAttachment:
				d.sess.AddAttachedFile(edit.AttachmentPath)
			case SessionEditPolicy:
				d.sess.SetSafetyPolicy(next.GetSafetyPolicy())
				d.sess.SetToolsApproved(next.ToolsApproved)
			case SessionEditPermissions:
				d.sess.SetPermissions(next.ClonePermissions())
			case SessionEditTitle:
				d.sess.SetTitle(edit.Title)
				d.events.Publish(h.sessionID, SessionTitle(h.sessionID, edit.Title))
			case SessionEditMessage:
				published := d.sess.OwnSnapshot()
				if position < 0 {
					published.AddMessage(edit.Message)
				} else {
					published.Messages[position].Message = edit.Message
				}
				d.sess = published
			case SessionEditSummary:
				published := d.sess.OwnSnapshot()
				published.Messages = append(published.Messages, *edit.Summary)
				d.sess = published
			case SessionEditTokens:
				d.sess.SetTokensAndCost(edit.InputTokens, edit.OutputTokens, edit.Cost)
			}
			if transcript {
				d.invalidatePersistenceTranscript()
			}
			snapshot = d.sess.Clone()
			d.r.sessionDrivers.signalWork()
			return nil
		}}, nil
	})
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (d *sessionDriver) invalidatePersistenceTranscript() {
	for _, observer := range d.r.observers {
		if p, ok := observer.(*PersistenceObserver); ok {
			p.invalidateTranscript(d.sessionIDLocked())
		}
	}
}
