package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestPaneSlashParserPreservesArgumentsAndNeverFallsThrough(t *testing.T) {
	root := splitTestRoot(t)
	parser := commands.NewParser(root.paneCommandCategory())
	for _, arg := range []string{"", "right second", "down \"会話 λ third\"", "bogus", "remove extra"} {
		cmd := parser.Parse("/panes " + arg)
		require.NotNil(t, cmd)
		msg, ok := cmd().(messages.OpenPanesMsg)
		require.True(t, ok)
		require.Equal(t, arg, msg.Arguments)
	}
}

func TestPaneSourceResolutionUsesExactIdentityOrUnambiguousNameTitle(t *testing.T) {
	root := splitTestRoot(t)
	root.sessionStates["second"].SetCurrentAgentName("reviewer")
	root.sessionStates["third"].SetCurrentAgentName("reviewer")
	for _, selector := range []string{"second", "\"会話 λ second\"", "'会話 λ second'"} {
		id, err := root.resolvePaneSource(selector)
		require.NoError(t, err)
		require.Equal(t, "second", id)
	}
	for _, selector := range []string{"reviewer", "sec", "\"unterminated", "'unterminated", ""} {
		_, err := root.resolvePaneSource(selector)
		require.Error(t, err)
	}
	label := ansi.Strip(root.paneChoiceLabel("second"))
	require.Contains(t, label, "reviewer")
	require.Contains(t, label, "会話 λ second")
	require.NotContains(t, label, "root:")
}

func TestPaneSlashDirectionsFocusRemoveSingleKeepCanonicalOwners(t *testing.T) {
	root := splitTestRoot(t)
	root.editor.SetValue("original draft")
	original := root.chatPage
	root.executePaneArguments("right second")
	require.Equal(t, []string{"profile", "second"}, root.panes.Sessions())
	root.executePaneArguments("down third")
	require.Len(t, root.panes.Sessions(), 3)
	root.executePaneArguments("next")
	require.Equal(t, "profile", root.paneFocus())
	require.Equal(t, "original draft", root.editor.Value())
	root.executePaneArguments("prev")
	require.Equal(t, "third", root.paneFocus())
	root.executePaneArguments("remove")
	require.Len(t, root.panes.Sessions(), 2)
	require.NotNil(t, root.supervisor.GetRunner("third"))
	root.executePaneArguments("single")
	require.Len(t, root.panes.Sessions(), 1)
	require.Same(t, original, root.chatPages["profile"])
	require.Equal(t, 3, root.supervisor.Count())
}

func TestPaneSlashActiveSourceAndInvalidArgumentsAreAtomic(t *testing.T) {
	root := splitTestRoot(t)
	root.executePaneArguments("left profile")
	require.Equal(t, []string{"profile", "second"}, root.panes.Sessions())
	before := root.panes.root
	root.editor.SetValue("keep")
	for _, arg := range []string{"unknown", "remove extra", "next third", "right missing", "resize 9", "resize invalid"} {
		require.NotNil(t, root.executePaneArguments(arg), "invalid syntax reports error")
		require.Same(t, before, root.panes.root)
		require.Equal(t, "keep", root.editor.Value())
	}
}

func TestPaneSlashDedicatedChooserAndResizeCancel(t *testing.T) {
	root := splitTestRoot(t)
	for _, arg := range []string{"", "right"} {
		cmd := root.executePaneArguments(arg)
		open, ok := firstOfType[dialog.OpenDialogMsg](collectMsgs(cmd))
		require.True(t, ok)
		open.Model.SetSize(120, 40)
		view := ansi.Strip(open.Model.View())
		require.Contains(t, view, "Panes")
		require.NotContains(t, view, "Type to search commands")
	}
	root.executePaneArguments("right second")
	root.executePaneArguments("down third")
	cmd := root.executePaneArguments("resize")
	_, ok := cmd().(dialog.OpenDialogMsg)
	require.True(t, ok, "nested resize requires a concise divider choice")
	before := root.panes.root
	root.executePaneArguments("resize 2")
	require.NotNil(t, root.paneGesture)
	root.handlePaneGestureKey(tea.KeyPressMsg{Code: tea.KeyRight})
	root.handlePaneGestureKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Nil(t, root.paneGesture)
	require.Same(t, before, root.panes.root)
}

func TestPaneSlashColdSourceHydratesOnlyAfterExplicitCommand(t *testing.T) {
	root, store := coldPaneRoot(t)
	root.executePaneArguments("right")
	require.Zero(t, store.reads.Load())
	require.Nil(t, root.chatPages["cold"])
	cmd := root.executePaneArguments("right cold")
	require.NotNil(t, root.paneHydration)
	root.cancelPaneGesture()
	for _, msg := range collectMsgs(cmd) {
		if result, ok := msg.(paneHydratedMsg); ok {
			root.finishPaneHydration(result)
		}
	}
	require.Len(t, root.panes.Sessions(), 1)
	require.Nil(t, root.chatPages["cold"])
}

func TestPaneArgumentCompletionUsesExactHiddenValueAndShortLabel(t *testing.T) {
	root := splitTestRoot(t)
	root.sessionStates["second"].SetCurrentAgentName("same-agent")
	root.sessionStates["third"].SetCurrentAgentName("same-agent")
	candidates := root.paneArgumentCandidatesFor("right ")
	require.NotEmpty(t, candidates)
	seen := false
	for _, candidate := range candidates {
		if candidate.Value != "right \"会話 λ second\"" {
			continue
		}
		seen = true
		require.Contains(t, candidate.Label, "same-agent")
		require.NotContains(t, candidate.Label, "root:")
		root.executePaneArguments(candidate.Value)
		require.Equal(t, "second", root.paneFocus())
	}
	require.True(t, seen)
	require.Len(t, root.paneArgumentCandidatesFor("resize "), 1)
	require.Empty(t, root.paneArgumentCandidatesFor("remove "))
}

func TestPaneChooserRejectsStaleTopologyBeforeAction(t *testing.T) {
	root := splitTestRoot(t)
	_, bounds, _ := root.measurePanes()
	root.panes = root.paneLayout()
	guard := paneChoiceGuard{root: root.panes.root, order: root.paneOrder(), focus: root.paneFocus(), bounds: bounds}
	root.splitPane("second", "profile", splitRight)
	before := root.panes.root
	root.handlePaneChoice(paneChosenMsg{guard: guard, action: messages.PaneActionMsg{Action: "single"}})
	require.Same(t, before, root.panes.root)
	require.Len(t, root.panes.Sessions(), 2)
}
