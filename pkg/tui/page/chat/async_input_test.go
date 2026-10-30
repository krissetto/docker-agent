package chat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
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

// perSessionTestRuntime gives every session its own recording handle, so a
// test can tell which session an input reached.
type perSessionTestRuntime struct {
	handles map[string]*sessionTestSession
}

func (r *perSessionTestRuntime) CreateSession(_ context.Context, sess *session.Session, _ runtime.SessionBinding) (runtime.SessionHandle, error) {
	if r.handles[sess.ID] == nil {
		r.handles[sess.ID] = &sessionTestSession{id: sess.ID, state: runtime.SessionStateRunning}
	}
	return r.handles[sess.ID], nil
}

func (r *perSessionTestRuntime) SessionByID(id string) (runtime.SessionHandle, error) {
	if h := r.handles[id]; h != nil {
		return h, nil
	}
	return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: id, Operation: "lookup"}
}

func (*perSessionTestRuntime) DeleteSession(context.Context, string) error { return nil }

// A steer or follow-up with a file attachment, dispatched on one session, must
// reach that session even when the app's state is replaced before completion.
func TestAttachedInputStaysOnDispatchSessionAcrossReplacement(t *testing.T) {
	t.Parallel()
	for _, followUp := range []bool{false, true} {
		name := "steer"
		if followUp {
			name = "follow-up"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			origin, other := session.New(), session.New()
			rt := &perSessionTestRuntime{handles: map[string]*sessionTestSession{}}
			a := app.New(t.Context(), rt, origin, runtime.SessionBinding{}, app.WithRuntimeServices(&queueTestServices{}))
			p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(origin)).(*chatPage)
			p.working = true
			file := filepath.Join(t.TempDir(), "notes.txt")
			require.NoError(t, os.WriteFile(file, []byte("attached λ"), 0o600))
			msg := messages.SendMsg{Content: "with file", Steer: !followUp, FollowUp: followUp, Attachments: []messages.Attachment{{Name: "notes.txt", FilePath: file}}}

			_, cmd := p.handleSendMsg(msg)
			require.NotNil(t, cmd)
			// The session changes after dispatch, before the async submission runs.
			a.ReplaceSession(t.Context(), other)
			msgs := flattenCmdMsgs(cmd)
			require.Len(t, msgs, 1)

			originHandle, otherHandle := rt.handles[origin.ID], rt.handles[other.ID]
			require.NotNil(t, originHandle)
			var inputs []runtime.TurnInput
			if followUp {
				require.Len(t, originHandle.submitted(), 1)
				inputs = originHandle.submitted()
			} else {
				require.Len(t, originHandle.sent(), 1)
				inputs = originHandle.sent()
			}
			require.NotEmpty(t, inputs[0].MultiContent, "the file attachment resolves against the dispatch session")
			var text strings.Builder
			for _, part := range inputs[0].MultiContent {
				text.WriteString(part.Text)
			}
			assert.Contains(t, text.String(), "attached λ")
			if otherHandle != nil {
				assert.Empty(t, otherHandle.sent())
				assert.Empty(t, otherHandle.submitted())
			}
		})
	}
}
