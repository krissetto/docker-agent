package messages

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/components/tool"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestOlderHistoryPreservesReaderAnchorAndWork(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(fmt.Sprintf("work=%t", compact), func(t *testing.T) {
			ar := animation.NewRuntime()
			renderers := tool.NewRegistry()
			renderers.Register("check", func(_ *animation.Runtime, msg *types.Message, _ service.SessionStateReader) layout.Model {
				return &labelView{label: msg.Content}
			})
			m := NewScrollableView(ar, 88, 8, &service.SessionState{}, WithToolRenderers(renderers)).(*model)
			t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
			m.SetCompactWorkLog(compact)
			prefix := session.New(session.WithID("s"))
			prefix.AddMessage(session.UserMessage("the original question"))
			call := tools.ToolCall{ID: "check-1", Function: tools.FunctionCall{Name: "check", Arguments: `{}`}}
			prefix.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "earlier thinking", ToolCalls: []tools.ToolCall{call}, ToolDefinitions: []tools.Tool{{Name: "check"}}}))
			tail := session.New(session.WithID("s"))
			tail.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleTool, ToolCallID: call.ID, Content: "boundary-tool-output"}))
			tail.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "recent thinking", Content: strings.Repeat("visible reader anchor\n\n", 15)}))
			BeginHistoryReplay(m, PrepareReplay(tail), nil, 2)
			finishPreparedReplay(t, m)
			finishHistoryReplay(t, m)
			m.scrollToTop()
			SetOlderHistoryAvailable(m, true)
			_, older := m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
			require.IsType(t, OlderHistoryRequestedMsg{}, older())
			m.View()
			m.setScrollOffset(m.slotOffset(len(m.views)-1) + 5)
			before := strings.Split(ansi.Strip(m.View()), "\n")
			require.Contains(t, strings.Join(before, "\n"), "visible reader anchor")
			beforeSticky := m.stickyWorkInset()
			results := make(map[string]string)
			AddHistoryToolResults(results, tail.Messages)
			PrependHistory(m, prefix, 0, results, nil)
			after := strings.Split(ansi.Strip(m.View()), "\n")
			if compact {
				require.Contains(t, after[0], "Work [+]")
				after = after[1:]
				before = before[beforeSticky:]
			}
			shared := min(len(before), len(after))
			for i := range shared {
				require.Equal(t, before[i][:m.contentWidth()], after[i][:m.contentWidth()], "reader body row %d", i)
			}
			require.Equal(t, 1, m.MessageTypeCount(types.MessageTypeAssistantReasoningBlock))
			require.Zero(t, *m.messages[0].SessionPosition)
			m.SetCompactWorkLog(false)
			m.SetSize(88, 100)
			m.scrollToTop()
			m.views[1].(*reasoningblock.Model).Toggle()
			m.invalidateItem(1)
			frame := ansi.Strip(m.View())
			for _, text := range []string{"the original question", "earlier thinking", "recent thinking", "boundary-tool-output"} {
				require.Equal(t, 1, strings.Count(frame, text), "%q in frame:\n%s", text, frame)
			}
		})
	}
}

// This includes wire decode, prepared replay and first render, but not process/PTY startup.
func BenchmarkSelectedHistoryFirstFrame(b *testing.B) {
	for _, count := range []int{1600, 8000} {
		history := session.New(session.WithID("s"))
		text := strings.Repeat("synthetic example with **formatted** content. ", 18)
		for i := range count {
			if i%2 == 0 {
				history.AddMessage(session.UserMessage(text))
			} else {
				history.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: text}))
			}
		}
		for _, bounded := range []bool{false, true} {
			b.Run(fmt.Sprintf("items=%d/bounded=%t", count, bounded), func(b *testing.B) {
				payload := history.Clone()
				start := 0
				if bounded {
					start = count - 50
					payload.Messages = payload.Messages[start:]
				}
				wire, err := json.Marshal(payload)
				require.NoError(b, err)
				b.ReportAllocs()
				for b.Loop() {
					var snapshot session.Session
					require.NoError(b, json.Unmarshal(wire, &snapshot))
					ar := animation.NewRuntime()
					m := NewScrollableView(ar, 100, 24, &service.SessionState{}).(*model)
					m.SetCompactWorkLog(true)
					if bounded {
						BeginHistoryReplay(m, PrepareReplay(&snapshot), nil, start)
					} else {
						BeginReplay(m, PrepareReplay(&snapshot), nil)
					}
					drainRetainedReplay(b, m)
					_ = m.View()
					m.StopAnimations()
					ar.Stop()
				}
				b.ReportMetric(float64(len(wire)), "wire-B/op")
			})
		}
	}
}
