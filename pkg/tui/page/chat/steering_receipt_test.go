package chat

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestSteeringAcceptedReceiptPromotesOneCard(t *testing.T) {
	sess := session.New(session.WithID("s"))
	a, _ := newSessionTestApp(t, sess, nil, nil)
	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	p.messages.SetSize(100, 200)
	accepted := &runtime.PendingUserMessageAcceptedEvent{SessionID: "s", TurnID: "incoming", Message: "distinct body", InputOrigin: session.InputOriginAgent, InputMode: "steer", SenderID: "child", SenderName: "worker", SessionPosition: 0}
	p.handleRuntimeEvent(accepted)
	require.Empty(t, p.messageQueue)
	require.Equal(t, 1, p.messages.MessageTypeCount(types.MessageTypeAgentInput))
	require.Contains(t, ansi.Strip(p.messages.View()), "accepted · awaiting consumption")
	require.NotContains(t, ansi.Strip(p.messages.View()), "sent a message")
	p.handleRuntimeEvent(&runtime.PendingUserMessagePromotedEvent{SessionID: "s", TurnID: "incoming", Message: accepted.Message, InputOrigin: accepted.InputOrigin, InputMode: accepted.InputMode, SenderID: accepted.SenderID, SenderName: accepted.SenderName, SessionPosition: 0})
	require.Equal(t, 1, p.messages.MessageTypeCount(types.MessageTypeAgentInput))
	require.Contains(t, ansi.Strip(p.messages.View()), "worker (child) sent a message >")
}

func TestSteeringReplayLateAcceptanceAndConcurrentWithdrawal(t *testing.T) {
	sess := session.New(session.WithID("s"))
	a, _ := newSessionTestApp(t, sess, nil, nil)
	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	p.messages.SetSize(100, 200)
	accepted := func(id string, pos int) *runtime.PendingUserMessageAcceptedEvent {
		return &runtime.PendingUserMessageAcceptedEvent{SessionID: "s", TurnID: id, Message: "same body", InputOrigin: session.InputOriginAgent, InputMode: "steer", SenderID: "child", SenderName: "worker", SessionPosition: pos}
	}
	for _, e := range []*runtime.PendingUserMessageAcceptedEvent{accepted("first", 0), accepted("second", 1), accepted("first", 0)} {
		p.handleRuntimeEvent(e)
	}
	require.Equal(t, 2, p.messages.MessageTypeCount(types.MessageTypeAgentInput))
	cancel := runtime.PendingUserMessageCanceled("s", "first", 0)
	p.handleRuntimeEvent(cancel)
	p.handleRuntimeEvent(cancel)
	p.handleRuntimeEvent(accepted("first", 0))
	require.Equal(t, 1, p.messages.MessageTypeCount(types.MessageTypeAgentInput))
	require.Equal(t, []string{"second"}, p.lifecycle.Pending)
	promoted := &runtime.PendingUserMessagePromotedEvent{SessionID: "s", TurnID: "second", Message: "same body", InputOrigin: session.InputOriginAgent, InputMode: "steer", SenderID: "child", SenderName: "worker", SessionPosition: 0}
	p.handleRuntimeEvent(promoted)
	p.handleRuntimeEvent(promoted)
	p.handleRuntimeEvent(accepted("second", 0))
	p.handleRuntimeEvent(&runtime.UserMessageEvent{SessionID: "s", TurnID: "second", Message: "same body", InputOrigin: session.InputOriginAgent, SessionPosition: 0})
	require.Equal(t, 1, p.messages.MessageTypeCount(types.MessageTypeAgentInput))
	require.Empty(t, p.lifecycle.Pending)
	require.NotContains(t, ansi.Strip(p.messages.View()), "awaiting consumption")
}

