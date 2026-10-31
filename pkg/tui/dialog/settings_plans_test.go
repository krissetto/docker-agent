package dialog

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gotest.tools/v3/golden"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

// The Plans sidebar toggle sits between Tools and Todos under Appearance. It is
// below the snapshot viewport at the usual sizes, so this test reveals the row
// first, then pins its placement, its default-off value and its mouse toggle.
func TestSettingsAppearancePlansRowScrolledIntoView(t *testing.T) {
	t.Parallel()

	for _, size := range []struct{ width, height int }{{80, 24}, {100, 40}, {140, 50}} {
		d := newTestSettingsDialog(t, messages.LayoutSettings{})
		d.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
		d.tab = tabAppearance
		d.selected[tabAppearance] = rowPlans
		d.prepareBody()
		d.revealRow()

		require.False(t, d.current.Layout.ShowPlans, "Plans is off by default")
		// Placement comes from the body row map, which does not depend on the viewport.
		require.Contains(t, d.rowLines, rowTools)
		require.Contains(t, d.rowLines, rowPlans)
		require.Contains(t, d.rowLines, rowTodos)
		assert.Less(t, d.rowLines[rowTools], d.rowLines[rowPlans], "%dx%d: Plans follows Tools", size.width, size.height)
		assert.Less(t, d.rowLines[rowPlans], d.rowLines[rowTodos], "%dx%d: Plans precedes Todos", size.width, size.height)

		// The revealed view shows the row unchecked and focused.
		view := ansi.Strip(d.View())
		lines := strings.Split(view, "\n")
		plans := settingsRowLine(t, lines, "Plans")
		assert.Contains(t, lines[plans], "[ ]", "%dx%d: Plans is unchecked", size.width, size.height)
		assert.Contains(t, lines[plans], "›", "%dx%d: the revealed row is the focused row", size.width, size.height)

		golden.Assert(t, view+"\n", fmt.Sprintf("settings-%dx%d-Appearance-plans.golden", size.width, size.height))

		var hit settingsHit
		found := false
		for _, candidate := range d.rowHits {
			if candidate.row == rowPlans && candidate.kind == settingsActivate {
				hit, found = candidate, true
				break
			}
		}
		require.True(t, found, "%dx%d: the Plans row has a mouse activation target", size.width, size.height)
		_ = collectMsgs(clickSettingsHit(t, d, hit))
		assert.True(t, d.current.Layout.ShowPlans, "%dx%d: clicking the Plans row turns it on", size.width, size.height)
	}
}

// settingsRowLine returns the index of the toggle row whose label is label.
func settingsRowLine(t *testing.T, lines []string, label string) int {
	t.Helper()
	for i, line := range lines {
		if strings.Contains(line, label) && strings.Contains(line, "[") && !strings.Contains(line, "Layout preview") {
			return i
		}
	}
	require.Failf(t, "row not visible", "%q is not in the revealed Appearance view", label)
	return -1
}
