package completion

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestWarmCompletionThemeSwitchPreservesLayoutAndSelection(t *testing.T) { //nolint:paralleltest // theme globals
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	m := New().(*manager)
	m.width = 60
	items := []Item{{Label: "Short", Description: "Description"}, {Label: "Another", Description: "Other description"}}
	m.Update(OpenMsg{Items: items})
	before := m.View()
	for _, ref := range []string{"default-light", "nord", "default"} {
		theme, err := styles.LoadTheme(ref)
		require.NoError(t, err)
		styles.ApplyTheme(theme)
		warm := m.View()
		fresh := New().(*manager)
		fresh.width = 60
		fresh.Update(OpenMsg{Items: items})
		assert.Equal(t, fresh.View(), warm, ref)
		assert.Equal(t, ansi.Strip(before), ansi.Strip(warm))
		assert.Equal(t, lipgloss.Width(before), lipgloss.Width(warm))
	}
}
