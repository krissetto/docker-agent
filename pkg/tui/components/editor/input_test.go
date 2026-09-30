package editor

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestSharedInputMatchesComposerSurface(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	for _, size := range [][2]int{{60, 12}, {20, 6}, {3, 2}, {1, 1}} {
		composer := New(nil).(*editor)
		composer.SetViewportSize(size[0], size[1])
		composer.SetSize(size[0], size[1])
		input := NewInput(InputConfig{Unlimited: true, NewlineKeys: []string{"enter", "ctrl+j", "shift+enter"}})
		input.SetSize(size[0], size[1])
		composer.SetValue("界é👩‍💻 text\nsecond line")
		input.SetValue(composer.Value())
		require.Equal(t, composer.View(), input.SurfaceView(), "same text, allocation, cursor, and normal surface")
		for _, msg := range []tea.Msg{
			tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModShift},
			tea.KeyPressMsg{Code: tea.KeyBackspace},
			tea.PasteMsg{Content: "界é\r\nnext"},
		} {
			composer.Update(msg)
			input.Update(msg)
			require.Equal(t, composer.Value(), input.Value())
			require.Equal(t, composer.View(), input.SurfaceView())
		}
		before, geometry := input.SurfaceView(), input.Layout()
		for range 3 {
			require.Equal(t, before, input.SurfaceView())
			require.Equal(t, geometry, input.Layout())
		}
		require.LessOrEqual(t, lipgloss.Width(before), size[0])
		require.LessOrEqual(t, lipgloss.Height(before), size[1])
	}
}

func TestSharedInputLiteralUnlimitedDraft(t *testing.T) {
	input := NewInput(InputConfig{Unlimited: true, NewlineKeys: []string{"enter"}})
	content := "@/tmp/example.png\n" + strings.Repeat("界é👩‍💻 text\n", 150) + "  trailing  "
	input.Update(tea.PasteMsg{Content: content})
	require.Equal(t, content, input.Value())
	input.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, content+"\n", input.Value())
	input.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt})
	require.Equal(t, content+"\n", input.Value(), "no composer send/steer policy")
}
