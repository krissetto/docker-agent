package chat

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// The first-response exit comes from a real stream: the page sees the answer
// and its stop. The exit leaves as Global work, so a hidden tab still applies
// it, and a routed exit carries the origin page and the app captured at
// dispatch. Swapping the page's app before the timer fires must not retarget it.
func TestFirstResponseExitFromRealStreamIsGlobalAndOriginBound(t *testing.T) {
	t.Parallel()
	sess := session.New(session.WithID("s"))
	a := app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(queueTestRuntime{}), app.WithExitAfterFirstResponse())
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	p := New(ar, t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	t.Cleanup(func() { Cleanup(p) })
	p.SetRoutingID("tab-2")
	p.messages.SetSize(100, 40)

	_, _ = p.UpdateEffects(runtime.StreamStarted("s", "root"))
	_, _ = p.UpdateEffects(runtime.AgentChoice("root", "s", "FIRST ANSWER", "answer"))
	_, effects := p.UpdateEffects(runtime.StreamStopped("s", "root", "normal"))
	require.NotNil(t, effects.Global, "the exit is application work, reported as Global")
	assert.Zero(t, countGlobal(runTimerCmd(t, effects.Visible)), "a hidden tab's exit is not chrome")

	cmd := effects.Cmd(false)
	b := app.New(t.Context(), nil, session.New(), runtime.SessionBinding{}, app.WithRuntimeServices(queueTestRuntime{}))
	p.app = b

	global := requireGlobal(t, runTimerCmd(t, cmd))
	assert.Equal(t, "tab-2", global.TabID)
	assert.Same(t, p, global.Origin, "origin is the page that produced the exit")
	assert.Same(t, a, global.Application, "application is the app captured at dispatch, not the swapped one")
	assert.IsType(t, msgtypes.ExitAfterFirstResponseMsg{}, global.Inner)
}

// An unrouted page has no tab to address, so its exit is the bare message.
func TestFirstResponseExitUnroutedIsBareMessage(t *testing.T) {
	t.Parallel()
	sess := session.New(session.WithID("s"))
	a := app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(queueTestRuntime{}), app.WithExitAfterFirstResponse())
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	p := New(ar, t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	t.Cleanup(func() { Cleanup(p) })
	p.messages.SetSize(100, 40)

	_, _ = p.UpdateEffects(runtime.StreamStarted("s", "root"))
	_, _ = p.UpdateEffects(runtime.AgentChoice("root", "s", "FIRST ANSWER", "answer"))
	_, effects := p.UpdateEffects(runtime.StreamStopped("s", "root", "normal"))
	msgs := runTimerCmd(t, effects.Cmd(false))
	require.Len(t, msgs, 1)
	assert.IsType(t, msgtypes.ExitAfterFirstResponseMsg{}, msgs[0])
}

func countGlobal(msgs []tea.Msg) int {
	n := 0
	for _, msg := range msgs {
		if _, ok := msg.(GlobalMsg); ok {
			n++
		}
	}
	return n
}

func requireGlobal(t *testing.T, msgs []tea.Msg) GlobalMsg {
	t.Helper()
	for _, msg := range msgs {
		if global, ok := msg.(GlobalMsg); ok {
			return global
		}
	}
	require.Failf(t, "no GlobalMsg", "got %d messages", len(msgs))
	return GlobalMsg{}
}
