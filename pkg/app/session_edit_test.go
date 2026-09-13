package app

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

type editingProjectionSession struct {
	projectionSession

	sess  *session.Session
	edit  runtime.SessionEdit
	store session.Store
}

func (h *editingProjectionSession) ID() string        { return h.sess.ID }
func (h *editingProjectionSession) AgentName() string { return h.sess.AgentName }
func (h *editingProjectionSession) Metadata() runtime.SessionMetadata {
	return runtime.SessionMetadata{SessionID: h.sess.ID}
}

func (h *editingProjectionSession) Edit(_ context.Context, edit runtime.SessionEdit) (*session.Session, error) {
	h.edit = edit
	if edit.ToggleToolsApproved {
		h.sess.ToggleYolo()
	}
	if edit.Kind == runtime.SessionEditAttachment {
		h.sess.AddAttachedFile(edit.AttachmentPath)
	}
	return h.sess.Clone(), nil
}

func (h *editingProjectionSession) Snapshot(context.Context) (*session.Session, error) {
	return h.sess.Clone(), nil
}

func (h *editingProjectionSession) RemoveAttachment(ctx context.Context, path string) error {
	h.sess.RemoveAttachedFile(path)
	if h.store != nil {
		return h.store.UpdateSession(ctx, h.sess)
	}
	return nil
}

func TestPolicyEditRoutesToOwnerAndPublishesDetachedMetadata(t *testing.T) {
	original := session.New(session.WithID("s"), session.WithSafetyPolicy(session.SafetyPolicyBalanced))
	owner := &editingProjectionSession{sess: original}
	a := newMetadataTestApp(t)
	a.replaceSessionState(sessionState{session: original, handle: owner})
	before := a.Session()
	require.NoError(t, a.EditSession(t.Context(), runtime.SessionEdit{Kind: runtime.SessionEditPolicy, ToggleToolsApproved: true}))
	assert.True(t, owner.edit.ToggleToolsApproved)
	assert.False(t, before.IsToolsApproved(), "read-only prior snapshot remains unchanged")
	assert.True(t, a.Session().IsToolsApproved())
	assert.NotSame(t, original, a.Session())
	projected := (<-a.events).(SessionEventMsg).Event.(*SessionViewEvent)
	assert.Equal(t, session.SafetyPolicyBalanced, projected.Session.GetPriorSafetyPolicy())
	require.NoError(t, a.EditSession(t.Context(), runtime.SessionEdit{Kind: runtime.SessionEditPolicy, ToggleToolsApproved: true}))
	assert.Equal(t, session.SafetyPolicyBalanced, a.Session().GetSafetyPolicy())
}
