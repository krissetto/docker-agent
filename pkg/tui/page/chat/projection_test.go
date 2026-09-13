package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestAuthoritativeResetReplacesChatTranscriptQueueAndStatus(t *testing.T) {
	sess := session.New(session.WithID("s"))
	a, _ := newSessionTestApp(t, sess, nil, nil)
	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	p.messages.AddUserMessage("stale")
	p.messages.AddUserMessage("also stale")
	p.messageQueue = []queuedMessage{{turnID: "stale", content: "old"}}
	p.working = true
	p.lifecycle = lifecycle.State{Streams: []lifecycle.Stream{{SessionID: "s"}}}
	recovered := session.New(session.WithID("s"))
	recovered.AddMessage(session.UserMessage("recovered"))
	snapshot := runtime.SessionSnapshot{Session: recovered, Status: runtime.SessionStatus{SessionID: "s", State: runtime.SessionStateSettled}, TranscriptPosition: 1, PendingInputs: []runtime.PendingInput{{TurnID: "new", Content: "queued"}}}
	p.Update(msgtypes.SessionRuntimeEventMsg{Event: &app.SessionResetEvent{Snapshot: snapshot}, Projection: &app.PresentationState{Lifecycle: lifecycle.FromSnapshot(snapshot), Status: snapshot.Status}})
	assert.Equal(t, 1, p.messages.MessageTypeCount(types.MessageTypeUser))
	assert.Equal(t, []queuedMessage{{turnID: "new", content: "queued"}}, p.messageQueue)
	assert.False(t, p.working)
	assert.Zero(t, p.lifecycle.Depth())
	assert.Equal(t, 1, p.snapshotEnd)
}

func TestTypedQueueAndPromotionReplay(t *testing.T) {
	sess := session.New(session.WithID("s"))
	a, _ := newSessionTestApp(t, sess, nil, nil)
	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	p.messages.SetSize(100, 200)
	origins := []session.InputOrigin{session.InputOriginRuntime, session.InputOriginAgent, session.InputOriginUser, "", "future"}
	var pending []runtime.PendingInput
	for i, origin := range origins {
		id := string(rune('a' + i))
		body := "same user body"
		if origin == session.InputOriginRuntime {
			body = "<system_info>runtime secret</system_info>"
		}
		if origin == session.InputOriginAgent {
			body = "clean agent body <system_info>agent literal</system_info>"
		}
		pending = append(pending, runtime.PendingInput{TurnID: id, Content: body, InputOrigin: origin, InputMode: "steer", SenderID: "child", SenderName: "worker", SessionPosition: i})
		msg := session.UserMessage(body)
		msg.TurnID, msg.Pending, msg.InputOrigin, msg.InputMode, msg.SenderID, msg.SenderName = id, true, origin, "steer", "child", "worker"
		sess.AddMessage(msg)
		p.handleRuntimeEvent(&runtime.PendingUserMessageAcceptedEvent{TurnID: id, Message: body, InputOrigin: origin, SessionPosition: i})
	}
	assert.Len(t, p.messageQueue, 3)
	assert.Len(t, p.lifecycle.Pending, 5)
	snapshot := runtime.SessionSnapshot{Session: sess, Status: runtime.SessionStatus{SessionID: "s"}, PendingInputs: pending}
	p.resetProjection(snapshot)
	assert.Len(t, p.messageQueue, 3)
	assert.Len(t, p.lifecycle.Pending, 5)
	for _, input := range pending {
		e := &runtime.PendingUserMessagePromotedEvent{TurnID: input.TurnID, Message: input.Content, InputOrigin: input.InputOrigin, InputMode: input.InputMode, SenderID: input.SenderID, SenderName: input.SenderName, SessionPosition: input.SessionPosition}
		p.handleRuntimeEvent(e)
		p.handleRuntimeEvent(e)
		p.handleRuntimeEvent(&runtime.UserMessageEvent{TurnID: e.TurnID, Message: e.Message, InputOrigin: e.InputOrigin, InputMode: e.InputMode, SenderID: e.SenderID, SenderName: e.SenderName, SessionPosition: e.SessionPosition})
		sess.PromotePendingUserMessageByTurnID(input.TurnID)
	}
	assert.Empty(t, p.messageQueue)
	assert.Equal(t, 3, p.messages.MessageTypeCount(types.MessageTypeUser))
	assert.Equal(t, 1, p.messages.MessageTypeCount(types.MessageTypeAgentInput))
	out := ansi.Strip(p.messages.View())
	assert.NotContains(t, out, "runtime secret")
	assert.Contains(t, out, "clean agent body <system_info>agent literal</system_info>")
	assert.Contains(t, out, "worker")
	assert.Contains(t, out, "child")
	assert.Equal(t, 3, strings.Count(out, "same user body"))
	snapshot.PendingInputs = nil
	snapshot.TranscriptPosition = sess.ItemCount()
	p.resetProjection(snapshot)
	p.handleRuntimeEvent(&runtime.PendingUserMessagePromotedEvent{TurnID: "b", Message: "clean agent body <system_info>agent literal</system_info>", InputOrigin: session.InputOriginAgent, InputMode: "steer", SenderID: "child", SenderName: "worker", SessionPosition: 1})
	assert.Equal(t, 1, p.messages.MessageTypeCount(types.MessageTypeAgentInput))
	assert.NotContains(t, ansi.Strip(p.messages.View()), "runtime secret")
}

