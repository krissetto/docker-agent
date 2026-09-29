package chat

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// agentSwitchCall records one SetAgentSwitching invocation on the sidebar.
type agentSwitchCall struct {
	switching bool
	from, to  string
}

// recordingSidebar wraps the real sidebar to record the delegation-related
// calls the chat page makes on it (and the results the sidebar returned). It
// only survives handlers that do not forward events through sidebar.Update
// (which replaces p.sidebar with the unwrapped model), which is exactly the
// set under test here.
type recordingSidebar struct {
	sidebar.Model

	switching []agentSwitchCall
	results   []sidebar.AgentSwitchResult
	activity  []string
}

func (r *recordingSidebar) SetAgentSwitching(switching bool, fromAgent, toAgent string) sidebar.AgentSwitchResult {
	r.switching = append(r.switching, agentSwitchCall{switching: switching, from: fromAgent, to: toAgent})
	res := r.Model.SetAgentSwitching(switching, fromAgent, toAgent)
	r.results = append(r.results, res)
	return res
}

func (r *recordingSidebar) SetAgentActivity(agentName string) tea.Cmd {
	r.activity = append(r.activity, agentName)
	return r.Model.SetAgentActivity(agentName)
}

// newSwitchingTestPage builds a chat page whose sidebar records delegation
// calls.
func newSwitchingTestPage(t *testing.T) (*chatPage, *recordingSidebar) {
	t.Helper()
	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(queueTestRuntime{})), service.NewSessionState(sess)).(*chatPage)
	// A tall layout keeps every sidebar section on screen regardless of how
	// long the checkout path and git branch rendered above them happen to be.
	_ = p.SetSize(160, 80)
	rec := &recordingSidebar{Model: p.sidebar}
	p.sidebar = rec
	// Tests start transfers without always stopping them; the cancel clears
	// the sidebar's animation subscription so no registration leaks on the
	// global coordinator. Sending through p.sidebar reaches the real model
	// even while it is still the wrapper (Update is promoted from the
	// embedded Model) and after a handler unwrapped it.
	t.Cleanup(func() {
		_, _ = p.sidebar.Update(msgtypes.StreamCancelledMsg{})
	})
	return p, rec
}

// TestAgentSwitchingReturnAddsTransition verifies the end of a transfer_task
// hop — matching its recorded start — adds the "child returned control to
// parent" transition to the chat and reaches the sidebar's return
// presentation.
func TestAgentSwitchingReturnAddsTransition(t *testing.T) {
	t.Parallel()

	p, rec := newSwitchingTestPage(t)

	handled, _ := p.handleRuntimeEvent(runtime.AgentSwitching(true, "root", "researcher"))
	require.True(t, handled)

	handled, cmd := p.handleRuntimeEvent(runtime.AgentSwitching(false, "researcher", "root"))
	require.True(t, handled)
	require.NotNil(t, cmd)

	out := ansi.Strip(p.messages.View())
	assert.Contains(t, out, "researcher")
	assert.Contains(t, out, "root")
	assert.Contains(t, out, types.AgentReturnLabel)

	require.Len(t, rec.switching, 2, "the sidebar sees both hop boundaries")
	assert.Equal(t, agentSwitchCall{switching: false, from: "researcher", to: "root"}, rec.switching[1])
	assert.True(t, rec.results[1].Accepted, "the stop closed its recorded start")
}

// TestAgentSwitchingStartAddsNoTransition verifies the start of a hop adds no
// return transition (the transfer_task tool call already narrates it) while
// still reaching the sidebar.
func TestAgentSwitchingStartAddsNoTransition(t *testing.T) {
	t.Parallel()

	p, rec := newSwitchingTestPage(t)

	handled, _ := p.handleRuntimeEvent(runtime.AgentSwitching(true, "root", "researcher"))
	require.True(t, handled)

	assert.NotContains(t, ansi.Strip(p.messages.View()), types.AgentReturnLabel)
	require.Len(t, rec.switching, 1)
	assert.Equal(t, agentSwitchCall{switching: true, from: "root", to: "researcher"}, rec.switching[0])
}

// TestAgentSwitchingStaleStopAddsNothing verifies a stop the sidebar did not
// accept — no recorded start to close — adds no transition: an empty stack
// stays empty and a stale stop cannot steal a different in-flight hop.
func TestAgentSwitchingStaleStopAddsNothing(t *testing.T) {
	t.Parallel()

	p, rec := newSwitchingTestPage(t)

	// Stop on an empty stack: no label.
	handled, _ := p.handleRuntimeEvent(runtime.AgentSwitching(false, "researcher", "root"))
	require.True(t, handled)
	assert.NotContains(t, ansi.Strip(p.messages.View()), types.AgentReturnLabel)
	require.Len(t, rec.results, 1)
	assert.False(t, rec.results[0].Accepted)

	// A different hop is in flight; the stale stop must not pop it nor label.
	handled, _ = p.handleRuntimeEvent(runtime.AgentSwitching(true, "root", "coder"))
	require.True(t, handled)
	handled, _ = p.handleRuntimeEvent(runtime.AgentSwitching(false, "researcher", "root"))
	require.True(t, handled)
	assert.NotContains(t, ansi.Strip(p.messages.View()), types.AgentReturnLabel)
	require.Len(t, rec.results, 3)
	assert.False(t, rec.results[2].Accepted, "the stale stop must not match the root→coder hop")

	// The genuine stop still labels.
	handled, _ = p.handleRuntimeEvent(runtime.AgentSwitching(false, "coder", "root"))
	require.True(t, handled)
	assert.Contains(t, ansi.Strip(p.messages.View()), types.AgentReturnLabel)
}

