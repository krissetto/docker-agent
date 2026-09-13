package tui

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

type paneLoadStore struct {
	session.Store

	reads   atomic.Int32
	gate    chan struct{}
	started chan struct{}
	failure bool
}

func (s *paneLoadStore) GetSession(ctx context.Context, id string) (*session.Session, error) {
	s.reads.Add(1)
	if s.started != nil {
		select {
		case s.started <- struct{}{}:
		default:
		}
	}
	if s.gate != nil {
		<-s.gate
	} // Deliberately late result even after cancel.
	if s.failure {
		return nil, errors.New("fixture load failure")
	}
	return s.Store.GetSession(ctx, id)
}

func coldPaneRoot(t *testing.T) (*appModel, *paneLoadStore) {
	t.Helper()
	root := splitTestRoot(t)
	store := &paneLoadStore{Store: session.NewInMemorySessionStore(), started: make(chan struct{}, 1)}
	past := session.New(session.WithID("persisted-cold"))
	past.Title = "RESTORED COLD"
	past.AddMessage(session.UserMessage("cold persisted transcript λ界"))
	require.NoError(t, store.AddSession(t.Context(), past))
	blank := session.New(session.WithID("cold"))
	a := app.New(t.Context(), nil, blank, runtime.SessionBinding{}, app.WithRuntimeServices(storeRuntime{store: store}))
	_, err := root.supervisor.AddSession(t.Context(), a, blank, "", nil)
	require.NoError(t, err)
	root.pendingRestores["cold"] = past.ID
	tabs, index := root.supervisor.GetTabs()
	root.setTabs(tabs, index)
	root.tabBar.SetMaxTitleLength(8)
	root.View()
	return root, store
}

type paneInitCounter struct {
	*splitRecordingPage

	inits *atomic.Int32
}

func (p *paneInitCounter) Init() tea.Cmd { p.inits.Add(1); return p.Page.Init() }

func TestActualProgramColdRestoredPaneDragAndPickerHydrateOnce(t *testing.T) {
	for _, method := range []string{"drag", "picker"} {
		t.Run(method, func(t *testing.T) {
			root, store := coldPaneRoot(t)
			root.editor.SetValue("original draft")
			x, y := paneTabPoint(t, root, "cold")
			_, bounds, _ := root.measurePanes()
			program := startPaneProgram(t, root)
			if method == "drag" {
				program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
				motion := tea.MouseMotionMsg{X: bounds.X, Y: bounds.Y, Button: tea.MouseLeft}
				program.Send(messages.PointerUpdateMsg{X: motion.X, Y: motion.Y, Motion: &motion})
				require.True(t, queryPaneProgram(t, program).gesture)
				require.Zero(t, store.reads.Load(), "preview performs no store read")
				program.Send(tea.MouseReleaseMsg{X: motion.X, Y: motion.Y, Button: tea.MouseLeft})
			} else {
				program.Send(messages.PaneActionMsg{Action: "split", Source: "cold", Target: "profile", Edge: "left"})
			}
			require.Eventually(t, func() bool { return queryPaneProgram(t, program).focus == "cold" }, 2*time.Second, time.Millisecond)
			s := queryPaneProgram(t, program)
			require.Equal(t, []string{"cold", "profile"}, s.panes)
			require.Contains(t, s.content, "cold persisted transcript")
			require.EqualValues(t, 1, store.reads.Load())
			var repeatedInits atomic.Int32
			var generation uint64
			program.Send(paneProgramAction{apply: func(m *appModel) tea.Cmd {
				generation, _ = m.supervisor.RouteGeneration("cold")
				p := &paneInitCounter{splitRecordingPage: &splitRecordingPage{Page: m.chatPages["cold"]}, inits: &repeatedInits}
				m.chatPages["cold"], m.chatPage = p, p
				return m.singlePane()
			}})
			queryPaneProgram(t, program)
			program.Send(messages.SwitchTabMsg{SessionID: "profile"})
			require.Equal(t, "original draft", queryPaneProgram(t, program).draft)
			program.Send(messages.PaneActionMsg{Action: "split", Source: "cold", Target: "profile", Edge: "right"})
			require.Eventually(t, func() bool { return queryPaneProgram(t, program).focus == "cold" }, time.Second, time.Millisecond)
			require.Zero(t, repeatedInits.Load(), "warm split must not rerun Init/media discovery")
			require.EqualValues(t, 1, store.reads.Load())
			check := make(chan uint64, 1)
			program.Send(paneProgramAction{apply: func(m *appModel) tea.Cmd { now, _ := m.supervisor.RouteGeneration("cold"); check <- now; return nil }})
			require.Equal(t, generation, <-check, "warm split creates no new route observer generation")
		})
	}
}

