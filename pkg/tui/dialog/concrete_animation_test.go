package dialog

import (
	"fmt"
	"strings"
	"testing"
	"testing/synctest"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func concretePaletteCommands(categories, each int) []commands.Category {
	result := make([]commands.Category, categories)
	for category := range categories {
		result[category].Name = fmt.Sprintf("Section %d", category)
		for item := range each {
			result[category].Commands = append(result[category].Commands, commands.Item{
				ID:          fmt.Sprintf("%d.%d", category, item),
				Label:       fmt.Sprintf("Command %d %d", category, item),
				Description: "A concrete command palette result",
				Category:    result[category].Name,
			})
		}
	}
	return result
}

func TestConcreteDialogsStayScreenCenteredThroughDynamicLifecycle(t *testing.T) {
	fixtures := []struct {
		name string
		new  func() Dialog
	}{
		{"commands", func() Dialog { return NewCommandPaletteDialog(concretePaletteCommands(3, 8)) }},
		{"settings", func() Dialog { return NewSettingsDialog(messages.Preferences{}, true) }},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runtime := animation.NewRuntime()
				mgr := &manager{runtime: runtime, width: 81, height: 31}
				_, cmd := mgr.handleOpen(OpenDialogMsg{Model: fixture.new()})
				if fixture.name == "commands" {
					require.NotNil(t, cmd, "command palette retains its cursor initialization")
				} else {
					require.Nil(t, cmd, "opening only registers animation")
				}
				tick := runtime.Continue()
				require.NotNil(t, tick)
				assert.Equal(t, int32(1), runtime.ActiveCount())

				assertConcreteRootFrame(t, mgr)
				stepConcreteDialog(t, runtime, &tick, mgr)
				assertConcreteRootFrame(t, mgr)

				finishConcreteDialog(t, runtime, &tick, mgr)
				settled := mgr.stack[0].renderHeight

				switch d := mgr.TopDialog().(type) {
				case *commandPaletteDialog:
					for i, query := range []string{"Command 0 0", "no result exists", ""} {
						d.textInput.SetValue(query)
						mgr.forwardToTop(tea.PasteMsg{})
						if i != 1 {
							require.True(t, mgr.stack[0].anim.Running(), "filter height changes retarget the shared outer transition")
						}
						if tick == nil {
							tick = runtime.Continue()
						}
						assertConcreteRootFrame(t, mgr)
						if mgr.stack[0].anim.Running() {
							stepConcreteDialog(t, runtime, &tick, mgr)
							assertConcreteRootFrame(t, mgr)
							finishConcreteDialog(t, runtime, &tick, mgr)
						}
					}
				case *settingsDialog:
					mgr.forwardToTop(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
					mgr.forwardToTop(tea.KeyPressMsg{Code: tea.KeyRight})
					mgr.forwardToTop(tea.KeyPressMsg{Code: tea.KeyEnter})
					d.selected[d.tab] = rowYOLO
					d.confirmYOLO = false
					mgr.forwardToTop(tea.KeyPressMsg{Code: tea.KeySpace})
					require.True(t, d.confirmYOLO)
					if tick == nil {
						tick = runtime.Continue()
					}
					finishConcreteDialog(t, runtime, &tick, mgr)
					assertConcreteRootFrame(t, mgr)
				}

				mgr.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
				if tick == nil {
					tick = runtime.Continue()
				}
				require.True(t, mgr.stack[0].anim.Running(), "terminal change retargets the shared outer transition")
				assertConcreteRootFrame(t, mgr)
				stepConcreteDialog(t, runtime, &tick, mgr)
				assertConcreteRootFrame(t, mgr)
				assert.NotEqual(t, settled, mgr.stack[0].targetHeight)

				mgr.handleClose()
				if tick == nil {
					tick = runtime.Continue()
				}
				assertConcreteRootFrame(t, mgr)
				stepConcreteDialog(t, runtime, &tick, mgr)
				assertConcreteRootFrame(t, mgr)
				mgr.stack[0].reopen(mgr.width, mgr.height)
				if tick == nil {
					tick = runtime.Continue()
				}
				assertConcreteRootFrame(t, mgr)
				finishConcreteDialog(t, runtime, &tick, mgr)
				mgr.handleClose()
				if tick == nil {
					tick = runtime.Continue()
				}
				finishConcreteDialog(t, runtime, &tick, mgr)
				mgr.handleTick(animation.TickMsg{})
				assert.Empty(t, mgr.stack)
				assert.Equal(t, int32(0), runtime.ActiveCount(), "cleanup leaves animation runtime quiescent")
			})
		})
	}
}

func assertConcreteRootFrame(t *testing.T, mgr *manager) {
	t.Helper()
	require.Len(t, mgr.stack, 1)
	layers := mgr.GetLayers()
	infos := mgr.GetLayerInfos()
	require.Len(t, layers, 1)
	require.Len(t, infos, 1)
	layer := layers[0]
	info := infos[0]
	assert.Equal(t, layer.GetX(), info.X, "lipgloss and flat root paths share animated X")
	assert.Equal(t, layer.GetY(), info.Y, "lipgloss and flat root paths share animated Y")
	assert.Equal(t, layer.Width(), lipgloss.Width(info.Content))
	assert.Equal(t, layer.Height(), lipgloss.Height(info.Content))
	assert.LessOrEqual(t, absInt(layer.GetX()-(mgr.width-layer.GetX()-layer.Width())), 1, "left/right margins are symmetric")
	expectedY, _ := mgr.stack[0].position(mgr.width, mgr.height)
	assert.Equal(t, max(0, expectedY), layer.GetY(), "anchored source offset determines the reveal origin")

	// Canvas.Render trims trailing whitespace; visible backdrop sentinels test actual occlusion.
	backdropRow := strings.Repeat("·", mgr.width)
	backdrop := strings.Repeat(backdropRow+"\n", mgr.height-1) + backdropRow
	root := lipgloss.NewCompositor(
		lipgloss.NewLayer(backdrop),
		lipgloss.NewLayer(info.Content).X(info.X).Y(info.Y),
	).Render()
	plain := strings.Split(ansi.Strip(root), "\n")
	for y := max(0, layer.GetY()); y < min(len(plain), layer.GetY()+layer.Height()); y++ {
		assert.Equal(t, mgr.width, lipgloss.Width(plain[y]), "decoded ANSI root preserves the full manager frame")
	}
}

func stepConcreteDialog(t *testing.T, runtime *animation.Runtime, tick *tea.Cmd, mgr *manager) {
	t.Helper()
	require.NotNil(t, *tick)
	msg, ok := (*tick)().(animation.TickMsg)
	require.True(t, ok)
	_, accepted := runtime.Accept(msg)
	require.True(t, accepted)
	mgr.handleTick(msg)
	*tick = runtime.Continue()
}

func finishConcreteDialog(t *testing.T, runtime *animation.Runtime, tick *tea.Cmd, mgr *manager) {
	t.Helper()
	for len(mgr.stack) > 0 && mgr.stack[0].anim.Running() {
		stepConcreteDialog(t, runtime, tick, mgr)
	}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
