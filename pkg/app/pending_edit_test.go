package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestPendingEditRoutesExactOwnerWithoutSnapshotInstall(t *testing.T) {
	a := newMetadataTestApp(t)
	owner := &editingProjectionSession{sess: session.New(session.WithID("s"))}
	a.replaceSessionState(sessionState{session: owner.sess, handle: owner})
	before, epoch := a.Session(), a.bridgeEpoch.Load()
	require.NoError(t, a.EditPendingMessage(t.Context(), "s", "turn", "new"))
	assert.Equal(t, runtime.SessionEditPendingMessage, owner.edit.Kind)
	assert.Equal(t, &runtime.PendingMessageEdit{TurnID: "turn", Content: "new"}, owner.edit.PendingMessage)
	assert.Same(t, before, a.Session())
	assert.Equal(t, epoch, a.bridgeEpoch.Load())
	assert.Empty(t, a.events)
	var typed *runtime.SessionError
	require.ErrorAs(t, a.EditPendingMessage(t.Context(), "other", "turn", "bad"), &typed)
	assert.Equal(t, runtime.SessionErrorWrongSession, typed.Kind)
	assert.Equal(t, "new", owner.edit.PendingMessage.Content)
	require.NoError(t, a.EditSession(t.Context(), runtime.SessionEdit{Kind: runtime.SessionEditPendingMessage, PendingMessage: &runtime.PendingMessageEdit{TurnID: "turn", Content: "again"}}))
	assert.Same(t, before, a.Session())
	assert.Empty(t, a.events)
}

func TestPendingEditProjectionReplacesWithoutInsertionOrLifecycleChange(t *testing.T) {
	a := newMetadataTestApp(t)
	a.projectEvent(&SessionResetEvent{Snapshot: runtime.SessionSnapshot{Session: session.New(session.WithID("s")), Status: runtime.SessionStatus{SessionID: "s", State: runtime.SessionStateRunning}}})
	for i, id := range []string{"first", "target", "last"} {
		event := runtime.PendingUserMessageAccepted("s", id, id, nil, i).(*runtime.PendingUserMessageAcceptedEvent)
		event.InputOrigin = session.InputOriginUser
		a.projectEvent(event)
	}
	before := a.Presentation()
	a.projectEvent(runtime.PendingUserMessageEdited("s", "target", "new", nil, 1))
	after := a.Presentation()
	require.Len(t, after.PendingInputs, 3)
	assert.Equal(t, "target", before.PendingInputs[1].Content)
	assert.Equal(t, "new", after.PendingInputs[1].Content)
	assert.Equal(t, before.Lifecycle, after.Lifecycle)
	assert.Equal(t, before.Status, after.Status)
	items := a.Session().MessagesSnapshot()
	require.Len(t, items, 3)
	assert.Equal(t, "new", items[1].Message.Message.Content)
	assert.True(t, items[1].Message.Pending)
	a.projectEvent(runtime.PendingUserMessageEdited("s", "missing", "do not insert", nil, 5))
	assert.Len(t, a.Presentation().PendingInputs, 3)
	assert.Equal(t, 3, a.Session().ItemCount())
	a.projectEvent(runtime.PendingUserMessageEdited("other", "target", "wrong", nil, 1))
	assert.Equal(t, "new", a.Presentation().PendingInputs[1].Content)
	assert.Equal(t, "new", a.Session().MessagesSnapshot()[1].Message.Message.Content)
}
