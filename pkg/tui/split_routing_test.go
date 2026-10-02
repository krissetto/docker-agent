package tui

import (
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

type paneSequenceMarker int

type paneCommandRecorder struct {
	*splitRecordingPage

	markers []int
	sends   []messages.SendMsg
}

func (p *paneCommandRecorder) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case paneSequenceMarker:
		p.markers = append(p.markers, int(msg))
		return p, nil
	case messages.SendMsg:
		p.sends = append(p.sends, msg)
		return p, nil
	}
	_, cmd := p.splitRecordingPage.Update(msg)
	return p, cmd
}

type (
	paneProgramAction struct{ apply func(*appModel) tea.Cmd }
	paneProgramQuery  struct{ reply chan paneProgramSnapshot }
)

type paneProgramSnapshot struct {
	focus, draft, content     string
	panes, order              []string
	gesture, modal, hydrating bool
	markers                   map[string][]int
	sends                     map[string][]messages.SendMsg
	active                    int32
	hydratedResults           int
}

type paneProgram struct {
	root            *appModel
	hydratedResults int
	windowReady     chan struct{}
}

func (*paneProgram) Init() tea.Cmd { return nil }
func (m *paneProgram) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(paneHydratedMsg); ok {
		m.hydratedResults++
	}
	switch msg := msg.(type) {
	case paneProgramAction:
		return m, msg.apply(m.root)
	case paneProgramQuery:
		s := paneProgramSnapshot{focus: m.root.paneFocus(), draft: m.root.editor.Value(), content: m.root.View().Content, panes: m.root.paneLayout().Sessions(), order: m.root.paneOrder(), gesture: m.root.paneGesture != nil, hydrating: m.root.paneHydration != nil, modal: m.root.dialogMgr.Open(), active: m.root.ar.ActiveCount(), hydratedResults: m.hydratedResults, markers: map[string][]int{}, sends: map[string][]messages.SendMsg{}}
		for id, page := range m.root.chatPages {
			if p, ok := page.(*paneCommandRecorder); ok {
				s.markers[id] = append([]int(nil), p.markers...)
				s.sends[id] = append([]messages.SendMsg(nil), p.sends...)
			}
		}
		msg.reply <- s
		return m, nil
	}
	_, cmd := m.root.Update(msg)
	if _, ok := msg.(tea.WindowSizeMsg); ok && m.windowReady != nil {
		close(m.windowReady)
		m.windowReady = nil
	}
	return m, cmd
}
func (m *paneProgram) View() tea.View { return m.root.View() }
func queryPaneProgram(t *testing.T, p *tea.Program) paneProgramSnapshot {
	t.Helper()
	reply := make(chan paneProgramSnapshot, 1)
	p.Send(paneProgramQuery{reply: reply})
	select {
	case snapshot := <-reply:
		return snapshot
	case <-time.After(2 * time.Second):
		t.Fatal("pane program snapshot timed out")
		return paneProgramSnapshot{}
	}
}

// Bubble Tea schedules its startup WindowSizeMsg asynchronously. Wait until
// that real boundary is processed before accepting fixture pointer transactions;
// otherwise a legitimate startup resize can cancel a press mid-gesture.
func startPaneProgram(t *testing.T, root *appModel) *tea.Program {
	t.Helper()
	ready := make(chan struct{})
	program := startTestProgram(t, root, &paneProgram{root: root, windowReady: ready}, tea.WithOutput(&cacheProgramWriter{}))
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("pane program initial window-size acknowledgement timed out")
	}
	return program
}

func installPaneRecorder(root *appModel, id string) *paneCommandRecorder {
	p := &paneCommandRecorder{splitRecordingPage: &splitRecordingPage{Page: root.chatPages[id]}}
	root.chatPages[id] = p
	if root.paneFocus() == id {
		root.chatPage = p
	}
	return p
}

