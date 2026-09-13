package messages

import (
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type presentationHistoryView struct {
	countedHistoryView

	initializations int
	resumptions     int
}

func (v *presentationHistoryView) Init() tea.Cmd            { v.initializations++; return nil }
func (v *presentationHistoryView) ResumeAnimation() tea.Cmd { v.resumptions++; return nil }

func TestResumeAnimationsPreservesHistorySelectionAndScroll(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	m := NewScrollableView(ar, 60, 12, &service.SessionState{}).(*model)
	for i := range 40 {
		view := &presentationHistoryView{countedHistoryView: countedHistoryView{content: fmt.Sprintf("λ界 history %d", i)}}
		m.messages = append(m.messages, types.User(view.content))
		m.views = append(m.views, view)
	}
	m.View()
	m.scrollOffset, m.userHasScrolled = 7, true
	m.selection.active = true
	m.selection.startLine, m.selection.endLine = 8, 9
	m.selection.startCol, m.selection.endCol = 1, 3
	selected := m.selection
	m.SetPosition(31, 17)
	line, column := m.mouseToLineCol(35, 20)
	require.Equal(t, 10, line)
	require.Equal(t, 4, column)
	r, misses, renders := m.ResizeCacheStats()
	for range 3 {
		m.StopAnimations()
		m.ResumeAnimations()
		m.View()
		require.Equal(t, selected, m.selection)
		require.Equal(t, 7, m.scrollOffset)
		r2, misses2, renders2 := m.ResizeCacheStats()
		require.Equal(t, r, r2)
		require.Equal(t, misses, misses2)
		require.Equal(t, renders, renders2)
	}
	for _, view := range m.views {
		v := view.(*presentationHistoryView)
		require.Zero(t, v.initializations, "visibility must not restart media or any other Init effect")
		require.Equal(t, 3, v.resumptions)
	}
	m.AddAssistantMessage("root", "")
	m.StopAnimations()
	require.False(t, ar.HasActive())
	m.ResumeAnimations()
	require.Equal(t, int32(1), ar.ActiveCount())
	m.RemoveSpinner()
	require.False(t, ar.HasActive())
}
