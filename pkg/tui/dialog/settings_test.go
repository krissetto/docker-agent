package dialog

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func newTestSettingsDialog(t *testing.T, layout messages.LayoutSettings) *settingsDialog {
	t.Helper()
	d, ok := NewSettingsDialog(messages.Preferences{Layout: layout, SendMode: messages.SendModeSteer, SplitDiffView: true, RenderImages: true, ShowBanner: true, TabTitleMaxLength: 20, SoundThreshold: 10}, true).(*settingsDialog)
	require.True(t, ok)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})
	return d
}

func TestSettingsDialogNormalizesValues(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	assert.Equal(t, messages.SidebarRight, d.current.Layout.SidebarPosition)
	assert.Equal(t, messages.InfoModeCompact, d.current.Layout.SidebarInfoMode,
		"empty info mode normalizes to compact")

	raw, ok := NewSettingsDialog(messages.Preferences{
		SendMode: messages.SendMode("bogus"),
		Layout:   messages.LayoutSettings{SidebarInfoMode: messages.SidebarInfoMode("bogus")},
	}, true).(*settingsDialog)
	require.True(t, ok)
	assert.Equal(t, messages.SendModeQueue, raw.current.SendMode, "unknown send mode normalizes to queue")
	assert.Equal(t, messages.InfoModeCompact, raw.current.Layout.SidebarInfoMode,
		"unknown info mode normalizes to compact")
}

func TestSettingsDialogNavigation(t *testing.T) {
	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	for row := 1; row < appearanceRowCount; row++ {
		d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		require.Equal(t, row, d.selected[d.tab])
	}
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	assert.Equal(t, settingsActions, d.focus)
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	assert.Equal(t, settingsActions, d.focus)
	d.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	assert.Equal(t, settingsControls, d.focus)
}

func TestSettingsDialogTabSwitching(t *testing.T) {
	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	require.Equal(t, settingsCategories, d.focus)
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	require.Equal(t, tabBehavior, d.tab)
	d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, settingsControls, d.focus)
	for row := 1; row < behaviorRowCount; row++ {
		d.Update(tea.KeyPressMsg{Code: tea.KeyTab})
		require.Equal(t, row, d.selected[d.tab])
	}
	d.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Equal(t, settingsActions, d.focus)
	d.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Equal(t, settingsCategories, d.focus)
	d.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	require.Equal(t, tabAppearance, d.tab)
	d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, rowTheme, d.selected[d.tab])
}

func TestSettingsDialogWithoutVisualsTab(t *testing.T) {
	t.Parallel()

	d, ok := NewSettingsDialog(messages.Preferences{SendMode: messages.SendModeSteer}, false).(*settingsDialog)
	require.True(t, ok)
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	require.Equal(t, tabAppearance, d.tab)

	view := ansi.Strip(d.View())
	assert.Contains(t, view, "Appearance")
	assert.NotContains(t, view, "Sidebar position")
	assert.NotContains(t, view, "Sidebar info mode")
	assert.NotContains(t, view, "Active agents only")
	assert.Contains(t, view, "Split diff view")

	assert.False(t, d.selectable(tabAppearance, rowInfoMode),
		"the info mode row is not selectable without a sidebar")
	assert.False(t, d.selectable(tabAppearance, rowActiveAgents),
		"the agent filter row is not selectable without a sidebar")
}

func TestSettingsDialogCyclesPositionAndPreviews(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.selected[tabAppearance] = rowPosition

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	require.NotNil(t, cmd)
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 1)
	preview, ok := msgs[0].(messages.PreviewLayoutMsg)
	require.True(t, ok, "changing a value must emit a live preview")
	assert.Equal(t, messages.SidebarLeft, preview.Layout.SidebarPosition)

	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	assert.Equal(t, messages.SidebarTop, d.current.Layout.SidebarPosition)

	// Cycling backwards from the start wraps around.
	d.current.Layout.SidebarPosition = messages.SidebarRight
	d.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	assert.Equal(t, messages.SidebarBottom, d.current.Layout.SidebarPosition)
}

func TestSettingsDialogCyclesSpacingAndPreviews(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	require.Equal(t, messages.SpacingNormal, d.current.Layout.SectionSpacing,
		"empty spacing normalizes to normal")
	d.selected[tabAppearance] = rowSpacing

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	require.NotNil(t, cmd)
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 1)
	preview, ok := msgs[0].(messages.PreviewLayoutMsg)
	require.True(t, ok, "changing the spacing must emit a live preview")
	assert.Equal(t, messages.SpacingRelaxed, preview.Layout.SectionSpacing)

	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	assert.Equal(t, messages.SpacingCompact, d.current.Layout.SectionSpacing, "cycling wraps around")

	d.current.Layout.SectionSpacing = messages.SpacingNormal
	d.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	assert.Equal(t, messages.SpacingCompact, d.current.Layout.SectionSpacing)
}

