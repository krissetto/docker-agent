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
)

func TestPanelDetailsSnapshotAndUnicode(t *testing.T) {
	t.Parallel()
	input := []string{"Workspace: 世界 👩‍💻", strings.Repeat("長いパス/", 20)}
	d := NewPanelDetailsDialog("Workspace 世界", input).(*panelDetailsDialog)
	input[0] = "mutated"
	lines := d.renderLines(12, 10)
	assert.NotContains(t, strings.Join(lines, "\n"), "mutated")
	for _, line := range lines {
		assert.LessOrEqual(t, ansi.StringWidth(line), 12)
	}
	for _, size := range [][2]int{{100, 40}, {30, 12}, {15, 8}} {
		d.SetSize(size[0], size[1])
		assert.LessOrEqual(t, lipgloss.Width(d.View()), size[0])
		assert.LessOrEqual(t, lipgloss.Height(d.View()), size[1])
	}
}

func TestPanelDetailsScrollAndClose(t *testing.T) {
	t.Parallel()
	lines := make([]string, 100)
	for i := range lines {
		lines[i] = fmt.Sprintf("Todo %03d", i)
	}
	d := NewPanelDetailsDialog("Todos", lines).(*panelDetailsDialog)
	d.SetSize(80, 20)
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	assert.Nil(t, cmd)
	assert.Positive(t, d.BodyScrollOffset())
	d.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	assert.Contains(t, ansi.Strip(d.View()), "Todo 099")
	d.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	assert.Zero(t, d.BodyScrollOffset())
	x, y, _, _ := d.BodyScrollBounds()
	d.Update(tea.MouseWheelMsg{X: x, Y: y, Button: tea.MouseWheelDown})
	assert.Positive(t, d.BodyScrollOffset())
	for _, key := range []tea.KeyPressMsg{{Code: tea.KeyEscape}, {Code: tea.KeyEnter}, {Code: 'q', Text: "q"}} {
		_, cmd := d.Update(key)
		require.NotNil(t, cmd)
		require.IsType(t, CloseDialogMsg{}, cmd())
	}
	_, cmd = d.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	assert.Nil(t, cmd, "content clicks cannot mutate the snapshot or start actions")
}

func TestPanelDetailsModalMouseClose(t *testing.T) {
	runtime := newDialogRuntime()
	mgr := New(runtime).(*manager)
	mgr.SetSize(80, 24)
	d := NewPanelDetailsDialog("Workspace", []string{"Read-only workspace details"}).(*panelDetailsDialog)
	mgr.handleOpen(OpenDialogMsg{Model: d})
	mgr.handleTick(advanceDialog(runtime, runtime.Continue(), dialogOpenDuration))
	x, y, _, _ := d.BodyScrollBounds()
	_, cmd := mgr.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.Nil(t, cmd)
	assert.True(t, mgr.HasActiveDialog(), "body click is consumed without closing or triggering background actions")
	row, col := d.Position()
	viewLines := strings.Split(ansi.Strip(d.View()), "\n")
	for line, text := range viewLines {
		if at := strings.Index(text, dialogCloseGlyph); at >= 0 {
			_, cmd = mgr.Update(tea.MouseClickMsg{X: col + ansi.StringWidth(text[:at]), Y: row + line, Button: tea.MouseLeft})
			require.NotNil(t, cmd)
			require.IsType(t, CloseDialogMsg{}, cmd())
			return
		}
	}
	t.Fatal("missing modal close control")
}
