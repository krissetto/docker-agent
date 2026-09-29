package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

type (
	openingInspect struct{ reply chan openingState }
	openingState   struct {
		phase            openingPhase
		opening, loading bool
		selected         string
		active           int32
		content          string
		focus, draft     string
	}
)

type (
	openingAction  struct{ apply func(*appModel) tea.Cmd }
	openingProgram struct{ *shellProgramModel }
)

func (m *openingProgram) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case openingInspect:
		s := openingState{loading: chat.Loading(m.root.chatPage), active: m.root.ar.ActiveCount(), content: m.root.View().Content, focus: m.root.paneFocus(), draft: m.root.editor.Value()}
		if r := m.root.opening; r != nil {
			s.opening = true
			s.phase = r.phase
		}
		for _, tab := range m.root.tabInfos {
			if tab.IsActive {
				s.selected = tab.SessionID
			}
		}
		msg.reply <- s
		return m, nil
	case openingAction:
		cmd := msg.apply(m.root)
		return m, core.MapCommand(tea.Batch(cmd, m.root.ar.Continue()), nil)
	}
	_, cmd := m.shellProgramModel.Update(msg)
	return m, cmd
}

func queryOpening(t *testing.T, p *tea.Program) openingState {
	t.Helper()
	reply := make(chan openingState, 1)
	p.Send(openingInspect{reply: reply})
	select {
	case s := <-reply:
		return s
	case <-time.After(time.Second):
		t.Fatal("opening blocked owner loop")
		return openingState{}
	}
}

func openingFixture(t *testing.T) (*appModel, *sourceRuntimeFixture) {
	t.Helper()
	root, rt := paneSourceFixture(t)
	sess := rt.prepared.info.Session
	sess.ParentID = "profile"
	rt.prepared.info.Attach = &runtime.SubagentAttachInfo{NodeID: "child-node", Session: sess, Agent: "root", ParentSessionID: "profile", ParentAgent: "root"}
	// The original fixture factory intentionally only tests resolved roots.
	// Replace its immutable app options through the factory's committed info.
	return root, rt
}

func TestOpeningProgramGatedAcquisitionCancelsWithoutStealingFocus(t *testing.T) {
	root, rt := openingFixture(t)
	rt.gate = make(chan struct{})
	defer close(rt.gate)
	program := startTestProgram(t, root, &openingProgram{&shellProgramModel{root: root}}, tea.WithOutput(&cacheProgramWriter{}))
	root.supervisor.SetProgram(program)
	program.Send(openingAction{apply: func(m *appModel) tea.Cmd {
		return m.beginSubagentOpening("child-node", "closed-source", "Worker", "root")
	}})
	require.Eventually(t, func() bool { return rt.prepares.Load() == 1 }, time.Second, time.Millisecond)
	first := queryOpening(t, program)
	require.Equal(t, "closed-source", first.selected)
	require.Equal(t, "profile", first.focus)
	require.Contains(t, ansi.Strip(first.content), "Loading...")
	require.Contains(t, ansi.Strip(first.content), "original draft")
	program.Send(tea.KeyPressMsg{Code: 'x', Text: "x"})
	require.Equal(t, "original draft", queryOpening(t, program).draft)
	require.Eventually(t, func() bool { return first.content != queryOpening(t, program).content }, time.Second, 10*time.Millisecond, "loader must animate behind blocked acquisition")
	program.Send(messages.SwitchTabMsg{SessionID: "second"})
	s := queryOpening(t, program)
	require.False(t, s.opening)
	require.Equal(t, "second", s.focus)
}

func TestOpeningProgramReplayAndOrderedFades(t *testing.T) {
	root, rt := openingFixture(t)
	// The root fixture's factory returns NewResolved without attachment metadata;
	// async replay is explicitly selected by opening independently of that detail.
	rt.prepared.info.Session.AddMessage(session.UserMessage("TARGET TRANSCRIPT"))
	program := startTestProgram(t, root, &openingProgram{&shellProgramModel{root: root}}, tea.WithOutput(&cacheProgramWriter{}))
	root.supervisor.SetProgram(program)
	program.Send(openingAction{apply: func(m *appModel) tea.Cmd {
		return m.beginSubagentOpening("child-node", "closed-source", "Worker", "root")
	}})
	seenOut, seenIn := false, false
	require.Eventually(t, func() bool {
		s := queryOpening(t, program)
		if s.opening && s.phase == openingLoaderOut {
			seenOut = true
			require.NotContains(t, ansi.Strip(s.content), "TARGET TRANSCRIPT")
		}
		if s.opening && s.phase == openingChatIn {
			seenIn = true
			require.True(t, seenOut)
			require.NotContains(t, ansi.Strip(s.content), "Loading...")
		}
		return !s.opening && s.focus == "closed-source"
	}, 4*time.Second, 10*time.Millisecond)
	require.True(t, seenOut)
	require.True(t, seenIn)
	s := queryOpening(t, program)
	require.Contains(t, ansi.Strip(s.content), "TARGET TRANSCRIPT")
	require.Eventually(t, func() bool { return queryOpening(t, program).active == 0 }, time.Second, 10*time.Millisecond)
}

