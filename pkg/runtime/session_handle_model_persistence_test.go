package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

type modelPersistFailStore struct {
	session.Store

	err error
}

func (s modelPersistFailStore) UpdateSession(context.Context, *session.Session) error { return s.err }

func TestDriverModelOverridePersistsCustomHistoryTransactionally(t *testing.T) {
	sess := session.New(session.WithAgentName("root"))
	sess.AgentModelOverrides = map[string]string{"root": "old/model"}
	sess.CustomModelsUsed = []string{"old/model"}
	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), sess))
	r := &LocalRuntime{sessionStore: store}
	d := &sessionDriver{r: r, sess: sess, events: newSessionEventHubWithLimits(8, 1<<20), interactions: map[string]sessionInteraction{}}
	require.NoError(t, d.SetModelOverride(t.Context(), "root", "new/model", nil))
	stored, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "new/model", stored.AgentModelOverrides["root"])
	assert.Equal(t, []string{"old/model", "new/model"}, stored.CustomModelsUsed)

	persistErr := errors.New("disk full")
	d.r.sessionStore = modelPersistFailStore{Store: store, err: persistErr}
	err = d.SetModelOverride(t.Context(), "root", "third/model", nil)
	require.ErrorIs(t, err, persistErr)
	assert.Equal(t, "new/model", sess.AgentModelOverrides["root"])
	assert.Equal(t, []string{"old/model", "new/model"}, sess.CustomModelsUsed)
}

func TestDriverModelOverrideRejectsCanceledResolution(t *testing.T) {
	sess := session.New(session.WithAgentName("root"))
	sess.SetAgentModelOverride("root", "old/model")
	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), sess))
	d := &sessionDriver{r: &LocalRuntime{sessionStore: store}, sess: sess, modelRef: "old/model"}
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // A provider may finish resolving after its request was canceled.
	require.ErrorIs(t, d.SetModelOverride(ctx, "root", "new/model", nil), context.Canceled)
	overrides, custom := sess.ModelStateSnapshot()
	assert.Equal(t, "old/model", overrides["root"])
	assert.Equal(t, []string{"old/model"}, custom)
	assert.Equal(t, "old/model", d.modelRef)
}
