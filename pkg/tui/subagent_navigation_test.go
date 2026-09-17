package tui

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

func TestLeanRootHiddenTabsNavigateWithoutClosingSessions(t *testing.T) {
	root := splitTestRoot(t)
	root.leanMode = true
	root.editor.SetValue("first draft")
	root.Update(messages.SwitchTabMsg{SessionID: "second"})
	require.Equal(t, "second", root.paneFocus())
	root.editor.SetValue("second draft")
	root.Update(messages.SwitchTabMsg{SessionID: "third"})
	root.Update(messages.ReturnToPreviousSessionMsg{})
	require.Equal(t, "second", root.paneFocus())
	require.Equal(t, "second draft", root.editor.Value())
	root.Update(messages.ReturnToPreviousSessionMsg{})
	require.Equal(t, "profile", root.paneFocus())
	require.Equal(t, "first draft", root.editor.Value())
	require.Equal(t, 3, root.supervisor.Count())
}

func TestLeanRootRendersExistingDialogLayer(t *testing.T) {
	root := splitTestRoot(t)
	root.leanMode = true
	root.updateDialogCmd(dialog.OpenDialogMsg{Model: &stubDialog{id: "lean-dialog"}})
	require.True(t, root.dialogMgr.Open())
	view := root.View()
	require.False(t, view.AltScreen)
	for _, layer := range root.dialogMgr.GetLayerInfos() {
		require.Contains(t, ansi.Strip(view.Content), ansi.Strip(layer.Content))
	}
}

type selectionClearPage struct {
	*splitRecordingPage

	clears int
}

func (p *selectionClearPage) ClearPresentationSelection() { p.clears++ }

func TestPaneFocusClearsOnlyOutgoingSelection(t *testing.T) {
	root := splitTestRoot(t)
	page := &selectionClearPage{splitRecordingPage: &splitRecordingPage{Page: root.chatPage}}
	root.chatPage, root.chatPages["profile"] = page, page
	root.editor.SetValue("draft and cursor retained")
	root.handleSwitchTab("second")
	require.Equal(t, 1, page.clears)
	require.Zero(t, page.bottom)
	root.handleSwitchTab("profile")
	require.Equal(t, "draft and cursor retained", root.editor.Value())
	root.handleSwitchTab("profile")
	require.Equal(t, 1, page.clears, "same owner is not deselected")
}

var _ chat.SplitPresentation = (*selectionClearPage)(nil)

func TestWithSupervisorReusesInitialCanonicalRegistration(t *testing.T) {
	root := splitTestRoot(t)
	owner := supervisor.New(nil)
	application := root.application
	_, err := owner.AddSession(t.Context(), application, application.Session(), "", nil)
	require.NoError(t, err)
	generation, _ := owner.RouteGeneration(application.Session().ID)
	model := New(t.Context(), nil, application, "", func() {}, WithSupervisor(owner), WithHideSidebar()).(*appModel)
	t.Cleanup(model.cleanupManagedResources)
	require.Same(t, owner, model.supervisor)
	require.Equal(t, 1, owner.Count(), "existing initial view is never registered twice")
	after, _ := owner.RouteGeneration(application.Session().ID)
	require.Equal(t, generation, after, "injected initial observer generation is unchanged")
}
