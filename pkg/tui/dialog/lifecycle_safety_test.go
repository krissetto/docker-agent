package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestEmptyManagerIgnoresTerminalInput(t *testing.T) {
	mgr := New(newDialogRuntime())
	for _, msg := range []tea.Msg{tea.MouseMotionMsg{}, tea.KeyPressMsg{Code: tea.KeyEscape}, tea.MouseClickMsg{}, tea.PasteMsg{Content: "draft"}} {
		_, cmd := mgr.Update(msg)
		assert.Nil(t, cmd)
	}
}

func TestInvisibleDialogDoesNotScheduleLifecycle(t *testing.T) {
	r := newDialogRuntime()
	mgr := New(r).(*manager)
	dlg := &lifecycleDialog{view: "hidden"}
	mgr.handleOpen(OpenDialogMsg{Model: dlg})
	assert.Zero(t, r.ActiveCount())
	assert.Nil(t, r.EnsureRunning())
	mgr.handleClose()
	assert.False(t, mgr.Open())
	assert.Equal(t, 1, dlg.cleaned)
}

func TestDialogResizeToInvisibleReleasesLease(t *testing.T) {
	r := newDialogRuntime()
	mgr := New(r).(*manager)
	mgr.SetSize(80, 24)
	mgr.handleOpen(OpenDialogMsg{Model: &lifecycleDialog{view: "visible"}})
	require.Equal(t, int32(1), r.ActiveCount())
	mgr.Update(tea.WindowSizeMsg{})
	assert.Zero(t, r.ActiveCount())
	assert.Nil(t, r.Continue())
	mgr.Cleanup()
}

func TestReconcileCancelsHiddenLifecycleWithoutAnswering(t *testing.T) {
	r := newDialogRuntime()
	mgr := New(r).(*manager)
	mgr.SetSize(80, 24)
	event := &runtime.MaxIterationsReachedEvent{SessionID: "session", RequestID: "request"}
	dlg := &lifecycleDialog{view: "draft"}
	mgr.handleOpen(OpenDialogMsg{Model: dlg, OriginatingEvent: event})
	mgr.handleHide()
	_, cmd := mgr.Update(ReconcileInteractionsMsg{SessionID: "session", Projection: &lifecycle.Projection{}})
	assert.Nil(t, cmd)
	assert.False(t, mgr.Open())
	assert.Zero(t, r.ActiveCount())
	assert.Equal(t, 1, dlg.cleaned)
	mgr.handleTick(animation.TickMsg{})
	mgr.Cleanup()
	assert.Equal(t, 1, dlg.cleaned)
}

func TestHiddenInteractionReopensBeforeDeduplication(t *testing.T) {
	r := newDialogRuntime()
	mgr := New(r).(*manager)
	mgr.SetSize(80, 24)
	event := &runtime.MaxIterationsReachedEvent{SessionID: "session", RequestID: "request"}
	dlg := &lifecycleDialog{view: "draft"}
	mgr.handleOpen(OpenDialogMsg{Model: dlg, OriginatingEvent: event})
	wrapper := mgr.stack[0].animatedDialog
	mgr.handleHide()
	mgr.handleOpen(OpenDialogMsg{Model: dlg, OriginatingEvent: event})
	require.Len(t, mgr.stack, 1)
	assert.Same(t, wrapper, mgr.stack[0].animatedDialog)
	assert.Same(t, dlg, mgr.TopDialog())
	assert.Equal(t, lifecycle.InteractionIdentity(event), mgr.stack[0].interactionKey)
	assert.Equal(t, int32(1), r.ActiveCount())
	mgr.Cleanup()
}

