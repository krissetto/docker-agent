package chat

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	chatmsg "github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestSteeringAcceptedReceiptDoesNotSplitLiveAssistantCommit(t *testing.T) {
	for _, restored := range []bool{false, true} {
		for _, continueAfterReceipt := range []bool{false, true} {
			t.Run(fmt.Sprintf("restored=%t/continue=%t", restored, continueAfterReceipt), func(t *testing.T) {
				sess := session.New(session.WithID("s"))
				if restored {
					sess.AddMessage(session.NewAgentMessage("root", &chatmsg.Message{Role: chatmsg.MessageRoleAssistant, Content: "historical answer"}))
				}
				a, _ := newSessionTestApp(t, sess, nil, nil)
				ar := animation.NewRuntime()
				p := New(ar, t.Context(), a, service.NewSessionState(sess)).(*chatPage)
				t.Cleanup(func() { Cleanup(p); ar.Stop() })
				p.messages.SetSize(100, 200)
				if restored {
					p.resetProjection(runtime.SessionSnapshot{Session: sess, Status: runtime.SessionStatus{SessionID: "s"}, TranscriptPosition: sess.ItemCount()})
				}
				p.handleRuntimeEvent(runtime.StreamStarted("s", "root"))
				p.handleRuntimeEvent(runtime.AgentChoice("root", "s", "unfinished "))
				p.handleRuntimeEvent(&runtime.PendingUserMessageAcceptedEvent{SessionID: "s", TurnID: "incoming", Message: "steering payload", InputOrigin: session.InputOriginAgent, InputMode: "steer", SenderID: "child", SenderName: "worker", SessionPosition: sess.ItemCount()})
				if continueAfterReceipt {
					p.handleRuntimeEvent(runtime.AgentChoice("root", "s", "answer"))
				}
				p.handleRuntimeEvent(runtime.MessageAddedAt("s", session.NewAgentMessage("root", &chatmsg.Message{Role: chatmsg.MessageRoleAssistant, Content: "unfinished answer"}), "root", sess.ItemCount()+1))
				p.handleRuntimeEvent(runtime.StreamStopped("s", "root", "normal"))
				want := 1
				if restored {
					want++
				}
				require.Equal(t, want, p.messages.MessageTypeCount(types.MessageTypeAssistant), "receipt must not leave partial/duplicate assistant bubbles")
				out := ansi.Strip(p.messages.View())
				require.Equal(t, 1, strings.Count(out, "unfinished answer"), "commit must reconcile same live assistant")
				if restored {
					require.Contains(t, out, "historical answer", "snapshot boundary stays immutable")
				}
				// Base has no receipt; repaired presentation adds it without changing assistant content.
				if p.messages.MessageTypeCount(types.MessageTypeAgentInput) > 0 {
					require.Contains(t, out, "accepted · awaiting consumption")
				}
			})
		}
	}
}
