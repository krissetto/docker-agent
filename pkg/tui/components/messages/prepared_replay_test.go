package messages

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/message"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// Run the commands the replay owner would route, including batched image loads.
func runReplayCommand(m *model, cmd tea.Cmd) int {
	if cmd == nil {
		return 0
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		workers := 0
		for _, child := range msg {
			workers += runReplayCommand(m, child)
		}
		return workers
	case ReplayRenderedMsg:
		ApplyReplayRender(m, msg)
		return 1
	default:
		_, next := m.Update(msg)
		return runReplayCommand(m, next)
	}
}

func finishPreparedReplay(t testing.TB, m *model) int {
	t.Helper()
	workers := 0
	for range 100 {
		done, cmd := ContinueReplay(m)
		workers += runReplayCommand(m, cmd)
		if done {
			return workers
		}
	}
	t.Fatal("replay did not finish; prepared artifact may be repeatedly discarded")
	return workers
}

func TestPreparedReplayTransfersOwnershipAndResolvesMedia(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	content := strings.Repeat("paragraph **body**\n\n", 1000)
	sess := &session.Session{Messages: []session.Item{
		assistantItem("worker", content, ""),
		assistantItem("other", content, ""),
	}}
	BeginReplay(m, PrepareReplay(sess), map[int][]types.AssistantMedia{0: {{ID: 1, Fallback: "pending media"}}})
	require.Equal(t, 2, finishPreparedReplay(t, m), "one preparation per oversized message, including finalized history")
	for _, view := range m.views {
		require.False(t, view.(message.Model).HasLiveRenderState(), "only the list retains replay ANSI")
	}
	require.Contains(t, ansi.Strip(strings.Join(m.renderedLines, "\n")), "pending media")
	m.UpdateAssistantMedia([]types.AssistantMedia{{ID: 1, Fallback: "resolved media"}})
	m.ensureAllItemsRendered()
	out := ansi.Strip(strings.Join(m.renderedLines, "\n"))
	require.Contains(t, out, "resolved media")
	require.NotContains(t, out, "pending media")
	fresh := message.New(animation.NewRuntime(), m.messages[0], nil)
	fresh.SetSize(m.contentWidth(), 0)
	start, end := m.lineOffsets[0], m.lineOffsets[1]-1
	require.Equal(t, fresh.View(), strings.Join(m.renderedLines[start:end], "\n"))

	m.SetSize(70, 12)
	require.True(t, ReconcileReplay(m))
	require.Equal(t, 2, finishPreparedReplay(t, m), "resize prepares once per new geometry")
	require.False(t, ReconcileReplay(m), "stable replay never reparses")
}

func TestPreparedReplayRejectsAsyncMediaResult(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	sess := &session.Session{Messages: []session.Item{assistantItem("worker", strings.Repeat("paragraph\n\n", 1700), "")}}
	BeginReplay(m, PrepareReplay(sess), map[int][]types.AssistantMedia{0: {{ID: 1, Fallback: "pending media"}}})
	var cmd tea.Cmd
	for range 100 {
		_, cmd = ContinueReplay(m)
		if ReplayWaiting(m) {
			break
		}
		runReplayCommand(m, cmd)
	}
	require.True(t, ReplayWaiting(m))
	// Resolve the media while the old immutable render inputs are on a worker.
	m.UpdateAssistantMedia([]types.AssistantMedia{{ID: 1, Fallback: "resolved media"}})
	// Execute the worker separately, but route its result only on this owner.
	result := make(chan tea.Msg, 1)
	go func() { result <- cmd() }()
	msg := <-result
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, child := range batch {
			runReplayCommand(m, child)
		}
	} else {
		ApplyReplayRender(m, msg.(ReplayRenderedMsg))
	}
	require.Equal(t, 1, finishPreparedReplay(t, m), "stale completion triggers exactly one current preparation")
	out := ansi.Strip(strings.Join(m.renderedLines, "\n"))
	require.Contains(t, out, "resolved media")
	require.NotContains(t, out, "pending media")
	require.False(t, m.views[0].(message.Model).HasLiveRenderState())
}

func BenchmarkPreparedReplayOwnership(b *testing.B) {
	content := strings.Repeat("paragraph **body**\n\n", 1000)
	sess := &session.Session{}
	for range 8 {
		sess.Messages = append(sess.Messages, assistantItem("worker", content, ""))
	}
	b.ReportAllocs()
	for b.Loop() {
		m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
		BeginReplay(m, PrepareReplay(sess), nil)
		workers := finishPreparedReplay(b, m)
		live := 0
		for _, view := range m.views {
			if view.(message.Model).HasLiveRenderState() {
				live++
			}
		}
		b.ReportMetric(float64(live), "retained-view-caches")
		b.ReportMetric(float64(workers), "workers/op")
		m.StopAnimations()
	}
}
