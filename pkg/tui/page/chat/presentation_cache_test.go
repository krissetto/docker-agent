package chat

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/messages"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestTranscriptPresentationCacheExactGeometryAndBoundedReuse(t *testing.T) {
	var cache presentationCache
	for _, raw := range []string{"", "λ界 👩‍💻\n\x1b[31mred\x1b[0m\nlast", strings.Repeat("wide line λ界\n", 50)} {
		for _, size := range [][2]int{{0, 0}, {1, 1}, {12, 3}, {120, 40}} {
			want := presentationView(raw, size[0], size[1])
			require.Equal(t, want, cache.render(raw, size[0], size[1]))
			require.Zero(t, testing.AllocsPerRun(10, func() {
				_ = cache.render(raw, size[0], size[1])
			}), "warm wrapping/truncation is allocation-free")
			require.Equal(t, raw, cache.raw, "only the latest raw frame remains owned")
			require.Equal(t, want, cache.rendered)
			require.Equal(t, size[0], cache.width)
			require.Equal(t, size[1], cache.height)
		}
	}
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	oldGeneration := cache.themeGeneration
	theme := *original
	theme.Colors.TextMuted = "#123456"
	styles.ApplyTheme(&theme)
	require.Equal(t, presentationView(cache.raw, cache.width, cache.height), cache.render(cache.raw, cache.width, cache.height))
	require.NotEqual(t, oldGeneration, cache.themeGeneration)
	require.Equal(t, styles.ThemeGeneration(), cache.themeGeneration)
}

type presentationReadMessages struct {
	messages.Model

	reads int
}

func (m *presentationReadMessages) View() string {
	m.reads++
	return m.Model.View()
}

func assertCurrentTranscriptPresentation(t *testing.T, p *chatPage) string {
	t.Helper()
	got := p.TranscriptView()
	sl := p.computeSidebarLayout()
	if g := p.splitPresentation; g != nil {
		sl.chatWidth, sl.chatHeight = g.Transcript.Width, g.Transcript.Height
	}
	raw := p.messagesView(sl)
	require.Equal(t, presentationView(raw, sl.chatWidth, sl.chatHeight), got, "cache must match forced rendering of the current raw frame")
	require.Equal(t, raw, p.transcriptPresentation.raw)
	if sl.chatWidth > 0 && sl.chatHeight > 0 {
		require.Len(t, strings.Split(got, "\n"), sl.chatHeight)
		for line := range strings.SplitSeq(got, "\n") {
			require.Equal(t, sl.chatWidth, ansi.StringWidth(line))
		}
	}
	return got
}

func TestTranscriptPresentationCacheAlwaysReadsCurrentPageFrame(t *testing.T) {
	p := newLayoutTestPage(t, msgtypes.SidebarRight)
	p.SetSize(160, 40)
	tracked := &presentationReadMessages{Model: p.messages}
	p.messages = tracked
	banner := assertCurrentTranscriptPresentation(t, p)
	p.SetShowBanner(false)
	require.NotEqual(t, banner, assertCurrentTranscriptPresentation(t, p))
	p.messages.AddUserMessage("first user λ界")
	p.messages.AppendToLastMessage("root", "stream first")
	first := assertCurrentTranscriptPresentation(t, p)
	for range 5 {
		// Actual sidebar-only page motion must still read the messages owner,
		// even when it returns byte-identical transcript output.
		_, _ = p.Update(tea.MouseMotionMsg{X: 159, Y: 5})
		before := tracked.reads
		require.Equal(t, first, p.TranscriptView())
		require.Equal(t, before+1, tracked.reads)
	}
	p.messages.AppendToLastMessage("root", " streamed tail")
	streamed := assertCurrentTranscriptPresentation(t, p)
	require.NotEqual(t, first, streamed)
	require.Contains(t, ansi.Strip(streamed), "streamed tail")
	p.messages.AppendAssistantMedia("root", []types.AssistantMedia{{ID: 42, Fallback: "media pending"}})
	pending := assertCurrentTranscriptPresentation(t, p)
	require.Contains(t, ansi.Strip(pending), "media pending")
	_, _ = p.Update(generatedMediaResolvedMsg{media: []types.AssistantMedia{{ID: 42, Fallback: "media resolved λ界"}}})
	resolved := assertCurrentTranscriptPresentation(t, p)
	require.NotEqual(t, pending, resolved)
	require.Contains(t, ansi.Strip(resolved), "media resolved")
	// Locate actual visible text rather than dragging in the user's bottom
	// padding row. Input coordinates include the transcript's page origin.
	textPoint := func(raw string) (int, int, int) {
		t.Helper()
		origin := p.MeasureSplitShell(p.width, p.height).TranscriptArea
		for row, line := range strings.Split(ansi.Strip(raw), "\n") {
			if start := strings.Index(line, "first user"); start >= 0 {
				return origin.X + ansi.StringWidth(line[:start]), origin.Y + row, row
			}
		}
		t.Fatal("visible user text not found")
		return 0, 0, 0
	}
	x, y, _ := textPoint(p.messages.View())
	p.messages.FocusAt(x, y)
	focused := assertCurrentTranscriptPresentation(t, p)
	focusedRaw := p.messages.View()
	x, y, row := textPoint(focusedRaw)
	_, _ = p.messages.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	_, _ = p.messages.Update(tea.MouseMotionMsg{X: x + len("first user"), Y: y, Button: tea.MouseLeft})
	selectedRaw := p.messages.View()
	require.True(t, p.messages.IsSelecting())
	require.NotEqual(t, strings.Split(focusedRaw, "\n")[row], strings.Split(selectedRaw, "\n")[row], "actual text cells must be highlighted before presentation wrapping")
	require.Contains(t, ansi.Strip(strings.Split(selectedRaw, "\n")[row]), "first user")
	selected := assertCurrentTranscriptPresentation(t, p)
	require.NotEqual(t, focused, selected, "live text-selection styling must reach the wrapper")
	p.ClearPresentationSelection()
	assertCurrentTranscriptPresentation(t, p)
	for _, size := range [][2]int{{40, 10}, {12, 3}, {0, 0}, {80, 20}} {
		t.Run(fmt.Sprintf("split-%dx%d", size[0], size[1]), func(t *testing.T) {
			p.SetSplitPresentation(&SplitPresentationGeometry{Transcript: PresentationRect{20, 10, size[0], size[1]}})
			before := assertCurrentTranscriptPresentation(t, p)
			p.SetPresentationVisible(false)
			p.SetPresentationVisible(true)
			require.Equal(t, before, assertCurrentTranscriptPresentation(t, p))
		})
	}
	p.SetSplitPresentation(nil)
	p.SetSize(100, 25)
	assertCurrentTranscriptPresentation(t, p)
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	theme := *original
	theme.Colors.TextMuted = "#654321"
	styles.ApplyTheme(&theme)
	_, _ = p.Update(msgtypes.ThemeChangedMsg{})
	assertCurrentTranscriptPresentation(t, p)
}
