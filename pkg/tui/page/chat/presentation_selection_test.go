package chat

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/messages"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
)

type selectionMessages struct {
	messages.Model

	clears int
	blurs  int
}

func (m *selectionMessages) ClearPresentationSelection() { m.clears++ }
func (m *selectionMessages) Blur() tea.Cmd               { m.blurs++; return nil }

func TestPresentationSelectionClearsOnlyTranscriptCapability(t *testing.T) {
	p := newLayoutTestPage(t, msgtypes.SidebarRight)
	tracked := &selectionMessages{Model: p.messages}
	p.messages = tracked
	p.SetSize(160, 40)
	layout := p.appliedLayout
	settings := p.GetSidebarSettings()
	p.ClearPresentationSelection()
	require.Equal(t, 1, tracked.clears)
	require.Zero(t, tracked.blurs)
	require.Equal(t, layout, p.appliedLayout)
	require.Equal(t, settings, p.GetSidebarSettings())
}

func TestCompactSidebarShellUsesAtMostTwoRows(t *testing.T) {
	for _, position := range []msgtypes.SidebarPosition{msgtypes.SidebarTop, msgtypes.SidebarBottom} {
		p := newLayoutTestPage(t, position)
		for _, width := range []int{20, 80, 160} {
			p.SetSize(width, 24)
			shell := p.MeasureSplitShell(width, 24)
			require.LessOrEqual(t, shell.Sidebar.Height, 2)
			require.GreaterOrEqual(t, shell.TranscriptArea.Height, 22)
			require.Equal(t, 24, shell.Sidebar.Height+shell.TranscriptArea.Height)
		}
	}
}

func TestSplitSidebarOverridePreservesPreferencesAcrossFocusAndGestures(t *testing.T) {
	first := newLayoutTestPage(t, msgtypes.SidebarRight)
	second := newLayoutTestPage(t, msgtypes.SidebarRight)
	first.SetSidebarSettings(SidebarSettings{PreferredWidth: 35})
	second.SetSidebarSettings(SidebarSettings{Collapsed: true, PreferredWidth: 55})
	first.SetSize(160, 40)
	second.SetSize(160, 40)
	firstSaved, secondSaved := first.GetSidebarSettings(), second.GetSidebarSettings()
	shared := firstSaved
	first.SetSplitSidebarSettings(&shared)
	second.SetSplitSidebarSettings(&shared)
	shared.PreferredWidth = 70
	width, ok := first.SplitSidebarSettings()
	require.True(t, ok)
	require.Equal(t, firstSaved, width, "caller mutation cannot move geometry")
	require.Equal(t, first.MeasureSplitShell(160, 40), second.MeasureSplitShell(160, 40))

	second.togglePresentationSidebar()
	toggled, ok := second.SplitSidebarSettings()
	require.True(t, ok)
	require.True(t, toggled.Collapsed)
	require.Equal(t, secondSaved, second.GetSidebarSettings())
	first.SetSplitSidebarSettings(&toggled)
	require.Equal(t, first.MeasureSplitShell(160, 40), second.MeasureSplitShell(160, 40))

	second.sidebarDragStartX = 100
	second.sidebarDragStartWidth = 35
	second.handleSidebarResize(90)
	resized, ok := second.SplitSidebarSettings()
	require.True(t, ok)
	require.False(t, resized.Collapsed)
	require.Equal(t, 45, resized.PreferredWidth)
	first.SetSplitSidebarSettings(&resized)
	require.Equal(t, first.MeasureSplitShell(160, 40), second.MeasureSplitShell(160, 40))
	require.Equal(t, firstSaved, first.GetSidebarSettings())
	require.Equal(t, secondSaved, second.GetSidebarSettings())

	first.SetSplitSidebarSettings(nil)
	second.SetSplitSidebarSettings(nil)
	_, ok = second.SplitSidebarSettings()
	require.False(t, ok)
	require.Equal(t, firstSaved, first.GetSidebarSettings())
	require.Equal(t, secondSaved, second.GetSidebarSettings())
	require.NotEqual(t, first.MeasureSplitShell(160, 40), second.MeasureSplitShell(160, 40), "leaving split restores each session's own geometry")
}
