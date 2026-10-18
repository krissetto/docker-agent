package dialog

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
)

// NewCloseWorkspaceDialog always asks before detaching a workspace's pane views.
func NewCloseWorkspaceDialog(paneCount int, confirm tea.Cmd) Dialog {
	d := newRemovalConfirmationDialog("Close workspace?", fmt.Sprintf("Close all %d panes in this workspace? Agents and subagents keep running.", paneCount), confirm).(*removalConfirmationDialog)
	d.confirmLabel = "Close workspace"
	return d
}
