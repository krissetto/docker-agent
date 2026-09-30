package completion

import (
	uv "github.com/charmbracelet/ultraviolet"
	"strings"
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

func TestSelectedCompletionBackgroundSpansLabelDescriptionAndPadding(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	for _, ref := range []string{"default", "nord", "default-light"} {
		theme, err := styles.LoadTheme(ref)
		require.NoError(t, err)
		styles.ApplyTheme(theme)
		for _, width := range []int{24, 80, 160} {
			m := New().(*manager)
			m.SetSize(width, 24)
			m.SetEditorBottom(4)
			m.Update(OpenMsg{Items: []Item{
				{Label: "Browse files…", Description: "Open file picker", Pinned: true},
				{Label: "a-long-file-name.txt", Description: "Unselected detail"},
			}})
			box, inner, _ := m.viewport()
			left := box.GetBorderLeftSize() + box.GetPaddingLeft()
			lines := strings.Split(m.View(), "\n")
			for index := range 2 {
				cells := uv.NewStyledString(lines[index+box.GetBorderTopSize()+box.GetPaddingTop()]).Lines(ansi.GraphemeWidth)[0]
				for x := left; x < left+inner; x++ {
					if index == 0 {
						assert.Equal(t, styles.MobyBlue, cells[x].Style.Bg, "selected content cell %d has no seam", x)
					} else {
						assert.Nil(t, cells[x].Style.Bg, "unselected rows retain terminal transparency")
					}
				}
			}
		}
	}
}
