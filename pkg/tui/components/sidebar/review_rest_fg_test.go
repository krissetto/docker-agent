package sidebar

import (
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/review"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// restingRowsFor builds the review section with view.go resting and panel.go open.
func restingRowsFor(t *testing.T, ref string) (*model, int, int) {
	t.Helper()
	theme, err := styles.LoadTheme(ref)
	require.NoError(t, err)
	styles.ApplyTheme(theme)
	m := gitSidebar(t)
	snap := gitSnapshot(review.StateReady)
	snap.Repositories[0].Worktrees[0].Files = append(snap.Repositories[0].Worktrees[0].Files,
		review.FileChange{Path: "pkg/tui/view.go", Group: review.GroupUnstaged, Status: "M", Added: 2})
	m.SetGitChanges(GitChanges{Available: true, Snapshot: snap, Open: review.DocumentKey{WorktreeID: "w1", Path: "pkg/tui/panel.go", Group: review.GroupUnstaged}})
	m.git.expanded = map[string]bool{gitSectionKey: true, "repo:r1": true, "wt:w1": true, "grp:w1:" + strconv.Itoa(int(review.GroupUnstaged)): true}
	m.git.rebuild()
	resting, _ := strconv.Atoi(gitLine(m, "view.go"))
	open, _ := strconv.Atoi(gitLine(m, "panel.go"))
	return m, resting, open
}

// Absent review_rest_fg (legacy) keeps the whole resting row at FadeLine(.55): guides, status,
// name and stats are all faded, and the open row is untouched. Checked on a non-docker-next theme.
func TestReviewRestingRowLegacyAbsentIsWholeRowFade(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates globals.
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	legacy, err := styles.LoadTheme(styles.LegacyDefaultThemeRef)
	require.NoError(t, err)
	require.Empty(t, legacy.Colors.ReviewRestFg)
	m, resting, open := restingRowsFor(t, styles.LegacyDefaultThemeRef)
	require.Nil(t, styles.ReviewRestFg(), "legacy theme keeps the whole-row fade")

	width := m.contentWidth(false)
	indent := min(2, max(0, width-1))
	legacyRow := func(line int) string {
		return strings.Repeat(" ", indent) + m.git.renderRow(m.git.displayRows[line-1], max(1, width-indent), false, 0)
	}
	assert.Equal(t, styles.FadeLine(legacyRow(resting), .55), m.git.line(width, resting), "resting row is exactly FadeLine(.55) over the legacy row")
	assert.Equal(t, legacyRow(open), m.git.line(width, open), "open row is not faded")
}

// Present review_rest_fg mutes only the resting filename. Guides, status and stats keep their
// normal colors, the open row is unchanged, and the visible geometry is identical to the legacy row.
func TestReviewRestFgPresentMutesOnlyFilename(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates globals.
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	m, resting, _ := restingRowsFor(t, styles.DefaultThemeRef)
	require.NotNil(t, styles.ReviewRestFg())
	width := m.contentWidth(false)
	present := m.git.line(width, resting)
	assert.NotEqual(t, styles.FadeLine(present, .55), present, "no whole-row fade when review_rest_fg is present")
	assert.Equal(t, "#97918a", styles.RGBToHex(styles.ColorToRGB(styles.ReviewRestFg())))

	legacyTheme, err := styles.LoadTheme(styles.LegacyDefaultThemeRef)
	require.NoError(t, err)
	styles.ApplyTheme(legacyTheme)
	legacyM, legacyResting, _ := restingRowsFor(t, styles.LegacyDefaultThemeRef)
	legacy := legacyM.git.line(legacyM.contentWidth(false), legacyResting)
	assert.Equal(t, ansi.Strip(legacy), ansi.Strip(present), "muting never changes the visible text or geometry")
}
