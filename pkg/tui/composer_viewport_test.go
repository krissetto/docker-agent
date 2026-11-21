package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestComposerWrappedDraftAfterTerminalResize(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 160, 24)
	draft := "first " + strings.Repeat("aaaa bbbb cccc dddd ", 6) + "END"
	root.Update(tea.PasteMsg{Content: draft})
	root.Update(tea.WindowSizeMsg{Width: 8, Height: 24})

	// The composer should paint its cursor row inside the same region that
	// receives editor input, even in the first frame after reflow.
	rows := strings.Split(root.View().Content, "\n")
	editorWidth, editorHeight := root.activeTab.editor.GetSize()
	require.Equal(t, 8, editorWidth)
	lastEditorRow := root.editorTop() + editorHeight - 1 - styles.EditorStyle.GetPaddingBottom()
	require.Greater(t, len(rows), lastEditorRow)
	require.Equal(t, regionEditor, root.hitTestRegion(lastEditorRow))
	require.Contains(t, ansi.Strip(rows[lastEditorRow]), "END")

	root.Update(messages.WheelCoalescedMsg{X: 1, Y: lastEditorRow, Delta: -100})
	require.Contains(t, ansi.Strip(root.activeTab.editor.View()), "first")
	require.Equal(t, draft, root.activeTab.editor.Value())
}
