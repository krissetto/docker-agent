package messages

import (
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/docker/docker-agent/pkg/subagent"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/subagentindex"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func spawnBurstModel(b testing.TB, reasoning bool) *model {
	b.Helper()
	index := subagentindex.New()
	tree := subagent.Snapshot{}
	for i := range 150 {
		tree.Nodes = append(tree.Nodes, subagent.NodeSnapshot{Node: subagent.Node{ID: subagent.NodeID(fmt.Sprint(i)), Agent: fmt.Sprintf("worker-%d", i)}})
	}
	index.Reset(tree)
	m := newModel(animation.NewRuntime(), 120, 40, &service.SessionState{}, index)
	for i := range 100 {
		m.AddUserMessage(fmt.Sprintf("Historical question %d", i))
		m.AppendToLastMessage("root", "Historical answer with **formatting**.")
	}
	if reasoning {
		m.AppendReasoning("root", "Plan parallel work.")
	}
	m.View()
	return m
}

func BenchmarkParallelSpawnBurst(b *testing.B) {
	for _, count := range []int{30, 150} {
		for _, reasoning := range []bool{false, true} {
			b.Run(fmt.Sprintf("calls=%d/reasoning=%t", count, reasoning), func(b *testing.B) {
				b.ReportAllocs()
				var maxFrame time.Duration
				for b.Loop() {
					b.StopTimer()
					m := spawnBurstModel(b, reasoning)
					b.StartTimer()
					for i := range count {
						args := fmt.Sprintf(`{"agent":"worker-%d","task":"Inspect assigned package and report correctness, concurrency and performance risks with evidence."}`, i)
						m.AddOrUpdateToolCall("root", tools.ToolCall{ID: fmt.Sprint(i), Function: tools.FunctionCall{Name: "spawn_subagent", Arguments: args}}, tools.Tool{}, types.ToolStatusPending)
						m.ScrollToBottom()
						frameStart := time.Now()
						m.View()
						maxFrame = max(maxFrame, time.Since(frameStart))
					}
					b.StopTimer()
					m.StopAnimations()
					b.StartTimer()
				}
				b.ReportMetric(float64(maxFrame.Nanoseconds()), "max-frame-ns")
			})
		}
	}
}

func BenchmarkParallelSpawnFragments(b *testing.B) {
	for _, reasoning := range []bool{false, true} {
		b.Run(fmt.Sprintf("reasoning=%t", reasoning), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				m := spawnBurstModel(b, reasoning)
				b.StartTimer()
				for fragment := range 256 {
					for i := range 32 {
						args := strings.Repeat("x", 128)
						if fragment == 0 {
							args = `{"agent":"coder","task":"` + args
						}
						if fragment == 255 {
							args += `"}`
						}
						m.AddOrUpdateToolCall("root", tools.ToolCall{ID: fmt.Sprint(i), Function: tools.FunctionCall{Name: "spawn_subagent", Arguments: args}}, tools.Tool{}, types.ToolStatusPending)
						m.ScrollToBottom()
						m.View()
					}
				}
				b.StopTimer()
				m.StopAnimations()
				b.StartTimer()
			}
		})
	}
}

func TestParallelSpawnOnlyRendersChangedPendingItem(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 120, 40, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	for i := range 30 {
		m.AddOrUpdateToolCall("root", tools.ToolCall{ID: fmt.Sprint(i), Function: tools.FunctionCall{Name: "spawn_subagent", Arguments: `{"agent":"coder","task":"`}}, tools.Tool{}, types.ToolStatusPending)
		m.View()
	}
	before := m.renderedMessages
	m.AddOrUpdateToolCall("root", tools.ToolCall{ID: "0", Function: tools.FunctionCall{Name: "spawn_subagent", Arguments: "fragment"}}, tools.Tool{}, types.ToolStatusPending)
	m.View()
	require.EqualValues(t, 1, m.renderedMessages-before, "unrelated pending tools retain their current frame")
}

func TestPendingToolCacheAdvancesSpinnerAndSharedIndex(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ar := animation.NewRuntime()
		m := newModel(ar, 120, 40, &service.SessionState{}, subagentindex.New())
		t.Cleanup(m.StopAnimations)
		call := tools.ToolCall{ID: "spawn", Function: tools.FunctionCall{Name: "spawn_subagent", Arguments: `{"agent":"coder","task":"pending"}`}}
		m.AddOrUpdateToolCall("root", call, tools.Tool{}, types.ToolStatusPending)
		first := m.View()
		before := m.renderedMessages
		require.Equal(t, first, m.View())
		require.Equal(t, before, m.renderedMessages)
		initialTick, accepted := ar.Accept(ar.Continue()().(animation.TickMsg))
		require.True(t, accepted)
		_ = initialTick
		time.Sleep(200 * time.Millisecond) //nolint:forbidigo // Advances synctest's fake clock.
		tick, ok := ar.Accept(ar.Continue()().(animation.TickMsg))
		require.True(t, ok)
		m.Update(tick)
		require.NotEqual(t, first, m.View(), "retained pending ranges are invalidated on spinner frame change")

		read := tools.ToolCall{ID: "read", Function: tools.FunctionCall{Name: "read_subagent", Arguments: `{"subagent_id":"child"}`}}
		m.AddOrUpdateToolCall("root", read, tools.Tool{}, types.ToolStatusRunning)
		require.NotContains(t, ansi.Strip(m.View()), "researcher")
		m.subagents.Reset(subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child", Agent: "researcher"}}}})
		m.RefreshInputReferences()
		require.Contains(t, ansi.Strip(m.View()), "researcher", "index updates invalidate cached attribution")
	})
}
