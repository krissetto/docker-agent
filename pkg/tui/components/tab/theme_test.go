package tab

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestSectionThemeSwitchKeepsGeometry(t *testing.T) { //nolint:paralleltest // theme globals
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	before := Render("Title", "Body", 40)
	for _, ref := range []string{"default-light", "nord", "default"} {
		theme, err := styles.LoadTheme(ref)
		require.NoError(t, err)
		styles.ApplyTheme(theme)
		got := Render("Title", "Body", 40)
		assert.Equal(t, ansi.Strip(before), ansi.Strip(got))
		assert.Equal(t, lipgloss.Width(before), lipgloss.Width(got))
		assert.Contains(t, got, styles.TabTitleStyle.PaddingRight(1).Render("Title"))
	}
}
