package chat

import (
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestAcceptedInputImmediateDistinguishesAdmissionFromWaiting(t *testing.T) {
	for _, tc := range []struct {
		name      string
		immediate string
		wantQueue bool
	}{
		{name: "immediate", immediate: `,"immediate":true`},
		{name: "explicit pending", immediate: `,"immediate":false`, wantQueue: true},
		{name: "legacy pending", wantQueue: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestChatPage(t)
			p.SetSize(160, 100)
			sidebarGeneration := p.sidebar.VisualGeneration()
			const turnID, body = "accepted-turn", "send this immediately"
			wire := `{"type":"pending_user_message_accepted","session_id":"` + p.app.Session().ID + `","turn_id":"` + turnID + `","message":"` + body + `","input_origin":"user","session_position":0` + tc.immediate + `}`
			var accepted runtime.PendingUserMessageAcceptedEvent
			require.NoError(t, json.Unmarshal([]byte(wire), &accepted), "decode remote event payload")
			require.Equal(t, strings.Contains(tc.immediate, "true"), accepted.Immediate)

			handled, _ := p.handleRuntimeEvent(&accepted)
			require.True(t, handled)
			require.Equal(t, []string{turnID}, p.lifecycle.Pending, "accepted identity still passes through lifecycle projection")
			require.Zero(t, p.messages.MessageTypeCount(types.MessageTypeUser), "acceptance alone does not render a transcript bubble")
			if tc.wantQueue {
				require.Equal(t, []queuedMessage{{turnID: turnID, content: body}}, p.messageQueue)
				require.Greater(t, p.sidebar.VisualGeneration(), sidebarGeneration, "genuinely waiting input updates the sidebar queue projection")
			} else {
				require.Empty(t, p.messageQueue, "immediate admission must not flash in the pending queue")
				require.Equal(t, sidebarGeneration, p.sidebar.VisualGeneration(), "immediate admission must not touch the sidebar queue projection")
			}

			_, _ = p.handleRuntimeEvent(&runtime.PendingUserMessagePromotedEvent{
				SessionID: accepted.SessionID, TurnID: turnID, Message: body,
				InputOrigin: session.InputOriginUser, SessionPosition: 0,
			})
			require.Empty(t, p.messageQueue)
			require.Empty(t, p.lifecycle.Pending)
			require.Equal(t, 1, p.messages.MessageTypeCount(types.MessageTypeUser), "promotion remains the transcript projection")
			require.Contains(t, ansi.Strip(p.messages.View()), body)
		})
	}
}

type recordingQueueSidebar struct {
	sidebar.Model
	updates [][]sidebar.QueuedMessage
}

func (s *recordingQueueSidebar) SetQueuedMessages(queued []sidebar.QueuedMessage) tea.Cmd {
	s.updates = append(s.updates, append([]sidebar.QueuedMessage(nil), queued...))
	return s.Model.SetQueuedMessages(queued)
}

