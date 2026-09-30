package dialog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
)

func TestPickerResizeContentMovesWithChrome(t *testing.T) {
	for _, kind := range []string{"file", "directory", "model"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			for i := range 12 {
				require.NoError(t, os.Mkdir(filepath.Join(root, fmt.Sprintf("item%02d", i)), 0700))
			}
			var d Dialog
			var filter func(string)
			var title string
			switch kind {
			case "file":
				picker := newTestFilePickerDialog(root)
				d = picker
				filter = picker.textInput.SetValue
				title = "Attach File"
			case "directory":
				picker := NewWorkingDirPickerDialog(t.Context(), nil, nil, nil, root).(*workingDirPickerDialog)
				d = picker
				filter = picker.textInput.SetValue
				title = "Select Working Directory"
			case "model":
				var models []runtime.ModelChoice
				for i := range 12 {
					models = append(models, runtime.ModelChoice{Name: fmt.Sprintf("item%02d", i), Ref: fmt.Sprintf("item%02d", i), Provider: "test"})
				}
				picker := NewModelPickerDialog(models).(*modelPickerDialog)
				d = picker
				filter = picker.textInput.SetValue
				title = "Select Model"
			}
			r := newDialogRuntime()
			mgr := &manager{runtime: r, width: 100, height: 45}
			mgr.handleOpen(OpenDialogMsg{Model: d})
			advanceResize(mgr, dialogOpenDuration)
			t.Cleanup(mgr.Cleanup)
			for _, query := range []string{"item00", ""} {
				old := mgr.GetLayerInfos()[0]
				oldTitle := contentRow(old.Content, title) + old.Y
				oldFooter := contentRow(old.Content, "↵") + old.Y
				filter(query)
				mgr.forwardToTop(tea.PasteMsg{})
				e := mgr.stack[0]
				require.True(t, e.resizing)
				start := r.Now()
				for _, percent := range []int{0, 25, 50, 75, 100} {
					if percent > 0 {
						mgr.handleTick(advanceDialog(r, r.Continue(), start+dialogResizeDuration*time.Duration(percent)/100))
					}
					info := mgr.GetLayerInfos()[0]
					frame := ansi.Strip(info.Content)
					t.Logf("query=%q percent=%d x=%d y=%d\n%s", query, percent, info.X, info.Y, frame)
					titleY := contentRow(frame, title)
					require.GreaterOrEqual(t, titleY, 0)
					if percent == 0 {
						assert.Equal(t, oldTitle, info.Y+titleY, "existing title must begin at its displayed position")
					}
					assert.Equal(t, contentRow(ansi.Strip(d.View()), title), titleY, "title follows the intermediate card origin")
					footerY := contentRow(frame, "↵")
					require.GreaterOrEqual(t, footerY, 0, "actions remain visible throughout resizing")
					finalLines := strings.Split(ansi.Strip(d.View()), "\n")
					require.Equal(t, len(finalLines)-contentRow(d.View(), "↵"), strings.Count(frame, "\n")+1-footerY, "footer follows the moving bottom")
					if percent == 0 {
						assert.Equal(t, oldFooter, info.Y+footerY)
					}
					if query != "" {
						require.Equal(t, contentRow(d.View(), "> item00"), contentRow(frame, "> item00"), "live query follows moving header")
						targetBody := contentRow(d.View(), "item00/")
						if targetBody >= 0 {
							require.Equal(t, targetBody, contentRow(frame, "item00/"), "retained first row follows moving body")
						}
					}
					assert.True(t, strings.HasPrefix(frame, "╭"))
					assert.True(t, strings.HasSuffix(frame, "╯"))
					if query != "" {
						require.Contains(t, frame, query)
						require.NotContains(t, frame, "item11")
					}
				}
			}
		})
	}
}

func contentRow(view, text string) int {
	for i, line := range strings.Split(ansi.Strip(view), "\n") {
		if strings.Contains(line, text) {
			return i
		}
	}
	return -1
}

func TestPickerResizeInterruptedTypingPreservesMovingContent(t *testing.T) {
	r := newDialogRuntime()
	mgr := &manager{runtime: r, width: 100, height: 45}
	d := NewCommandPaletteDialog(concretePaletteCommands(3, 8)).(*commandPaletteDialog)
	mgr.handleOpen(OpenDialogMsg{Model: d})
	advanceResize(mgr, dialogOpenDuration)
	t.Cleanup(mgr.Cleanup)
	d.textInput.SetValue("Command 0")
	mgr.forwardToTop(tea.PasteMsg{})
	advanceResize(mgr, dialogResizeDuration/3)
	before := mgr.GetLayerInfos()[0]
	d.textInput.SetValue("Command 0 0")
	mgr.forwardToTop(tea.PasteMsg{})
	after := mgr.GetLayerInfos()[0]
	require.Equal(t, before.Y, after.Y)
	require.Equal(t, contentRow(before.Content, "Command Palette"), contentRow(after.Content, "Command Palette"))
	require.Contains(t, ansi.Strip(after.Content), "Command 0 0")
	require.NotContains(t, ansi.Strip(after.Content), "Command 0 1")
	advanceResize(mgr, dialogResizeDuration)
	require.Zero(t, r.ActiveCount())
}

func TestResizeCardChromeFollowsIntermediateWidth(t *testing.T) {
	r := newDialogRuntime()
	mgr := &manager{runtime: r, width: 100, height: 45}
	d := NewCommandPaletteDialog(concretePaletteCommands(1, 8))
	mgr.handleOpen(OpenDialogMsg{Model: d})
	advanceResize(mgr, dialogOpenDuration)
	t.Cleanup(mgr.Cleanup)
	mgr.Update(tea.WindowSizeMsg{Width: 120, Height: 45})
	for mgr.stack[0].anim.Running() {
		e := mgr.stack[0]
		lines := strings.Split(ansi.Strip(e.view()), "\n")
		require.True(t, strings.HasPrefix(lines[0], "╭"))
		require.True(t, strings.HasSuffix(lines[0], "╮"))
		require.True(t, strings.HasPrefix(lines[len(lines)-1], "╰"))
		require.True(t, strings.HasSuffix(lines[len(lines)-1], "╯"))
		for _, line := range lines {
			require.Equal(t, e.renderWidth, lipgloss.Width(line))
		}
		mgr.handleTick(acceptedDialogTick(r, r.Continue()))
	}
	require.Equal(t, d.View(), mgr.stack[0].view(), "settled geometry and concrete hitboxes share the exact destination")
	require.Zero(t, r.ActiveCount())
}