func TestSettingsDialogCyclesInfoModeAndPreviews(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	require.Equal(t, messages.InfoModeCompact, d.current.Layout.SidebarInfoMode,
		"the info mode starts at the compact default")
	d.selected[tabAppearance] = rowInfoMode

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	require.NotNil(t, cmd)
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 1)
	preview, ok := msgs[0].(messages.PreviewLayoutMsg)
	require.True(t, ok, "changing the info mode must emit a live preview")
	assert.Equal(t, messages.InfoModeDetailed, preview.Layout.SidebarInfoMode)

	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	assert.Equal(t, messages.InfoModeCompact, d.current.Layout.SidebarInfoMode, "cycling wraps around")

	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	assert.Equal(t, messages.InfoModeDetailed, d.current.Layout.SidebarInfoMode, "cycling backwards wraps around")
	msgs = collectMsgs(cmd)
	require.Len(t, msgs, 1)
	_, ok = msgs[0].(messages.PreviewLayoutMsg)
	assert.True(t, ok, "cycling backwards must also emit a live preview")
}

func TestSettingsDialogAppliesInfoMode(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.selected[tabAppearance] = rowInfoMode
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})

	_, cmd := d.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 2, "apply must close the dialog and emit the settings")
	applied, ok := msgs[1].(messages.ApplySettingsMsg)
	require.True(t, ok)
	assert.Equal(t, messages.InfoModeDetailed, applied.Preferences.Layout.SidebarInfoMode)
}

func TestSettingsDialogEscapeRestoresInfoMode(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.selected[tabAppearance] = rowInfoMode
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	require.Equal(t, messages.InfoModeDetailed, d.current.Layout.SidebarInfoMode)

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 2)
	cancel, ok := msgs[1].(messages.CancelLayoutPreviewMsg)
	require.True(t, ok, "esc after an info mode change must restore the original layout")
	assert.Equal(t, messages.InfoModeCompact, cancel.Original.SidebarInfoMode)
}

func TestSettingsDialogTogglesSection(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.selected[tabAppearance] = rowUsage

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 1)
	preview, ok := msgs[0].(messages.PreviewLayoutMsg)
	require.True(t, ok)
	assert.True(t, preview.Layout.HideUsage, "space must hide the usage section")

	d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	assert.False(t, d.current.Layout.HideUsage, "space must toggle back")
}

func TestSettingsDialogTogglesActiveAgentsOnly(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.selected[tabAppearance] = rowActiveAgents

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 1)
	preview, ok := msgs[0].(messages.PreviewLayoutMsg)
	require.True(t, ok, "toggling the agent filter must emit a live preview")
	assert.True(t, preview.Layout.ActiveAgentsOnly, "space must enable the filter")

	d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	assert.False(t, d.current.Layout.ActiveAgentsOnly, "space must toggle back")

	assert.Contains(t, ansi.Strip(d.View()), "Active agents only")
}

func TestSettingsDialogActiveAgentsRowDisabledWhenAgentsHidden(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{HideAgents: true, ActiveAgentsOnly: true})
	assert.False(t, d.selectable(tabAppearance, rowActiveAgents),
		"the nested row is not selectable while Agents is hidden")

	d.selected[tabAppearance] = rowAgents
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	assert.Equal(t, rowTools, d.selected[tabAppearance], "navigation skips the disabled nested row")

	assert.Contains(t, ansi.Strip(d.View()), "Active agents only",
		"the disabled row still renders (muted)")

	// Re-showing Agents makes the nested row selectable again.
	d.selected[tabAppearance] = rowAgents
	d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	require.False(t, d.current.Layout.HideAgents)
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	assert.Equal(t, rowActiveAgents, d.selected[tabAppearance])
}

func TestSettingsDialogTogglesSessionPath(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.selected[tabAppearance] = rowSessionPath

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 1)
	preview, ok := msgs[0].(messages.PreviewLayoutMsg)
	require.True(t, ok, "toggling the session path must emit a live preview")
	assert.True(t, preview.Layout.HideSessionPath, "space must hide the session path")

	d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	assert.False(t, d.current.Layout.HideSessionPath, "space must toggle back")
}

func TestSettingsDialogTogglesSendModeWithoutPreview(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.selectTab(tabBehavior)
	d.prepareBody()
	require.Equal(t, tabBehavior, d.tab)
	require.Equal(t, messages.SendModeSteer, d.current.SendMode)

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	assert.Nil(t, cmd, "the send mode has no live preview")
	assert.Equal(t, messages.SendModeQueue, d.current.SendMode)

	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	assert.Equal(t, messages.SendModeSteer, d.current.SendMode, "cycling wraps around")
}

func TestSettingsDialogTogglesCacheStablePrompts(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.tab = tabBehavior
	d.selected[tabBehavior] = rowCacheStablePrompts

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	assert.Nil(t, cmd)
	assert.True(t, d.current.CacheStablePrompts)
	assert.Contains(t, ansi.Strip(d.View()), "Cache-stable dynamic prompts")
}

