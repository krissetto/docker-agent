package completion

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestCompletionViewportBoundsAndSelectedRowSurviveResize(t *testing.T) {
	m := New().(*manager)
	var items []Item
	for i := range 24 {
		items = append(items, Item{Label: fmt.Sprintf("item%02d", i), Description: strings.Repeat("long 界 description\n\t", 40), Value: fmt.Sprintf("value-%d", i)})
	}
	m.Update(OpenMsg{Items: items})
	for range 18 {
		m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	for _, size := range [][2]int{{80, 24}, {24, 8}, {2, 3}, {1, 1}, {80, 24}} {
		m.SetSize(size[0], size[1])
		m.SetEditorBottom(1)
		view := m.View()
		require.Equal(t, 18, m.selected)
		if size[1] <= 2 {
			require.Empty(t, view)
			require.Empty(t, m.GetLayers(), "no popup is painted over the only editor cell")
			continue
		}
		require.LessOrEqual(t, lipgloss.Height(view), size[1]-2)
		for line := range strings.SplitSeq(view, "\n") {
			require.LessOrEqual(t, ansi.StringWidth(line)+2*m.popupX(), size[0])
		}
		if size[0] >= 24 {
			require.Contains(t, ansi.Strip(view), "item18", "resized scroll window retains selected item")
		}
		require.Len(t, m.GetLayers(), 1)
		require.Equal(t, items, m.items, "render normalization never changes completion values")
	}
}

func TestCompletionLongLabelsAndLoadingStayWithinAllocatedCells(t *testing.T) {
	m := New().(*manager)
	m.SetSize(18, 8)
	m.SetEditorBottom(2)
	m.Update(OpenMsg{Items: []Item{{Label: strings.Repeat("界\n", 100), Description: strings.Repeat("details", 100), Value: "original"}}})
	for _, query := range []string{"", "missing"} {
		m.Update(QueryMsg{Query: query})
		m.Update(SetLoadingMsg{Loading: true})
		view := m.View()
		require.LessOrEqual(t, lipgloss.Width(view)+2*m.popupX(), 18)
		require.LessOrEqual(t, lipgloss.Height(view), 5)
	}
}

func TestCompletionAllocationDoesNotDiscardFilteredState(t *testing.T) {
	m := New().(*manager)
	items := []Item{{Label: "Restart Toolset", Value: "/toolset-restart"}, {Label: "Exit", Value: "/exit"}}
	m.Update(OpenMsg{Items: items, MatchMode: MatchPrefix})
	m.Update(QueryMsg{Query: "tool"})
	require.True(t, m.Open())
	require.Equal(t, items[:1], m.filteredItems)
	require.Empty(t, m.View(), "an unallocated popup must not paint over the editor")
	require.Empty(t, m.GetLayers())
	for _, size := range [][2]int{{80, 24}, {0, 0}, {1, 1}, {80, 24}} {
		m.SetSize(size[0], size[1])
		require.True(t, m.Open())
		require.Equal(t, items[:1], m.filteredItems, "geometry never changes matching or exact values")
		if size[0] == 80 {
			require.Contains(t, m.View(), "Restart Toolset")
			require.Len(t, m.GetLayers(), 1)
		} else {
			require.Empty(t, m.View())
			require.Empty(t, m.GetLayers(), "explicit zero/tiny geometry cannot use a guessed default size")
		}
	}
}
