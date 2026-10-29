package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

// gitHeaderRow returns the root frame row that paints the Files - Git header, as raw ANSI.
func gitHeaderRow(t *testing.T, h *reviewHarness) (int, string) {
	t.Helper()
	h.m.viewCacheValid = false
	for y, line := range strings.Split(h.m.composeView().Content, "\n") {
		if strings.Contains(ansi.Strip(line), "Files - Git") {
			return y, line
		}
	}
	t.Fatal("Files - Git header not rendered")
	return -1, ""
}

// gitGlyphX is the disclosure glyph column, a header target outside the Files/Git mode labels.
func gitGlyphX(line string) int {
	plain := ansi.Strip(line)
	return ansi.StringWidth(plain[:strings.LastIndexAny(plain, "›⌄")])
}

// Real pointer routing: header click expands and collapses, pointer enters the header,
// then leaves into chat; the rendered header returns to the exact muted collapsed row with keyboard focus kept.
func TestGitHeaderPointerExpandCollapseLeaveRestsMutedWithFocus(t *testing.T) {
	h := newReviewHarness(t, 220)
	useUnscannedSidebarProvider(h)
	withRealPage(t, h)
	h.pump(h.m.syncGitReview())
	page, ok := h.m.gitPage("s1")
	require.True(t, ok)
	h.pump(page.SetGitFocused(true))
	h.settle()
	require.True(t, page.GitFocused())

	clickHeader := func() {
		y, line := gitHeaderRow(t, h)
		x := gitGlyphX(line)
		_, cmd := h.m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
		h.pump(cmd)
		_, cmd = h.m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
		h.pump(cmd)
		h.settle()
	}
	clickHeader()
	require.True(t, page.GitWatchState().Open, "header click expands the section")
	clickHeader()
	require.False(t, page.GitWatchState().Open, "header click collapses the section")
	require.True(t, page.GitFocused(), "keyboard focus is retained across the clicks")

	y, rest := gitHeaderRow(t, h)
	require.NotContains(t, rest, styles.HighlightWhiteStyle.Render("Files - Git"), "retained focus must not bold the header")

	_, cmd := h.m.Update(tea.MouseMotionMsg{X: gitGlyphX(rest), Y: y})
	h.pump(cmd)
	h.settle()
	_, hovered := gitHeaderRow(t, h)
	require.NotEqual(t, rest, hovered, "pointer entry brightens the header through the shared ramp")

	_, cmd = h.m.Update(tea.MouseMotionMsg{X: 2, Y: 2})
	h.pump(cmd)
	h.settle()
	_, left := gitHeaderRow(t, h)
	assert.Equal(t, rest, left, "pointer leave restores the exact muted collapsed header row")
	assert.NotContains(t, left, styles.HighlightWhiteStyle.Render("Files - Git"))
	assert.True(t, page.GitFocused(), "keyboard focus survives the pointer leave")
}
