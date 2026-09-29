package panel

import (
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestRightAlignmentUnicodeHitZonesAndNoticePriority(t *testing.T) {
	t.Parallel()
	elements := []Element{{messages.PanelWorkspace, "界é"}, {messages.PanelTodos, "1/2 todos"}}
	notice := lipgloss.NewStyle().Bold(true).Render("Notice") + strings.Repeat(" ", 34)
	frame := Render(notice, 40, elements, "")
	require.Equal(t, 40, ansi.StringWidth(frame.Content))
	require.True(t, strings.HasSuffix(ansi.Strip(frame.Content), "界é · 1/2 todos"))
	require.True(t, strings.HasPrefix(frame.Content, lipgloss.NewStyle().Bold(true).Render("Notice")))
	require.Len(t, frame.Zones, 2)
	for _, zone := range frame.Zones {
		for x := zone.Start; x < zone.End; x++ {
			id, ok := frame.Hit(x)
			require.True(t, ok)
			require.Equal(t, zone.ID, id)
		}
	}
	_, hit := frame.Hit(frame.Zones[0].End)
	require.False(t, hit)
	_, hit = frame.Hit(40)
	require.False(t, hit)
	for _, width := range []int{0, 1, 2, 4, 8, 16, 40} {
		row := strings.Repeat("x", min(6, width)) + strings.Repeat(" ", max(0, width-6))
		f := Render(row, width, elements, "")
		require.Equal(t, width, ansi.StringWidth(f.Content))
		require.True(t, strings.HasPrefix(ansi.Strip(f.Content), strings.Repeat("x", min(6, width))))
		for _, zone := range f.Zones {
			require.GreaterOrEqual(t, zone.Start, min(6, width))
			require.LessOrEqual(t, zone.End, width)
		}
	}
	require.Empty(t, Render("notice [Act]", 12, elements, "").Zones, "actions at right retain priority")
	require.Equal(t, notice, Render(notice, 40, nil, "").Content)
}
