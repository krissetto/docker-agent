package messages

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/components/workblock"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestReasoningPreviewPersistsAcrossToolRounds(t *testing.T) {
	for _, compact := range []bool{false, true} {
		for _, expanded := range []bool{false, true} {
			for _, resumed := range []bool{false, true} {
				t.Run(fmt.Sprintf("compact=%v/expanded=%v/resumed=%v", compact, expanded, resumed), func(t *testing.T) {
					m := orderedTestModel(t)
					m.SetCompactWorkLog(compact)
					m.AddUserMessage("request")
					m.SetWorkStatus(true, 0)
					m.AppendReasoningContent("s", "first", "worker", "before ")
					m.AppendReasoningContent("s", "first", "worker", "tools")
					block := m.views[1].(*reasoningblock.Model)
					block.SetExpandedImmediately(expanded)
					m.View()
					if compact {
						firstWorkBlock(m).SetExpandedImmediately(expanded)
					}
					for round := range 3 {
						call := tools.ToolCall{ID: fmt.Sprintf("call-%d", round), Function: tools.FunctionCall{Name: "check", Arguments: `{}`}}
						m.AddOrUpdateToolCall("worker", call, tools.Tool{Name: "check"}, types.ToolStatusPending)
						m.AddOrUpdateToolCall("worker", call, tools.Tool{}, types.ToolStatusRunning)
						m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: call.ID, Response: "done"}, types.ToolStatusCompleted)
						// Resizes and hide/resume snap animations, not semantic preview state.
						m.SetSize(99-round, 100)
						m.StopAnimations()
						m.ResumeAnimations()
						frame := ansi.Strip(m.View())
						require.Contains(t, frame, "before tools", "round %d", round)
						require.True(t, block.ReasoningActive())
					}
					calls := make([]tools.ToolCall, 3)
					for i := range calls {
						calls[i] = tools.ToolCall{ID: fmt.Sprintf("call-%d", i)}
					}
					m.CompleteAssistant(runtime.MessageAdded("s", session.NewAgentMessage("worker", &chat.Message{
						Role: chat.MessageRoleAssistant, MessageID: "first", ToolCalls: calls,
						Presentation: []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "before tools"}, {Type: chat.AssistantPartToolCall, ToolCallID: calls[2].ID}},
					}), "worker").(*runtime.MessageAddedEvent))
					m.SetSize(95, 100)
					require.Contains(t, ansi.Strip(m.View()), "before tools", "tool-bearing commit preserves preview")

					want := "before tools"
					if resumed {
						// Empty chunks cannot create boundaries or consume the first real chunk.
						m.AppendReasoningContent("s", "first", "worker", "")
						for _, chunk := range []string{"after", " ", "tools"} {
							m.AppendReasoningContent("s", "first", "worker", chunk)
						}
						want += "\n\nafter tools"
					}
					require.Equal(t, want, block.Reasoning())
					require.Equal(t, want, m.messages[1].Content)
					lines, _ := block.PreviewLines()
					for i := range lines {
						lines[i] = strings.TrimSpace(lines[i])
					}
					require.Equal(t, strings.Split(strings.ReplaceAll(want, "\n\n", "\n"), "\n"), lines)
					frame := ansi.Strip(m.View())
					require.Equal(t, 1, strings.Count(frame, "before tools"))
					if resumed {
						require.Equal(t, 1, strings.Count(frame, "after tools"))
					}
					// A final answer can have a new model message identity.
					m.AppendAssistantContent("s", "answer", "worker", "final answer")
					require.False(t, block.ReasoningActive())
					m.SetWorkStatus(false, 0)
				})
			}
		}
	}
}

func TestReasoningChunksAndToolBoundaryStayIncremental(t *testing.T) {
	m := orderedTestModel(t)
	m.SetCompactWorkLog(true)
	for range 100 {
		m.AddUserMessage("old request")
		m.AppendToLastMessage("worker", "old answer")
	}
	m.AddUserMessage("request")
	m.SetWorkStatus(true, 0)
	m.AppendReasoningContent("s", "r", "worker", "first")
	m.View()
	before := m.stats.projectionRebuilds
	epochs := m.stats.epochs
	for range 50 {
		m.AppendReasoningContent("s", "r", "worker", " chunk")
		m.View()
	}
	require.Equal(t, before, m.stats.projectionRebuilds, "ordinary deltas never rebuild full history")
	call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "check", Arguments: `{}`}}
	m.AddOrUpdateToolCall("worker", call, tools.Tool{Name: "check"}, types.ToolStatusRunning)
	m.View()
	before = m.stats.projectionRebuilds
	m.AppendReasoningContent("s", "r", "worker", "")
	m.AppendReasoningContent("s", "r", "worker", "second")
	for range 50 {
		m.AppendReasoningContent("s", "r", "worker", " chunk")
		m.View()
	}
	block := m.views[len(m.views)-1].(*reasoningblock.Model)
	require.Equal(t, "first"+strings.Repeat(" chunk", 50)+"\n\nsecond"+strings.Repeat(" chunk", 50), block.Reasoning())
	require.Equal(t, before, m.stats.projectionRebuilds)
	require.Equal(t, epochs, m.stats.epochs, "chunks and tools do not reset transcript layout epochs")
	t.Logf("100 reasoning deltas over 100 old exchanges: zero full projection rebuilds, zero layout epochs")
}

