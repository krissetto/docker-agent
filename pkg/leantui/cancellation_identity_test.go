package leantui

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

type leanCancellationHandle struct {
	runtime.SessionHandle

	id            string
	turnID        string
	stops         int
	cancelledTurn string
}

func (h *leanCancellationHandle) Cancel(ctx context.Context, turnID string) (runtime.CancelResult, error) {
	h.stops++
	h.cancelledTurn = turnID
	return h.SessionHandle.Cancel(ctx, turnID)
}

type leanCancellationRuntime struct {
	runtime.SessionRuntime

	handle *leanCancellationHandle
}

func (r *leanCancellationRuntime) CreateSession(context.Context, *session.Session, runtime.SessionBinding) (runtime.SessionHandle, error) {
	return r.handle, nil
}

func (r *leanCancellationRuntime) SessionByID(string) (runtime.SessionHandle, error) {
	return r.handle, nil
}

func cancellationModel(t *testing.T) (*model, *leanCancellationHandle) {
	t.Helper()
	provider := &viewerLifecycleProvider{calls: make(chan viewerLifecycleCall, 4)}
	local, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("agent", "fixture", agent.WithModel(provider)))), runtime.WithSessionStore(session.NewInMemorySessionStore()))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(local)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	sess := session.New(session.WithAgentName("agent"), session.WithWorkingDir(t.TempDir()))
	handle, err := owner.Runtime().CreateSession(t.Context(), sess, runtime.SessionBinding{AgentName: "agent"})
	require.NoError(t, err)
	accepted, err := handle.Submit(t.Context(), runtime.TurnInput{Content: "active fake provider"})
	require.NoError(t, err)
	viewerLifecycleNextCall(t, provider)
	snapshot, err := handle.Snapshot(t.Context())
	require.NoError(t, err)
	wrapped := &leanCancellationHandle{id: handle.ID(), turnID: accepted.TurnID, SessionHandle: handle}
	m := bareModel(80)
	m.app = app.New(t.Context(), &leanCancellationRuntime{handle: wrapped, SessionRuntime: owner.Runtime()}, snapshot, runtime.SessionBinding{AgentName: "agent"}, app.WithRuntimeServices(&viewerLifecycleServices{}))
	t.Cleanup(m.app.Close)
	m.app.Start(t.Context())
	require.Eventually(t, func() bool { p := m.app.Presentation(); return p != nil && p.Status.TurnID == accepted.TurnID }, time.Second, time.Millisecond)
	return m, wrapped
}

func TestLeanCapturedInterruptCannotCancelReplacement(t *testing.T) {
	for _, mode := range []string{"always", "double-tap"} {
		t.Run(mode, func(t *testing.T) {
			m, handle := cancellationModel(t)
			m.lifecycle.Status = runtime.SessionStateRunning
			m.interruptMode = mode
			m.handleKey(t.Context(), ui.Key{Typ: ui.KeyEsc})
			require.NotNil(t, m.interruptCancel)
			intent := m.interruptCancel
			m.app.ReplaceSession(t.Context(), m.app.Session())
			require.Eventually(t, func() bool { return m.app.Presentation() != nil }, time.Second, time.Millisecond)
			if mode == "always" {
				m.handleKey(t.Context(), ui.Key{Typ: ui.KeyRune, Runes: []rune{'y'}})
			} else {
				m.handleKey(t.Context(), ui.Key{Typ: ui.KeyEsc})
			}
			assert.Zero(t, handle.stops, "confirmation buffered across replacement cannot retarget the current view")
			m.handleEvent(t.Context(), &app.SessionResetEvent{Snapshot: runtime.SessionSnapshot{Status: m.app.Presentation().Status}})
			assert.False(t, m.interruptPending)
			assert.Nil(t, m.interruptCancel)
			assert.Equal(t, runtime.CancelNotActive, intent())
			assert.Zero(t, handle.stops)
			status, err := handle.Status(t.Context())
			require.NoError(t, err)
			assert.Equal(t, runtime.SessionStateRunning, status.State)
			assert.Equal(t, status.TurnID, m.app.Presentation().Status.TurnID)
			m.interruptMode = "none"
			m.handleKey(t.Context(), ui.Key{Typ: ui.KeyEsc})
			assert.Equal(t, 1, handle.stops)
			assert.Equal(t, status.TurnID, handle.cancelledTurn)
			require.NoError(t, handle.AwaitTurn(t.Context(), status.TurnID))
		})
	}
}
