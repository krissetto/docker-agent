package messages

import (
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// workScaleTranscript builds exactly messages source rows as human exchanges of
// four rows each: user, reasoning with one nested tool, tool call, answer.
func workScaleTranscript(tb testing.TB, messages int) *model {
	tb.Helper()
	ar := animation.NewRuntime()
	tb.Cleanup(ar.Stop)
	m := newModel(ar, 100, 40, &service.SessionState{})
	for exchange := range messages / 4 {
		m.addMessage(&types.Message{Type: types.MessageTypeUser, Content: fmt.Sprintf("question %d", exchange)})
		reasoning := reasoningblock.New(ar, fmt.Sprintf("thinking-%d", exchange), "root", m.sessionState)
		reasoning.SetReasoning(fmt.Sprintf("considering option %d", exchange))
		reasoning.AddToolCall(types.ToolCallMessage("root", tools.ToolCall{ID: fmt.Sprintf("n%d", exchange), Function: tools.FunctionCall{Name: "read_file", Arguments: "{}"}}, tools.Tool{Name: "read_file"}, types.ToolStatusCompleted))
		m.messages = append(m.messages, &types.Message{Type: types.MessageTypeAssistantReasoningBlock, Content: fmt.Sprintf("considering option %d", exchange)})
		m.views = append(m.views, reasoning)
		m.addMessage(types.ToolCallMessage("root", tools.ToolCall{ID: fmt.Sprintf("t%d", exchange), Function: tools.FunctionCall{Name: "write_file", Arguments: "{}"}}, tools.Tool{Name: "write_file"}, types.ToolStatusCompleted))
		m.addMessage(&types.Message{Type: types.MessageTypeAssistant, Sender: "root", Content: fmt.Sprintf("answer %d", exchange)})
	}
	m.View()
	return m
}

// BenchmarkWorkScale compares compact Work off and on over 100, 1000 and 6000
// actual messages: a dirty full re-layout, a steady viewport frame, and a hover
// over a grouped row.
func BenchmarkWorkScale(b *testing.B) {
	for _, size := range []int{100, 1000, 6000} {
		for _, enabled := range []bool{false, true} {
			name := fmt.Sprintf("%d/off", size)
			if enabled {
				name = fmt.Sprintf("%d/on", size)
			}
			b.Run(name+"/relayout", func(b *testing.B) {
				m := workScaleTranscript(b, size)
				m.SetCompactWorkLog(enabled)
				m.View()
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					m.renderDirty = true
					m.ensureAllItemsRendered()
				}
			})
			b.Run(name+"/frame", func(b *testing.B) {
				m := workScaleTranscript(b, size)
				m.SetCompactWorkLog(enabled)
				m.View()
				b.ReportAllocs()
				b.ResetTimer()
				for i := range b.N {
					m.scrollOffset = (i * 7) % max(1, m.totalHeight-m.height)
					m.invalidateView()
					_ = m.View()
				}
			})
			if enabled {
				b.Run(name+"/hover", func(b *testing.B) {
					m := workScaleTranscript(b, size)
					m.SetCompactWorkLog(true)
					m.View()
					b.ReportAllocs()
					b.ResetTimer()
					for range b.N {
						m.handleMouseMotion(tea.MouseMotionMsg{X: 4, Y: 1})
						_ = m.View()
					}
				})
			}
		}
	}
}
