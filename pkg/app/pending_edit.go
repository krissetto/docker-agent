package app

import (
	"context"

	"github.com/docker/docker-agent/pkg/runtime"
)

// EditPendingMessage targets the captured owner, never the subsequently selected session.
func (a *App) EditPendingMessage(ctx context.Context, sessionID, turnID, content string) error {
	state := a.state()
	if state.handle == nil {
		return state.operationError("edit_pending_message")
	}
	if sessionID == "" || state.handle.ID() != sessionID || state.session == nil || state.session.ID != sessionID {
		return &runtime.SessionError{Kind: runtime.SessionErrorWrongSession, SessionID: sessionID, RequestID: turnID, Operation: "edit_pending_message"}
	}
	_, err := state.handle.Edit(ctx, runtime.SessionEdit{Kind: runtime.SessionEditPendingMessage, PendingMessage: &runtime.PendingMessageEdit{TurnID: turnID, Content: content}})
	return err
}