func TestReasoningPreviewReplayIsSettledAndResumesOnce(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(fmt.Sprint(compact), func(t *testing.T) {
			call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "check", Arguments: `{}`}}
			sess := session.New(session.WithID("s"))
			sess.AddMessage(session.UserMessage("request"))
			sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{
				Role: chat.MessageRoleAssistant, ReasoningContent: "beforeafter", ToolCalls: []tools.ToolCall{call},
				Presentation: []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "before"}, {Type: chat.AssistantPartToolCall, ToolCallID: call.ID}, {Type: chat.AssistantPartReasoning, Text: "after"}},
			}))
			m := orderedTestModel(t)
			m.SetCompactWorkLog(compact)
			BeginReplay(m, PrepareReplay(sess), nil)
			finishPreparedReplay(t, m)
			block := m.views[1].(*reasoningblock.Model)
			require.Equal(t, "before\n\nafter", block.Reasoning())
			require.False(t, block.ReasoningActive(), "historical reasoning never starts live animation")
			m.View()
			if compact {
				firstWorkBlock(m).SetExpandedImmediately(false)
				m.invalidateAllItems()
				require.Contains(t, ansi.Strip(m.View()), "before")
				firstWorkBlock(m).SetExpandedImmediately(true)
				m.invalidateAllItems()
			}
			frame := ansi.Strip(m.View())
			require.Equal(t, 1, strings.Count(frame, "before"))
			require.Equal(t, 1, strings.Count(frame, "after"))
			m.SetWorkStatus(true, 0)
			if compact {
				firstWorkBlock(m).SetExpandedImmediately(false)
			}
			m.AppendReasoning("worker", " resumed")
			m.AppendReasoning("worker", " chunks")
			require.Equal(t, "before\n\nafter\n\n resumed chunks", block.Reasoning())
			require.Contains(t, ansi.Strip(m.View()), "resumed chunks")
		})
	}
}

func TestOlderCompletionDoesNotSplitResumedReasoningChunks(t *testing.T) {
	m := orderedTestModel(t)
	m.SetCompactWorkLog(true)
	m.AddUserMessage("request")
	m.SetWorkStatus(true, 0)
	m.AppendReasoningContent("s", "current", "worker", "before")
	call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "check", Arguments: `{}`}}
	m.AddOrUpdateToolCall("worker", call, tools.Tool{Name: "check"}, types.ToolStatusRunning)
	m.AppendReasoningContent("s", "current", "worker", "after")
	for _, chunk := range []string{" ", "tools", " again"} {
		m.CompleteAssistant(runtime.MessageAdded("s", session.NewAgentMessage("worker", &chat.Message{
			Role: chat.MessageRoleAssistant, MessageID: "older", Presentation: []chat.AssistantPart{},
		}), "worker").(*runtime.MessageAddedEvent))
		m.AppendReasoningContent("s", "current", "worker", chunk)
	}
	block := m.views[1].(*reasoningblock.Model)
	require.Equal(t, "before\n\nafter tools again", block.Reasoning(), "an unrelated completion cannot create a boundary per delta")
	require.True(t, block.ReasoningActive())
}

func TestOlderReasoningCommitCannotRewriteNewerPreview(t *testing.T) {
	for _, ordered := range []bool{false, true} {
		t.Run(fmt.Sprint(ordered), func(t *testing.T) {
			m := orderedTestModel(t)
			m.SetCompactWorkLog(true)
			m.AddUserMessage("request")
			m.SetWorkStatus(true, 0)
			m.AppendReasoningContent("s", "current", "worker", "new reasoning")
			msg := &chat.Message{Role: chat.MessageRoleAssistant, MessageID: "older", ReasoningContent: "old reasoning"}
			if ordered {
				msg.Presentation = []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "old reasoning"}}
			}
			m.CompleteAssistant(runtime.MessageAdded("s", session.NewAgentMessage("worker", msg), "worker").(*runtime.MessageAddedEvent))
			m.AppendReasoningContent("s", "current", "worker", " continues")
			block := m.views[1].(*reasoningblock.Model)
			require.Equal(t, "new reasoning continues", block.Reasoning())
			require.True(t, block.ReasoningActive())
		})
	}
}

