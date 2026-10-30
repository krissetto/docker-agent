package dialog

import (
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"gotest.tools/v3/golden"

	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// TestSettingsDialogSnapshots pins every settings category (Appearance,
// Behavior, Notifications, Panel) at the sizes upstream uses. Golden files are
// regenerated only with `-update` after the diff has been reviewed.
func TestSettingsDialogSnapshots(t *testing.T) {
	paths.SetConfigDir(t.TempDir())
	t.Cleanup(func() { paths.SetConfigDir("") })
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })

	for _, themeRef := range []string{"default", "default-light"} {
		t.Run(themeRef, func(t *testing.T) {
			theme, err := styles.LoadTheme(themeRef)
			if err != nil {
				t.Fatal(err)
			}
			styles.ApplyTheme(theme)
			for _, size := range []struct{ width, height int }{{80, 24}, {100, 40}, {140, 50}} {
				d := newTestSettingsDialog(t, messages.LayoutSettings{})
				d.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
				for tab, name := range settingsTabLabels {
					d.tab = tab
					golden.Assert(t, ansi.Strip(d.View())+"\n", fmt.Sprintf("settings-%dx%d-%s.golden", size.width, size.height, name))
				}
			}
		})
	}
}
