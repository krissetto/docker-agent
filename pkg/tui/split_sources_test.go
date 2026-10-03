package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

type sourcePreparedFixture struct {
	info    runtime.PreparedSessionViewInfo
	handle  *lifecycleHandle
	commits int
	aborts  atomic.Int32
	err     error
}

func (p *sourcePreparedFixture) Info() runtime.PreparedSessionViewInfo {
	info := p.info
	info.Session = info.Session.Clone()
	return info
}

func (p *sourcePreparedFixture) Commit(context.Context) (runtime.CommittedSessionView, error) {
	p.commits++
	return runtime.CommittedSessionView{SessionHandle: p.handle, Info: p.Info()}, p.err
}
func (p *sourcePreparedFixture) Abort() { p.aborts.Add(1) }

type sourceRuntimeFixture struct {
	*lifecycleSessions

	prepared *sourcePreparedFixture
	prepares atomic.Int32
	gate     chan struct{}
	metadata []runtime.SessionSummaryEntry
	listings atomic.Int32
	listGate chan struct{}
}

func (r *sourceRuntimeFixture) ListSessionSummaries(context.Context, runtime.SessionSummaryOptions) ([]runtime.SessionSummaryEntry, error) {
	r.listings.Add(1)
	if r.listGate != nil {
		<-r.listGate
	}
	return r.metadata, nil
}

func (r *sourceRuntimeFixture) PrepareSessionView(context.Context, string) (runtime.PreparedSessionView, error) {
	r.prepares.Add(1)
	if r.gate != nil {
		<-r.gate
	}
	return r.prepared, nil
}