func TestSettingsDialogTogglesShowBanner(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.selected[tabAppearance] = rowShowBanner
	d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	require.False(t, d.current.ShowBanner)
	assert.Contains(t, ansi.Strip(d.View()), "Show startup banner")

	_, cmd := d.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 2, "apply must close the dialog and emit the settings")
	applied, ok := msgs[1].(messages.ApplySettingsMsg)
	require.True(t, ok)
	assert.False(t, applied.Preferences.ShowBanner)
}

func TestSettingsDialogApplyEmitsApplySettings(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.selected[tabAppearance] = rowTools
	d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	d.selectTab(tabBehavior)
	d.prepareBody()
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})

	_, cmd := d.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 2, "apply must close the dialog and emit the settings")
	_, ok := msgs[0].(CloseDialogMsg)
	require.True(t, ok)
	applied, ok := msgs[1].(messages.ApplySettingsMsg)
	require.True(t, ok)
	assert.True(t, applied.Preferences.Layout.HideTools)
	assert.Equal(t, messages.SendModeQueue, applied.Preferences.SendMode)
}

func TestSettingsDialogApplySendModeChangeOnly(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.selectTab(tabBehavior)
	d.prepareBody()
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})

	_, cmd := d.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 2)
	applied, ok := msgs[1].(messages.ApplySettingsMsg)
	require.True(t, ok, "a send-mode-only change must still be applied")
	assert.Equal(t, messages.SendModeQueue, applied.Preferences.SendMode)
}

func TestSettingsDialogApplyWithoutChangesOnlyCloses(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.selected[tabAppearance] = rowPosition

	_, cmd := d.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 1)
	_, ok := msgs[0].(CloseDialogMsg)
	assert.True(t, ok, "no changes: apply just closes")
}

func TestSettingsDialogEscapeRestoresOriginal(t *testing.T) {
	t.Parallel()

	original := messages.LayoutSettings{SidebarPosition: messages.SidebarLeft, SectionSpacing: messages.SpacingNormal, SidebarInfoMode: messages.InfoModeCompact}
	d := newTestSettingsDialog(t, original)
	d.selected[tabAppearance] = rowPosition
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 2)
	_, ok := msgs[0].(CloseDialogMsg)
	require.True(t, ok)
	cancel, ok := msgs[1].(messages.CancelLayoutPreviewMsg)
	require.True(t, ok, "esc after a change must restore the original layout")
	assert.Equal(t, original, cancel.Original)
}

func TestSettingsDialogEscapeWithSendModeChangeOnlyCloses(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.selectTab(tabBehavior)
	d.prepareBody()
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 1, "the send mode never previews, so there is nothing to roll back")
	_, ok := msgs[0].(CloseDialogMsg)
	assert.True(t, ok)
}

func TestSettingsDialogViewShowsVisualsRows(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	view := ansi.Strip(d.View())

	assert.Contains(t, view, "Settings")
	assert.Contains(t, view, "Appearance")
	assert.Contains(t, view, "Behavior")
	assert.Contains(t, view, "Sidebar position")
	assert.Contains(t, view, "Right")
	assert.Contains(t, view, "Section spacing")
	assert.Contains(t, view, "Normal")
	assert.Contains(t, view, "Sidebar info mode")
	assert.Contains(t, view, "‹ Compact ›", "the info mode selector shows the compact default")
	assert.Contains(t, view, "Session path")
	assert.Contains(t, view, "Token usage")
	assert.Contains(t, view, "Agents")
	assert.Contains(t, view, "Active agents only")
	assert.Contains(t, view, "Tools")
	assert.Contains(t, view, "Todos")
}

func TestSettingsDialogViewShowsBehaviorRows(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.selectTab(tabBehavior)
	d.prepareBody()
	view := ansi.Strip(d.View())

	assert.Contains(t, view, "While agent is working")
	assert.Contains(t, view, "‹ Steer ›", "single enum shows the current mode")
	assert.NotContains(t, view, "○")
	assert.Contains(t, view, "mid-turn")
	assert.NotContains(t, view, "Hold until the current turn ends")
	assert.NotContains(t, view, "Sidebar position", "behavior tab must not render visuals rows")

	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	view = ansi.Strip(d.View())
	assert.Contains(t, view, "‹ Queue ›")
	assert.Contains(t, view, "Hold until the current turn ends.")
}

func TestStepValueClamps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                     string
		current, delta, expected int
	}{
		{name: "increments", current: 10, delta: 1, expected: 12},
		{name: "decrements", current: 10, delta: -1, expected: 8},
		{name: "minimum", current: 1, delta: -1, expected: 1},
		{name: "maximum", current: 20, delta: 1, expected: 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, stepValue(tt.current, tt.delta, 2, 1, 20))
		})
	}
}