func TestActualProgramSplitNestedSequencePreservesOriginOrderAndFramework(t *testing.T) {
	root := splitTestRoot(t)
	installPaneRecorder(root, "profile")
	installPaneRecorder(root, "second")
	root.splitPane("second", "profile", splitRight)
	root.editor.SetValue("focused draft")
	program := startPaneProgram(t, root)
	program.Send(paneProgramAction{apply: func(m *appModel) tea.Cmd {
		mark := func(n int) tea.Cmd { return func() tea.Msg { return paneSequenceMarker(n) } }
		return m.routePaneCmd("profile", core.Sequence(mark(1), core.Sequence(mark(2), tea.Batch(mark(3))), func() tea.Msg { return messages.RequestFocusMsg{Target: messages.PanelMessages} }, mark(4)))
	}})
	require.Eventually(t, func() bool { return len(queryPaneProgram(t, program).markers["profile"]) == 4 }, time.Second, time.Millisecond)
	s := queryPaneProgram(t, program)
	require.Equal(t, []int{1, 2, 3, 4}, s.markers["profile"])
	require.Empty(t, s.markers["second"])
	require.Equal(t, "second", s.focus)
	require.Equal(t, "focused draft", s.draft)
	// The command-result type is unchanged, so Bubble Tea's OSC52 handler
	// (not a page Update) owns clipboard writes. No host clipboard opener runs.
	clipboard := core.SetClipboard("λ pane copy")
	require.Equal(t, reflect.TypeOf(tea.SetClipboard("λ pane copy")()), reflect.TypeOf(paneOriginCommand("profile", 1, clipboard)()))
	require.Equal(t, reflect.TypeOf(tea.SetPrimaryClipboard("")()), reflect.TypeOf(paneOriginCommand("profile", 1, core.SetPrimaryClipboard("x"))()))
	require.Nil(t, paneOriginCommand("profile", 1, nil))
	require.Nil(t, paneOriginCommand("profile", 1, func() tea.Msg { return nil })())
	routed := messages.RoutedMsg{SessionID: "third", RouteGeneration: 123, Inner: paneSequenceMarker(9)}
	require.Equal(t, routed, paneOriginCommand("profile", 1, func() tea.Msg { return routed })())
	require.IsType(t, animation.TickMsg{}, paneOriginCommand("profile", 1, func() tea.Msg { return animation.TickMsg{} })())
}

func TestActualProgramSplitStaleOriginFence(t *testing.T) {
	root := splitTestRoot(t)
	installPaneRecorder(root, "profile")
	root.splitPane("second", "profile", splitRight)
	program := startPaneProgram(t, root)
	program.Send(paneProgramAction{apply: func(m *appModel) tea.Cmd {
		generation, _ := m.supervisor.RouteGeneration("profile")
		return paneOriginCommand("profile", generation+1, tea.Sequence(func() tea.Msg { return paneSequenceMarker(1) }, func() tea.Msg { return messages.SendMsg{Content: "must not send"} }))
	}})
	program.Send(paneProgramAction{apply: func(m *appModel) tea.Cmd {
		return m.routePaneCmd("profile", func() tea.Msg { return paneSequenceMarker(2) })
	}})
	require.Eventually(t, func() bool { return len(queryPaneProgram(t, program).markers["profile"]) == 1 }, time.Second, time.Millisecond)
	s := queryPaneProgram(t, program)
	require.Equal(t, []int{2}, s.markers["profile"])
	require.Empty(t, s.sends["profile"])
	require.Equal(t, "second", s.focus)
}

