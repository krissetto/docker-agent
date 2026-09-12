package app

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

// appModelSession is a session that owns its model override the way the real
// session does: SetModel mutates and "persists" the session it holds,
// and Snapshot projects that session back to the App.
type appModelSession struct {
	projectionSession

	agentName string
	sess      *session.Session
	setErr    error
	setCalls  []string
	models    []runtime.ModelChoice
	modelRef  string
}

func (a *appModelSession) Metadata() runtime.SessionMetadata {
	metadata := a.projectionSession.Metadata()
	metadata.Capabilities.ModelSwitching = a.models != nil
	return metadata
}

func (a *appModelSession) AgentName() string {
	if a.agentName != "" {
		return a.agentName
	}
	return a.projectionSession.AgentName()
}

func (a *appModelSession) SupportsModelSwitching() bool                          { return a.models != nil }
func (a *appModelSession) AvailableModels(context.Context) []runtime.ModelChoice { return a.models }
func (a *appModelSession) SetModel(_ context.Context, modelRef string) error {
	a.setCalls = append(a.setCalls, modelRef)
	if a.setErr != nil {
		return a.setErr
	}
	a.modelRef = modelRef
	if a.sess != nil {
		if a.sess.AgentModelOverrides == nil {
			a.sess.AgentModelOverrides = map[string]string{}
		}
		a.sess.AgentModelOverrides[a.AgentName()] = modelRef
	}
	return nil
}

func (a *appModelSession) Snapshot(context.Context) (*session.Session, error) {
	if a.sess == nil {
		return nil, errors.New("no session")
	}
	return a.sess.Clone(), nil
}

func (a *appModelSession) RefreshModelsCatalog(context.Context) error { return runtime.ErrUnsupported }

func TestAppModelSwitchingUsesSessionCapabilityAndBinding(t *testing.T) {
	sess := session.New(session.WithAgentName("root"))
	sess.AgentModelOverrides = map[string]string{"worker": "other/model"}
	handle := &appModelSession{agentName: "worker", sess: sess, models: []runtime.ModelChoice{{Ref: "other/model"}}}
	a := &App{currentState: sessionState{session: sess, handle: handle, binding: runtime.SessionBinding{AgentName: "worker"}}, events: make(chan any, 8)}

	assert.True(t, a.SupportsModelSwitching())
	models := a.AvailableModels(t.Context())
	require.Len(t, models, 1)
	assert.True(t, models[0].IsCurrent)

	require.NoError(t, a.SetCurrentAgentModel(t.Context(), "openai/gpt-4o"))
	assert.Equal(t, []string{"openai/gpt-4o"}, handle.setCalls)
	assert.Equal(t, "openai/gpt-4o", handle.modelRef)
	assert.Equal(t, "openai/gpt-4o", a.Session().AgentModelOverrides["worker"], "the App must project the session-owned override")
	assert.Equal(t, "root", a.Session().AgentName, "model switching must not follow mutable/global agent state")
}

func TestAppModelSwitchFailureLeavesProjectionUntouched(t *testing.T) {
	setErr := errors.New("provider unavailable")
	sess := session.New(session.WithAgentName("worker"))
	sess.AgentModelOverrides = map[string]string{"worker": "old/model"}
	handle := &appModelSession{agentName: "worker", sess: sess, models: []runtime.ModelChoice{{Ref: "new/model"}}, modelRef: "old/model", setErr: setErr}
	a := &App{currentState: sessionState{session: sess, handle: handle, binding: runtime.SessionBinding{AgentName: "worker"}}, events: make(chan any, 8)}

	err := a.SetCurrentAgentModel(t.Context(), "new/model")
	require.ErrorIs(t, err, setErr)
	assert.Equal(t, "old/model", handle.modelRef)
	assert.Equal(t, "old/model", a.Session().AgentModelOverrides["worker"])
}

func TestAppModelSwitchingWithoutSessionCapabilityIsUnsupported(t *testing.T) {
	a := &App{currentState: sessionState{session: session.New(), handle: &projectionSession{}}}

	assert.False(t, a.SupportsModelSwitching())
	assert.Nil(t, a.AvailableModels(t.Context()))
	require.ErrorIs(t, a.SetCurrentAgentModel(t.Context(), "openai/gpt-4o"), runtime.ErrUnsupported)
}
