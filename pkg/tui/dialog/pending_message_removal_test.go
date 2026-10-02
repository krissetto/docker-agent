package dialog

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
)

func TestPendingRemovalConfirmationKeyboardAndSingleResponse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		keys    []tea.KeyPressMsg
		confirm bool
	}{
		{"escape", []tea.KeyPressMsg{{Code: tea.KeyEscape}}, false},
		{"default-enter", []tea.KeyPressMsg{{Code: tea.KeyEnter}}, false},
		{"no", []tea.KeyPressMsg{{Code: 'n', Text: "n"}}, false},
		{"yes", []tea.KeyPressMsg{{Code: 'y', Text: "y"}}, true},
		{"navigate", []tea.KeyPressMsg{{Code: tea.KeyRight}, {Code: tea.KeyEnter}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			d := NewPendingMessageRemovalDialog(func() tea.Msg { calls++; return nil }).(*pendingMessageRemovalDialog)
			d.SetSize(100, 30)
			view := ansi.Strip(d.View())
			require.Contains(t, view, "Cancel")
			require.Contains(t, view, "Remove queued message")
			require.NotContains(t, strings.ToLower(view), "esc")
			for _, key := range tc.keys {
				_, cmd := d.Update(key)
				collectMsgs(cmd)
			}
			want := 0
			if tc.confirm {
				want = 1
			}
			require.Equal(t, want, calls)
			_, cmd := d.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
			require.Nil(t, cmd, "cancellation or confirmation settles once")
		})
	}
}

func TestPendingRemovalConfirmationMeasuredPillHits(t *testing.T) {
	for _, size := range [][2]int{{100, 30}, {40, 12}, {24, 8}} {
		probe := NewPendingMessageRemovalDialog(nil).(*pendingMessageRemovalDialog)
		probe.SetSize(size[0], size[1])
		view := probe.View()
		row, col := probe.Position()
		dl := NewDialogLayout(view, row, col)
		counts := map[rune]int{}
		for y := row; y < row+dl.Height; y++ {
			for x := col; x < col+dl.Width; x++ {
				key, hit := probe.ActionKeyAt(x, y, dl)
				if !hit {
					continue
				}
				counts[key.Code]++
				calls := 0
				d := NewPendingMessageRemovalDialog(func() tea.Msg { calls++; return nil }).(*pendingMessageRemovalDialog)
				d.SetSize(size[0], size[1])
				_, cmd := d.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
				collectMsgs(cmd)
				want := 0
				if key.Code == 'y' {
					want = 1
				}
				require.Equal(t, want, calls, "size=%v cell=%d,%d", size, x, y)
			}
		}
		require.Positive(t, counts['n'])
		require.Positive(t, counts['y'])
	}
}

func TestPendingRemovalOutsideAndCloseChromeOnlyCancel(t *testing.T) {
	for _, closeButton := range []bool{false, true} {
		calls := 0
		d := NewPendingMessageRemovalDialog(func() tea.Msg { calls++; return nil }).(*pendingMessageRemovalDialog)
		d.SetSize(100, 30)
		var cmd tea.Cmd
		if closeButton {
			view := d.View()
			row, col := d.Position()
			dl := NewDialogLayout(view, row, col)
			x, y, ok := closeControlCell(dl.Width, dl.Height)
			require.True(t, ok)
			_, cmd = d.Update(tea.MouseClickMsg{X: col + x, Y: row + y, Button: tea.MouseLeft})
		} else {
			mgr := &manager{runtime: animation.NewRuntime(), width: 100, height: 30}
			mgr.handleOpen(OpenDialogMsg{Model: d})
			cmd = mgr.handleOutsideClickDismiss(tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft})
			t.Cleanup(mgr.Cleanup)
		}
		require.NotNil(t, cmd)
		collectMsgs(cmd)
		require.Zero(t, calls)
		_, cmd = d.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
		require.Nil(t, cmd)
	}
}