func TestImmediateAcceptanceRemovesSnapshotPendingProjectionBeforePromotion(t *testing.T) {
	p := newTestChatPage(t)
	p.SetSize(160, 100)
	const turnID, body = "startup-turn", "start right away"
	snapshot := runtime.SessionSnapshot{
		Session: p.app.Session(),
		Status:  runtime.SessionStatus{SessionID: p.app.Session().ID},
		PendingInputs: []runtime.PendingInput{{
			TurnID: turnID, Content: body, InputOrigin: session.InputOriginUser, SessionPosition: 0,
		}},
	}
	p.resetProjection(snapshot)
	require.Equal(t, []queuedMessage{{turnID: turnID, content: body}}, p.messageQueue)
	recording := &recordingQueueSidebar{Model: p.sidebar}
	p.sidebar = recording
	_ = p.sidebar.SetQueuedMessages([]sidebar.QueuedMessage{{ID: turnID, Text: body}})
	require.Contains(t, ansi.Strip(p.sidebar.View()), body, "snapshot pending input is visible before the immediate event")
	beforeRemoval := p.sidebar.VisualGeneration()

	accepted := &runtime.PendingUserMessageAcceptedEvent{
		SessionID: p.app.Session().ID, TurnID: turnID, Message: body,
		InputOrigin: session.InputOriginUser, SessionPosition: 0, Immediate: true,
	}
	handled, _ := p.handleRuntimeEvent(accepted)
	require.True(t, handled)
	require.Empty(t, p.messageQueue, "live immediate event supersedes the stale snapshot queue entry")
	require.Greater(t, p.sidebar.VisualGeneration(), beforeRemoval, "removing the snapshot entry syncs the sidebar once")
	require.Empty(t, recording.updates[len(recording.updates)-1], "immediate admission clears the stale sidebar queue entry")
	require.Zero(t, p.messages.MessageTypeCount(types.MessageTypeUser), "acceptance still does not duplicate the transcript")

	promoted := &runtime.PendingUserMessagePromotedEvent{
		SessionID: accepted.SessionID, TurnID: turnID, Message: body,
		InputOrigin: session.InputOriginUser, SessionPosition: 0,
	}
	p.handleRuntimeEvent(promoted)
	p.handleRuntimeEvent(promoted)
	require.Empty(t, p.messageQueue)
	require.Equal(t, 1, p.messages.MessageTypeCount(types.MessageTypeUser), "promotion remains idempotent transcript ownership")
	require.Equal(t, 1, strings.Count(ansi.Strip(p.messages.View()), body))
}

func TestImmediateNonUserAcceptanceRemovesSnapshotReceiptBeforePromotion(t *testing.T) {
	for _, origin := range []session.InputOrigin{session.InputOriginAgent, session.InputOriginRuntime} {
		t.Run(string(origin), func(t *testing.T) {
			p := newTestChatPage(t)
			p.SetSize(160, 100)
			const turnID, body = "startup-agent-turn", "already promoted input"
			input := session.UserMessage(body)
			input.TurnID, input.Pending, input.InputOrigin, input.InputMode, input.SenderName = turnID, true, origin, "turn", "worker"
			p.app.Session().AddMessage(input)
			p.resetProjection(runtime.SessionSnapshot{
				Session: p.app.Session(),
				Status:  runtime.SessionStatus{SessionID: p.app.Session().ID},
				PendingInputs: []runtime.PendingInput{{
					TurnID: turnID, Content: body, InputOrigin: origin, InputMode: "turn", SenderName: "worker", SessionPosition: 0,
				}},
			})
			if origin == session.InputOriginAgent {
				require.Equal(t, 1, p.messages.MessageTypeCount(types.MessageTypeAgentInput), "snapshot pending agent input creates a receipt card")
			} else {
				require.Zero(t, p.messages.MessageTypeCount(types.MessageTypeAgentInput), "runtime-origin inputs are not visible receipt cards")
			}

			accepted := &runtime.PendingUserMessageAcceptedEvent{
				SessionID: p.app.Session().ID, TurnID: turnID, Message: body,
				InputOrigin: origin, InputMode: "turn", SenderName: "worker", SessionPosition: 0, Immediate: true,
			}
			handled, _ := p.handleRuntimeEvent(accepted)
			require.True(t, handled)
			require.Zero(t, p.messages.MessageTypeCount(types.MessageTypeAgentInput), "immediate admission removes any stale pending receipt for every origin")
			require.Zero(t, p.messages.MessageTypeCount(types.MessageTypeUser), "immediate acceptance itself never duplicates transcript")

			promoted := &runtime.PendingUserMessagePromotedEvent{
				SessionID: accepted.SessionID, TurnID: turnID, Message: body,
				InputOrigin: origin, InputMode: "turn", SenderName: "worker", SessionPosition: 0,
			}
			p.handleRuntimeEvent(promoted)
			p.handleRuntimeEvent(promoted)
			if origin == session.InputOriginAgent {
				require.Equal(t, 1, p.messages.MessageTypeCount(types.MessageTypeAgentInput), "promotion restores one canonical transcript receipt")
				require.Contains(t, ansi.Strip(p.messages.View()), "From worker sent a message >", "promoted agent input is restored as a single receipt")
			} else {
				require.Zero(t, p.messages.MessageTypeCount(types.MessageTypeAgentInput), "runtime-origin inputs remain hidden")
			}
		})
	}
}
