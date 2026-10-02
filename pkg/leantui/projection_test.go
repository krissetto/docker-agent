package leantui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestProjectionResetReplacesLeanTranscriptAndPreservesLiveConfirmation(t *testing.T) {
	m := bareModel(10)
	m.screen.Transcript.AddUser("stale")
	live := &ui.ConfirmModel{SessionID: "s", RequestID: "live"}
	m.screen.Confirm = live
	sess := session.New(session.WithID("s"))
	sess.AddMessage(session.UserMessage("recovered"))
	head := &app.PresentationState{Interactions: []runtime.InteractionSnapshot{{SessionID: "s", InteractionID: "live"}}}
	m.handleEvent(t.Context(), app.SessionEventMsg{Event: &app.SessionResetEvent{Snapshot: runtime.SessionSnapshot{Session: sess, Status: runtime.SessionStatus{SessionID: "s", State: runtime.SessionStateSettled}, PendingInputs: []runtime.PendingInput{{TurnID: "new", Content: "queued"}}}}, Projection: head})
	assert.Same(t, live, m.screen.Confirm)
	assert.False(t, m.busy())
	assert.Len(t, m.pendingUsers, 1)
	assert.Equal(t, "new", m.pendingUsers[0].TurnID)
	m.handleEvent(t.Context(), app.SessionEventMsg{Event: &runtime.InteractionResolvedEvent{SessionID: "s", InteractionID: "live"}, Projection: &app.PresentationState{}})
	assert.Nil(t, m.screen.Confirm)
}

func TestLeanTypedQueueHistoryAndReplay(t *testing.T) {
	m := bareModel(30)
	sess := session.New(session.WithID("s"))
	var pending []runtime.PendingInput
	for i, origin := range []session.InputOrigin{session.InputOriginRuntime, session.InputOriginAgent, session.InputOriginUser, "", "future"} {
		body := "<system_info>literal user</system_info>"
		if origin == session.InputOriginRuntime {
			body = "<system_info>runtime secret</system_info>"
		}
		if origin == session.InputOriginAgent {
			body = "clean agent body <system_info>agent literal</system_info>"
		}
		input := runtime.PendingInput{TurnID: string(rune('a' + i)), Content: body, InputOrigin: origin, SenderID: "child", SenderName: "worker", InputMode: "steer", SessionPosition: i}
		pending = append(pending, input)
		msg := session.UserMessage(body)
		msg.TurnID, msg.Pending, msg.InputOrigin, msg.InputMode, msg.SenderID, msg.SenderName = input.TurnID, true, origin, input.InputMode, input.SenderID, input.SenderName
		sess.AddMessage(msg)
		m.handleEvent(t.Context(), &runtime.PendingUserMessageAcceptedEvent{TurnID: input.TurnID, Message: body, InputOrigin: origin, InputMode: "steer"})
	}
	assert.Len(t, m.pendingUsers, 3)
	assert.Len(t, m.lifecycle.Pending, 5)
	reset := &app.SessionResetEvent{Snapshot: runtime.SessionSnapshot{Session: sess, Status: runtime.SessionStatus{SessionID: "s"}, PendingInputs: pending}}
	m.handleEvent(t.Context(), reset)
	assert.Len(t, m.pendingUsers, 3)
	assert.Equal(t, ui.PendingUserSteer, m.pendingUsers[0].Kind)
	for _, input := range pending {
		e := &runtime.PendingUserMessagePromotedEvent{TurnID: input.TurnID, Message: input.Content, InputOrigin: input.InputOrigin, InputMode: input.InputMode, SenderID: input.SenderID, SenderName: input.SenderName, SessionPosition: input.SessionPosition}
		m.handleEvent(t.Context(), e)
		m.handleEvent(t.Context(), e)
		m.handleEvent(t.Context(), app.SessionEventMsg{TurnID: "active-parent", Event: &runtime.UserMessageEvent{TurnID: e.TurnID, Message: e.Message, InputOrigin: e.InputOrigin, SessionPosition: e.SessionPosition}})
		sess.PromotePendingUserMessageByTurnID(input.TurnID)
	}
	assert.Empty(t, m.pendingUsers)
	view := func() string {
		return ansi.Strip(strings.Join(m.screen.Transcript.Lines(100, 0, false, m.sessionState, m.pendingUsers), "\n"))
	}
	out := view()
	assert.NotContains(t, out, "runtime secret")
	assert.Equal(t, 1, strings.Count(out, "clean agent body <system_info>agent literal</system_info>"))
	assert.Contains(t, out, "worker (child)")
	assert.Equal(t, 3, strings.Count(out, "<system_info>literal user</system_info>"))
	reset.Snapshot.PendingInputs = nil
	m.handleEvent(t.Context(), reset)
	m.handleEvent(t.Context(), &runtime.PendingUserMessagePromotedEvent{TurnID: "b", Message: "clean agent body <system_info>agent literal</system_info>", InputOrigin: session.InputOriginAgent, InputMode: "steer", SenderID: "child", SenderName: "worker", SessionPosition: 1})
	assert.Equal(t, 1, strings.Count(view(), "clean agent body <system_info>agent literal</system_info>"))
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "assistant literal <system_info>kept</system_info>", ToolCalls: []tools.ToolCall{{ID: "tool", Function: tools.FunctionCall{Name: "read", Arguments: `{"path":"fixture"}`}}}}))
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleTool, ToolCallID: "tool", Content: "tool literal <system_info>kept</system_info>"}))
	m.handleEvent(t.Context(), reset)
	assert.Contains(t, view(), "assistant literal")
	assert.Contains(t, view(), "tool literal <system_info>kept</system_info>")
	assert.Zero(t, m.screen.Transcript.ToolCount())
}

