package chat

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestCanonicalStopWaitsForMatchingSettlement(t *testing.T) {
	for _, outcome := range []runtime.TurnOutcome{runtime.TurnCompleted, runtime.TurnCanceled, runtime.TurnFailed} {
		t.Run(string(outcome), func(t *testing.T) {
			sess := session.New()
			a, _ := newSessionTestApp(t, sess, nil, nil)
			p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
			p.applyProjection(runtime.SessionSnapshot{Status: runtime.SessionStatus{SessionID: sess.ID, State: runtime.SessionStateSettled}}, false)
			p.handleRuntimeEvent(msgtypes.SessionRuntimeEventMsg{OriginSessionID: sess.ID, TurnID: "one", Sequence: 1, Event: runtime.StreamStarted(sess.ID, "root")})
			p.handleRuntimeEvent(runtime.StreamStopped(sess.ID, "root", "normal"))
			require.True(t, p.IsWorking(), "provider completion is not durable settlement")
			require.Zero(t, p.lifecycle.Depth(), "presentation stream has ended")
			require.Equal(t, "one", p.lifecycle.TurnID)
			for _, event := range []*runtime.TurnSettledEvent{{SessionID: "foreign", TurnID: "one"}, {SessionID: sess.ID, TurnID: "old"}} {
				p.handleRuntimeEvent(event)
				require.True(t, p.IsWorking())
			}
			p.handleRuntimeEvent(&runtime.TurnSettledEvent{SessionID: sess.ID, TurnID: "one", Outcome: outcome})
			require.False(t, p.IsWorking())
			require.Empty(t, p.lifecycle.TurnID)
			p.handleRuntimeEvent(msgtypes.SessionRuntimeEventMsg{OriginSessionID: sess.ID, TurnID: "two", Sequence: 2, Event: runtime.StreamStarted(sess.ID, "root")})
			p.handleRuntimeEvent(&runtime.TurnSettledEvent{SessionID: sess.ID, TurnID: "one", Outcome: outcome})
			require.True(t, p.IsWorking(), "old settlement cannot finish a successor")
		})
	}
}

func TestCanonicalSettledResetRecoversMissedTerminalEvent(t *testing.T) {
	sess := session.New()
	a, _ := newSessionTestApp(t, sess, nil, nil)
	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	p.applyProjection(runtime.SessionSnapshot{Status: runtime.SessionStatus{SessionID: sess.ID, State: runtime.SessionStateRunning, TurnID: "missed"}}, false)
	require.True(t, p.IsWorking())
	p.applyProjection(runtime.SessionSnapshot{Status: runtime.SessionStatus{SessionID: sess.ID, State: runtime.SessionStateSettled}}, false)
	require.False(t, p.IsWorking())
	require.Empty(t, p.lifecycle.TurnID)
}

func TestCompatibilityStopWithoutCanonicalIdentityFinishes(t *testing.T) {
	sess := session.New()
	a, _ := newSessionTestApp(t, sess, nil, nil)
	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	p.handleRuntimeEvent(runtime.StreamStarted(sess.ID, "root"))
	p.handleRuntimeEvent(runtime.StreamStopped(sess.ID, "root", "normal"))
	require.False(t, p.IsWorking())
}
