package dialog

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
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
			d := NewPendingMessageRemovalDialog("Selected queued message", func() tea.Msg { calls++; return nil }).(*removalConfirmationDialog)
			d.SetSize(100, 30)
			view := ansi.Strip(d.View())
			require.Contains(t, view, "Cancel")
			require.Contains(t, view, "Remove queued message")
			require.NotRegexp(t, `(?i)\besc(?:ape)?\b`, view)
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
		probe := NewPendingMessageRemovalDialog("Selected queued message", nil).(*removalConfirmationDialog)
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
				d := NewPendingMessageRemovalDialog("Selected queued message", func() tea.Msg { calls++; return nil }).(*removalConfirmationDialog)
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
		d := NewPendingMessageRemovalDialog("Selected queued message", func() tea.Msg { calls++; return nil }).(*removalConfirmationDialog)
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

func TestTodoRemovalUsesSharedSafeConfirmation(t *testing.T) {
	for _, code := range []rune{tea.KeyEscape, tea.KeyEnter, 'y'} {
		calls := 0
		d := NewTodoRemovalDialog("Selected todo description", func() tea.Msg { calls++; return nil })
		d.SetSize(100, 30)
		view := ansi.Strip(d.View())
		require.Contains(t, view, "Remove todo")
		require.Contains(t, view, "Selected todo description")
		require.NotContains(t, view, "Remove this todo?")
		require.Contains(t, view, "Cancel")
		require.NotRegexp(t, `(?i)\besc(?:ape)?\b`, view)
		_, cmd := d.Update(tea.KeyPressMsg{Code: code})
		collectMsgs(cmd)
		if code == 'y' {
			require.Equal(t, 1, calls)
		} else {
			require.Zero(t, calls)
		}
	}
}

func TestRemovalConfirmationDisplaysPlainText(t *testing.T) {
	for _, newDialog := range []func(string, tea.Cmd) Dialog{NewPendingMessageRemovalDialog, NewTodoRemovalDialog} {
		text := "**literal** λ界\r\nsecond\tline\n\x1b[31mred\x1b[0m\x1b]52;c;payload\a\r\b\x00\u009b"
		d := newDialog(text, nil).(*removalConfirmationDialog)
		d.SetSize(120, 40)
		require.Equal(t, "**literal** λ界\nsecond\tline\n�[31mred�[0m�]52;c;payload�����", d.text)
		view := d.View()
		require.NotContains(t, view, "\x1b[31m")
		require.NotContains(t, view, "\x1b]52;")
		plain := ansi.Strip(view)
		require.Contains(t, plain, "**literal** λ界")
		require.Contains(t, plain, "second    line")
		require.NotContains(t, plain, "Remove this")
		_, _, body, _ := d.content()
		lines := strings.Split(ansi.Strip(body), "\n")
		require.Equal(t, "**literal** λ界", strings.TrimRight(lines[0], " "))
		require.Equal(t, "second    line", strings.TrimRight(lines[1], " "))
	}
}

func TestRemovalConfirmationLongContentScrollsWithinBounds(t *testing.T) {
	text := "first line\n" + strings.Repeat("long λ界 text ", 200) + "\nlast line"
	for _, newDialog := range []func(string, tea.Cmd) Dialog{NewPendingMessageRemovalDialog, NewTodoRemovalDialog} {
		for _, size := range [][2]int{{120, 40}, {40, 12}, {24, 8}, {16, 8}, {8, 3}, {1, 1}} {
			d := newDialog(text, nil).(*removalConfirmationDialog)
			d.SetSize(size[0], size[1])
			view := d.View()
			require.LessOrEqual(t, lipgloss.Width(view), size[0])
			require.LessOrEqual(t, lipgloss.Height(view), size[1])
			require.Equal(t, text, d.text, "the snapshot is not truncated")
			if size[0] < 24 {
				continue
			}
			require.Contains(t, ansi.Strip(view), "Cancel")
			require.NotContains(t, ansi.Strip(view), "last line")
			_, _ = d.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
			require.Positive(t, d.BodyScrollOffset())
			d.EnsureBodyLineVisible(10000)
			require.Contains(t, ansi.Strip(d.View()), "last line")
			require.Contains(t, ansi.Strip(d.View()), "Cancel")
			require.LessOrEqual(t, lipgloss.Width(d.View()), size[0])
			require.LessOrEqual(t, lipgloss.Height(d.View()), size[1])
		}
	}
}

func TestPendingRemovalWithoutText(t *testing.T) {
	for _, text := range []string{"", " \n\t"} {
		d := NewPendingMessageRemovalDialog(text, nil)
		d.SetSize(100, 30)
		require.Contains(t, ansi.Strip(d.View()), "(no text)")
	}
}