func TestActualProgramColdPaneLoadCancellationAndErrorKeepLayoutDraft(t *testing.T) {
	for _, action := range []string{"escape", "blur", "modal", "resize", "focus", "closed", "replaced", "error"} {
		t.Run(action, func(t *testing.T) {
			root, store := coldPaneRoot(t)
			store.gate = make(chan struct{})
			var release sync.Once
			defer release.Do(func() { close(store.gate) })
			store.failure = action == "error"
			root.editor.SetValue("preserve original")
			program := startPaneProgram(t, root)
			before := queryPaneProgram(t, program)
			program.Send(messages.PaneActionMsg{Action: "split", Source: "cold", Target: "profile", Edge: "left"})
			select {
			case <-store.started:
			case <-time.After(time.Second):
				t.Fatal("load did not start")
			}
			pending := queryPaneProgram(t, program)
			require.Equal(t, before.panes, pending.panes, "load pending has not committed layout")
			require.Contains(t, pending.content, "Loading session for pane")
			switch action {
			case "escape":
				program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
			case "blur":
				program.Send(tea.BlurMsg{})
			case "modal":
				program.Send(dialog.OpenDialogMsg{Model: &stubDialog{id: "cold-modal"}})
			case "resize":
				program.Send(tea.WindowSizeMsg{Width: 119, Height: 40})
			case "focus":
				program.Send(messages.SwitchTabMsg{SessionID: "second"})
			case "closed":
				program.Send(paneProgramAction{apply: func(m *appModel) tea.Cmd { m.supervisor.CloseSession("cold"); return nil }})
			case "replaced":
				program.Send(paneProgramAction{apply: func(m *appModel) tea.Cmd {
					sess := session.New(session.WithID("replacement"))
					a := app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
					m.supervisor.ReplaceRunnerApp(t.Context(), "cold", supervisor.SpawnedSession{App: a}, "")
					return nil
				}})
			}
			queryPaneProgram(t, program)
			release.Do(func() { close(store.gate) })
			// Queue a ready/result marker after the blocked command returns by
			// waiting for pending token to retire on the owning event loop.
			require.Eventually(t, func() bool { s := queryPaneProgram(t, program); return !s.hydrating && s.hydratedResults == 1 }, time.Second, time.Millisecond)
			s := queryPaneProgram(t, program)
			require.NotContains(t, s.content, "Loading session for pane", "loading notice is owned by pending token, no lingering timer")
			if action != "focus" {
				require.Equal(t, before.panes, s.panes)
				require.Equal(t, "preserve original", s.draft)
			} else {
				require.Equal(t, "second", s.focus)
			}
			if action == "closed" {
				require.NotContains(t, s.order, "cold")
			} else {
				require.Equal(t, before.order, s.order)
			}
			check := make(chan bool, 1)
			program.Send(paneProgramAction{apply: func(m *appModel) tea.Cmd {
				check <- m.chatPages["cold"] == nil && m.pendingRestores["cold"] == "persisted-cold"
				return nil
			}})
			require.True(t, <-check, "cancel/error never initializes or consumes pending restore")
			if action == "error" {
				require.Contains(t, s.content, "fixture load failure")
			}
		})
	}
}

func TestColdPaneCanceledPreviewDoesNotReadOrInitialize(t *testing.T) {
	root, store := coldPaneRoot(t)
	x, y := paneTabPoint(t, root, "cold")
	_, bounds, _ := root.measurePanes()
	root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	root.Update(tea.MouseMotionMsg{X: bounds.X, Y: bounds.Y, Button: tea.MouseLeft})
	root.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Zero(t, store.reads.Load())
	require.Nil(t, root.chatPages["cold"])
	require.Equal(t, "persisted-cold", root.pendingRestores["cold"])
}

var _ chat.SplitPresentation = (*paneInitCounter)(nil)
