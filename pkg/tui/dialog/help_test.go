package dialog

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/help"
)

func TestHelpSnapshotDeduplicatesActionsNotSharedKeys(t *testing.T) {
	doc := help.Document{Context: "Draft", Current: []help.Section{{ID: "editing", Title: "Editing", Entries: []help.Entry{
		{ID: "delete", Keys: []string{"backspace", "ctrl+h", "ctrl+h"}, Description: "Delete character"},
		{ID: "delete", Keys: []string{"backspace"}, Description: "Delete character"},
		{ID: "other", Keys: []string{"backspace"}, Description: "Other action", Condition: "Other context"},
	}}}}
	d := NewHelpDialog(doc).(*helpDialog)
	require.Len(t, d.document.Current[0].Entries, 2)
	assert.Equal(t, []string{"backspace", "ctrl+h"}, d.document.Current[0].Entries[0].Keys)
	doc.Current[0].Entries[0].Keys[0] = "mutated"
	doc.Current[0].Entries[0].Description = "mutated"
	assert.Equal(t, "backspace", d.document.Current[0].Entries[0].Keys[0])
	assert.Equal(t, "Delete character", d.document.Current[0].Entries[0].Description)
}

func TestHelpSemanticPagesWrappingAndTinyBounds(t *testing.T) {
	doc := help.Document{Context: "Selected 工具", Current: []help.Section{{ID: "current", Title: "Current action", Entries: []help.Entry{{
		ID: "long", Keys: []string{"ctrl+shift+right", "alt+shift+right", "alt+shift+f"},
		Description: strings.Repeat("Unicode 工具 action with long description ", 8), Condition: "Selected input only",
	}}}}, Reference: ReferenceHelp()}
	d := NewHelpDialog(doc).(*helpDialog)
	for _, width := range []int{1, 2, 5, 12, 24, 60, 110} {
		for _, page := range []int{0, 1, len(doc.Reference)} {
			d.page = page
			for _, line := range d.renderContent(width, 20) {
				assert.LessOrEqual(t, ansi.StringWidth(line), width, "width %d page %d: %q", width, page, line)
			}
		}
	}
	d.page = 0
	text := ansi.Strip(strings.Join(d.renderContent(100, 20), "\n"))
	assert.Contains(t, text, "This context")
	assert.Contains(t, text, "ctrl+shift+right / alt+shift+right / alt+shift+f")
	assert.NotContains(t, text, "Control Key Shortcuts")
	assert.NotContains(t, text, "Done filtering")
	assert.NotContains(t, text, "Dialog safety", "reference stays on a separate named page")
	for _, size := range [][2]int{{1, 1}, {5, 3}, {20, 8}, {60, 20}, {160, 55}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			mgr := New(animation.NewRuntime()).(*manager)
			defer mgr.Cleanup()
			mgr.SetSize(size[0], size[1])
			d := NewHelpDialog(doc).(*helpDialog)
			mgr.handleOpen(OpenDialogMsg{Model: d})
			view := mgr.View()
			assert.LessOrEqual(t, lipgloss.Width(view), size[0])
			assert.LessOrEqual(t, lipgloss.Height(view), size[1])
			assert.Empty(t, d.actionRows)
			assert.Zero(t, d.actionFooterHeight)
		})
	}
}

func TestHelpCategoryNavigationResetsScrollAndPreservesDismissAliases(t *testing.T) {
	d := NewHelpDialog(help.Document{Context: "Draft", Current: ReferenceHelp(), Reference: ReferenceHelp()}).(*helpDialog)
	d.SetSize(100, 25)
	d.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	require.Positive(t, d.scrollview.ScrollOffset())
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	assert.Equal(t, 1, d.page)
	assert.Zero(t, d.scrollview.ScrollOffset())
	assert.Contains(t, ansi.Strip(strings.Join(d.renderContent(80, 20), "\n")), "Reference · Dialog safety")
	d.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	assert.Zero(t, d.page)
	d.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	assert.Equal(t, len(d.document.Reference), d.page)
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	assert.Zero(t, d.page)
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModCtrl})
	assert.Zero(t, d.page, "modified editing arrows are not category aliases")
	for _, k := range []tea.KeyPressMsg{{Code: tea.KeyEnter}, {Code: 'q', Text: "q"}, {Code: tea.KeyEscape}} {
		_, cmd := d.Update(k)
		require.NotNil(t, cmd)
		assert.IsType(t, CloseDialogMsg{}, cmd())
	}
}

func TestHelpNoUnderlineOrCloseActions(t *testing.T) {
	d := NewHelpDialog(help.Document{Current: ReferenceHelp(), Reference: ReferenceHelp()}).(*helpDialog)
	d.SetSize(100, 40)
	assertToolActionsNotUnderlined(t, d.View())
	require.Empty(t, d.actions)
	require.Empty(t, d.actionRows)
	require.Zero(t, d.actionFooterHeight)
}