func TestSettingsDialogThemeRowOpensPicker(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	_, ok := cmd().(messages.OpenThemePickerMsg)
	assert.True(t, ok)
}

func TestSettingsDialogConfirmsYOLOBeforeEnabling(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.tab = tabBehavior
	d.selected[tabBehavior] = rowYOLO
	d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	assert.False(t, d.current.YOLO)
	assert.True(t, d.confirmYOLO)
	d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	assert.True(t, d.current.YOLO)
	assert.False(t, d.confirmYOLO)
}

func TestSettingsDialogSoundThresholdDisabledWhenSoundOff(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.tab = tabNotifications
	d.moveSelection(1)
	assert.Equal(t, rowWarnOnCacheMiss, d.selected[tabNotifications], "disabled threshold is skipped")
	d.selected[tabNotifications] = rowSound
	d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	d.moveSelection(1)
	assert.Equal(t, rowSoundThreshold, d.selected[tabNotifications])
}

func TestSettingsDialogTogglesCacheMissWarning(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.tab = tabNotifications
	d.selected[tabNotifications] = rowWarnOnCacheMiss
	d.Update(tea.KeyPressMsg{Code: tea.KeySpace})

	assert.True(t, d.current.WarnOnCacheMiss)
	assert.Contains(t, ansi.Strip(d.View()), "Warn when a turn misses the cache")
}

func TestRenderLayoutPreviewReflectsSections(t *testing.T) {
	t.Parallel()

	full := ansi.Strip(renderLayoutPreview(messages.LayoutSettings{}, previewMaxWidth))
	assert.Contains(t, full, "chat")
	assert.Contains(t, full, "input")
	assert.Contains(t, full, "session/path", "a visible session path shows in the session label")
	assert.Contains(t, full, "usage")
	assert.Contains(t, full, "todos")

	trimmed := ansi.Strip(renderLayoutPreview(messages.LayoutSettings{
		HideSessionPath: true,
		HideUsage:       true,
		HideTodos:       true,
	}, previewMaxWidth))
	assert.NotContains(t, trimmed, "session/path", "a hidden session path shortens the session label")
	assert.Contains(t, trimmed, "session")
	assert.NotContains(t, trimmed, "usage")
	assert.NotContains(t, trimmed, "todos")
	assert.Contains(t, trimmed, "agents")
}

func TestRenderLayoutPreviewPositions(t *testing.T) {
	t.Parallel()

	for _, position := range sidebarPositions {
		preview := ansi.Strip(renderLayoutPreview(messages.LayoutSettings{SidebarPosition: position}, previewMaxWidth))
		assert.Contains(t, preview, "chat", "position %s", position)
		assert.Contains(t, preview, "session", "position %s", position)
	}

	// Band layouts list the sections on a single line; the full label is
	// wider than the band and gets truncated at its tail.
	band := ansi.Strip(renderLayoutPreview(messages.LayoutSettings{SidebarPosition: messages.SidebarTop}, previewMaxWidth))
	assert.Contains(t, band, "session/path · usage · agents · tools")

	hidden := ansi.Strip(renderLayoutPreview(messages.LayoutSettings{
		SidebarPosition: messages.SidebarTop,
		HideSessionPath: true,
	}, previewMaxWidth))
	assert.Contains(t, hidden, "session · usage · agents · tools · todos")

	// Narrow widths truncate the band list instead of overflowing.
	narrow := ansi.Strip(renderLayoutPreview(messages.LayoutSettings{SidebarPosition: messages.SidebarTop}, previewMinWidth))
	assert.Contains(t, narrow, "session")
	assert.LessOrEqual(t, lipgloss.Height(narrow), 5)
	assert.LessOrEqual(t, lipgloss.Width(narrow), previewMinWidth)
}

func TestSettingsSmallViewportSelectionAndApplyMouseAction(t *testing.T) {
	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.SetSize(40, 12)
	d.View()
	for range appearanceRowCount {
		d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		d.View()
	}
	require.Equal(t, rowTodos, d.selected[d.tab])
	height := d.bodyHeight
	line := d.rowLines[rowTodos] - d.BodyScrollOffset()
	require.GreaterOrEqual(t, line, 0)
	require.Less(t, line, height, "last selected setting is visible, not clipped")
	footerKey := tea.KeyPressMsg{}
	view := d.View()
	row, col := d.Position()
	dl := NewDialogLayout(view, row, col)
	for y := row; y < row+dl.Height; y++ {
		for x := col; x < col+dl.Width; x++ {
			if k, hit := d.ActionKeyAt(x, y, dl); hit && k.String() == "ctrl+s" {
				footerKey = k
			}
		}
	}
	require.Equal(t, "ctrl+s", footerKey.String(), "Apply must not invoke Enter's theme selection behavior")
	d.selected[d.tab] = rowTheme
	_, cmd := d.Update(footerKey)
	require.NotNil(t, cmd)
	msgs := collectMsgs(cmd)
	require.NotEmpty(t, msgs)
	assert.IsType(t, CloseDialogMsg{}, msgs[0])
}