func TestLateAnonymousCompletionCannotRewriteCurrentReasoning(t *testing.T) {
	for _, id := range []string{"", "legacy:1"} {
		for _, ordered := range []bool{false, true} {
			t.Run(fmt.Sprintf("id=%q/ordered=%v", id, ordered), func(t *testing.T) {
				m := orderedTestModel(t)
				m.SetCompactWorkLog(true)
				m.AddUserMessage("request")
				m.SetWorkStatus(true, 0)
				m.AppendReasoningContent("s", "current", "worker", "new reasoning")
				block := m.views[1].(*reasoningblock.Model)
				tracker := m.streamedReasoning
				msg := &chat.Message{Role: chat.MessageRoleAssistant, MessageID: id, ReasoningContent: "old reasoning"}
				if ordered {
					msg.Presentation = []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "old reasoning"}}
				}
				m.CompleteAssistant(runtime.MessageAdded("s", session.NewAgentMessage("worker", msg), "worker").(*runtime.MessageAddedEvent))
				require.Same(t, tracker, m.streamedReasoning)
				require.Same(t, block, m.views[1])
				require.Equal(t, "new reasoning", block.Reasoning())
				require.Equal(t, "new reasoning", m.messages[1].Content)
				require.True(t, block.ReasoningActive())
				m.AppendReasoningContent("s", "current", "worker", " continues")
				require.Equal(t, "new reasoning continues", block.Reasoning())
			})
		}
	}
}

func TestCollapsedWorkPreviewReloadUsesOnlyDisplayableExchangeReasoning(t *testing.T) {
	for _, tc := range []struct {
		name         string
		reasoning    string
		presentation []chat.AssistantPart
		want         []string
	}{
		{name: "legacy short", reasoning: "visible-one", want: []string{"visible-one"}},
		{name: "ordered tail", reasoning: "aggregate-not-displayable", presentation: []chat.AssistantPart{
			{Type: chat.AssistantPartReasoning, Text: "visible-one\n\nvisible-two"},
			{Type: chat.AssistantPartContent, Text: "intermediate"},
			{Type: chat.AssistantPartReasoning, Text: "visible-three\n\nvisible-four"},
		}, want: []string{"visible-two", "visible-three", "visible-four"}},
		{name: "ordered presentation suppresses aggregate", reasoning: "aggregate-not-displayable", presentation: []chat.AssistantPart{{Type: chat.AssistantPartContent, Text: "public content only"}}},
		{name: "tool only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := session.New(session.WithID("reload-preview"))
			sess.AddMessage(session.UserMessage("earlier request"))
			sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "earlier-exchange"}))
			sess.AddMessage(session.UserMessage("current request"))
			call := tools.ToolCall{ID: "reload-call", Function: tools.FunctionCall{Name: "probe"}}
			msg := &chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: tc.reasoning, Presentation: tc.presentation, ToolCalls: []tools.ToolCall{call},
				OpenAIResponse: &chat.OpenAIResponse{Source: "test", Output: []json.RawMessage{json.RawMessage(`{"type":"reasoning","encrypted_content":"opaque-not-displayable"}`)}},
			}
			sess.AddMessage(session.NewAgentMessage("worker", msg))
			sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: "standalone", Function: tools.FunctionCall{Name: "probe"}}}}))
			sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: "final answer"}))
			data, err := json.Marshal(sess)
			require.NoError(t, err)
			var reloaded session.Session
			require.NoError(t, json.Unmarshal(data, &reloaded))
			m := orderedTestModel(t)
			m.SetCompactWorkLog(true)
			BeginReplay(m, PrepareReplay(&reloaded), nil)
			finishPreparedReplay(t, m)
			m.View()
			var work *workblock.Model
			for _, candidate := range m.workBlocks {
				for _, item := range candidate.Items() {
					if item.SourceIndex > 2 {
						work = candidate
						break
					}
				}
			}
			require.NotNil(t, work)
			work.SetExpandedImmediately(false)
			frame := ansi.Strip(work.View())
			require.Equal(t, 1+len(tc.want), work.Height(), "replay has no live blank budget")
			require.NotContains(t, frame, "earlier-exchange")
			require.NotContains(t, frame, "aggregate-not-displayable")
			require.NotContains(t, frame, "opaque-not-displayable")
			for _, line := range tc.want {
				require.Equal(t, 1, strings.Count(frame, line))
			}
			if len(tc.want) == 3 {
				require.NotContains(t, frame, "visible-one")
			}
			work.SetExpandedImmediately(true)
			frame = ansi.Strip(work.View())
			for _, line := range tc.want {
				require.Equal(t, 1, strings.Count(frame, line), "expanded source history is not duplicated")
			}
			require.NotContains(t, frame, "opaque-not-displayable")
			require.NotContains(t, frame, "aggregate-not-displayable")
		})
	}
}
