package tui

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/editor"
)

type disposalRecordingEditor struct {
	editor.Editor
	cleanups int
}

func (e *disposalRecordingEditor) Cleanup() {
	e.cleanups++
	e.Editor.Cleanup()
}

func TestCreateSessionComponentsDisposesEditorWithoutChangingFocus(t *testing.T) {
	root := splitTestRoot(t)
	old := &disposalRecordingEditor{Editor: root.editors["second"]}
	root.editors["second"] = old
	focusedEditor, focusedPage := root.editor, root.chatPage
	a := root.supervisor.GetRunner("second").App
	root.createSessionComponents("second", a, a.Session())
	require.Equal(t, 1, old.cleanups)
	require.NotSame(t, old, root.editors["second"])
	require.Same(t, focusedEditor, root.editor)
	require.Same(t, focusedPage, root.chatPage)
	root.disposeSessionComponents("second")
	root.disposeSessionComponents("second")
	require.Equal(t, 1, old.cleanups)
	require.NotContains(t, root.editors, "second")
	require.NotContains(t, root.chatPages, "second")
	require.NotContains(t, root.sessionStates, "second")
}
