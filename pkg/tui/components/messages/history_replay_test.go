package messages

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func historyReplayFixture(count int) *session.Session {
	sess := session.New(session.WithID("history-replay"))
	for i := range count {
		sess.Messages = append(sess.Messages, assistantItem("worker", strings.Repeat("paragraph `code` content\n\n", 20)+fmt.Sprintf("history-%04d **result**", i), ""))
	}
	return sess
}

func finishHistoryReplay(tb testing.TB, m *model) {
	tb.Helper()
	for range 10000 {
		if !HistoryPending(m) {
			return
		}
		runReplayCommand(m, ContinueHistory(m))
	}
	tb.Fatal("older history did not finish")
}

func TestReplayPresentsRecentHistoryBeforeRenderingPrefix(t *testing.T) {
	for _, count := range []int{20, 100, 1000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
			t.Cleanup(m.StopAnimations)
			sess := historyReplayFixture(count)
			BeginReplay(m, PrepareReplay(sess), nil)
			finishPreparedReplay(t, m)
			require.True(t, HistoryPending(m))
			require.Equal(t, count-12, m.historyStart)
			_, _, renders := m.ResizeCacheStats()
			require.EqualValues(t, 12, renders, "first usable transcript renders a bounded suffix")
			require.Contains(t, ansi.Strip(m.View()), fmt.Sprintf("history-%04d", count-1))

			m.AddUserMessage("new user input while older history warms")
			m.AppendToLastMessage("worker", "live assistant tail")
			require.Contains(t, ansi.Strip(m.View()), "live assistant tail")
			finishHistoryReplay(t, m)
			require.Zero(t, m.historyStart)
			require.Contains(t, ansi.Strip(m.View()), "live assistant tail")
			m.scrollToTop()
			require.Contains(t, ansi.Strip(m.View()), "paragraph code content")
			require.Equal(t, 0, m.scrollOffset)
			all := ansi.Strip(strings.Join(m.renderedLines, "\n"))
			last := -1
			for i := range count {
				marker := fmt.Sprintf("history-%04d", i)
				require.Equal(t, 1, strings.Count(all, marker))
				pos := strings.Index(all, marker)
				require.Greater(t, pos, last)
				last = pos
			}
		})
	}
}

func TestHistoryReplayPrependsWithoutMovingReaderOrSelection(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	BeginReplay(m, PrepareReplay(historyReplayFixture(50)), nil)
	finishPreparedReplay(t, m)
	m.scrollPageUp()
	before := m.View()
	oldOffset := m.scrollOffset
	m.selection.start(oldOffset+1, 2)
	m.selection.update(oldOffset+2, 5)
	m.selection.anchorTo()
	start, end := m.selection.startLine, m.selection.endLine
	runReplayCommand(m, ContinueHistory(m))
	delta := m.scrollOffset - oldOffset
	require.Positive(t, delta)
	require.Equal(t, start+delta, m.selection.startLine)
	require.Equal(t, end+delta, m.selection.endLine)
	require.Equal(t, start+delta, m.selection.anchorStartLine)
	m.selection.clear()
	require.Equal(t, before, m.View(), "prepending must retain the same visible rows")
}

func TestHistoryReplayResetRejectsInFlightPrefixRender(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	sess := historyReplayFixture(14)
	sess.Messages[1] = assistantItem("worker", strings.Repeat("old oversized prefix\n\n", 1500), "")
	BeginReplay(m, PrepareReplay(sess), nil)
	finishPreparedReplay(t, m)
	worker := ContinueHistory(m)
	require.NotNil(t, worker)
	old := m.historyReplay
	replacement := historyReplayFixture(12)
	replacement.Messages[11] = assistantItem("worker", "replacement authoritative tail", "")
	BeginReplay(m, PrepareReplay(replacement), nil)
	runReplayCommand(m, worker)
	require.NotSame(t, old, m.historyReplay)
	finishPreparedReplay(t, m)
	finishHistoryReplay(t, m)
	require.Contains(t, ansi.Strip(m.View()), "replacement authoritative tail")
	require.NotContains(t, strings.Join(m.renderedLines, "\n"), "old oversized prefix")
}

func BenchmarkReplayFirstUsable(b *testing.B) {
	for _, count := range []int{20, 100, 1000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			sess := historyReplayFixture(count)
			b.ReportAllocs()
			for b.Loop() {
				start := time.Now()
				m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
				BeginReplay(m, PrepareReplay(sess), nil)
				finishPreparedReplay(b, m)
				_ = m.View()
				first := time.Since(start)
				_, _, renders := m.ResizeCacheStats()
				b.ReportMetric(float64(first.Nanoseconds()), "first-usable-ns/op")
				b.ReportMetric(float64(renders), "first-rendered-messages/op")
				m.StopAnimations()
			}
		})
	}
}

func TestHistoryReplayGeometryChangeAndShortCards(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 80, 40, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	sess := historyReplayFixture(100)
	for i := range sess.Messages {
		sess.Messages[i] = assistantItem("worker", fmt.Sprintf("short card %d", i), "")
	}
	BeginReplay(m, PrepareReplay(sess), nil)
	finishPreparedReplay(t, m)
	require.Equal(t, 60, m.historyStart)
	require.GreaterOrEqual(t, m.totalHeight, m.height)
	m.SetSize(60, 20)
	_ = m.View()
	finishHistoryReplay(t, m)
	all := ansi.Strip(strings.Join(m.renderedLines, "\n"))
	require.Contains(t, all, "short card 0")
	require.Contains(t, ansi.Strip(m.View()), "short card 99")
	for i, offset := range m.lineOffsets {
		if i > 0 {
			require.Greater(t, offset, m.lineOffsets[i-1])
		}
	}
}