func TestSettingsActionsRemainCancelApply(t *testing.T) {
	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	for tab := range tabCount {
		d.tab = tab
		for row := range d.rowCount() {
			d.selected[tab] = row
			actions := d.actions()
			require.Len(t, actions, 2)
			assert.Equal(t, "Cancel", actions[0].Label)
			assert.Equal(t, "Apply", actions[1].Label)
			for _, action := range actions {
				assert.False(t, action.Default, "controls own Enter until actions are focused")
			}
		}
	}
	d.tab = tabNotifications
	d.selected[d.tab] = rowSoundThreshold
	d.current.Sound = false
	before := d.current
	d.changeValue(1)
	require.Equal(t, before, d.current, "disabled selection cannot mutate settings")
}

func TestSettingsSelectedSectionUsesColorOnly(t *testing.T) {
	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	for tab := range tabCount {
		d.tab = tab
		view := d.renderTabBar(100)
		assertToolActionsNotUnderlined(t, view)
		for _, label := range settingsTabLabels {
			assert.Contains(t, ansi.Strip(view), label)
		}
		assert.Equal(t, tab, d.tab, "render preserves the selected section")
	}
}

func TestSettingsDialogDimInactivePanesApplyAndCancel(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		prefs := messages.Preferences{DimInactivePanes: enabled}
		d := NewSettingsDialog(prefs, false).(*settingsDialog)
		d.SetSize(100, 40)
		d.selected[tabAppearance] = rowDimInactivePanes
		_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
		assert.Nil(t, cmd, "dimming is staged until Save, not a layout preview")
		assert.Equal(t, !enabled, d.current.DimInactivePanes)
		assert.Equal(t, enabled, d.original.DimInactivePanes)
		assert.Contains(t, ansi.Strip(d.View()), "Dim inactive panes")
		applied, ok := findMsg[messages.ApplySettingsMsg](collectMsgs(d.apply()))
		require.True(t, ok)
		assert.Equal(t, !enabled, applied.Preferences.DimInactivePanes)

		cancelled := collectMsgs(d.cancel())
		assert.True(t, hasMsg[CloseDialogMsg](cancelled))
		assert.False(t, hasMsg[messages.ApplySettingsMsg](cancelled))
		assert.False(t, hasMsg[messages.PreviewLayoutMsg](cancelled))
	}
}

func TestSettingsDialogDimInactivePanesMouseToggle(t *testing.T) {
	d := NewSettingsDialog(messages.Preferences{DimInactivePanes: true}, false).(*settingsDialog)
	d.SetSize(100, 40)
	x, y, _, _ := d.BodyScrollBounds()
	line, ok := d.rowLines[rowDimInactivePanes]
	require.True(t, ok)
	_, cmd := d.Update(tea.MouseClickMsg{X: x + 4, Y: y + line - d.BodyScrollOffset(), Button: tea.MouseLeft})
	assert.Nil(t, cmd)
	assert.Equal(t, rowDimInactivePanes, d.selected[tabAppearance])
	assert.False(t, d.current.DimInactivePanes)
}

func TestSettingsSharedHeaderTabsUseVisibleRowsOnly(t *testing.T) {
	d := NewSettingsDialog(messages.Preferences{}, true).(*settingsDialog)
	for _, size := range [][2]int{{100, 40}, {40, 12}, {24, 6}, {8, 3}, {100, 40}} {
		d.tab = tabAppearance
		d.SetSize(size[0], size[1])
		view := d.View()
		tabY, visible := d.headerRow(2)
		x, y, _, _ := d.BodyScrollBounds()
		if !visible {
			_, _ = d.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: x, Y: y - 1})
			require.Equal(t, tabAppearance, d.tab, "hidden tab controls cannot handle a title-row click")
			continue
		}
		row, _ := d.Position()
		lines := strings.Split(ansi.Strip(view), "\n")
		require.Contains(t, lines[tabY-row], settingsTabLabels[0])
		if size[1] >= 12 {
			titleY, titleVisible := d.headerRow(0)
			require.True(t, titleVisible)
			require.Equal(t, titleY+2, tabY)
		}
		// Click the first cell of the next tab, not a fixed historical row.
		targetX := x + len(settingsTabLabels[0]) + 3
		_, _, width, _ := d.BodyScrollBounds()
		if targetX < x+width-d.bodyScroll.ReservedCols() {
			_, _ = d.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: targetX, Y: tabY})
			require.Equal(t, tabBehavior, d.tab)
		}
	}
}