func TestSplitAcceptedComposerSendKeepsOriginAfterFocusChange(t *testing.T) {
	root := splitTestRoot(t)
	first := installPaneRecorder(root, "profile")
	second := installPaneRecorder(root, "second")
	root.editor.SetValue("accepted A λ")
	cmd := root.updateEditorCmd(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	root.handleSwitchTab("second")
	root.editor.SetValue("B untouched")
	for _, msg := range collectMsgs(cmd) {
		if _, ok := msg.(messages.RoutedMsg); ok {
			root.Update(msg)
		}
	}
	require.Len(t, first.sends, 1)
	require.Equal(t, "accepted A λ", first.sends[0].Content)
	require.Empty(t, second.sends)
	require.Equal(t, "B untouched", root.editor.Value())
	require.Equal(t, "second", root.paneFocus())
}

func TestActualProgramSplitGestureFocusModalAndOrderedRelease(t *testing.T) {
	root := splitTestRoot(t)
	x, y := paneTabPoint(t, root, "second")
	_, bounds, _ := root.measurePanes()
	program := startPaneProgram(t, root)
	program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	pressed := queryPaneProgram(t, program)
	require.Equal(t, "profile", pressed.focus)
	require.True(t, pressed.gesture, "tab press captures before ordered coalesced release")
	motion := tea.MouseMotionMsg{X: bounds.X, Y: bounds.Y, Button: tea.MouseLeft}
	program.Send(messages.PointerBoundaryMsg{Pending: messages.PointerUpdateMsg{X: motion.X, Y: motion.Y, Motion: &motion}, Event: tea.MouseReleaseMsg{X: motion.X, Y: motion.Y, Button: tea.MouseLeft}})
	s := queryPaneProgram(t, program)
	require.Equal(t, []string{"second", "profile"}, s.panes)
	require.Equal(t, "second", s.focus)
	require.False(t, s.gesture)
	program.Send(paneProgramAction{apply: func(m *appModel) tea.Cmd { m.editor.SetValue("second program draft"); return nil }})
	program.Send(paneProgramAction{apply: func(m *appModel) tea.Cmd {
		r := m.paneGeometry.Panes["profile"]
		_, cmd := m.Update(tea.MouseClickMsg{X: r.X, Y: r.Y, Button: tea.MouseLeft})
		return cmd
	}})
	require.Equal(t, "profile", queryPaneProgram(t, program).focus)
	program.Send(messages.SwitchTabMsg{SessionID: "second"})
	require.Equal(t, "second program draft", queryPaneProgram(t, program).draft)
	program.Send(paneProgramAction{apply: func(m *appModel) tea.Cmd {
		x, y := 0, m.contentHeight+1
		for col := tabFrameOrigin(); col < m.width; col++ {
			if id, ok := m.tabBar.TabBodyAt(col-tabFrameOrigin(), 0); ok && id == "third" {
				x = col
				break
			}
		}
		m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
		m.Update(tea.MouseMotionMsg{X: m.paneBounds.X, Y: m.paneBounds.Y, Button: tea.MouseLeft})
		return m.updateDialogCmd(dialog.OpenDialogMsg{Model: &stubDialog{id: "program-modal"}})
	}})
	s = queryPaneProgram(t, program)
	require.True(t, s.modal)
	require.False(t, s.gesture)
	require.Equal(t, []string{"second", "profile"}, s.panes)
	require.NotContains(t, s.content, "░", "no behind-modal split overlay")
	program.Send(tea.MouseReleaseMsg{X: bounds.X, Y: bounds.Y, Button: tea.MouseLeft})
	require.Equal(t, s.panes, queryPaneProgram(t, program).panes)
}

func TestSplitSendAttachmentFollowUpAndStaleClosedReplacedOwners(t *testing.T) {
	for _, retire := range []string{"focus", "closed", "replaced", "reopened"} {
		t.Run(retire, func(t *testing.T) {
			root := splitTestRoot(t)
			first := installPaneRecorder(root, "profile")
			second := installPaneRecorder(root, "second")
			payload := messages.SendMsg{Content: "origin A", FollowUp: true, Attachments: []messages.Attachment{{Name: "paste-1", Content: "attached λ界", MimeType: "text/plain"}}}
			accepted := root.stampEditorSend(func() tea.Msg { return payload })
			root.handleSwitchTab("second")
			root.editor.SetValue("B preserved")
			switch retire {
			case "closed":
				root.supervisor.CloseSession("profile")
			case "reopened":
				root.supervisor.CloseSession("profile")
				sess := session.New(session.WithID("profile"))
				a := app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
				_, err := root.supervisor.AddSession(t.Context(), a, sess, "", nil)
				require.NoError(t, err)
			case "replaced":
				sess := session.New(session.WithID("replacement"))
				a := app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
				root.supervisor.ReplaceRunnerApp(t.Context(), "profile", supervisor.SpawnedSession{App: a}, "")
			}
			_, cmd := root.Update(accepted())
			if retire == "focus" {
				require.Equal(t, []messages.SendMsg{payload}, first.sends)
			} else {
				require.Empty(t, first.sends)
				var reported bool
				for _, msg := range collectMsgs(cmd) {
					if n, ok := msg.(notification.ShowMsg); ok {
						reported = strings.Contains(n.Text, "Unsent draft: origin A")
					}
				}
				require.True(t, reported, "retired send reports unsent content without writing another editor")
			}
			require.Empty(t, second.sends)
			require.Equal(t, "B preserved", root.editor.Value())
		})
	}
}

func TestThinkingCycleCommandPreservesExactOriginAndUnrelatedMessages(t *testing.T) {
	action := messages.CycleThinkingLevelMsg{SessionID: "canonical-session", AgentName: "agent", ModelRef: "provider/model"}
	command := tea.Sequence(
		func() tea.Msg { return messages.SwitchTabMsg{SessionID: "other"} },
		tea.Batch(func() tea.Msg { return action }),
	)
	results := collectMsgs(stampThinkingCycleCommand("routing-owner", 7, command))
	var found bool
	for _, result := range results {
		routed, ok := result.(messages.RoutedMsg)
		if !ok {
			continue
		}
		require.Equal(t, "routing-owner", routed.SessionID)
		require.EqualValues(t, 7, routed.RouteGeneration)
		cycle, ok := routed.Inner.(messages.CycleThinkingLevelMsg)
		require.True(t, ok)
		require.EqualValues(t, 7, cycle.RouteGeneration)
		require.Equal(t, action.SessionID, cycle.SessionID)
		found = true
	}
	require.True(t, found)
	require.Contains(t, results, tea.Msg(messages.SwitchTabMsg{SessionID: "other"}))
}

func TestThinkingCycleRejectsZeroStaleAndForeignProjection(t *testing.T) {
	root := splitTestRoot(t)
	generation, _ := root.supervisor.RouteGeneration("profile")
	valid := messages.CycleThinkingLevelMsg{SessionID: root.application.Session().ID, AgentName: root.sessionState.CurrentAgentName(), ModelRef: root.application.CurrentAgentModel(root.ctx()), RouteGeneration: generation}
	for _, mutate := range []func(*messages.CycleThinkingLevelMsg){
		func(msg *messages.CycleThinkingLevelMsg) { msg.RouteGeneration = 0 },
		func(msg *messages.CycleThinkingLevelMsg) { msg.RouteGeneration++ },
		func(msg *messages.CycleThinkingLevelMsg) { msg.SessionID = "foreign" },
		func(msg *messages.CycleThinkingLevelMsg) { msg.AgentName = "foreign" },
		func(msg *messages.CycleThinkingLevelMsg) { msg.ModelRef = "different/model" },
	} {
		msg := valid
		mutate(&msg)
		_, cmd := root.handleScopedThinkingCycle(msg)
		require.Nil(t, cmd)
	}
	root.handleSwitchTab("second")
	_, cmd := root.Update(messages.RoutedMsg{SessionID: "profile", RouteGeneration: generation, Inner: valid})
	require.Nil(t, cmd, "queued footer action cannot switch or mutate the newly focused owner")
}

func TestThinkingCycleCanonicalProjectionMatchesConfiguredAlias(t *testing.T) {
	root := splitTestRoot(t)
	root.sessionState.SetCurrentAgentName("worker")
	root.sessionState.SetAvailableAgents([]runtime.AgentDetails{{Name: "worker", Provider: "provider", Model: "configured alias", ModelID: "canonical-model", ModelName: "Friendly name", CanCycleThinking: true}})
	root.application.TrackCurrentAgentModel("configured alias")
	require.Equal(t, "provider/canonical-model", root.thinkingModelReference())
	root.application.TrackCurrentAgentModel("provider/configured alias")
	require.Equal(t, "provider/canonical-model", root.thinkingModelReference())
	root.application.TrackCurrentAgentModel("provider/canonical-model")
	require.Equal(t, "provider/canonical-model", root.thinkingModelReference())
	root.application.TrackCurrentAgentModel("provider/replacement")
	require.Equal(t, "provider/replacement", root.thinkingModelReference(), "stale TeamInfo must not validate the previous model click")
}
