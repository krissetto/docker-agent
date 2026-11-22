package dialog

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func openResizeTestDialog(t *testing.T, d Dialog, width, height int) (*manager, *dialogResizeClock) {
	t.Helper()
	scheduler := &dialogResizeClock{now: time.Unix(1, 0)}
	mgr := NewWithAnimation(animation.NewRuntimeWithScheduler(scheduler)).(*manager)
	mgr.Update(tea.WindowSizeMsg{Width: width, Height: height})
	mgr.Update(OpenDialogMsg{Model: d})
	finishDialogAnimation(t, mgr, mgr.clock)
	t.Cleanup(func() { mgr.handleCloseAll() })
	return mgr, scheduler
}

type dialogResizeClock struct {
	now  time.Time
	jump time.Duration
}

func (c *dialogResizeClock) Now() time.Time { return c.now }

func (c *dialogResizeClock) Tick(delay time.Duration, deliver func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		if c.jump != 0 {
			delay, c.jump = c.jump, 0
		}
		c.now = c.now.Add(delay)
		return deliver(c.now)
	}
}

func advanceResizeTestClock(t *testing.T, mgr *manager, scheduler *dialogResizeClock, elapsed time.Duration) {
	t.Helper()
	cmd := mgr.clock.EnsureRunning()
	require.NotNil(t, cmd)
	scheduler.jump = elapsed
	tick, ok := mgr.clock.Accept(cmd().(animation.TickMsg))
	require.True(t, ok)
	mgr.Update(tick)
}

func resizeLayerRect(layer *lipgloss.Layer) [4]int {
	return [4]int{layer.GetX(), layer.GetY(), layer.Width(), layer.Height()}
}

func requireResizeStaticLayer(t *testing.T, mgr *manager, d Dialog) {
	t.Helper()
	layer := mgr.GetLayers()[0]
	row, col := d.Position()
	require.Equal(t, col, layer.GetX())
	require.Equal(t, row, layer.GetY())
	require.Equal(t, renderClose(d.View(), false), layer.GetContent())
	require.False(t, mgr.clock.HasActive())
}

func TestDialogResizeSettingsTerminal(t *testing.T) {
	t.Parallel()
	d := NewSettingsDialog(messages.Preferences{}, true)
	mgr, scheduler := openResizeTestDialog(t, d, 90, 24)
	before := resizeLayerRect(mgr.GetLayers()[0])
	mgr.Update(tea.WindowSizeMsg{Width: 130, Height: 38})
	atStart := mgr.GetLayers()[0]
	require.Equal(t, before, resizeLayerRect(atStart))
	row, col := d.Position()
	view := d.View()
	final := [4]int{col, row, lipgloss.Width(view), lipgloss.Height(view)}
	advanceResizeTestClock(t, mgr, scheduler, 90*time.Millisecond)
	middle := mgr.GetLayers()[0]
	var half [4]int
	for i := range half {
		half[i] = before[i] + (final[i]-before[i]+1)/2
	}
	require.Equal(t, half, resizeLayerRect(middle))
	for _, layer := range []*lipgloss.Layer{atStart, middle} {
		frame := ansi.Strip(layer.GetContent())
		t.Logf("intermediate %v\n%s", resizeLayerRect(layer), frame)
		require.Contains(t, frame, "Settings")
		require.Contains(t, frame, "Cancel")
		require.Contains(t, frame, closeGlyph)
		require.True(t, strings.HasPrefix(frame, "╭"))
		require.True(t, strings.HasSuffix(frame, "╯"))
	}
	advanceResizeTestClock(t, mgr, scheduler, 90*time.Millisecond)
	requireResizeStaticLayer(t, mgr, d)
	t.Logf("final %v\n%s", resizeLayerRect(mgr.GetLayers()[0]), ansi.Strip(mgr.View()))
}

func resizeTestCommandPalette() Dialog {
	var categories []commands.Category
	for group := range 3 {
		category := commands.Category{Name: fmt.Sprintf("Group %d", group)}
		for item := range 8 {
			category.Commands = append(category.Commands, commands.Item{Label: fmt.Sprintf("Command %d %d", group, item), Category: category.Name})
		}
		categories = append(categories, category)
	}
	return NewCommandPaletteDialog(categories)
}

func TestDialogResizeCommandPaletteKeepsInputLive(t *testing.T) {
	t.Parallel()
	d := resizeTestCommandPalette()
	mgr, scheduler := openResizeTestDialog(t, d, 100, 42)
	before := resizeLayerRect(mgr.GetLayers()[0])
	mgr.Update(tea.PasteMsg{Content: "Command 0"})
	require.True(t, mgr.stack[0].resize.anim.Running())
	require.Equal(t, before, resizeLayerRect(mgr.GetLayers()[0]))
	advanceResizeTestClock(t, mgr, scheduler, 60*time.Millisecond)
	middle := mgr.GetLayers()[0]
	mgr.Update(tea.PasteMsg{Content: " 0"})
	require.Equal(t, resizeLayerRect(middle), resizeLayerRect(mgr.GetLayers()[0]))
	mgr.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	mgr.Update(tea.KeyPressMsg{Code: '1', Text: "1"})
	advanceResizeTestClock(t, mgr, scheduler, 90*time.Millisecond)
	layer := mgr.GetLayers()[0]
	frame := ansi.Strip(layer.GetContent())
	t.Logf("intermediate %v\n%s", resizeLayerRect(layer), frame)
	require.Contains(t, frame, "> Command 0 1")
	require.NotContains(t, frame, "Command 0 0")
	require.NotContains(t, frame, "Command 2 7")
	lines := strings.Split(frame, "\n")
	require.Contains(t, lines[len(lines)-3], "Cancel", "pills follow the moving bottom")
	advanceResizeTestClock(t, mgr, scheduler, 90*time.Millisecond)
	requireResizeStaticLayer(t, mgr, d)
	t.Logf("final %v\n%s", resizeLayerRect(mgr.GetLayers()[0]), ansi.Strip(mgr.View()))
}

func TestDialogResizeClickUsesMovingFooter(t *testing.T) {
	t.Parallel()
	d := resizeTestCommandPalette()
	mgr, scheduler := openResizeTestDialog(t, d, 100, 42)
	mgr.Update(tea.PasteMsg{Content: "Command 0 0"})
	advanceResizeTestClock(t, mgr, scheduler, 90*time.Millisecond)
	layer := mgr.GetLayers()[0]
	lines := strings.Split(ansi.Strip(layer.GetContent()), "\n")
	y := len(lines) - 3
	prefix, _, ok := strings.Cut(lines[y], "Cancel")
	require.True(t, ok)
	_, cmd := mgr.Update(tea.MouseClickMsg{X: layer.GetX() + ansi.StringWidth(prefix), Y: layer.GetY() + y, Button: tea.MouseLeft})
	require.Equal(t, []tea.Msg{CloseDialogMsg{}}, collectMsgs(cmd))
	layer = mgr.GetLayers()[0] // The click can change focus before the close message.
	mgr.Update(CloseDialogMsg{})
	require.False(t, mgr.Open(), "resizing does not delay logical dismissal")
	require.Equal(t, resizeLayerRect(layer), resizeLayerRect(mgr.GetLayers()[0]))
	require.Equal(t, layer.GetContent(), mgr.GetLayers()[0].GetContent())
	finishDialogAnimation(t, mgr, mgr.clock)
}