func TestTypedInputNoticesAndModeSurvivePromotionAndReset(t *testing.T) {
	sess := session.New(session.WithID("s"))
	a, _ := newSessionTestApp(t, sess, nil, nil)
	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	p.messages.SetSize(100, 200)
	for i, origin := range []session.InputOrigin{session.InputOriginRuntime, session.InputOriginAgent, session.InputOriginAgent} {
		id := string(rune('a' + i))
		mode := "steer"
		body := "clean steering body"
		switch i {
		case 0:
			body = "<system_info>private runtime payload</system_info>"
		case 2:
			mode, body = "turn", "original parent delegation"
		}
		p.handleRuntimeEvent(&runtime.PendingUserMessageAcceptedEvent{TurnID: id, Message: body, InputOrigin: origin, InputMode: mode, SessionPosition: i})
		assert.Empty(t, p.messageQueue)
		promoted := &runtime.PendingUserMessagePromotedEvent{TurnID: id, Message: body, InputOrigin: origin, InputMode: mode, SenderName: "worker", SenderID: "12345678-long-id", SessionPosition: i}
		p.handleRuntimeEvent(promoted)
		p.handleRuntimeEvent(&runtime.UserMessageEvent{TurnID: id, Message: body, InputOrigin: origin, InputMode: mode, SenderName: promoted.SenderName, SenderID: promoted.SenderID, SessionPosition: i})
		input := session.UserMessage(body)
		input.InputOrigin, input.InputMode, input.SenderName, input.SenderID, input.TurnID = origin, mode, promoted.SenderName, promoted.SenderID, id
		sess.AddMessage(input)
	}
	check := func() {
		out := ansi.Strip(p.messages.View())
		assert.Equal(t, 1, strings.Count(out, "worker (ref 12345) has replied"))
		assert.Contains(t, out, "worker (ref 12345)")
		assert.Equal(t, 1, strings.Count(out, "original parent delegation"))
		assert.NotContains(t, out, "private runtime payload")
		assert.NotContains(t, out, "12345678-long-id")
		assert.Zero(t, p.messages.MessageTypeCount(types.MessageTypeUser))
		assert.Zero(t, p.messages.MessageTypeCount(types.MessageTypeAssistant))
		assert.Empty(t, p.messageQueue)
	}
	check()
	p.resetProjection(runtime.SessionSnapshot{Session: sess, Status: runtime.SessionStatus{SessionID: "s"}, TranscriptPosition: sess.ItemCount()})
	check()
}

func TestDirectTypedInputMode(t *testing.T) {
	for _, mode := range []string{"turn", "steer"} {
		t.Run(mode, func(t *testing.T) {
			sess := session.New(session.WithID("s"))
			a, _ := newSessionTestApp(t, sess, nil, nil)
			p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
			p.messages.SetSize(100, 40)
			p.handleRuntimeEvent(&runtime.UserMessageEvent{TurnID: "direct", Message: "clean input", InputOrigin: session.InputOriginAgent, InputMode: mode, SenderName: "worker", SenderID: "12345678-long-id"})
			assert.Equal(t, 1, p.messages.MessageTypeCount(types.MessageTypeAgentInput))
			assert.Contains(t, ansi.Strip(p.messages.View()), "worker (ref 12345)")
			assert.Empty(t, p.messageQueue)
		})
	}
}

func TestPendingEditChangesPayloadInPlaceWithoutAdmissionOrPromotion(t *testing.T) {
	p := newTestChatPage(t)
	p.messageQueue = []queuedMessage{{turnID: "first", content: "unchanged"}, {turnID: "second", content: "old full payload"}, {turnID: "third", content: "unchanged tail"}}
	before := p.messages.MessageTypeCount(types.MessageTypeUser)
	handled, _ := p.handleRuntimeEvent(runtime.PendingUserMessageEdited(p.app.Session().ID, "second", "new\nfull payload", nil, 2))
	assert.True(t, handled)
	assert.Equal(t, []queuedMessage{{turnID: "first", content: "unchanged"}, {turnID: "second", content: "new\nfull payload"}, {turnID: "third", content: "unchanged tail"}}, p.messageQueue)
	assert.Equal(t, before, p.messages.MessageTypeCount(types.MessageTypeUser), "editing accepted input does not append a transcript turn")
	p.handleRuntimeEvent(runtime.PendingUserMessageEdited(p.app.Session().ID, "already-consumed", "late payload", nil, 0))
	assert.Len(t, p.messageQueue, 3, "late edit event cannot recreate consumed input")
}