// TestAgentSwitchingReturnAfterCancelStaysSilent verifies hop boundaries
// unwinding after the user cancelled the stream are dropped entirely: the
// delegation they belong to was already torn down visually.
func TestAgentSwitchingReturnAfterCancelStaysSilent(t *testing.T) {
	t.Parallel()

	p, rec := newSwitchingTestPage(t)

	// The hop started before the cancel; its end races the cancel teardown.
	handled, _ := p.handleRuntimeEvent(runtime.AgentSwitching(true, "root", "researcher"))
	require.True(t, handled)
	p.streamCancelled = true

	handled, cmd := p.handleRuntimeEvent(runtime.AgentSwitching(false, "researcher", "root"))
	require.True(t, handled)
	assert.Nil(t, cmd)
	assert.NotContains(t, ansi.Strip(p.messages.View()), types.AgentReturnLabel)
	assert.Len(t, rec.switching, 1, "the sidebar sees no boundary after the cancel")
}

// TestChildActivityEventsAckSidebar verifies the child-activity signals —
// reasoning, content, and (partial) tool calls, which models may emit without
// any text — all acknowledge the sidebar's outbound transfer presentation.
func TestChildActivityEventsAckSidebar(t *testing.T) {
	t.Parallel()

	p, rec := newSwitchingTestPage(t)
	p.handleRuntimeEvent(runtime.AgentSwitching(true, "root", "researcher"))

	events := []tea.Msg{
		runtime.AgentChoiceReasoning("researcher", "s1", "thinking…"),
		runtime.AgentChoice("researcher", "s1", "found it"),
		runtime.PartialToolCall(tools.ToolCall{ID: "t1"}, tools.Tool{Name: "shell"}, "researcher"),
	}
	for _, evt := range events {
		handled, _ := p.handleRuntimeEvent(evt)
		require.Truef(t, handled, "%T must be handled", evt)
	}

	assert.Equal(t, []string{"researcher", "researcher", "researcher"}, rec.activity,
		"each useful child event acknowledges the transfer")
}

// TestChildActivityIgnoredAfterCancel verifies content events of a cancelled
// stream do not reach the sidebar's activity tracking.
func TestChildActivityIgnoredAfterCancel(t *testing.T) {
	t.Parallel()

	p, rec := newSwitchingTestPage(t)
	p.streamCancelled = true

	handled, _ := p.handleRuntimeEvent(runtime.AgentChoice("researcher", "s1", "late chunk"))
	require.True(t, handled)
	assert.Empty(t, rec.activity)
}

// TestScheduleTransferTimersWrapsInRoutedMsg executes the real timer command
// produced for a routed page (with a synthetic short duration) and verifies
// the expiry is wrapped in a messages.RoutedMsg addressed to the page's tab,
// while a page without a routing identity delivers the raw payload.
func TestScheduleTransferTimersWrapsInRoutedMsg(t *testing.T) {
	t.Parallel()

	type payload struct{ n int }

	p, _ := newSwitchingTestPage(t)
	p.SetRoutingID("tab-1")
	cmd := p.scheduleTransferTimers([]sidebar.TransferTimer{{Duration: time.Millisecond, Msg: payload{n: 42}}})
	require.NotNil(t, cmd)

	msgs := runTimerCmd(t, cmd)
	require.Len(t, msgs, 1)
	assert.Equal(t, msgtypes.RoutedMsg{SessionID: "tab-1", Inner: payload{n: 42}}, msgs[0],
		"the expiry is addressed to the owning tab")

	p.pendingTimers = nil
	p.SetRoutingID("")
	cmd = p.scheduleTransferTimers([]sidebar.TransferTimer{{Duration: time.Millisecond, Msg: payload{n: 7}}})
	msgs = runTimerCmd(t, cmd)
	require.Len(t, msgs, 1)
	assert.Equal(t, payload{n: 7}, msgs[0], "standalone pages deliver the raw payload")
}

// runTimerCmd executes a (possibly batched) command, following nested batches,
// and returns the messages it produces.
func runTimerCmd(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var msgs []tea.Msg
		for _, inner := range batch {
			msgs = append(msgs, runTimerCmd(t, inner)...)
		}
		return msgs
	}
	return []tea.Msg{msg}
}

