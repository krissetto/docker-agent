package chat

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	chatmsg "github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/workblock"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// turnEvent delivers a runtime event bound to a root turn, as the session stream does.
func turnEvent(p *chatPage, sessionID, turnID string, event runtime.Event) tea.Cmd {
	_, cmd := p.handleRuntimeEvent(msgtypes.SessionRuntimeEventMsg{Event: event, OriginSessionID: sessionID, TurnID: turnID, Sequence: 1})
	return cmd
}

func newTimingPage(t *testing.T, id string) (*chatPage, *session.Session) {
	t.Helper()
	sess := session.New(session.WithID(id))
	a, _ := newSessionTestApp(t, sess, nil, nil)
	ar := animation.NewRuntime()
	p := New(ar, t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	t.Cleanup(func() { Cleanup(p); ar.Stop() })
	p.SetSize(100, 30)
	// The runtime seeds the lifecycle identity before any turn event arrives.
	p.applyProjection(runtime.SessionSnapshot{Status: runtime.SessionStatus{SessionID: sess.ID, State: runtime.SessionStateSettled}}, false)
	return p, sess
}

func timingFor(turnID string, duration time.Duration, outcome string) *session.TurnTiming {
	return &session.TurnTiming{Version: session.TurnTimingVersion, Basis: session.TurnTimingBasisActiveWall, TurnID: turnID, DurationMs: duration.Milliseconds(), Outcome: outcome}
}

// rootWorkBlock returns the single root Work block, or nil when none exists.
func rootWorkBlock(t *testing.T, p *chatPage) *workblock.Model {
	t.Helper()
	_ = p.messages.View() // headers are synced on render
	owner, ok := p.messages.(interface {
		WorkBlockAtSource(source int) *workblock.Model
	})
	require.True(t, ok)
	for source := range 16 {
		if block := owner.WorkBlockAtSource(source); block != nil {
			return block
		}
	}
	return nil
}

func TestTurnWaitingStateFollowsInteractionsAndSettlesFromTiming(t *testing.T) {
	p, sess := newTimingPage(t, "turn-wait-resolve")
	turnEvent(p, sess.ID, "turn-1", &runtime.UserMessageEvent{SessionID: sess.ID, Message: "deploy it", SessionPosition: -1, TurnID: "turn-1"})
	turnEvent(p, sess.ID, "turn-1", runtime.StreamStarted(sess.ID, "root"))
	turnEvent(p, sess.ID, "turn-1", runtime.AgentChoiceReasoning("root", sess.ID, "Checking the deploy."))
	call := tools.ToolCall{ID: "deploy-call", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"deploy"}`}}
	turnEvent(p, sess.ID, "turn-1", &runtime.ToolCallConfirmationEvent{SessionID: sess.ID, RequestID: call.ID, ToolCall: call, ToolDefinition: tools.Tool{Name: "shell"}})

	block := rootWorkBlock(t, p)
	require.NotNil(t, block)
	require.Equal(t, "Waiting for approval", block.Status().Waiting)
	require.False(t, p.IsWorking(), "the input spinner stops while blocked on approval")
	require.Contains(t, ansi.Strip(p.messages.View()), "Waiting for approval")
	require.NotContains(t, ansi.Strip(p.messages.View()), "Working")

	turnEvent(p, sess.ID, "turn-1", &runtime.InteractionResolvedEvent{SessionID: sess.ID, InteractionID: "other", Reason: runtime.InteractionResponded})
	require.Equal(t, "Waiting for approval", rootWorkBlock(t, p).Status().Waiting, "an unrelated resolution keeps the wait")

	turnEvent(p, sess.ID, "turn-1", &runtime.InteractionResolvedEvent{SessionID: sess.ID, InteractionID: call.ID, Reason: runtime.InteractionResponded})
	require.Empty(t, rootWorkBlock(t, p).Status().Waiting, "resolving clears the wait")
	require.True(t, rootWorkBlock(t, p).Status().Active)
	require.True(t, p.IsWorking(), "resolving re-arms the spinner while the turn runs")
	require.Contains(t, ansi.Strip(p.messages.View()), "Working")

	turnEvent(p, sess.ID, "turn-1", runtime.StreamStopped(sess.ID, "root", "normal"))
	require.False(t, rootWorkBlock(t, p).Status().Active)
	require.Zero(t, rootWorkBlock(t, p).Status().Elapsed, "no label before the runtime settles the turn")

	turnEvent(p, sess.ID, "turn-1", &runtime.TurnSettledEvent{SessionID: sess.ID, TurnID: "turn-1", Outcome: runtime.TurnCompleted, Timing: timingFor("turn-1", 12400*time.Millisecond, session.TurnTimingOutcomeCompleted)})
	require.Equal(t, 12400*time.Millisecond, rootWorkBlock(t, p).Status().Elapsed)
	require.Contains(t, ansi.Strip(p.messages.View()), "· 12s")
}

func TestSettledLabelFromTimingOutcomesAndOmissions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timing  func(turnID string) *session.TurnTiming
		outcome runtime.TurnOutcome
		want    string
	}{
		{name: "completed", timing: func(id string) *session.TurnTiming {
			return timingFor(id, 12400*time.Millisecond, session.TurnTimingOutcomeCompleted)
		}, outcome: runtime.TurnCompleted, want: "· 12s"},
		{name: "canceled reads as stopped", timing: func(id string) *session.TurnTiming {
			return timingFor(id, 12400*time.Millisecond, session.TurnTimingOutcomeCanceled)
		}, outcome: runtime.TurnCanceled, want: "Stopped after 12s"},
		{name: "failed", timing: func(id string) *session.TurnTiming {
			return timingFor(id, 3*time.Second, session.TurnTimingOutcomeFailed)
		}, outcome: runtime.TurnFailed, want: "Failed after 3s"},
		{name: "sub-second omitted", timing: func(id string) *session.TurnTiming {
			return timingFor(id, 999*time.Millisecond, session.TurnTimingOutcomeCompleted)
		}, outcome: runtime.TurnCompleted, want: ""},
		{name: "legacy turn without timing omitted", timing: func(string) *session.TurnTiming { return nil }, outcome: runtime.TurnCompleted, want: ""},
		{name: "mismatched timing omitted", timing: func(id string) *session.TurnTiming {
			return timingFor("another-turn", 12*time.Second, session.TurnTimingOutcomeCompleted)
		}, outcome: runtime.TurnCompleted, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, sess := newTimingPage(t, "timing-"+strings.ReplaceAll(tc.name, " ", "-"))
			turnEvent(p, sess.ID, "turn-x", &runtime.UserMessageEvent{SessionID: sess.ID, Message: "work", SessionPosition: -1, TurnID: "turn-x"})
			turnEvent(p, sess.ID, "turn-x", runtime.StreamStarted(sess.ID, "root"))
			turnEvent(p, sess.ID, "turn-x", runtime.AgentChoiceReasoning("root", sess.ID, "Thinking."))
			turnEvent(p, sess.ID, "turn-x", runtime.StreamStopped(sess.ID, "root", "normal"))
			turnEvent(p, sess.ID, "turn-x", &runtime.TurnSettledEvent{SessionID: sess.ID, TurnID: "turn-x", Outcome: tc.outcome, Timing: tc.timing("turn-x")})
			view := ansi.Strip(p.messages.View())
			if tc.want == "" {
				require.NotContains(t, view, "after")
				require.NotContains(t, view, "·")
				return
			}
			require.Contains(t, view, tc.want)
		})
	}
}

func TestReloadRestoresTimingAndKeepsLiveTimingsAcrossReload(t *testing.T) {
	p, sess := newTimingPage(t, "timing-reload")
	request := session.UserMessage("persisted request")
	request.TurnID = "persisted-turn"
	sess.AddMessage(request)
	sess.AddMessage(session.NewAgentMessage("root", &chatmsg.Message{Role: chatmsg.MessageRoleAssistant, ReasoningContent: "persisted reasoning"}))
	sess.SetTurnTiming("persisted-turn", *timingFor("persisted-turn", 65*time.Second, session.TurnTimingOutcomeCompleted))

	p.resetProjection(runtime.SessionSnapshot{Session: sess, Status: runtime.SessionStatus{SessionID: sess.ID, State: runtime.SessionStateSettled}, TranscriptPosition: sess.ItemCount()})
	require.Contains(t, ansi.Strip(p.messages.View()), "· 1m5s", "persisted timing renders on reload")

	// Learned live before a reload of the same transcript must survive it.
	turnEvent(p, sess.ID, "live-turn", &runtime.UserMessageEvent{SessionID: sess.ID, Message: "live request", SessionPosition: -1, TurnID: "live-turn"})
	turnEvent(p, sess.ID, "live-turn", runtime.StreamStarted(sess.ID, "root"))
	turnEvent(p, sess.ID, "live-turn", runtime.AgentChoiceReasoning("root", sess.ID, "Live work."))
	turnEvent(p, sess.ID, "live-turn", runtime.StreamStopped(sess.ID, "root", "normal"))
	turnEvent(p, sess.ID, "live-turn", &runtime.TurnSettledEvent{SessionID: sess.ID, TurnID: "live-turn", Outcome: runtime.TurnCompleted, Timing: timingFor("live-turn", 7*time.Second, session.TurnTimingOutcomeCompleted)})
	require.Contains(t, ansi.Strip(p.messages.View()), "· 7s")

	p.resetProjection(runtime.SessionSnapshot{Session: sess, Status: runtime.SessionStatus{SessionID: sess.ID, State: runtime.SessionStateSettled}, TranscriptPosition: sess.ItemCount()})
	require.Contains(t, ansi.Strip(p.messages.View()), "· 1m5s")
}

func TestReattachToRunningSessionStartsLiveTimer(t *testing.T) {
	p, sess := newTimingPage(t, "timing-reattach")
	require.True(t, p.streamStartTime.IsZero())
	before := time.Now()
	p.applyProjection(runtime.SessionSnapshot{Status: runtime.SessionStatus{SessionID: sess.ID, State: runtime.SessionStateRunning, TurnID: "running-turn"}}, false)
	require.True(t, p.IsWorking())
	require.False(t, p.streamStartTime.IsZero(), "a reattached running turn starts the live timer")
	require.False(t, p.streamStartTime.Before(before))

	started := p.streamStartTime
	p.applyProjection(runtime.SessionSnapshot{Status: runtime.SessionStatus{SessionID: sess.ID, State: runtime.SessionStateRunning, TurnID: "running-turn"}}, false)
	require.Equal(t, started, p.streamStartTime, "a later projection keeps the existing start")

	p.applyProjection(runtime.SessionSnapshot{Status: runtime.SessionStatus{SessionID: sess.ID, State: runtime.SessionStateSettled}}, false)
	require.True(t, p.streamStartTime.IsZero())
}

func TestReattachWhileBlockedKeepsWaitingState(t *testing.T) {
	sess := session.New(session.WithID("timing-reattach-blocked"))
	request := session.UserMessage("blocked request")
	request.TurnID = "blocked-turn"
	sess.AddMessage(request)
	sess.AddMessage(session.NewAgentMessage("root", &chatmsg.Message{Role: chatmsg.MessageRoleAssistant, ReasoningContent: "blocked work"}))
	a, _ := newSessionTestApp(t, sess, nil, nil)
	ar := animation.NewRuntime()
	p := New(ar, t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	t.Cleanup(func() { Cleanup(p); ar.Stop() })
	p.SetSize(100, 30)
	call := tools.ToolCall{ID: "blocked-call", Function: tools.FunctionCall{Name: "shell"}}
	p.resetProjection(runtime.SessionSnapshot{
		Session:            sess,
		TranscriptPosition: sess.ItemCount(),
		Status:             runtime.SessionStatus{SessionID: sess.ID, State: runtime.SessionStateRunning, TurnID: "blocked-turn"},
		Interactions: []runtime.InteractionSnapshot{{
			SessionID:     sess.ID,
			InteractionID: call.ID,
			Event:         &runtime.ToolCallConfirmationEvent{SessionID: sess.ID, ToolCall: call, ToolDefinition: tools.Tool{Name: "shell"}},
		}},
	})
	require.False(t, p.IsWorking(), "the input spinner stays off while blocked")
	require.Contains(t, ansi.Strip(p.messages.View()), "Waiting for approval")
}

func TestSettledOutcomeWithoutDurationStaysExplicit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timing  *session.TurnTiming
		outcome runtime.TurnOutcome
		want    string
		absent  string
	}{
		{name: "canceled without timing", outcome: runtime.TurnCanceled, want: "· Stopped", absent: "after"},
		{name: "failed sub-second", timing: timingFor("turn-y", 200*time.Millisecond, session.TurnTimingOutcomeFailed), outcome: runtime.TurnFailed, want: "· Failed", absent: "after"},
		{name: "completed without timing", outcome: runtime.TurnCompleted, absent: "· "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, sess := newTimingPage(t, "outcome-"+strings.ReplaceAll(tc.name, " ", "-"))
			turnEvent(p, sess.ID, "turn-y", &runtime.UserMessageEvent{SessionID: sess.ID, Message: "work", SessionPosition: -1, TurnID: "turn-y"})
			turnEvent(p, sess.ID, "turn-y", runtime.StreamStarted(sess.ID, "root"))
			turnEvent(p, sess.ID, "turn-y", runtime.AgentChoiceReasoning("root", sess.ID, "Thinking."))
			turnEvent(p, sess.ID, "turn-y", runtime.StreamStopped(sess.ID, "root", "normal"))
			turnEvent(p, sess.ID, "turn-y", &runtime.TurnSettledEvent{SessionID: sess.ID, TurnID: "turn-y", Outcome: tc.outcome, Timing: tc.timing})
			view := ansi.Strip(p.messages.View())
			if tc.want != "" {
				require.Contains(t, view, tc.want)
			}
			if tc.absent == "after" {
				require.NotContains(t, view, "after")
			}
			if tc.absent == "· " && tc.want == "" {
				require.NotContains(t, view, "· ")
			}
		})
	}
}

// The runtime correlates interactions by the request's RequestID, which is also the
// snapshot's InteractionID and the resolved event's InteractionID. A snapshot key that
// differs from the payload must be authoritative because resolution uses it.
func TestInteractionCorrelationUsesSnapshotKeyNotPayloadCopy(t *testing.T) {
	sess := session.New(session.WithID("correlation-snapshot"))
	request := session.UserMessage("needs approval")
	request.TurnID = "correlated-turn"
	sess.AddMessage(request)
	sess.AddMessage(session.NewAgentMessage("root", &chatmsg.Message{Role: chatmsg.MessageRoleAssistant, ReasoningContent: "waiting work"}))
	a, _ := newSessionTestApp(t, sess, nil, nil)
	ar := animation.NewRuntime()
	p := New(ar, t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	t.Cleanup(func() { Cleanup(p); ar.Stop() })
	p.SetSize(100, 30)
	call := tools.ToolCall{ID: "payload-id", Function: tools.FunctionCall{Name: "shell"}}
	p.resetProjection(runtime.SessionSnapshot{
		Session:            sess,
		TranscriptPosition: sess.ItemCount(),
		Status:             runtime.SessionStatus{SessionID: sess.ID, State: runtime.SessionStateRunning, TurnID: "correlated-turn"},
		Interactions: []runtime.InteractionSnapshot{{
			SessionID:     sess.ID,
			InteractionID: "map-key",
			Event:         &runtime.ToolCallConfirmationEvent{SessionID: sess.ID, RequestID: "payload-id", ToolCall: call, ToolDefinition: tools.Tool{Name: "shell"}},
		}},
	})
	require.Equal(t, waitingForApprovalLabel, rootWorkBlock(t, p).Status().Waiting)

	turnEvent(p, sess.ID, "correlated-turn", &runtime.InteractionResolvedEvent{SessionID: sess.ID, InteractionID: "payload-id", Reason: runtime.InteractionResponded})
	require.Equal(t, waitingForApprovalLabel, rootWorkBlock(t, p).Status().Waiting, "a payload copy never resolves the snapshot's interaction")

	turnEvent(p, sess.ID, "correlated-turn", &runtime.InteractionResolvedEvent{SessionID: sess.ID, InteractionID: "map-key", Reason: runtime.InteractionResponded})
	require.Empty(t, rootWorkBlock(t, p).Status().Waiting)
}

// Elicitation and max-iterations requests carry their correlation ID as RequestID,
// the same value the runtime resolves them by.
func TestElicitationAndMaxIterationsWaitsResolveByRequestID(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event runtime.Event
		id    string
		label string
	}{
		{name: "elicitation", event: &runtime.ElicitationRequestEvent{SessionID: "x", RequestID: "elicit-1", ElicitationID: "elicit-1", Message: "which?"}, id: "elicit-1", label: waitingForAnswerLabel},
		{name: "max iterations", event: &runtime.MaxIterationsReachedEvent{SessionID: "x", RequestID: "iter-1", MaxIterations: 5}, id: "iter-1", label: waitingForAnswerLabel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, sess := newTimingPage(t, "correlation-"+tc.id)
			switch event := tc.event.(type) {
			case *runtime.ElicitationRequestEvent:
				event.SessionID = sess.ID
			case *runtime.MaxIterationsReachedEvent:
				event.SessionID = sess.ID
			}
			turnEvent(p, sess.ID, "wait-turn", &runtime.UserMessageEvent{SessionID: sess.ID, Message: "ask", SessionPosition: -1, TurnID: "wait-turn"})
			turnEvent(p, sess.ID, "wait-turn", runtime.StreamStarted(sess.ID, "root"))
			turnEvent(p, sess.ID, "wait-turn", runtime.AgentChoiceReasoning("root", sess.ID, "Asking."))
			turnEvent(p, sess.ID, "wait-turn", tc.event)
			require.Equal(t, tc.label, rootWorkBlock(t, p).Status().Waiting)
			turnEvent(p, sess.ID, "wait-turn", &runtime.InteractionResolvedEvent{SessionID: sess.ID, InteractionID: tc.id, Reason: runtime.InteractionResponded})
			require.Empty(t, rootWorkBlock(t, p).Status().Waiting)
			require.True(t, p.IsWorking(), "the spinner re-arms once the wait clears")
		})
	}
}
