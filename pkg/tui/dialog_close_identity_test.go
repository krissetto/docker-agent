package tui

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/dialog"
)

// B20: a stale close from an already-answered confirmation must not close a
// newer confirmation that opened before the close message was delivered.
func TestStaleConfirmationCloseDoesNotCloseNewerDialog(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 120, 40)
	newConfirmation := func(id string) dialog.Dialog {
		return dialog.NewToolConfirmationDialog(root.ar, &runtime.ToolCallConfirmationEvent{
			SessionID: "s", RequestID: id,
			ToolCall:       tools.ToolCall{ID: id, Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"true"}`}},
			ToolDefinition: tools.Tool{Name: "shell"},
		}, root.sessionState)
	}
	first := newConfirmation("first")
	_, _ = root.Update(dialog.OpenDialogMsg{Model: first})
	closer, ok := first.(dialog.SemanticCloser)
	require.True(t, ok)
	answer := closer.CancelDialogCmd()

	second := newConfirmation("second")
	_, _ = root.Update(dialog.OpenDialogMsg{Model: second})
	require.Same(t, second, root.dialogMgr.TopDialog())

	for _, msg := range collectMsgs(answer) {
		_, _ = root.Update(msg)
	}
	require.False(t, root.dialogMgr.Closing(), "the newer confirmation must not start closing")
	require.Same(t, second, root.dialogMgr.TopDialog(), "the newer confirmation must stay open")
}
