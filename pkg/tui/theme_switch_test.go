package tui

import (
	"image/color"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestThemeSwitchRestylesTabEditors(t *testing.T) {
	setupAutoThemeTest(t)
	const darkRef, lightRef = "docker-dark", "white-cobalt"
	styles.ApplyThemeRef(darkRef)
	m, _, _ := frozenClockRoot(t, 120, 40)
	m.activeTab.editor.SetValue("QQ")
	background := editor.New(nil)
	background.SetValue("QQ")
	m.tabs["background"] = &tabModel{editor: background}
	m.tabs["unloaded"] = &tabModel{}

	checkEditors := func(want color.Color) {
		t.Helper()
		assertEditorForeground(t, m.activeTab.editor.View(), want, "active tab")
		assertEditorForeground(t, m.tabs["background"].editor.View(), want, "background tab")
	}
	darkForeground := styles.TextPrimary
	checkEditors(darkForeground)

	_, _ = m.Update(messages.ThemePreviewMsg{ThemeRef: lightRef})
	require.Equal(t, lightRef, styles.CurrentTheme().Ref)
	lightForeground := styles.TextPrimary
	require.NotEqual(t, darkForeground, lightForeground)
	checkEditors(lightForeground)

	_, _ = m.Update(messages.ThemeCancelPreviewMsg{OriginalRef: darkRef})
	require.Equal(t, darkRef, styles.CurrentTheme().Ref)
	checkEditors(darkForeground)

	// The editor must already use the new styles before any returned command runs.
	_, _ = m.Update(messages.ChangeThemeMsg{ThemeRef: lightRef})
	assert.Equal(t, lightRef, styles.GetPersistedThemeRef())
	checkEditors(lightForeground)
}

func assertEditorForeground(t *testing.T, view string, want color.Color, tab string) {
	t.Helper()
	found := false
	for _, line := range uv.NewStyledString(view).Lines(ansi.GraphemeWidth) {
		for _, cell := range line {
			if cell.Content == "Q" {
				found = true
				require.NotNil(t, cell.Style.Fg, "%s: missing foreground", tab)
				assert.Equal(t, color.RGBAModel.Convert(want), color.RGBAModel.Convert(cell.Style.Fg), "%s: stale foreground", tab)
			}
		}
	}
	require.True(t, found, "%s: editor text is absent", tab)
}
