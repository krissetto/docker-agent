package dialog

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/runtime"
)

func TestReconcileInteractionsPreservesLiveDraftAndUnrelatedDialogs(t *testing.T) {
	mgr := New().(*manager)
	stale := &runtime.MaxIterationsReachedEvent{SessionID: "s", RequestID: "stale"}
	live := &runtime.MaxIterationsReachedEvent{SessionID: "s", RequestID: "live"}
	other := &runtime.MaxIterationsReachedEvent{SessionID: "other", RequestID: "stale"}
	mgr.handleOpen(OpenDialogMsg{Model: NewMaxIterationsDialog(3, "s", "stale"), OriginatingEvent: stale})
	draft := NewMaxIterationsDialog(3, "s", "live")
	mgr.handleOpen(OpenDialogMsg{Model: draft, OriginatingEvent: live})
	mgr.handleOpen(OpenDialogMsg{Model: NewToolRejectionReasonDialog("s", "live")})
	child := mgr.TopDialog()
	modal := NewExitConfirmationDialog()
	mgr.handleOpen(OpenDialogMsg{Model: modal})
	mgr.handleOpen(OpenDialogMsg{Model: NewMaxIterationsDialog(3, "other", "stale"), OriginatingEvent: other})
	head := &lifecycle.Projection{Interactions: []runtime.InteractionSnapshot{{Event: live}}}
	mgr.Update(ReconcileInteractionsMsg{SessionID: "s", Projection: head})
	assert.Len(t, mgr.stack, 4)
	assert.Same(t, draft, mgr.stack[0].dialog)
	assert.Same(t, child, mgr.stack[1].dialog)
	assert.Same(t, modal, mgr.stack[2].dialog)
	// Recreated snapshot payloads for a still-live ID must not reopen the prompt.
	mgr.handleOpen(OpenDialogMsg{Model: NewMaxIterationsDialog(3, "s", "live"), OriginatingEvent: &runtime.MaxIterationsReachedEvent{SessionID: "s", RequestID: "live"}})
	assert.Len(t, mgr.stack, 4)
	mgr.Update(ReconcileInteractionsMsg{SessionID: "s", Projection: &lifecycle.Projection{}})
	assert.Len(t, mgr.stack, 2, "vanished root and dependent rejection dialog are removed, unrelated modals remain")
	assert.Same(t, modal, mgr.stack[0].dialog)
}