func TestSemanticCancellationRespondsOnceBeforeCloseDelivery(t *testing.T) {
	ref := ElicitationRef{SessionID: "session", RequestID: "request", ElicitationID: "elicitation"}
	for _, fixture := range []struct {
		name string
		new  func() Dialog
	}{
		{"form", func() Dialog { return NewElicitationDialog("question", nil, nil, ref) }},
		{"URL", func() Dialog { return NewURLElicitationDialog(t.Context(), "question", "", ref) }},
		{"OAuth", func() Dialog { return NewOAuthAuthorizationDialog("server", ref) }},
		{"max iterations", func() Dialog { return NewMaxIterationsDialog(3, "session", "request") }},
		{"tool", func() Dialog {
			return NewToolConfirmationDialog(newDialogRuntime(), &runtime.ToolCallConfirmationEvent{SessionID: "session", RequestID: "request"}, &service.SessionState{})
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			d := fixture.new()
			first := collectMsgs(d.(SemanticCloser).CancelDialogCmd())
			require.Len(t, first, 2)
			response, ok := first[1].(messages.InteractionResponseMsg)
			require.True(t, ok)
			assert.Equal(t, "session", response.SessionID)
			assert.Equal(t, "request", response.Response.InteractionID)
			assert.Nil(t, d.(SemanticCloser).CancelDialogCmd())
			if response.Response.Kind == runtime.InteractionElicitation {
				assert.NotEqual(t, tools.ElicitationActionAccept, response.Response.Elicitation.Action)
			}
			CleanupDialog(d)
		})
	}
}

func TestClosingDialogKeepsUnderlyingDraftInaccessible(t *testing.T) {
	r := newDialogRuntime()
	mgr := New(r).(*manager)
	mgr.SetSize(80, 24)
	first := &lifecycleDialog{view: "first"}
	second := &lifecycleDialog{view: "second"}
	mgr.handleOpen(OpenDialogMsg{Model: first})
	mgr.handleOpen(OpenDialogMsg{Model: second})
	mgr.handleClose()
	assert.Nil(t, mgr.TopDialog())
	before := len(first.updates)
	mgr.Update(tea.PasteMsg{Content: "must not modify underlying draft"})
	assert.Len(t, first.updates, before)
	mgr.Cleanup()
}

func BenchmarkDialogLifecycleFrame(b *testing.B) {
	for _, alpha := range []struct {
		name  string
		value float64
	}{{"fading", 0.5}, {"settled", 1}} {
		b.Run(alpha.name, func(b *testing.B) {
			dlg := NewExitConfirmationDialog()
			dlg.SetSize(100, 40)
			a := &animatedDialog{dialog: dlg, renderWidth: 50, renderHeight: 10, renderAlpha: alpha.value}
			b.ReportAllocs()
			for b.Loop() {
				_ = a.viewWithChrome(true, false)
			}
		})
	}
}

type stoppableLifecycleDialog struct {
	lifecycleDialog

	stops int
}

func (d *stoppableLifecycleDialog) StopAnimations() { d.stops++ }
func TestHideStopsChildAnimationWithoutCleaningDraft(t *testing.T) {
	r := newDialogRuntime()
	mgr := New(r).(*manager)
	mgr.SetSize(80, 24)
	dlg := &stoppableLifecycleDialog{lifecycleDialog: lifecycleDialog{view: "draft"}}
	mgr.handleOpen(OpenDialogMsg{Model: dlg})
	mgr.handleHide()
	assert.Equal(t, 1, dlg.stops)
	assert.Zero(t, dlg.cleaned)
	mgr.stack[0].anim.Cancel()
	mgr.handleTick(animation.TickMsg{})
	assert.False(t, mgr.Open())
	assert.Zero(t, dlg.cleaned)
	assert.Zero(t, r.ActiveCount())
}

func TestToolApprovalIgnoresRepeatedInputBeforeCloseDelivery(t *testing.T) {
	d := NewToolConfirmationDialog(newDialogRuntime(), &runtime.ToolCallConfirmationEvent{SessionID: "session", RequestID: "request"}, &service.SessionState{})
	_, first := d.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	require.NotNil(t, first)
	_, second := d.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	assert.Nil(t, second)
	assert.Nil(t, d.(SemanticCloser).CancelDialogCmd())
	msgs := collectMsgs(first)
	require.Len(t, msgs, 2)
	response, ok := msgs[1].(messages.InteractionResponseMsg)
	require.True(t, ok)
	assert.Equal(t, "session", response.SessionID)
	assert.Equal(t, "request", response.Response.InteractionID)
	assert.Equal(t, runtime.ResumeApprove(), response.Response.Resume)
	CleanupDialog(d)
}