// TestAgentSwitchingArmsRoutedTimersForBackgroundDispatch verifies a hop
// boundary records its routed timer commands so TakeRoutedTimers can hand
// them to the appModel when the page is hidden (its regular command is
// discarded there), and that the collection drains exactly once.
func TestAgentSwitchingArmsRoutedTimersForBackgroundDispatch(t *testing.T) {
	t.Parallel()

	p, _ := newSwitchingTestPage(t)
	p.SetRoutingID("tab-1")

	model, _ := p.Update(runtime.AgentSwitching(true, "root", "researcher"))
	page := model.(*chatPage)

	timers := page.TakeRoutedTimers()
	require.NotNil(t, timers, "a start's min/max timers are collectable for background dispatch")
	assert.Nil(t, page.TakeRoutedTimers(), "the collection drains once")

	// The next Update clears leftovers so a later drain cannot re-arm
	// timers that the active path already dispatched.
	model, _ = page.Update(runtime.AgentSwitching(false, "researcher", "root"))
	page = model.(*chatPage)
	require.NotNil(t, page.TakeRoutedTimers(), "an accepted stop arms the Return timer")

	model, _ = page.Update(msgtypes.StreamCancelledMsg{})
	page = model.(*chatPage)
	assert.Nil(t, page.TakeRoutedTimers(), "updates without boundaries arm nothing")
}

// TestRoutedTimerExpiryDrivesSidebarOnOwnerPage chains the real pieces: the
// hop boundary's timers are wrapped for the page's tab, and delivering their
// inner payloads back to the page (as the appModel's routing does) drives the
// sidebar's presentation windows.
func TestRoutedTimerExpiryDrivesSidebarOnOwnerPage(t *testing.T) {
	t.Parallel()

	p, rec := newSwitchingTestPage(t)
	p.SetRoutingID("tab-1")
	p.sessionState.SetCurrentAgentName("root")

	handled, _ := p.handleRuntimeEvent(runtime.AgentSwitching(true, "root", "researcher"))
	require.True(t, handled)
	layoutCmd := tea.Batch(rec.results[0].Cmd, p.reconcileSidebarLayout(), p.ar.Continue())
	var nextTick func(tea.Cmd) (animation.TickMsg, bool)
	nextTick = func(cmd tea.Cmd) (animation.TickMsg, bool) {
		if cmd == nil {
			return animation.TickMsg{}, false
		}
		msg := cmd()
		if tick, ok := msg.(animation.TickMsg); ok {
			return tick, true
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, nested := range batch {
				if tick, found := nextTick(nested); found {
					return tick, true
				}
			}
		}
		return animation.TickMsg{}, false
	}
	advance := func(cmd tea.Cmd) tea.Cmd {
		if tick, found := nextTick(cmd); found {
			if accepted, current := p.ar.Accept(tick); current {
				_, updateCmd := p.Update(accepted)
				return tea.Batch(updateCmd, p.ar.Continue())
			}
		}
		return nil
	}
	// Reveal the recap, then explicitly expand its initially folded rows.
	for range 30 {
		layoutCmd = advance(layoutCmd)
	}
	summaryY := renderedLineContaining(t, p.sidebar.View(), "subagents")
	sl := p.computeSidebarLayout()
	_, expandCmd := p.sidebar.Update(tea.MouseClickMsg{X: styles.AppPadding + sl.sidebarStartX + 2, Y: summaryY, Button: tea.MouseLeft})
	layoutCmd = tea.Batch(layoutCmd, expandCmd, p.ar.Continue())
	// The page owner commits the shared spinner registration after the hop.
	for step := 0; step < 30 && !strings.Contains(ansi.Strip(p.sidebar.View()), "researcher"); step++ {
		layoutCmd = advance(layoutCmd)
	}
	require.Contains(t, ansi.Strip(p.sidebar.View()), "researcher", "hop relation enters through its owner animation command")
	require.Contains(t, ansi.Strip(p.sidebar.View()), "root")
	require.Contains(t, ansi.Strip(p.sidebar.View()), "►")
	require.Len(t, rec.results, 1)
	timers := rec.results[0].Timers
	require.Len(t, timers, 2)

	// Rewrap each real payload through the page's routed tick (short
	// duration), check the envelope targets this tab, and deliver the
	// unwrapped payload to the page as handleRoutedMsg does.
	for _, timer := range timers {
		msg := p.routedTimerCmd(sidebar.TransferTimer{Duration: time.Millisecond, Msg: timer.Msg})()
		routed, ok := msg.(msgtypes.RoutedMsg)
		require.True(t, ok)
		assert.Equal(t, "tab-1", routed.SessionID)

		_, timerCmd := p.Update(routed.Inner)
		layoutCmd = tea.Batch(layoutCmd, timerCmd)
	}

	// Min then max elapsed without activity: drain the existing presentation exit.
	for step := 0; step < 30 && strings.Contains(ansi.Strip(p.sidebar.View()), "►"); step++ {
		layoutCmd = advance(layoutCmd)
	}
	assert.NotContains(t, ansi.Strip(p.sidebar.View()), "►")
}
