package chat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/app"
	chatmsg "github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	list "github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestManagedHistoryRecoversLargeTailAndOlderItems(t *testing.T) {
	for _, mode := range []string{"item endpoint", "old server missing cursors", "old server missing route", "item exceeds endpoint cap"} {
		t.Run(mode, func(t *testing.T) {
			const id, epoch = "s", "epoch"
			const end, cursor = 8, 7
			full := session.New(session.WithID(id))
			addTool := func(n int) {
				id := fmt.Sprintf("tool-%d", n)
				full.AddMessage(session.NewAgentMessage("root", &chatmsg.Message{Role: chatmsg.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: id, Function: tools.FunctionCall{Name: "check", Arguments: `{"arg":"value"}`}}}, ToolDefinitions: []tools.Tool{{Name: "check"}}}))
				full.AddMessage(session.NewAgentMessage("root", &chatmsg.Message{Role: chatmsg.MessageRoleTool, ToolCallID: id, Content: fmt.Sprintf("TOOL-%d-RECOVERED\n", n) + strings.Repeat("payload line\n", 23_000)}))
			}
			addUser := func(n int) {
				full.AddMessage(session.UserMessage(fmt.Sprintf("USER-%d-RECOVERED", n), chatmsg.MessagePart{Type: chatmsg.MessagePartTypeDocument, Document: &chatmsg.Document{Name: fmt.Sprintf("attachment-%d.txt", n), MimeType: "text/plain", Source: chatmsg.DocumentSource{InlineText: strings.Repeat("document data\n", 23_000)}}}))
			}
			addUser(0)
			addTool(0)
			full.AddMessage(session.NewAgentMessage("root", &chatmsg.Message{Role: chatmsg.MessageRoleAssistant, Content: "visible older answer"}))
			addUser(1)
			addTool(1)
			full.AddMessage(session.NewAgentMessage("root", &chatmsg.Message{Role: chatmsg.MessageRoleAssistant, Content: "visible tail answer"}))
			older := api.SessionHistoryPage{Projection: "tail", SessionID: id, Epoch: epoch, Cursor: cursor, TranscriptPosition: end, History: api.SessionHistory{StartPosition: 0, EndPosition: 4, TotalItems: end}, Items: []api.SessionHistoryItem{{Position: 1, Item: full.Messages[1]}, {Position: 3, Item: full.Messages[3]}}}
			omission := func(pos int) api.SessionHistoryOmittedRange {
				out := api.SessionHistoryOmittedRange{StartPosition: pos, EndPosition: pos + 1, Reason: "item_exceeds_response_byte_limit"}
				if mode != "old server missing cursors" {
					out.Items = []api.SessionHistoryOmittedItem{{Position: pos, Cursor: fmt.Sprintf("item-%d", pos)}}
				}
				return out
			}
			older.History.OmittedRanges = []api.SessionHistoryOmittedRange{omission(0), omission(2)}
			var fallbackCount, itemCount atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case api.SessionAPIPath + "/s/history":
					assert.Equal(t, "older", r.URL.Query().Get("cursor"))
					assert.NoError(t, json.NewEncoder(w).Encode(older))
				case api.SessionAPIPath + "/s/history/item":
					itemCount.Add(1)
					if mode == "old server missing route" {
						http.NotFound(w, r)
						return
					}
					if mode == "item exceeds endpoint cap" {
						http.Error(w, `{"error":"history_item_too_large"}`, http.StatusRequestEntityTooLarge)
						return
					}
					var position int
					_, err := fmt.Sscanf(r.URL.Query().Get("cursor"), "item-%d", &position)
					if !assert.NoError(t, err) {
						return
					}
					assert.NoError(t, json.NewEncoder(w).Encode(api.SessionHistoryItemPage{SessionID: id, Epoch: epoch, Cursor: cursor, TranscriptPosition: end, Position: position, Item: full.Messages[position]}))
				case api.SessionAPIPath + "/s/snapshot":
					fallbackCount.Add(1)
					snapshot := api.SessionSnapshot[runtime.SessionState, runtime.InteractionKind, any]{Session: full, Status: api.SessionStatus[runtime.SessionState]{SessionID: id}, Epoch: epoch, Cursor: cursor, TranscriptPosition: end}
					assert.NoError(t, json.NewEncoder(w).Encode(snapshot))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client, err := runtime.NewClient(server.URL, runtime.WithHTTPClient(server.Client()))
			require.NoError(t, err)
			transport, err := runtime.NewSessionTransport(client, runtime.WithSelectedSessionHistory())
			require.NoError(t, err)
			tail := runtime.SessionSnapshot{Session: session.New(session.WithID(id)), Epoch: epoch, Cursor: cursor, TranscriptPosition: end, HistoryCursor: "older", History: &session.BoundedItemPage{StartPosition: 4, BeforePosition: end, OmittedPositions: []int{4, 6}}, Items: []session.BoundedItem{{Position: 5, Item: full.Messages[5]}, {Position: 7, Item: full.Messages[7]}}, HistoryItemCursors: make(map[int]string)}
			for _, pos := range []int{4, 6} {
				if o := omission(pos); len(o.Items) > 0 {
					tail.HistoryItemCursors[pos] = o.Items[0].Cursor
				}
			}
			tail.Session.Messages = app.HistoryItems(tail, end)
			a := app.New(t.Context(), transport, tail.Session, runtime.SessionBinding{})
			ar := animation.NewRuntime()
			p := New(ar, t.Context(), a, service.NewSessionState(tail.Session), WithCompactWorkLog(true)).(*chatPage)
			p.messages.SetSize(100, 100)
			t.Cleanup(func() { p.messages.StopAnimations(); ar.Stop() })
			p.updateReplay(p.resetProjection(tail)())
			var started *historyItemStartMsg
			var walk func(tea.Cmd)
			walk = func(cmd tea.Cmd) {
				if cmd == nil {
					return
				}
				switch msg := cmd().(type) {
				case tea.BatchMsg:
					for _, child := range msg {
						walk(child)
					}
				case historyItemStartMsg:
					started = &msg
				}
			}
			for range 100 {
				if p.replay == nil {
					break
				}
				_, cmd := p.updateReplay(replayContinueMsg{generation: p.replayGeneration})
				if p.replay == nil {
					walk(cmd)
				}
			}
			frame := func() string { return ansi.Strip(p.messages.View()) }
			require.Nil(t, p.replay)
			require.Equal(t, 2, strings.Count(frame(), "Loading large transcript item"))
			require.Zero(t, itemCount.Load()+fallbackCount.Load(), "first usable frame precedes all recovery I/O")
			require.NotContains(t, frame(), "USER-1-RECOVERED")
			require.Contains(t, frame(), " Work ")
			p.handleRuntimeEvent(runtime.MessageAddedAt(id, full.Messages[7].Message, "root", 7))
			p.handleRuntimeEvent(runtime.AgentChoice("root", id, "unique live answer"))
			p.handleRuntimeEvent(runtime.MessageAddedAt(id, session.NewAgentMessage("root", &chatmsg.Message{Role: chatmsg.MessageRoleAssistant, Content: "unique live answer"}), "root", end))
			finishItems := func() {
				t.Helper()
				for range 2 {
					require.NotNil(t, started)
					_, load := p.updateReplay(*started)
					started = nil
					_, cmd := p.updateReplay(load())
					walk(cmd)
				}
			}
			finishItems()
			require.Contains(t, frame(), "USER-1-RECOVERED")
			require.Contains(t, frame(), "attachment-1.txt")
			require.NotContains(t, frame(), "Loading large transcript item")
			_, load := p.updateReplay(list.OlderHistoryRequestedMsg{})
			require.NotNil(t, load)
			_, apply := p.updateReplay(load())
			walk(apply)
			require.Equal(t, 2, strings.Count(frame(), "Loading large transcript item"))
			finishItems()
			p.messages.Update(tea.KeyPressMsg{Code: tea.KeyHome})
			got := frame()
			for _, text := range []string{"USER-0-RECOVERED", "attachment-0.txt", "visible older answer", "USER-1-RECOVERED", "attachment-1.txt", "visible tail answer", "unique live answer"} {
				require.Equal(t, 1, strings.Count(got, text), "%s", text)
			}
			require.NotContains(t, got, "Loading large transcript item")
			require.Equal(t, end, p.snapshotEnd)
			for n := range 2 {
				require.True(t, p.messages.NavigateToToolCall(fmt.Sprintf("tool-%d", n)))
				require.Contains(t, frame(), fmt.Sprintf("TOOL-%d-RECOVERED", n))
			}
			if mode == "item endpoint" {
				require.EqualValues(t, 4, itemCount.Load())
				require.Zero(t, fallbackCount.Load())
			} else {
				require.EqualValues(t, 1, fallbackCount.Load(), "one fallback even after loading an older page")
			}
		})
	}
}