func paneSourceFixture(t *testing.T) (*appModel, *sourceRuntimeFixture) {
	t.Helper()
	// Real-program source tests use one runtime created by New for every
	// component; do not retrofit a wall clock onto a frozen split fixture.
	initial := session.New(session.WithID("profile"), session.WithAgentName("root"))
	initialApp := app.New(t.Context(), nil, initial, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	root := newSidebarProgramRoot(t, initialApp)
	root.hideSidebar = true
	for _, id := range []string{"second", "third"} {
		sess := session.New(session.WithID(id), session.WithAgentName("root"))
		application := app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
		_, err := root.supervisor.AddSession(t.Context(), application, sess, "", nil)
		require.NoError(t, err)
		root.createSessionComponents(id, application, sess)
		root.chatPages[id].Init()
	}
	t.Cleanup(func() {
		for _, page := range root.chatPages {
			chat.Cleanup(page)
		}
	})
	root.resizeAll()
	closed := session.New(session.WithID("closed-source"), session.WithAgentName("root"), session.WithTitle("Closed source"))
	closed.AddMessage(session.UserMessage("closed transcript"))
	handle := &lifecycleHandle{id: closed.ID}
	rt := &sourceRuntimeFixture{lifecycleSessions: newLifecycleSessions(), prepared: &sourcePreparedFixture{handle: handle, info: runtime.PreparedSessionViewInfo{SessionID: closed.ID, RootSessionID: closed.ID, Session: closed, Binding: runtime.SessionBinding{AgentName: "root"}}}}
	a := app.New(t.Context(), rt, root.application.Session(), runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	root.application = a
	root.supervisor.GetRunner("profile").App = a
	rt.metadata = []runtime.SessionSummaryEntry{{SessionID: closed.ID, Title: "Closed source", AgentName: "root", RequiresConfirmation: true}}
	root.editor.SetValue("original draft")
	scope := supervisor.NewViewOwnerScope()
	identity := supervisor.ViewOwnerIdentity{Scope: scope, Source: "fixture", RootSessionID: closed.ID, RootBinding: runtime.SessionBinding{AgentName: "root"}}
	require.NoError(t, root.supervisor.ConfigureSessionViews(t.Context(), supervisor.HostViewConfig{
		Resolve: func(context.Context, string) (supervisor.ViewOwnerIdentity, error) { return identity, nil },
		Factory: func(context.Context, supervisor.ViewOwnerIdentity) (supervisor.ViewOwnerResources, error) {
			panic("existing fixture owner duplicated")
		},
		MaxRetainedViewOwners: supervisor.DefaultMaxRetainedViewOwners,
	}))
	require.NoError(t, root.supervisor.RegisterSessionOwner(identity, supervisor.ViewOwnerResources{
		Services: a.Runtime(), Sessions: rt,
		NewApp: func(ctx context.Context, committed runtime.CommittedSessionView) (*app.App, error) {
			return app.NewResolved(ctx, rt, committed, app.WithRuntimeServices(a.Runtime()))
		},
	}, true))
	return root, rt
}

func TestClosedPaneSourcePrepareCommitAdoptsExactlyOnce(t *testing.T) {
	root, rt := paneSourceFixture(t)
	// A single-view fixture has no stored tree until its first transaction.
	// Materialize the canonical starting leaf once, as pointer/picker entry
	// does, so this assertion tracks an actual retained topology token.
	root.panes = root.paneLayout()
	before := root.panes.root
	cmd := root.beginPaneSource("closed-source", "profile", splitRight)
	require.Zero(t, rt.prepares.Load(), "prepare IO runs only in command")
	require.Equal(t, "profile", root.paneFocus())
	prepared := cmd().(paneSourcePreparedMsg)
	require.Same(t, before, root.panes.root)
	commit := root.finishPaneSourcePrepared(prepared)
	require.Zero(t, rt.prepared.commits, "commit IO runs only in command")
	result := commit().(paneSourceCommittedMsg)
	root.finishPaneSourceCommitted(result)
	require.Equal(t, "closed-source", root.paneFocus())
	require.Equal(t, []string{"profile", "closed-source"}, root.panes.Sessions())
	require.Same(t, rt.prepared.handle, root.application.SessionHandle())
	require.Equal(t, 4, root.supervisor.Count())
	root.finishPaneSourceCommitted(result)
	require.Equal(t, 4, root.supervisor.Count(), "late duplicate completion cannot add another view")
	require.Zero(t, rt.prepared.handle.releases.Load())
	root.handleSwitchTab("profile")
	require.Equal(t, "original draft", root.editor.Value())
}

func TestClosedPaneSourceCancellationAtEveryPhaseLeavesLayoutAndDriver(t *testing.T) {
	for _, phase := range []string{"before-prepare", "after-prepare", "after-commit", "commit-error", "foreign-modal"} {
		t.Run(phase, func(t *testing.T) {
			root, rt := paneSourceFixture(t)
			cmd := root.beginPaneSource("closed-source", "profile", splitRight)
			before := root.panes.root
			if phase == "before-prepare" {
				root.cancelPaneGesture()
			}
			prepared := cmd().(paneSourcePreparedMsg)
			if phase == "after-prepare" {
				root.cancelPaneGesture()
			}
			commit := root.finishPaneSourcePrepared(prepared)
			if commit != nil {
				if phase == "commit-error" {
					rt.prepared.err = errors.New("durable commit failed")
				}
				result := commit().(paneSourceCommittedMsg)
				if phase == "after-commit" {
					root.cancelPaneGesture()
				}
				if phase == "foreign-modal" {
					root.updateDialogCmd(dialog.OpenDialogMsg{Model: &stubDialog{id: "foreign"}})
				}
				root.finishPaneSourceCommitted(result)
			}
			require.Same(t, before, root.panes.root)
			require.Equal(t, "profile", root.paneFocus())
			require.Equal(t, "original draft", root.editor.Value())
			require.Nil(t, root.chatPages["closed-source"])
			require.Zero(t, rt.prepared.handle.releases.Load(), "canceled presentation never releases canonical execution")
			if phase == "before-prepare" {
				require.Zero(t, rt.prepares.Load(), "canceled host acquisition allocates no preparation")
				require.Zero(t, rt.prepared.commits)
				require.Zero(t, rt.prepared.aborts.Load(), "there is no unpublished preparation to retire")
			} else {
				require.Positive(t, rt.prepared.aborts.Load())
			}
		})
	}
}

func TestClosedPaneSourceWaitsForExactPickerClosing(t *testing.T) {
	root, _ := paneSourceFixture(t)
	picker := &stubDialog{id: "source-picker"}
	root.updateDialogCmd(dialog.OpenDialogMsg{Model: picker})
	root.updateDialogCmd(dialog.CloseDialogMsg{})
	cmd := root.beginPaneSource("closed-source", "profile", splitRight)
	prepared := cmd().(paneSourcePreparedMsg)
	commit := root.finishPaneSourcePrepared(prepared)
	require.NotNil(t, commit)
	root.finishPaneSourceCommitted(commit().(paneSourceCommittedMsg))
	require.Equal(t, "profile", root.paneFocus(), "canonical adoption waits for picker close")
	root.dialogMgr.Cleanup()
	root.adoptPaneSource()
	require.Equal(t, "closed-source", root.paneFocus())
}

func TestActualProgramClosedPaneCancelWhilePrepareIsBlocked(t *testing.T) {
	root, rt := paneSourceFixture(t)
	rt.gate = make(chan struct{})
	program := startPaneProgram(t, root)
	program.Send(paneProgramAction{apply: func(m *appModel) tea.Cmd { return m.beginPaneSource("closed-source", "profile", splitRight) }})
	require.Eventually(t, func() bool { return rt.prepares.Load() == 1 }, time.Second, time.Millisecond)
	program.Send(tea.BlurMsg{})
	snapshot := queryPaneProgram(t, program)
	require.Equal(t, "profile", snapshot.focus)
	require.Equal(t, "original draft", snapshot.draft)
	close(rt.gate)
	// A marker sent after release is not an IO completion barrier. Poll only
	// event-loop-owned transaction state; late prepare must remain canceled.
	require.Eventually(t, func() bool {
		check := make(chan bool, 1)
		program.Send(paneProgramAction{apply: func(m *appModel) tea.Cmd {
			check <- m.paneSource == nil && m.chatPages["closed-source"] == nil
			return nil
		}})
		return <-check && rt.prepared.aborts.Load() > 0
	}, time.Second, time.Millisecond)
	require.Zero(t, rt.prepared.handle.releases.Load())
}

func TestActualProgramPaneCatalogPreviewDoesNotPrepareUntilConfirm(t *testing.T) {
	root, rt := paneSourceFixture(t)
	program := startPaneProgram(t, root)
	program.Send(messages.OpenPanesMsg{Arguments: "right"})
	require.Eventually(t, func() bool { return queryPaneProgram(t, program).modal }, time.Second, time.Millisecond)
	require.EqualValues(t, 1, rt.listings.Load())
	require.Zero(t, rt.prepares.Load(), "metadata chooser has not prepared/hydrated a transcript")
	program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Eventually(t, func() bool { return !queryPaneProgram(t, program).modal }, time.Second, time.Millisecond)
	require.Equal(t, "original draft", queryPaneProgram(t, program).draft)
	program.Send(messages.OpenPanesMsg{Arguments: "right \"Closed source\""})
	require.Eventually(t, func() bool { return queryPaneProgram(t, program).focus == "closed-source" }, 2*time.Second, time.Millisecond)
	require.EqualValues(t, 1, rt.prepares.Load())
	require.Len(t, queryPaneProgram(t, program).order, 4)
}

type dormantResumeHandle struct {
	*lifecycleHandle

	mu          sync.Mutex
	session     *session.Session
	dormant     bool
	pending     int
	sequence    uint64
	events      chan runtime.SessionEvent
	resumes     atomic.Int32
	cancelCalls atomic.Int32
}

func (h *dormantResumeHandle) Observe(ctx context.Context, options runtime.ObserveOptions) (runtime.Observation, error) {
	if options.Tree {
		return runtime.Observation{}, runtime.ErrUnsupported
	}
	h.mu.Lock()
	h.events = make(chan runtime.SessionEvent, 8)
	status := runtime.SessionStatus{SessionID: h.id, AgentName: "root", Dormant: h.dormant, Pending: h.pending}
	snapshot := h.session.Clone()
	events := h.events
	h.mu.Unlock()
	return runtime.Observation{Initial: []runtime.SessionSnapshot{{Session: snapshot, Status: status}}, Events: events, Cancel: func() {}}, nil
}

func (h *dormantResumeHandle) Status(context.Context) (runtime.SessionStatus, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return runtime.SessionStatus{SessionID: h.id, AgentName: "root", Dormant: h.dormant, Pending: h.pending}, nil
}

func (h *dormantResumeHandle) Edit(_ context.Context, edit runtime.SessionEdit) (*session.Session, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if edit.Kind != runtime.SessionEditResume {
		return nil, errors.New("unexpected edit")
	}
	h.resumes.Add(1)
	h.dormant = false
	h.sequence++
	h.events <- runtime.SessionEvent{SessionID: h.id, Sequence: h.sequence, Event: &runtime.DormancyChangedEvent{SessionID: h.id, Dormant: false}}
	return h.session.Clone(), nil
}

func (h *dormantResumeHandle) Cancel(context.Context, string) (runtime.CancelResult, error) {
	h.cancelCalls.Add(1)
	return runtime.CancelResult{}, nil
}

type dormantResumeRuntime struct {
	*lifecycleSessions

	handle *dormantResumeHandle
}

func (r *dormantResumeRuntime) SessionByID(id string) (runtime.SessionHandle, error) {
	if id == r.handle.id {
		return r.handle, nil
	}
	return r.lifecycleSessions.SessionByID(id)
}

func TestActualProgramDormantResumeFullAndRootLeanUsesCanonicalEdit(t *testing.T) {
	for _, lean := range []bool{false, true} {
		for _, pending := range []int{0, 2} {
			t.Run(fmt.Sprintf("lean=%t/pending=%d", lean, pending), func(t *testing.T) {
				sess := session.New(session.WithID("resume-origin"), session.WithAgentName("root"), session.WithTitle("Restored session"))
				handle := &dormantResumeHandle{lifecycleHandle: &lifecycleHandle{id: sess.ID}, session: sess, dormant: true, pending: pending}
				rt := &dormantResumeRuntime{lifecycleSessions: newLifecycleSessions(), handle: handle}
				application, err := app.NewResolved(t.Context(), rt, runtime.CommittedSessionView{SessionHandle: handle, Info: runtime.PreparedSessionViewInfo{SessionID: sess.ID, Session: sess, Binding: runtime.SessionBinding{AgentName: "root"}}}, app.WithRuntimeServices(stubRuntime{}))
				require.NoError(t, err)
				root := newSidebarProgramRoot(t, application)
				root.leanMode = lean
				root.editor.SetValue("preserved draft")
				root.resizeAll()
				program := startPaneProgram(t, root)
				require.Eventually(t, func() bool {
					return strings.Contains(ansi.Strip(queryPaneProgram(t, program).content), "Restored · paused")
				}, 2*time.Second, time.Millisecond)
				beforeStats := make(chan [3]uint64, 1)
				program.Send(paneProgramAction{apply: func(m *appModel) tea.Cmd {
					a, b, c := m.chatPage.(resizeCacheReporter).ResizeCacheStats()
					beforeStats <- [3]uint64{a, b, c}
					generation, _ := m.supervisor.RouteGeneration(m.paneFocus())
					return stampThinkingCycleCommand(m.paneFocus(), generation, func() tea.Msg { return messages.ResumeSessionMsg{} })
				}})
				var resumeDiagnostic string
				defer func() {
					if t.Failed() {
						t.Log(resumeDiagnostic)
					}
				}()
				require.Eventually(t, func() bool {
					check := make(chan string, 1)
					program.Send(paneProgramAction{apply: func(m *appModel) tea.Cmd {
						status := m.application.Presentation().Status
						check <- fmt.Sprintf("resumes=%d dormant=%t cache=%t focus=%s cachedstatus=%+v currentstatus=%+v freshheader=%q frame=%q", handle.resumes.Load(), status.Dormant, m.viewCacheValid, m.paneFocus(), m.viewPaneStatuses, m.visiblePaneStatuses(), ansi.Strip(m.paneTitle(m.paneFocus(), m.width)), ansi.Strip(m.View().Content))
						return nil
					}})
					resumeDiagnostic = <-check
					return handle.resumes.Load() == 1 && !strings.Contains(ansi.Strip(queryPaneProgram(t, program).content), "Restored · paused")
				}, 2*time.Second, time.Millisecond)
				require.Equal(t, "preserved draft", queryPaneProgram(t, program).draft)
				afterStats := make(chan [3]uint64, 1)
				program.Send(paneProgramAction{apply: func(m *appModel) tea.Cmd {
					a, b, c := m.chatPage.(resizeCacheReporter).ResizeCacheStats()
					afterStats <- [3]uint64{a, b, c}
					return nil
				}})
				require.Equal(t, <-beforeStats, <-afterStats, "status-only resume cannot rebuild transcript messages")
				require.Eventually(t, func() bool { return queryPaneProgram(t, program).active == 0 }, time.Second, time.Millisecond, "settled status-only header owns no animation lease")
				require.Zero(t, handle.submits.Load(), "Resume is Edit, not dummy submission")
				require.Zero(t, handle.cancelCalls.Load())
				require.Zero(t, handle.releases.Load())
			})
		}
	}
}

func TestActualProgramResumeKeepsOriginalOwnerAfterFocusChange(t *testing.T) {
	sess := session.New(session.WithID("resume-A"), session.WithAgentName("root"))
	handle := &dormantResumeHandle{lifecycleHandle: &lifecycleHandle{id: sess.ID}, session: sess, dormant: true}
	rt := &dormantResumeRuntime{lifecycleSessions: newLifecycleSessions(), handle: handle}
	application, err := app.NewResolved(t.Context(), rt, runtime.CommittedSessionView{SessionHandle: handle, Info: runtime.PreparedSessionViewInfo{SessionID: sess.ID, Session: sess, Binding: runtime.SessionBinding{AgentName: "root"}}}, app.WithRuntimeServices(stubRuntime{}))
	require.NoError(t, err)
	root := newSidebarProgramRoot(t, application)
	other := session.New(session.WithID("resume-B"), session.WithAgentName("root"))
	otherApp := app.New(t.Context(), rt, other, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	_, err = root.supervisor.AddSession(t.Context(), otherApp, other, "", nil)
	require.NoError(t, err)
	root.editor.SetValue("A draft")
	program := startPaneProgram(t, root)
	require.Eventually(t, func() bool {
		return strings.Contains(ansi.Strip(queryPaneProgram(t, program).content), "Restored · paused")
	}, 2*time.Second, time.Millisecond)
	program.Send(paneProgramAction{apply: func(m *appModel) tea.Cmd {
		generation, _ := m.supervisor.RouteGeneration(sess.ID)
		accepted := stampThinkingCycleCommand(sess.ID, generation, func() tea.Msg { return messages.ResumeSessionMsg{SessionID: sess.ID} })
		_, switchCmd := m.handleSwitchTab(other.ID)
		m.editor.SetValue("B draft")
		return tea.Batch(switchCmd, accepted)
	}})
	require.Eventually(t, func() bool { return handle.resumes.Load() == 1 }, 2*time.Second, time.Millisecond)
	snapshot := queryPaneProgram(t, program)
	require.Equal(t, other.ID, snapshot.focus)
	require.Equal(t, "B draft", snapshot.draft)
	require.Zero(t, handle.submits.Load())
	require.Zero(t, handle.cancelCalls.Load())
	require.Zero(t, handle.releases.Load())
}

func TestPendingTwoDormancyChangeInvalidatesCachedAndForcedHeaderEqually(t *testing.T) {
	sess := session.New(session.WithID("pending-two"), session.WithAgentName("root"))
	handle := &dormantResumeHandle{lifecycleHandle: &lifecycleHandle{id: sess.ID}, session: sess, dormant: true, pending: 2}
	rt := &dormantResumeRuntime{lifecycleSessions: newLifecycleSessions(), handle: handle}
	application, err := app.NewResolved(t.Context(), rt, runtime.CommittedSessionView{SessionHandle: handle, Info: runtime.PreparedSessionViewInfo{SessionID: sess.ID, Session: sess, Binding: runtime.SessionBinding{AgentName: "root"}}}, app.WithRuntimeServices(stubRuntime{}))
	require.NoError(t, err)
	root := newSidebarProgramRoot(t, application)
	root.View()
	require.True(t, root.viewPaneStatuses[sess.ID].Dormant)
	require.Equal(t, 2, root.viewPaneStatuses[sess.ID].Pending)
	_, err = handle.Edit(t.Context(), runtime.SessionEdit{Kind: runtime.SessionEditResume})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return !application.Presentation().Status.Dormant }, time.Second, time.Millisecond)
	// No root Update or recovery click: the canonical key alone must fence the
	// previous complete frame when the atomic App head changes asynchronously.
	cached := root.View().Content
	require.NotContains(t, ansi.Strip(cached), "Restored · paused")
	require.False(t, root.viewPaneStatuses[sess.ID].Dormant)
	root.viewCacheValid = false
	forced := root.View().Content
	require.Equal(t, cached, forced)
}
