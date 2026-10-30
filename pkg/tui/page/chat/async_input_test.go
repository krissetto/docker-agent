package chat

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// flattenCmdMsgs runs cmd and collects every message it produces, expanding batches.
func flattenCmdMsgs(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, flattenCmdMsgs(c)...)
		}
		return out
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}

func newSteerablePage(t *testing.T) (*chatPage, *sessionTestSession) {
	t.Helper()
	sess := session.New()
	a, handle := newSessionTestApp(t, sess, nil, &sessionTestSession{id: sess.ID, state: runtime.SessionStateRunning})
	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	p.working = true
	return p, handle
}

// An accepted steer is owned by the runtime. Closing the page afterwards drops the
// UI result but never withdraws the message.
func TestAcceptedSteerIsNotWithdrawnWhenPageCloses(t *testing.T) {
	t.Parallel()
	p, handle := newSteerablePage(t)
	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "extra context", Steer: true})
	accepted := cmd()
	require.IsType(t, steerSentMsg{}, accepted)

	Cleanup(p)
	_, next := p.Update(accepted)
	assert.Nil(t, next, "a result for a closed page must not produce UI effects")
	require.Len(t, handle.sent(), 1, "the accepted steer reached the runtime")
	assert.Empty(t, handle.withdrawn(), "closing a page never withdraws an accepted message")
}

func TestAcceptedFollowUpIsNotWithdrawnWhenPageReloads(t *testing.T) {
	t.Parallel()
	p, handle := newSteerablePage(t)
	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "later", FollowUp: true})
	accepted := cmd()
	require.IsType(t, followUpSentMsg{}, accepted)

	// A reload replaces the page for the same conversation; the old result must not land on the new page.
	reloaded := New(animation.NewRuntime(), t.Context(), p.app, service.NewSessionState(p.app.Session())).(*chatPage)
	Cleanup(p)
	_, next := reloaded.Update(accepted)
	assert.Nil(t, next)
	_, next = p.Update(accepted)
	assert.Nil(t, next)
	require.Len(t, handle.submitted(), 1)
	assert.Empty(t, handle.withdrawn())
	Cleanup(reloaded)
}

// A submission that has not reached the runtime when its page closes is dropped
// before admission, so nothing is accepted and nothing needs withdrawing.
func TestSubmissionNotYetAdmittedIsDroppedWhenPageCloses(t *testing.T) {
	t.Parallel()
	p, handle := newSteerablePage(t)
	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "too late", Steer: true})
	Cleanup(p)
	assert.Nil(t, cmd(), "a page closed before admission produces no result")
	assert.Empty(t, handle.sent())
	assert.Empty(t, handle.withdrawn())
}

// The submission uses the app captured at dispatch, not the page's app after a replacement.
func TestSubmissionUsesAppCapturedAtDispatch(t *testing.T) {
	t.Parallel()
	p, original := newSteerablePage(t)
	replacementSess := session.New()
	replacementApp, replacement := newSessionTestApp(t, replacementSess, nil, &sessionTestSession{id: replacementSess.ID, state: runtime.SessionStateRunning})
	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "original tab", Steer: true})
	p.app = replacementApp
	msgs := flattenCmdMsgs(cmd)
	require.Len(t, msgs, 1)
	assert.IsType(t, steerSentMsg{}, msgs[0])
	require.Len(t, original.sent(), 1)
	assert.Empty(t, replacement.sent(), "the replacement app never receives the original page's input")
}

// Hidden pages keep their Local submission running and drop Visible chrome.
func TestHiddenSendKeepsLocalSubmissionAndDropsVisibleChrome(t *testing.T) {
	t.Parallel()
	p, handle := newSteerablePage(t)
	_, effects := p.UpdateEffects(messages.SendMsg{Content: "hidden steer", Steer: true})
	msgs := flattenCmdMsgs(effects.Cmd(false))
	require.Len(t, msgs, 1)
	assert.IsType(t, steerSentMsg{}, msgs[0])
	require.Len(t, handle.sent(), 1, "hidden submission reaches the runtime")

	_, effects = p.UpdateEffects(messages.SendMsg{Content: "exit"})
	assert.Empty(t, flattenCmdMsgs(effects.Cmd(false)), "the immediate exit command is visible-only")
	assert.NotEmpty(t, flattenCmdMsgs(effects.Cmd(true)), "visible pages still dispatch it")
}