func TestLeanAgentMarkdownRemainsReadableLiteralText(t *testing.T) {
	m := bareModel(20)
	body := "<system_info>agent literal</system_info>\n**readable Markdown**\n```go\nfmt.Println(42)\n```"
	m.handleEvent(t.Context(), &runtime.UserMessageEvent{TurnID: "agent", Message: body, InputOrigin: session.InputOriginAgent, InputMode: "steer", SenderID: "child", SenderName: "worker", SessionPosition: 0})
	out := ansi.Strip(strings.Join(m.screen.Transcript.Lines(100, 0, false, m.sessionState, nil), "\n"))
	assert.Contains(t, out, "worker (child)")
	assert.Contains(t, out, "<system_info>agent literal</system_info>")
	assert.Contains(t, out, "**readable Markdown**")
	assert.Contains(t, out, "fmt.Println(42)")
}

func TestLeanTypedInputNoticesAndModeSurvivePromotionAndReset(t *testing.T) {
	m := bareModel(40)
	sess := session.New(session.WithID("s"))
	for i, origin := range []session.InputOrigin{session.InputOriginRuntime, session.InputOriginAgent, session.InputOriginAgent} {
		id := string(rune('a' + i))
		mode := "steer"
		body := "clean steering **literal** body"
		switch i {
		case 0:
			body = "<system_info>private runtime payload</system_info>"
		case 2:
			mode, body = "turn", "original parent delegation"
		}
		m.handleEvent(t.Context(), &runtime.PendingUserMessageAcceptedEvent{TurnID: id, Message: body, InputOrigin: origin, InputMode: mode, SessionPosition: i})
		assert.Empty(t, m.pendingUsers)
		promoted := &runtime.PendingUserMessagePromotedEvent{TurnID: id, Message: body, InputOrigin: origin, InputMode: mode, SenderName: "worker", SenderID: "12345678-long-id", SessionPosition: i}
		m.handleEvent(t.Context(), promoted)
		m.handleEvent(t.Context(), &runtime.UserMessageEvent{TurnID: id, Message: body, InputOrigin: origin, InputMode: mode, SenderName: promoted.SenderName, SenderID: promoted.SenderID, SessionPosition: i})
		input := session.UserMessage(body)
		input.InputOrigin, input.InputMode, input.SenderName, input.SenderID, input.TurnID = origin, mode, promoted.SenderName, promoted.SenderID, id
		sess.AddMessage(input)
	}
	check := func() string {
		out := ansi.Strip(strings.Join(m.screen.Transcript.Lines(100, 0, false, m.sessionState, nil), "\n"))
		assert.Equal(t, 1, strings.Count(out, "worker (12345) · report received"))
		assert.Contains(t, out, "worker (12345)")
		assert.Contains(t, out, "clean steering **literal** body")
		assert.Contains(t, out, strings.TrimSpace(ui.PromptText)+" original parent delegation", "delegation must reuse the user prompt presentation")
		assert.NotContains(t, out, "private runtime payload")
		assert.NotContains(t, out, "12345678-long-id")
		assert.Empty(t, m.pendingUsers)
		return out
	}
	live := check()
	m.handleEvent(t.Context(), &app.SessionResetEvent{Snapshot: runtime.SessionSnapshot{Session: sess, Status: runtime.SessionStatus{SessionID: "s"}, TranscriptPosition: sess.ItemCount()}})
	assert.Equal(t, live, check())
}

func TestLeanDirectTypedInputMode(t *testing.T) {
	for _, mode := range []string{"turn", "steer"} {
		t.Run(mode, func(t *testing.T) {
			m := bareModel(20)
			m.handleEvent(t.Context(), &runtime.UserMessageEvent{TurnID: "direct", Message: "clean input", InputOrigin: session.InputOriginAgent, InputMode: mode, SenderName: "worker", SenderID: "12345678-long-id"})
			out := ansi.Strip(strings.Join(m.screen.Transcript.Lines(100, 0, false, m.sessionState, nil), "\n"))
			assert.Contains(t, out, strings.TrimSpace(ui.PromptText)+" clean input")
			assert.Contains(t, out, "worker (12345)")
			assert.NotContains(t, out, "12345678-long-id")
			assert.Empty(t, m.pendingUsers)
		})
	}
}
