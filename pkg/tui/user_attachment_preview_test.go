package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestUserAttachmentPreviewRoutesToOriginAndRejectsStaleOwnerGeneration(t *testing.T) {
	root := splitTestRoot(t)
	attachment := types.UserAttachment{Name: "inline.txt", MediaType: "text/plain", Text: "stored body", Available: true}
	root.Update(messages.OpenUserAttachmentPreviewMsg{SessionID: "second", Attachment: attachment})
	require.True(t, root.dialogMgr.Open())
	assert.Equal(t, "profile", root.paneFocus(), "preview must not retarget current focus")
	assert.Contains(t, root.dialogMgr.TopDialog().View(), "stored body")
	root.dialogMgr.Cleanup()

	generation, ok := root.supervisor.RouteGeneration("second")
	require.True(t, ok)
	root.Update(messages.RoutedMsg{SessionID: "second", RouteGeneration: generation + 1, Inner: messages.OpenUserAttachmentPreviewMsg{SessionID: "second", Attachment: attachment}})
	assert.False(t, root.dialogMgr.Open(), "stale preview route is ignored")
	root.Update(messages.OpenUserAttachmentPreviewMsg{SessionID: "gone", Attachment: attachment})
	assert.False(t, root.dialogMgr.Open(), "closed or unknown owner is ignored")
	mismatched := messages.RoutedMsg{SessionID: "second", RouteGeneration: generation, Inner: messages.OpenUserAttachmentPreviewMsg{SessionID: "profile", Attachment: attachment}}
	root.Update(mismatched)
	assert.False(t, root.dialogMgr.Open(), "payload owner must match routed owner")
	command := stampAttachmentPreviewCommand("second", generation, func() tea.Msg {
		return messages.OpenUserAttachmentPreviewMsg{SessionID: "second", Attachment: attachment}
	})
	routed, ok := command().(messages.RoutedMsg)
	require.True(t, ok)
	assert.Equal(t, generation, routed.RouteGeneration)
	assert.Equal(t, "second", routed.SessionID)

	root.Update(dialog.OpenDialogMsg{Model: dialog.NewHelpDialog(root.helpDocument())})
	root.Update(messages.OpenUserAttachmentPreviewMsg{SessionID: "second", Attachment: attachment})
	assert.Contains(t, root.dialogMgr.TopDialog().View(), "Help")
	root.dialogMgr.Cleanup()
}
