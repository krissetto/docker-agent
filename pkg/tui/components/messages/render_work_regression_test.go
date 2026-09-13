package messages

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/message"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestDirtyRebuildReservesFlattenedHistoryCapacity(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	for i := range 600 {
		view := &countedHistoryView{content: fmt.Sprintf("history-%d", i)}
		m.messages = append(m.messages, types.User(view.content))
		m.views = append(m.views, view)
	}
	m.AppendToLastMessage("root", "active tail")
	m.ensureAllItemsRendered()
	require.NotNil(t, m.activeSegments)
	before := append([]string(nil), m.renderedLines...)
	for range 3 {
		m.invalidateItem(len(m.messages) - 1)
		m.ensureAllItemsRendered()
		require.Equal(t, before, m.renderedLines)
		require.Equal(t, len(before), cap(m.renderedLines), "unchanged flattened history needs no append-growth headroom")
		require.Equal(t, len(before), m.activeSegments.start)
		require.Equal(t, m.totalHeight, m.activeSegments.start+m.activeSegments.height())
	}
	for _, view := range m.views[:600] {
		require.Equal(t, 1, view.(*countedHistoryView).renders)
	}
}

func TestDirtyRebuildKeepsUnreadCachedRangesWhenPrefixChangesHeight(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	var views []*countedHistoryView
	for i := range 80 {
		view := &countedHistoryView{content: fmt.Sprintf("history-%d", i)}
		views = append(views, view)
		m.messages = append(m.messages, types.User(view.content))
		m.views = append(m.views, view)
	}
	m.ensureAllItemsRendered()
	for _, prefix := range []string{strings.Repeat("expanded\n", 40) + "prefix", "short"} {
		views[0].content = prefix
		m.invalidateItem(0)
		m.ensureAllItemsRendered()
		expected := strings.Split(prefix, "\n")
		for i := 1; i < len(views); i++ {
			expected = append(expected, "", views[i].content)
		}
		require.Equal(t, expected, m.renderedLines, "rebuild must not overwrite cached source ranges before reading them")
		require.Equal(t, len(expected), m.totalHeight)
		require.Nil(t, m.activeSegments)
		for i, view := range views[1:] {
			require.Equal(t, 1, view.renders)
			require.Equal(t, view.content, m.renderedLines[m.lineOffsets[i+1]])
		}
	}
}

// BenchmarkRendererFixedHistory controls tail mutation internally: committing each
// iteration would start a new canonical message and make the workload grow.
func BenchmarkRendererFixedHistory(b *testing.B) {
	for _, mode := range []string{"unchanged", "dirty", "refresh"} {
		b.Run(mode, func(b *testing.B) {
			m := NewScrollableView(animation.NewRuntime(), 120, 40, &service.SessionState{}).(*model)
			m.SetSize(120, 40)
			b.Cleanup(m.StopAnimations)
			for i := range 600 {
				m.AppendToLastMessage(strconv.Itoa(i%2), strconv.Itoa(i)+strings.Repeat("content ", 20))
			}
			_ = m.View()
			tails := [2]string{strings.Repeat("content ", 20) + "tail-A", strings.Repeat("content ", 20) + "tail-B"}
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				if mode != "unchanged" {
					m.messages[599].Content = tails[i%2]
					m.views[599].(message.Model).SetMessage(m.messages[599])
					if mode == "dirty" {
						m.invalidateItem(599)
					} else {
						m.refreshRenderedItem(599)
					}
				}
				_ = m.View()
			}
			b.StopTimer()
			require.Len(b, m.messages, 600, "controlled renderer benchmark must not grow committed history")
		})
	}
}
