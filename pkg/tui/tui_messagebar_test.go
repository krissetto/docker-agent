package tui

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/messagebar"
	"github.com/docker/docker-agent/pkg/tui/dialog"
)

func TestRootMessageBarStableBlankRowGeometryAndFocus(t *testing.T) {
	for _, lean := range []bool{false, true} {
		root, _, _ := wallClockRoot(t, 120, 40)
		root.leanMode = lean
		if lean {
			root.initSessionComponents("profile", root.application, root.application.Session())
			root.chatPage.Init()
		}
		root.messageBar = messagebar.New()
		root.resizeAll()
		root.viewCacheValid = false
		for _, width := range []int{8, 40, 120} {
			_, _ = root.Update(tea.WindowSizeMsg{Width: width, Height: 40})
			rows := strings.Split(root.View().Content, "\n")
			require.Len(t, rows, 40)
			require.Equal(t, width, ansi.StringWidth(rows[39]))
			require.Empty(t, strings.TrimSpace(ansi.Strip(rows[39])), "idle message row is the bottom margin, not version text")
			if !lean {
				require.Equal(t, regionContextUsage, root.hitTestRegion(38), "context joins message row without an extra gap")
			}
			require.Equal(t, regionMessageBar, root.hitTestRegion(39))
			require.Equal(t, 1, root.messageBar.Height())
		}
		root.focusedPanel = PanelContent
		root.switchFocus()
		require.Equal(t, PanelEditor, root.focusedPanel, "no actions retain ordinary focus cycle")
		var calls atomic.Int32
		action := func() tea.Msg { calls.Add(1); return nil }
		_, _ = root.Update(messagebar.SetMessageMsg{Text: "Future notice", Actions: []messagebar.Action{{Label: "Do", Command: action}}})
		root.focusedPanel = PanelContent
		root.switchFocus()
		require.Equal(t, PanelMessageBar, root.focusedPanel)
		require.True(t, root.messageBar.Focused())
		_, helpCmd := root.Update(tea.KeyPressMsg{Code: 'h', Mod: tea.ModCtrl})
		_, help := firstOfType[dialog.OpenDialogMsg](collectMsgs(helpCmd))
		require.True(t, help, "message strip does not steal global help")
		_, cmd := root.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		_ = collectMsgs(cmd)
		require.Equal(t, int32(1), calls.Load())
		_, _ = root.Update(tea.KeyPressMsg{Code: tea.KeyTab})
		require.Equal(t, PanelEditor, root.focusedPanel)
		require.False(t, root.messageBar.Focused())
		_, _ = root.Update(messagebar.ClearMessageMsg{})
		require.False(t, root.messageBar.HasActions())
		require.NotContains(t, ansi.Strip(root.View().Content), "Future notice")
	}
}

func TestActualProgramMessageBarClickActionAndIdle(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	root.messageBar = messagebar.New()
	root.resizeAll()
	root.editor.SetValue("DRAFT-PRESERVED")
	root.editor.Blur()
	var calls atomic.Int32
	action := func() tea.Msg { calls.Add(1); return messagebar.ClearMessageMsg{} }
	_, _ = root.Update(messagebar.SetMessageMsg{Text: "Notice", Actions: []messagebar.Action{{Label: "Act", Command: action}}})
	writer := &cacheProgramWriter{}
	model := &sidebarHoverProgram{root: root}
	program := startTestProgram(t, root, model, tea.WithOutput(writer))
	require.Eventually(t, func() bool { return len(model.snapshot()) > 0 }, time.Second, time.Millisecond)
	frames := model.snapshot()
	rows := strings.Split(frames[len(frames)-1].content, "\n")
	row := ansi.Strip(rows[len(rows)-1])
	before, _, found := strings.Cut(row, "Act")
	require.True(t, found)
	program.Send(tea.MouseClickMsg{X: ansi.StringWidth(before), Y: len(rows) - 1, Button: tea.MouseLeft})
	require.Eventually(t, func() bool { return calls.Load() == 1 }, time.Second, time.Millisecond)
	require.Eventually(t, func() bool {
		f := model.snapshot()
		return !strings.Contains(ansi.Strip(f[len(f)-1].content), "Notice")
	}, time.Second, time.Millisecond)
	frames = model.snapshot()
	require.Zero(t, frames[len(frames)-1].active)
	select {
	case <-time.After(80 * time.Millisecond):
	case <-t.Context().Done():
		t.Fatal("cancelled awaiting terminal flush")
	}
	writes := len(writer.snapshot())
	require.Never(t, func() bool { return len(writer.snapshot()) != writes }, 80*time.Millisecond, time.Millisecond, "static message strip owns no idle writes")
}

func TestRootMessageBarMarginsOwnGeometryAndPointerOrigin(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	root.messageBar = messagebar.New()
	var calls atomic.Int32
	action := func() tea.Msg { calls.Add(1); return nil }
	for _, width := range []int{0, 1, 2, 3, 8, 40, 120} {
		root.handleWindowResize(width, 40)
		root.messageBar.SetMessage(messagebar.Message{Text: "Notice", Actions: []messagebar.Action{{Label: "Act", Command: action}}})
		row := root.renderMessageBar()
		require.Equal(t, width, ansi.StringWidth(row))
		require.Equal(t, messageBarWidth(width), ansi.StringWidth(root.messageBar.View()))
		for _, x := range []int{-1, width} {
			_, cmd := root.Update(tea.MouseClickMsg{X: x, Y: 39, Button: tea.MouseLeft})
			require.Nil(t, cmd, "outer cells cannot activate a component action")
		}
		if width >= 3 {
			require.True(t, strings.HasPrefix(ansi.Strip(row), " "))
			require.True(t, strings.HasSuffix(ansi.Strip(row), " "))
			_, cmd := root.Update(tea.MouseClickMsg{X: 0, Y: 39, Button: tea.MouseLeft})
			require.Nil(t, cmd)
			_, cmd = root.Update(tea.MouseClickMsg{X: width - 1, Y: 39, Button: tea.MouseLeft})
			require.Nil(t, cmd)
		}
	}
	require.Zero(t, calls.Load())
}

func TestRootContextAndMessageRowsAreAdjacentIdleAndActive(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	root.messageBar = messagebar.New()
	root.resizeAll()
	for _, text := range []string{"", "Message on final row", ""} {
		root.messageBar.SetMessage(messagebar.Message{Text: text})
		root.viewCacheValid = false
		rows := strings.Split(root.View().Content, "\n")
		_, editorHeight := root.editor.GetSize()
		contextY := root.editorTop() + editorHeight
		messageY := root.height - root.messageBarHeight()
		require.Equal(t, 38, contextY)
		require.Equal(t, 39, messageY)
		require.Equal(t, contextY+1, messageY, "no blank row exists between context and message component")
		require.Len(t, rows, 40)
		require.Contains(t, rows[contextY], "▄")
		require.Equal(t, text, strings.TrimSpace(ansi.Strip(rows[messageY])))
	}
}
