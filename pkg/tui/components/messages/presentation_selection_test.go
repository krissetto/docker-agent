package messages

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
	"github.com/docker/docker-agent/pkg/tui/widgets/textarea"
)

func TestClearPresentationSelectionPreservesReaderAndDraft(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	m := NewScrollableView(ar, 60, 8, &service.SessionState{}).(*model)
	for i := range 40 {
		view := &countedHistoryView{content: fmt.Sprintf("history %d", i)}
		m.messages = append(m.messages, types.User(view.content))
		m.views = append(m.views, view)
	}
	m.View()
	m.scrollOffset, m.userHasScrolled = 7, true
	m.focused, m.selectedMessageIndex = true, 9
	m.inlineEditTextarea = textarea.New()
	m.inlineEditTextarea.SetValue("independent draft\nsecond line")
	m.selection = selectionState{
		active: true, mouseButtonDown: true, anchored: true,
		startLine: 8, startCol: 1, endLine: 9, endCol: 3,
		anchorStartLine: 8, anchorStartCol: 1, anchorEndLine: 8, anchorEndCol: 3,
		lastClickTime: time.Now(), lastClickLine: 8, lastClickCol: 1,
		clickCount: 2, pendingCopyID: 12,
	}
	m.View()
	_, _, renders := m.ResizeCacheStats()
	leases := ar.ActiveCount()
	m.ClearPresentationSelection()
	require.Equal(t, -1, m.selectedMessageIndex)
	require.Equal(t, selectionState{pendingCopyID: 13}, m.selection)
	require.True(t, m.focused, "the optional seam is not Blur")
	require.Equal(t, 7, m.scrollOffset)
	require.True(t, m.userHasScrolled)
	require.Equal(t, "independent draft\nsecond line", m.inlineEditTextarea.Value())
	require.Equal(t, leases, ar.ActiveCount())
	_, cmd := m.Update(DebouncedCopyMsg{ClickID: 12})
	require.Nil(t, cmd, "stale copy cannot affect a future selection")
	_, cmd = m.Update(AutoScrollTickMsg{Direction: 1})
	require.Nil(t, cmd, "released drag cannot autoscroll")
	m.View()
	require.Equal(t, 7, m.scrollOffset)
	_, _, after := m.ResizeCacheStats()
	require.Equal(t, renders+1, after, "only the previously selected item rerenders")
}

func TestClearPresentationSelectionIdleKeepsWarmTranscript(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	m := NewScrollableView(ar, 60, 8, &service.SessionState{}).(*model)
	view := &countedHistoryView{content: "warm history"}
	m.messages = append(m.messages, types.User(view.content))
	m.views = append(m.views, view)
	m.View()
	beforeRebuilds, beforeMisses, beforeRenders := m.ResizeCacheStats()
	generation := m.VisualGeneration()
	// Even a pending press without a highlighted range must release capture
	// and invalidate queued copies without dirtying transcript presentation.
	m.selection = selectionState{active: true, mouseButtonDown: true, pendingCopyID: 12}
	for range 3 {
		m.ClearPresentationSelection()
		m.View()
	}
	require.Equal(t, selectionState{pendingCopyID: 15}, m.selection)
	require.Equal(t, generation, m.VisualGeneration())
	rebuilds, misses, renders := m.ResizeCacheStats()
	require.Equal(t, beforeRebuilds, rebuilds)
	require.Equal(t, beforeMisses, misses)
	require.Equal(t, beforeRenders, renders)
	_, cmd := m.Update(DebouncedCopyMsg{ClickID: 12})
	require.Nil(t, cmd)
	_, cmd = m.Update(AutoScrollTickMsg{Direction: 1})
	require.Nil(t, cmd)
}

func TestClearPresentationSelectionTextRangeRepaints(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	m := NewScrollableView(ar, 60, 8, &service.SessionState{}).(*model)
	view := &countedHistoryView{content: "selected history"}
	m.messages = append(m.messages, types.User(view.content))
	m.views = append(m.views, view)
	m.View()
	m.selection = selectionState{active: true, mouseButtonDown: true, startLine: 0, startCol: 0, endLine: 0, endCol: 3, pendingCopyID: 4}
	generation := m.VisualGeneration()
	m.ClearPresentationSelection()
	require.True(t, m.renderDirty)
	require.Greater(t, m.VisualGeneration(), generation)
	require.Equal(t, selectionState{pendingCopyID: 5}, m.selection)
	m.View()
	require.False(t, m.renderDirty)
}