func TestSettingsStructuralLeadingGapOwnedOnlyBySharedHeader(t *testing.T) {
	for _, tab := range []int{tabAppearance, tabBehavior, tabNotifications} {
		for _, size := range [][2]int{{100, 30}, {45, 12}, {20, 6}, {100, 30}} {
			d := NewSettingsDialog(messages.Preferences{}, true).(*settingsDialog)
			d.tab = tab
			d.SetSize(size[0], size[1])
			_, _, body, _, _ := d.bodyParts()
			bodyLines := strings.Split(ansi.Strip(body), "\n")
			require.NotEmpty(t, strings.TrimSpace(bodyLines[0]), "generated body starts with content, never padded structural space")
			if tab == tabAppearance {
				require.Zero(t, d.rowLines[rowTheme], "first control hit anchor has no generated leading spacer")
			}
			view := d.View()
			row, _ := d.Position()
			lines := strings.Split(ansi.Strip(view), "\n")
			if tabY, visible := d.headerRow(2); visible {
				require.Equal(t, 1, d.bodyHeaderGap)
				require.Greater(t, d.bodyY, tabY, "wrapped category strip precedes body")

			}
			if size[1] == 30 {
				require.Contains(t, lines[d.bodyY-row], strings.TrimSpace(bodyLines[0]), "first body row is painted at the shared hit origin")
			}
		}
	}
}

func TestSettingsDialogDefaultsToQueue(t *testing.T) {
	t.Parallel()

	d := NewSettingsDialog(messages.Preferences{}, true).(*settingsDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})
	d.selectTab(tabBehavior)
	d.prepareBody()
	require.Equal(t, messages.SendModeQueue, d.current.SendMode)
	assert.Contains(t, ansi.Strip(d.View()), "‹ Queue ›")
	assert.NotContains(t, ansi.Strip(d.View()), "○")
}

func TestSettingsEnterEditsEveryEnabledControlWithoutApplying(t *testing.T) {
	for tab := range tabCount {
		d := newTestSettingsDialog(t, messages.LayoutSettings{})
		d.tab = tab
		d.current.Sound = true
		for row := range d.rowCount() {
			d.selected[tab] = row
			before := d.current
			_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			msgs := collectMsgs(cmd)
			assert.False(t, hasMsg[CloseDialogMsg](msgs), "tab %d row %d must not close", tab, row)
			assert.False(t, hasMsg[messages.ApplySettingsMsg](msgs), "tab %d row %d must not apply", tab, row)
			if !d.selectable(tab, row) {
				assert.Equal(t, before, d.current)
			}
		}
	}
}

func TestSettingsFocusOwnsSelectionAndActions(t *testing.T) {
	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.selected[d.tab] = rowShowBanner
	d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.False(t, d.current.ShowBanner)
	_, _, body, footer, _ := d.bodyParts()
	assert.Contains(t, ansi.Strip(body), "› Show startup banner")
	assert.NotContains(t, ansi.Strip(footer), "↵", "no false Enter footer default in controls")
	d.setFocus(settingsActions)
	_, _, body, footer, _ = d.bodyParts()
	assert.NotContains(t, ansi.Strip(body), "› [", "controls relinquish selection styling")
	assert.Contains(t, ansi.Strip(footer), "Apply ↵")
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	applied, ok := findMsg[messages.ApplySettingsMsg](collectMsgs(cmd))
	require.True(t, ok)
	assert.False(t, applied.Preferences.ShowBanner)

	for _, focus := range []settingsFocus{settingsControls, settingsCategories, settingsActions} {
		d := newTestSettingsDialog(t, messages.LayoutSettings{})
		d.current.Snapshot = !d.current.Snapshot
		d.setFocus(focus)
		_, cmd := d.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
		assert.True(t, hasMsg[messages.ApplySettingsMsg](collectMsgs(cmd)), "Ctrl+S is global")
	}
}

func TestSettingsStableGeometryAndWarningSlot(t *testing.T) {
	for _, size := range [][2]int{{165, 47}, {80, 24}, {40, 12}, {24, 6}} {
		d := newTestSettingsDialog(t, messages.LayoutSettings{})
		d.SetSize(size[0], size[1])
		view := d.View()
		width, height := lipgloss.Width(view), lipgloss.Height(view)
		for tab := range tabCount {
			d.selectTab(tab)
			for _, focus := range []settingsFocus{settingsCategories, settingsControls, settingsActions} {
				d.setFocus(focus)
				d.prepareBody()
				view = d.View()
				assert.Equal(t, width, lipgloss.Width(view))
				assert.LessOrEqual(t, lipgloss.Height(view), min(34, size[1]), "content-sized categories stay bounded")
				assert.LessOrEqual(t, height, size[1])
			}
		}
		d.setFocus(settingsControls)
		d.tab = tabBehavior
		d.selected[d.tab] = rowYOLO
		d.prepareBody()
		_, _, body, footer, _ := d.bodyParts()
		bodyHeight, footerHeight := lipgloss.Height(body), lipgloss.Height(footer)
		for range 3 {
			d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			_, _, body, footer, _ = d.bodyParts()
			assert.Equal(t, bodyHeight, lipgloss.Height(body), "YOLO confirmation has a stable reserved slot")
			assert.Equal(t, footerHeight, lipgloss.Height(footer))
			assert.LessOrEqual(t, lipgloss.Height(d.View()), min(34, size[1]))
		}
	}
}