func TestSteeringSnapshotPendingReceiptSurvivesResetAndPromotion(t *testing.T) {
	for _, withTranscript := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending metadata", true: "pending transcript"}[withTranscript], func(t *testing.T) {
			sess := session.New(session.WithID("s"))
			a, _ := newSessionTestApp(t, sess, nil, nil)
			p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
			p.messages.SetSize(100, 200)
			if withTranscript {
				input := session.UserMessage("restored payload")
				input.TurnID, input.Pending, input.InputOrigin, input.InputMode, input.SenderID, input.SenderName = "pending", true, session.InputOriginAgent, "steer", "child", "worker"
				sess.AddMessage(input)
			}
			snapshot := runtime.SessionSnapshot{Session: sess, Status: runtime.SessionStatus{SessionID: "s"}, PendingInputs: []runtime.PendingInput{{TurnID: "pending", Content: "restored payload", InputOrigin: session.InputOriginAgent, InputMode: "steer", SenderID: "child", SenderName: "worker", SessionPosition: 0}}}
			p.resetProjection(snapshot)
			require.Equal(t, 1, p.messages.MessageTypeCount(types.MessageTypeAgentInput))
			require.Contains(t, ansi.Strip(p.messages.View()), "accepted · awaiting consumption")
			p.handleRuntimeEvent(&runtime.PendingUserMessagePromotedEvent{SessionID: "s", TurnID: "pending", Message: "restored payload", InputOrigin: session.InputOriginAgent, InputMode: "steer", SenderID: "child", SenderName: "worker", SessionPosition: 0})
			require.Equal(t, 1, p.messages.MessageTypeCount(types.MessageTypeAgentInput))
			require.NotContains(t, ansi.Strip(p.messages.View()), "awaiting consumption")
		})
	}
}

func TestSteeringAsyncRestoreReconcilesBufferedPromotion(t *testing.T) {
	sess := session.New(session.WithID("s"))
	input := session.UserMessage("async restored steering")
	input.TurnID, input.Pending, input.InputOrigin, input.InputMode, input.SenderID, input.SenderName = "pending", true, session.InputOriginAgent, "steer", "child", "worker"
	sess.AddMessage(input)
	a, _ := newSessionTestApp(t, sess, nil, nil)
	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	p.messages.SetSize(100, 200)
	EnableAsyncReplay(p)
	snapshot := runtime.SessionSnapshot{Session: sess, Status: runtime.SessionStatus{SessionID: "s"}, PendingInputs: []runtime.PendingInput{{TurnID: "pending", Content: input.Message.Content, InputOrigin: input.InputOrigin, InputMode: input.InputMode, SenderID: input.SenderID, SenderName: input.SenderName, SessionPosition: 0}}}
	cmd := p.resetProjection(snapshot)
	handled, _ := p.updateReplay(cmd())
	require.True(t, handled)
	promoted := &runtime.PendingUserMessagePromotedEvent{SessionID: "s", TurnID: "pending", Message: input.Message.Content, InputOrigin: input.InputOrigin, InputMode: input.InputMode, SenderID: input.SenderID, SenderName: input.SenderName, SessionPosition: 0}
	handled, _ = p.updateReplay(promoted)
	require.True(t, handled, "live transition is buffered behind replay barrier")
	for steps := 0; p.replay != nil && steps < 10; steps++ {
		p.updateReplay(replayContinueMsg{generation: p.replayGeneration})
	}
	require.Nil(t, p.replay)
	require.Equal(t, 1, p.messages.MessageTypeCount(types.MessageTypeAgentInput))
	require.NotContains(t, ansi.Strip(p.messages.View()), "awaiting consumption")
	require.Empty(t, p.lifecycle.Pending)
}

func TestSteeringLateAcceptanceDoesNotRecreateUserQueue(t *testing.T) {
	p := newTestChatPage(t)
	accepted := runtime.PendingUserMessageAccepted(p.app.Session().ID, "user-steering", "body", nil, 0)
	p.handleRuntimeEvent(accepted)
	p.handleRuntimeEvent(runtime.PendingUserMessagePromoted(p.app.Session().ID, "user-steering", "body", nil, 0))
	p.handleRuntimeEvent(accepted)
	require.Empty(t, p.messageQueue, "late acceptance cannot requeue consumed steering")
	require.Empty(t, p.lifecycle.Pending)
	p.handleRuntimeEvent(runtime.PendingUserMessageAccepted(p.app.Session().ID, "withdrawn", "body", nil, 1))
	p.handleRuntimeEvent(runtime.PendingUserMessageCanceled(p.app.Session().ID, "withdrawn", 1))
	p.handleRuntimeEvent(runtime.PendingUserMessageAccepted(p.app.Session().ID, "withdrawn", "body", nil, 1))
	require.Empty(t, p.messageQueue, "late acceptance cannot revive withdrawn steering")
	require.Empty(t, p.lifecycle.Pending)
}