type blockedOpeningHandle struct {
	*lifecycleHandle

	started, gate, cancelled chan struct{}
}

func (h *blockedOpeningHandle) Observe(context.Context, runtime.ObserveOptions) (runtime.Observation, error) {
	close(h.started)
	<-h.gate
	return runtime.Observation{Events: make(chan runtime.SessionEvent), Cancel: func() { close(h.cancelled) }}, nil
}

func TestOpeningProgramCloseDoesNotWaitForBlockedObservationAndShutdownDrains(t *testing.T) {
	root, _ := openingFixture(t)
	sess := session.New(session.WithID("blocked-observe"), session.WithAgentName("root"))
	h := &blockedOpeningHandle{lifecycleHandle: &lifecycleHandle{id: sess.ID}, started: make(chan struct{}), gate: make(chan struct{}), cancelled: make(chan struct{})}
	a, err := app.NewResolved(t.Context(), newLifecycleSessions(), runtime.CommittedSessionView{SessionHandle: h, Info: runtime.PreparedSessionViewInfo{SessionID: sess.ID, Session: sess, Binding: runtime.SessionBinding{AgentName: "root"}}}, app.WithRuntimeServices(stubRuntime{}))
	require.NoError(t, err)
	_, err = root.supervisor.AddSession(t.Context(), a, sess, "", nil)
	require.NoError(t, err)
	root.createSessionComponents(sess.ID, a, sess)
	root.handleSwitchTab(sess.ID)
	program := startTestProgram(t, root, &openingProgram{&shellProgramModel{root: root}}, tea.WithOutput(&cacheProgramWriter{}))
	root.supervisor.SetProgram(program)
	select {
	case <-h.started:
	case <-time.After(time.Second):
		t.Fatal("observation did not start")
	}
	program.Send(messages.CloseTabMsg{SessionID: sess.ID})
	s := queryOpening(t, program)
	require.NotEqual(t, sess.ID, s.focus)
	program.Send(messages.SwitchTabMsg{SessionID: "second"})
	require.Equal(t, "second", queryOpening(t, program).focus)
	shutdown := make(chan struct{})
	go func() { root.supervisor.Shutdown(); close(shutdown) }()
	select {
	case <-shutdown:
		t.Fatal("shutdown failed to drain detached observation")
	case <-time.After(30 * time.Millisecond):
	}
	close(h.gate)
	select {
	case <-h.cancelled:
	case <-time.After(time.Second):
		t.Fatal("observation not cancelled")
	}
	select {
	case <-shutdown:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish")
	}
}

type openingLifetimeServices struct {
	*openSubagentRuntime

	events chan runtime.Event
}

func (s *openingLifetimeServices) EmitStartupInfo(ctx context.Context, _ *session.Session, sink runtime.EventSink) {
	for {
		select {
		case event := <-s.events:
			sink.Emit(event)
		case <-ctx.Done():
			return
		}
	}
}

func TestOpeningProgramInstalledObserverSurvivesPresentationCancellation(t *testing.T) {
	for _, switchEarly := range []bool{false, true} {
		t.Run(map[bool]string{false: "settled", true: "switch during fade"}[switchEarly], func(t *testing.T) {
			root, _ := openingFixture(t)
			sess := session.New(session.WithID("retained-observer"), session.WithAgentName("root"))
			info := runtime.SubagentAttachInfo{NodeID: "lifetime-node", Session: sess, Agent: "root"}
			services := &openingLifetimeServices{openSubagentRuntime: &openSubagentRuntime{closeTabRuntime: newCloseTabRuntime("root", false), info: info}, events: make(chan runtime.Event, 1)}
			origin := app.New(t.Context(), &openSubagentSessions{}, root.application.Session(), runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(services))
			root.application = origin
			root.supervisor.GetRunner(root.paneFocus()).App = origin
			program := startTestProgram(t, root, &openingProgram{&shellProgramModel{root: root}}, tea.WithOutput(&cacheProgramWriter{}))
			root.supervisor.SetProgram(program)
			program.Send(openingAction{apply: func(m *appModel) tea.Cmd { return m.beginSubagentOpening("lifetime-node", sess.ID, "Retained", "root") }})
			require.Eventually(t, func() bool {
				s := queryOpening(t, program)
				if switchEarly {
					return s.opening && s.phase == openingChatIn
				}
				return !s.opening && s.focus == sess.ID
			}, 4*time.Second, 10*time.Millisecond)
			if switchEarly {
				program.Send(messages.SwitchTabMsg{SessionID: "second"})
				require.Equal(t, "second", queryOpening(t, program).focus)
			}
			services.events <- runtime.UserMessage("OBSERVER STILL LIVE", sess.ID, nil)
			if switchEarly {
				program.Send(messages.SwitchTabMsg{SessionID: sess.ID})
			}
			require.Eventually(t, func() bool {
				return strings.Contains(ansi.Strip(queryOpening(t, program).content), "OBSERVER STILL LIVE")
			}, 2*time.Second, 10*time.Millisecond)
		})
	}
}