func TestSettingsPageAndWheelScrollDoNotSnapToSelection(t *testing.T) {
	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.SetSize(80, 24)
	d.selected[d.tab] = rowTheme
	for _, msg := range []tea.Msg{
		tea.KeyPressMsg{Code: tea.KeyPgDown},
		tea.MouseWheelMsg{Button: tea.MouseWheelDown},
	} {
		d.bodyScroll.ScrollToTop()
		before := d.BodyScrollOffset()
		_, cmd := d.Update(msg)
		assert.Nil(t, cmd)
		assert.Greater(t, d.BodyScrollOffset(), before)
		assert.Equal(t, rowTheme, d.selected[d.tab])
		offset := d.BodyScrollOffset()
		d.View()
		assert.Equal(t, offset, d.BodyScrollOffset())
	}
	d.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	assert.Zero(t, d.BodyScrollOffset(), "explicit navigation reveals the selected row")
}

func clickSettingsHit(t *testing.T, d *settingsDialog, hit settingsHit) tea.Cmd {
	t.Helper()
	d.EnsureBodyLineVisible(hit.y)
	d.View()
	x, y, _, height := d.BodyScrollBounds()
	line := hit.y - d.BodyScrollOffset()
	require.GreaterOrEqual(t, line, 0)
	require.Less(t, line, height)
	_, cmd := d.Update(tea.MouseClickMsg{X: x + hit.x, Y: y + line, Button: tea.MouseLeft})
	return cmd
}

func TestSettingsExplicitMouseTargetsAtWrappedWidths(t *testing.T) {
	for _, width := range []int{24, 40, 80, 165} {
		for _, delta := range []int{-1, 1} {
			d := newTestSettingsDialog(t, messages.LayoutSettings{})
			d.SetSize(width, 24)
			found := false
			for _, hit := range d.rowHits {
				if hit.row == rowPosition && hit.kind == settingsAdjust && hit.delta == delta {
					msgs := collectMsgs(clickSettingsHit(t, d, hit))
					assert.True(t, hasMsg[messages.PreviewLayoutMsg](msgs))
					assert.Equal(t, cycleValue(sidebarPositions, messages.SidebarRight, delta), d.current.Layout.SidebarPosition)
					found = true
					break
				}
			}
			require.True(t, found)
		}
		d := newTestSettingsDialog(t, messages.LayoutSettings{})
		d.SetSize(width, 24)
		for _, hit := range d.rowHits {
			if hit.row == rowPosition && hit.kind == settingsFocusRow {
				assert.Nil(t, clickSettingsHit(t, d, hit))
				assert.Equal(t, messages.SidebarRight, d.current.Layout.SidebarPosition, "enum label click only focuses")
			}
			if hit.row == rowExpandThinking {
				// The last wrapped line must activate the same row as its label.
				hit.y += hit.height - 1
				assert.Nil(t, clickSettingsHit(t, d, hit))
				assert.True(t, d.current.ExpandThinking)
			}
		}
	}
}

func TestSettingsDisabledChildrenRetainValuesAndHaveNoHits(t *testing.T) {
	d := newTestSettingsDialog(t, messages.LayoutSettings{HideAgents: true, ActiveAgentsOnly: true})
	for _, hit := range d.rowHits {
		assert.NotEqual(t, rowActiveAgents, hit.row)
	}
	d.selected[d.tab] = rowActiveAgents
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	assert.Nil(t, cmd)
	assert.True(t, d.current.Layout.ActiveAgentsOnly)
	d.tab = tabNotifications
	d.current.SoundThreshold = 42
	d.selected[d.tab] = rowSoundThreshold
	d.prepareBody()
	for _, hit := range d.rowHits {
		assert.NotEqual(t, rowSoundThreshold, hit.row)
	}
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	assert.Equal(t, 42, d.current.SoundThreshold)
	_, _, body, _, _ := d.bodyParts()
	assert.Contains(t, ansi.Strip(body), "42 seconds")
	assert.Contains(t, ansi.Strip(body), "Enable sound")
}

func TestSettingsLayoutChecksAtWideNormalNarrowSizes(t *testing.T) {
	for _, size := range [][2]int{{165, 47}, {80, 24}, {30, 12}} {
		d := newTestSettingsDialog(t, messages.LayoutSettings{})
		d.SetSize(size[0], size[1])
		for tab := range tabCount {
			d.selectTab(tab)
			d.prepareBody()
			d.bodyScroll.ScrollToTop()
			view := ansi.Strip(d.View())
			assert.Contains(t, view, settingsTabLabels[tab], "active category stays visible in narrow tab strip")
			assert.Contains(t, view, "Cancel")
			assert.Contains(t, view, "Apply")
			assert.LessOrEqual(t, lipgloss.Width(view), size[0])
			assert.LessOrEqual(t, lipgloss.Height(view), size[1])
			t.Logf("Settings %dx%d / %s:\n%s", size[0], size[1], settingsTabLabels[tab], view)
		}
	}
}

