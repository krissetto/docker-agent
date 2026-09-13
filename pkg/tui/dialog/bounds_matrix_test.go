package dialog

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/help"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestTopLevelDialogRenderedBoundsMatrix(t *testing.T) {
	sizes := []struct{ w, h int }{{1, 1}, {8, 3}, {16, 8}, {24, 6}, {40, 12}, {60, 24}, {120, 40}}
	factories := []struct {
		name string
		new  func() Dialog
	}{
		{"ctrl-k", func() Dialog { return NewCommandPaletteDialog([]commands.Category{{Name: "General"}}) }},
		{"settings", func() Dialog { return NewSettingsDialog(messages.Preferences{}, true) }},
		{"settings-behavior", func() Dialog {
			d := NewSettingsDialog(messages.Preferences{}, true).(*settingsDialog)
			d.tab = tabBehavior
			return d
		}},
		{"settings-notifications", func() Dialog {
			d := NewSettingsDialog(messages.Preferences{}, true).(*settingsDialog)
			d.tab = tabNotifications
			return d
		}},
		{"theme", func() Dialog { return NewThemePickerDialog(nil, "") }},
		{"model", func() Dialog { return NewModelPickerDialog(nil) }},
		{"files", func() Dialog { return newTestFilePickerDialog(t.TempDir()) }},
		{"working-directory", func() Dialog { return NewWorkingDirPickerDialog(t.Context(), nil, nil, nil, t.TempDir()) }},
		{"sessions", func() Dialog { return NewSessionBrowserDialog(nil, "") }},
		{"plans", func() Dialog { return NewPlanBrowserDialog(plans.ListResult{}) }},
		{"plan-detail", func() Dialog { return NewPlanDetailDialog(plans.Plan{Content: strings.Repeat("body\n", 30)}) }},
		{"plan-status", func() Dialog { return newPlanStatusDialog("plan", "active", 1) }},
		{"plan-delete", func() Dialog { return newPlanDeleteConfirmDialog("plan", 1) }},
		{"plan-name", newPlanNameDialog},
		{"effort", func() Dialog { return NewEffortPickerDialog([]effort.Level{effort.Low, effort.High}, effort.Low) }},
		{"snapshots", func() Dialog { return NewSnapshotsDialog(make([]int, 30)) }},
		{"help", func() Dialog { return NewHelpDialog(help.Document{}) }},
		{"tools", func() Dialog { return NewToolsDialog(nil, nil) }},
		{"skills", func() Dialog { return NewSkillsDialog(nil) }},
		{"permissions", func() Dialog { return NewPermissionsDialog(nil, false) }},
		{"attachment", func() Dialog { return NewAttachmentPreviewDialog(nil, "Preview", strings.Repeat("line\n", 30)) }},
		{"agent", func() Dialog {
			return NewAgentDetailsDialog(runtime.AgentDetails{}, runtime.AgentConfigInfo{}, nil, nil)
		}},
		{"context", func() Dialog { return NewContextDialog(&runtime.ContextBreakdown{}) }},
		{"cost", func() Dialog { return NewCostDialog(session.New()) }},
	}
	for _, f := range factories {
		for _, size := range sizes {
			t.Run(fmt.Sprintf("%s/%dx%d", f.name, size.w, size.h), func(t *testing.T) {
				d := f.new()
				d.SetSize(size.w, size.h)
				view := d.View()
				require.LessOrEqual(t, lipgloss.Width(view), size.w)
				require.LessOrEqual(t, lipgloss.Height(view), size.h)
				row, col := d.Position()
				require.GreaterOrEqual(t, row, 0)
				require.GreaterOrEqual(t, col, 0)
				require.LessOrEqual(t, col+lipgloss.Width(view), size.w)
				require.LessOrEqual(t, row+lipgloss.Height(view), size.h)
			})
		}
	}
}

func TestSettingsWideNarrowWideRetainsSelectionAndBounds(t *testing.T) {
	d := NewSettingsDialog(messages.Preferences{}, true).(*settingsDialog)
	d.selected[d.tab] = d.rowCount() - 1
	selected := d.selected[d.tab]
	for _, size := range [][2]int{{120, 40}, {24, 6}, {120, 40}} {
		d.SetSize(size[0], size[1])
		view := d.View()
		require.LessOrEqual(t, lipgloss.Width(view), size[0])
		require.LessOrEqual(t, lipgloss.Height(view), size[1])
	}
	require.Equal(t, selected, d.selected[d.tab])
	_, _ = d.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Less(t, d.selected[d.tab], selected)
}

func TestEmptyPickerActionsCannotActivateSelection(t *testing.T) {
	fixtures := []struct {
		name string
		new  func() Dialog
	}{
		{"commands", func() Dialog { return NewCommandPaletteDialog(nil) }},
		{"theme", func() Dialog { return NewThemePickerDialog(nil, "") }},
		{"model", func() Dialog { return NewModelPickerDialog(nil) }},
		{"effort", func() Dialog { return NewEffortPickerDialog(nil, "") }},
		{"sessions", func() Dialog { return NewSessionBrowserDialog(nil, "") }},
		{"plans", func() Dialog { return NewPlanBrowserDialog(plans.ListResult{}) }},
	}
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			d := f.new()
			d.SetSize(80, 24)
			view := d.View()
			row, col := d.Position()
			dl := NewDialogLayout(view, row, col)
			actions := d.(interface {
				ActionKeyAt(x, y int, dl DialogLayout) (tea.KeyPressMsg, bool)
			})
			for y := row; y < row+dl.Height; y++ {
				for x := col; x < col+dl.Width; x++ {
					k, hit := actions.ActionKeyAt(x, y, dl)
					if hit {
						require.NotEqual(t, tea.KeyEnter, k.Code, "empty list must not advertise an actionable selection")
					}
				}
			}
		})
	}
}
