package message

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type openerDefaultClock struct{ now time.Time }

func (c *openerDefaultClock) Now() time.Time { return c.now }
func (c *openerDefaultClock) Tick(delay time.Duration, create func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg { c.now = c.now.Add(delay); return create(c.now) }
}

func openerDefaultReport(body string) *types.Message {
	return &types.Message{
		Type:        types.MessageTypeAgentInput,
		InputOrigin: session.InputOriginAgent,
		SenderName:  "reviewer",
		Content:     body,
	}
}

func openerDefaultSettle(t *testing.T, ar *animation.Runtime, view *messageModel) {
	t.Helper()
	for range 500 {
		cmd := ar.Continue()
		if cmd == nil {
			return
		}
		tick, ok := ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		view.Update(tick)
	}
	require.Zero(t, ar.ActiveCount(), "the disclosure transition settles")
}

func TestSessionOpenerDefaultOpensWithoutAnimation(t *testing.T) {
	ar := animation.NewRuntimeWithScheduler(&openerDefaultClock{now: time.Unix(1, 0)})
	view := New(ar, openerDefaultReport("OPENER_BODY"), nil)
	t.Cleanup(view.StopAnimation)
	require.NotContains(t, stripANSI(view.Render(80)), "OPENER_BODY")

	require.True(t, view.SetSessionOpenerDefault(true))
	require.True(t, view.IsExpanded())
	require.Zero(t, ar.ActiveCount(), "the default opens without a transition lease")
	require.Contains(t, stripANSI(view.Render(80)), "OPENER_BODY")
	require.False(t, view.SetSessionOpenerDefault(true), "an already open default is a no-op")
}

func TestSessionOpenerDefaultYieldsToUserDisclosure(t *testing.T) {
	ar := animation.NewRuntimeWithScheduler(&openerDefaultClock{now: time.Unix(1, 0)})
	view := New(ar, openerDefaultReport("OPENER_BODY"), nil)
	t.Cleanup(view.StopAnimation)
	require.True(t, view.SetSessionOpenerDefault(true))

	view.Toggle()
	openerDefaultSettle(t, ar, view)
	require.False(t, view.IsExpanded())
	require.NotContains(t, stripANSI(view.Render(80)), "OPENER_BODY")
	require.False(t, view.SetSessionOpenerDefault(true), "a user collapse outranks the default")

	view.Toggle()
	openerDefaultSettle(t, ar, view)
	require.True(t, view.IsExpanded())
	require.False(t, view.SetSessionOpenerDefault(false), "a user reopen outranks closing the default")
	require.Contains(t, stripANSI(view.Render(80)), "OPENER_BODY")
}

func TestSessionOpenerDefaultIgnoresOtherMessages(t *testing.T) {
	ar := animation.NewRuntimeWithScheduler(&openerDefaultClock{now: time.Unix(1, 0)})
	for _, msg := range []*types.Message{
		types.Agent(types.MessageTypeAssistant, "root", "reply"),
		{Type: types.MessageTypeRuntimeNotice, Content: "notice"},
		types.User("human"),
	} {
		view := New(ar, msg, nil)
		t.Cleanup(view.StopAnimation)
		require.False(t, view.SetSessionOpenerDefault(true), "%v is not an incoming assignment", msg.Type)
		require.False(t, view.IsExpanded())
	}
}