func TestSettingsThemeSaveIsIndependentOfDraftCancel(t *testing.T) {
	paths.SetConfigDir(t.TempDir())
	t.Cleanup(func() { paths.SetConfigDir("") })
	require.NoError(t, styles.SaveThemeToUserConfig("nord"))
	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.current.Snapshot = true
	d.current.Layout.HideUsage = true
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	assert.True(t, hasMsg[messages.OpenThemePickerMsg](collectMsgs(cmd)))
	require.NoError(t, styles.SaveThemeToUserConfig("dracula"))
	d.Update(messages.ThemeChangedMsg{})
	_, _, body, _, _ := d.bodyParts()
	assert.Contains(t, ansi.Strip(body), "dracula")
	assert.Contains(t, ansi.Strip(body), "saves separately")
	cancelled := collectMsgs(d.cancel())
	assert.True(t, hasMsg[messages.CancelLayoutPreviewMsg](cancelled))
	assert.False(t, hasMsg[messages.ApplySettingsMsg](cancelled))
	assert.Equal(t, "dracula", styles.GetPersistedThemeRef(), "Settings cancel never rolls back the theme picker save")
}

func TestSettingsAllPreferencesSurviveApply(t *testing.T) {
	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	for tab := range tabCount {
		d.tab = tab
		for row := range d.rowCount() {
			if tab == tabAppearance && (row == rowTheme || row == rowAgents) {
				continue
			}
			d.selected[tab] = row
			d.changeValue(1)
			if tab == tabBehavior && row == rowYOLO {
				d.changeValue(1)
			}
		}
	}
	want := d.current
	applied, ok := findMsg[messages.ApplySettingsMsg](collectMsgs(d.apply()))
	require.True(t, ok)
	assert.True(t, want.Equal(applied.Preferences), "every drafted preference survives the unchanged apply payload")
	assert.True(t, applied.Preferences.YOLO)
	assert.True(t, applied.Preferences.Layout.ActiveAgentsOnly)
	assert.NotEqual(t, d.original.TabTitleMaxLength, applied.Preferences.TabTitleMaxLength)
	assert.NotEqual(t, d.original.SoundThreshold, applied.Preferences.SoundThreshold)
}

func TestSettingsAdaptiveHelpNeverCutsInstructionsAtNormalWidth(t *testing.T) {
	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.SetSize(80, 24)
	assert.Contains(t, ansi.Strip(d.View()), "Tab / ↑↓ move · Enter edit · Ctrl+S apply")
	d.setFocus(settingsCategories)
	d.prepareBody()
	assert.Contains(t, ansi.Strip(d.View()), "←/→ category · Enter open · Tab next")
}

func TestSettingsTransparentBackgroundDraftApplyCancel(t *testing.T) {
	d := NewSettingsDialog(messages.Preferences{TransparentBackground: true}, false).(*settingsDialog)
	d.SetSize(100, 40)
	d.selected[tabAppearance] = rowTransparentBackground
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	assert.Nil(t, cmd, "transparency is staged, not a live preview")
	assert.False(t, d.current.TransparentBackground)
	assert.True(t, d.original.TransparentBackground)
	assert.Contains(t, ansi.Strip(d.View()), "Transparent background")
	cancel := collectMsgs(d.cancel())
	assert.False(t, hasMsg[messages.ApplySettingsMsg](cancel))
	applied, ok := findMsg[messages.ApplySettingsMsg](collectMsgs(d.apply()))
	require.True(t, ok)
	assert.False(t, applied.Preferences.TransparentBackground)
}

func TestSettingsWrappedCategoriesAndCompactContent(t *testing.T) {
	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.SetSize(30, 12)
	for _, label := range settingsTabLabels {
		assert.Contains(t, ansi.Strip(d.View()), label)
	}
	for _, hit := range d.tabHits {
		if hit.row != tabPanel {
			continue
		}
		x, _, _, _ := d.BodyScrollBounds()
		y, ok := d.headerRow(2)
		require.True(t, ok)
		d.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: x + hit.x, Y: y + hit.y})
		assert.Equal(t, tabPanel, d.tab)
		assert.Equal(t, settingsControls, d.focus)
		break
	}
	d.SetSize(165, 47)
	assert.Less(t, lipgloss.Height(d.View()), 20, "three panel controls do not fill a giant fixed-height card")
	_, _, body, _, _ := d.bodyParts()
	assert.Equal(t, 3, strings.Count(ansi.Strip(body), "[x]"))
	assert.NotContains(t, ansi.Strip(body), "Move up")
}
