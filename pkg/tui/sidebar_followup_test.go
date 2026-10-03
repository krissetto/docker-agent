package tui

import (
	"context"
	"image/color"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	tuiinput "github.com/docker/docker-agent/pkg/tui/input"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type sidebarAttachRuntime struct {
	*closeTabRuntime

	infos   map[subagent.NodeID]runtime.SubagentAttachInfo
	lookups atomic.Int32
}

func (r *sidebarAttachRuntime) SubagentAttachInfo(id subagent.NodeID) (runtime.SubagentAttachInfo, bool) {
	r.lookups.Add(1)
	info, ok := r.infos[id]
	return info, ok
}

func (r *sidebarAttachRuntime) SubagentNodeForSession(id string) (subagent.NodeID, bool) {
	for node, info := range r.infos {
		if info.Session != nil && info.Session.ID == id {
			return node, true
		}
	}
	return "", false
}

func sidebarProgramSnapshot(t *testing.T, program *tea.Program) shellSnapshot {
	t.Helper()
	reply := make(chan shellSnapshot, 1)
	program.Send(shellSnapshotMsg{reply: reply})
	select {
	case result := <-reply:
		return result
	case <-time.After(time.Second):
		t.Fatal("sidebar snapshot timed out")
		return shellSnapshot{}
	}
}

func expandProgramSubagents(t *testing.T, program *tea.Program, descendant string) shellSnapshot {
	t.Helper()
	frame := sidebarProgramSnapshot(t, program)
	x, y := sidebarProgramPoint(t, frame.content, "subagents")
	program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.Eventually(t, func() bool {
		frame = sidebarProgramSnapshot(t, program)
		return frame.active == 0 && strings.Contains(ansi.Strip(frame.content), descendant)
	}, 3*time.Second, time.Millisecond, "explicit expansion reveals the fixture's descendant rows: active=%d frame=%s", frame.active, ansi.Strip(frame.content))
	return frame
}

func sidebarProgramPoint(t *testing.T, frame, label string) (x, y int) {
	t.Helper()
	for y, line := range strings.Split(frame, "\n") {
		prefix, _, found := strings.Cut(ansi.Strip(line), label)
		if found {
			return ansi.StringWidth(prefix), y
		}
	}
	t.Fatalf("sidebar label %q absent from frame:\n%s", label, ansi.Strip(frame))
	return 0, 0
}

func newSidebarProgramRoot(t *testing.T, application *app.App) *appModel {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	root := New(t.Context(), nil, application, "", func() {}).(*appModel)
	t.Cleanup(root.cleanupManagedResources)
	_ = root.init() // The wrapper owns scheduling; do not reserve and discard a tick lease.
	root.handleWindowResize(120, 40)
	return root
}

func TestActualProgramSidebarAttachedTreeUsesSelectedCanonicalRoot(t *testing.T) {
	for _, withChildren := range []bool{false, true} {
		name := "leaf"
		if withChildren {
			name = "branch"
		}
		t.Run(name, func(t *testing.T) {
			parent := session.New(session.WithID("sidebar-parent-full-session"), session.WithAgentName("director"))
			parent.Title = "PARENT-TAB"
			child := session.New(session.WithID("sidebar-child-full-session"), session.WithAgentName("worker"))
			child.Title = "CHILD-TAB"
			child.AddMessage(session.UserMessage("SELECTED-CHILD-TRANSCRIPT"))
			childNode := subagent.NodeSnapshot{Node: subagent.Node{ID: "child-full-canonical-node", SessionID: child.ID, Agent: "worker", Name: "SelectedWorker", State: subagent.NodeCompleted}}
			grand := session.New(session.WithID("sidebar-grandchild-full-session"), session.WithAgentName("reviewer"))
			if withChildren {
				childNode.Children = []subagent.NodeSnapshot{{Node: subagent.Node{ID: "grand-full-canonical-node", SessionID: grand.ID, Agent: "reviewer", Name: "NestedReviewer", State: subagent.NodeCompleted}}}
			}
			tree := subagent.Snapshot{Root: subagent.SessionRootID(parent.ID), Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: subagent.SessionRootID(parent.ID), SessionID: parent.ID, Agent: "director", State: subagent.NodeCompleted}, Children: []subagent.NodeSnapshot{childNode}}}}
			parent.SetSubagentTree(&tree)
			child.SetSubagentTree(&tree)
			grand.SetSubagentTree(&tree)
			services := &sidebarAttachRuntime{closeTabRuntime: newCloseTabRuntime("director", false), infos: map[subagent.NodeID]runtime.SubagentAttachInfo{
				childNode.Node.ID:           {NodeID: childNode.Node.ID, Agent: "worker", Session: child, ParentAgent: "director", ParentSessionID: parent.ID},
				"grand-full-canonical-node": {NodeID: "grand-full-canonical-node", Agent: "reviewer", Session: grand, ParentAgent: "worker", ParentSessionID: child.ID},
			}}
			views := services.preparedViews(parent)
			application := app.New(t.Context(), views, parent, runtime.SessionBinding{AgentName: "director"}, app.WithRuntimeServices(services))
			root := newSidebarProgramRoot(t, application)
			_, _ = root.updateWithLifecycle(runtime.TeamInfo([]runtime.AgentDetails{{Name: "director"}, {Name: "worker"}}, "director"))
			program := startTestProgram(t, root, &shellProgramModel{root: root}, tea.WithOutput(&cacheProgramWriter{}))
			root.supervisor.SetProgram(program)
			frame := expandProgramSubagents(t, program, "SelectedWorker")
			x, y := sidebarProgramPoint(t, frame.content, "SelectedWorker")
			program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			var selected shellSnapshot
			require.Eventually(t, func() bool {
				selected = sidebarProgramSnapshot(t, program)
				plain := ansi.Strip(selected.content)
				return selected.activeID == child.ID && selected.active == 0 &&
					strings.Contains(plain, "SELECTED-CHILD-TRANSCRIPT") && strings.Contains(plain, "parent: director")
			}, 3*time.Second, time.Millisecond)
			if withChildren {
				selected = expandProgramSubagents(t, program, "NestedReviewer")
			}
			program.Send(runtime.TeamInfo([]runtime.AgentDetails{{Name: "root"}, {Name: "worker"}}, "worker"))
			require.Eventually(t, func() bool {
				selected = sidebarProgramSnapshot(t, program)
				plain := ansi.Strip(selected.content)
				return selected.activeID == child.ID && selected.active == 0 &&
					strings.Contains(plain, "SELECTED-CHILD-TRANSCRIPT") && strings.Contains(plain, "parent: director") &&
					(!withChildren || strings.Contains(plain, "NestedReviewer"))
			}, 3*time.Second, time.Millisecond)
			require.Contains(t, ansi.Strip(selected.content), "SELECTED-CHILD-TRANSCRIPT")
			require.Contains(t, ansi.Strip(selected.content), "parent: director")
			require.NotContains(t, ansi.Strip(selected.content), "SelectedWorker", "selected agent is not its own descendant")
			require.NotContains(t, ansi.Strip(selected.content), "▶ root")
			if withChildren {
				x, y = sidebarProgramPoint(t, selected.content, "NestedReviewer")
				program.Send(tea.MouseMotionMsg{X: x, Y: y})
				var hovered shellSnapshot
				require.Eventually(t, func() bool {
					hovered = sidebarProgramSnapshot(t, program)
					return hovered.activeID == child.ID && hovered.active == 0 && strings.Contains(ansi.Strip(hovered.content), "NestedReviewer")
				}, 3*time.Second, time.Millisecond)
				// This test selects canonical identities, not a moving row. The
				// independent animation/hit tests exercise transient coordinates.
				x, y = sidebarProgramPoint(t, hovered.content, "NestedReviewer")
				row := strings.Split(ansi.Strip(hovered.content), "\n")[y]
				require.Contains(t, row, "NestedReviewer", "click targets the currently painted canonical label")
				require.NotContains(t, row, "⌄", "genuine grandchild leaf has no branch action")
				program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
				var lastGrandchildSnapshot shellSnapshot
				clickX, clickY := x, y
				t.Cleanup(func() {
					if t.Failed() {
						labelPoint := func(content string) [2]int {
							for row, line := range strings.Split(ansi.Strip(content), "\n") {
								if prefix, _, found := strings.Cut(line, "NestedReviewer"); found {
									return [2]int{ansi.StringWidth(prefix), row}
								}
							}
							return [2]int{-1, -1}
						}
						clickRow := func(content string) string {
							rows := strings.Split(ansi.Strip(content), "\n")
							if clickY >= 0 && clickY < len(rows) {
								return rows[clickY]
							}
							return "<outside>"
						}
						t.Logf("grandchild phases selected(id=%q leases=%d ticks=%d row=%q) hovered(id=%q leases=%d ticks=%d row=%q) final(id=%q leases=%d ticks=%d row=%q)", selected.activeID, selected.active, selected.ticks, clickRow(selected.content), hovered.activeID, hovered.active, hovered.ticks, clickRow(hovered.content), lastGrandchildSnapshot.activeID, lastGrandchildSnapshot.active, lastGrandchildSnapshot.ticks, clickRow(lastGrandchildSnapshot.content))
						t.Logf("grandchild click=(%d,%d) hovered_label=%v final_label=%v selected=%q hovered=%q final_id=%q final_tabs=%q final_modal=%v final_overlay=%v final_focus=%v final_frame=%q", clickX, clickY, labelPoint(hovered.content), labelPoint(lastGrandchildSnapshot.content), selected.content, hovered.content, lastGrandchildSnapshot.activeID, lastGrandchildSnapshot.tabIDs, lastGrandchildSnapshot.open, lastGrandchildSnapshot.overlay, lastGrandchildSnapshot.focus, lastGrandchildSnapshot.content)
					}
				})
				require.Eventually(t, func() bool {
					lastGrandchildSnapshot = sidebarProgramSnapshot(t, program)
					return lastGrandchildSnapshot.activeID == grand.ID && lastGrandchildSnapshot.active == 0 &&
						strings.Contains(ansi.Strip(lastGrandchildSnapshot.content), "parent: worker")
				}, 3*time.Second, time.Millisecond)
				leaf := sidebarProgramSnapshot(t, program)
				require.NotContains(t, ansi.Strip(leaf.content), "NestedReviewer", "leaf has no self row")
				require.NotContains(t, ansi.Strip(leaf.content), "SelectedWorker")
				x, y = sidebarProgramPoint(t, leaf.content, "parent: worker")
				program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
				require.Eventually(t, func() bool {
					back := sidebarProgramSnapshot(t, program)
					return back.activeID == child.ID && back.active == 0 && strings.Contains(ansi.Strip(back.content), "parent: director")
				}, 3*time.Second, time.Millisecond)
			} else {
				require.NotContains(t, ansi.Strip(selected.content), "NestedReviewer")
			}
			back := sidebarProgramSnapshot(t, program)
			x, y = sidebarProgramPoint(t, back.content, "parent: director")
			program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			require.Eventually(t, func() bool {
				back := sidebarProgramSnapshot(t, program)
				return back.activeID == parent.ID && back.active == 0 && strings.Contains(ansi.Strip(back.content), "SelectedWorker")
			}, 3*time.Second, time.Millisecond)
		})
	}
}

var _ app.Services = (*sidebarAttachRuntime)(nil)

type sidebarModelHandle struct {
	*lifecycleHandle

	id          string
	discoveries atomic.Int32
}

func (h *sidebarModelHandle) ID() string { return h.id }

func (*sidebarModelHandle) AgentName() string { return "worker" }

func (*sidebarModelHandle) SetModel(context.Context, string) error { return nil }

func (h *sidebarModelHandle) Metadata() runtime.SessionMetadata {
	return runtime.SessionMetadata{SessionID: h.id, AgentName: "worker", Capabilities: runtime.SessionCapabilities{ModelSwitching: true}}
}

func (h *sidebarModelHandle) AvailableModels(context.Context) []runtime.ModelChoice {
	h.discoveries.Add(1)
	return []runtime.ModelChoice{{Name: "PickerChoice", Ref: "fixture/choice", Provider: "fixture", Model: "choice"}}
}

type sidebarModelSessions struct {
	*openSubagentSessions

	handle *sidebarModelHandle
}

func (s *sidebarModelSessions) SessionByID(string) (runtime.SessionHandle, error) {
	return s.handle, nil
}

func TestActualProgramSidebarExistingModelPicker(t *testing.T) {
	for _, position := range []messages.SidebarPosition{messages.SidebarRight, messages.SidebarTop, messages.SidebarBottom} {
		t.Run(string(position), func(t *testing.T) {
			sess := session.New(session.WithID("sidebar-actions-session"), session.WithAgentName("worker"), session.WithWorkingDir("/workspace/full/path/sidebar-workspace"))
			sess.Title = "ACTION-TAB"
			handle := &sidebarModelHandle{lifecycleHandle: &lifecycleHandle{id: sess.ID}, id: sess.ID}
			application := app.New(t.Context(), &sidebarModelSessions{openSubagentSessions: &openSubagentSessions{}, handle: handle}, sess, runtime.SessionBinding{AgentName: "worker"}, app.WithRuntimeServices(stubRuntime{}))
			require.Same(t, handle, application.SessionHandle(), "model-switch capability resolves only when initial SetModel reconciliation succeeds")
			root := newSidebarProgramRoot(t, application)
			root.layoutSettings.SidebarPosition = position
			root.chatPage.SetLayoutSettings(root.layoutSettings)
			root.resizeAll()
			root.editor.SetValue("DRAFT-UNCHANGED")
			program := startTestProgram(t, root, &shellProgramModel{root: root}, tea.WithOutput(&cacheProgramWriter{}))
			program.Send(runtime.TeamInfo([]runtime.AgentDetails{{Name: "worker", Model: "ACTIVE-MODEL-DISPLAY", Provider: "fixture-provider"}}, "worker"))
			require.Eventually(t, func() bool {
				s := sidebarProgramSnapshot(t, program)
				return s.active == 0 && strings.Contains(ansi.Strip(s.content), "ACTIVE-MODEL-DISPLAY")
			}, time.Second, time.Millisecond)
			for _, label := range []string{"ACTIVE-MODEL-DISPLAY", "fixture-provider"} {
				frame := sidebarProgramSnapshot(t, program)
				x, y := sidebarProgramPoint(t, frame.content, label)
				program.Send(tea.MouseMotionMsg{X: x, Y: y})
				require.Eventually(t, func() bool {
					s := sidebarProgramSnapshot(t, program)
					return s.active == 0 && s.content != frame.content
				}, time.Second, time.Millisecond, "model/provider hover settles through the shared clock")
				hovered := sidebarProgramSnapshot(t, program)
				require.NotEqual(t, frame.content, hovered.content, "model/provider hover routes through the real root")
				program.Send(tea.MouseMotionMsg{X: x, Y: y})
				require.Equal(t, hovered.content, sidebarProgramSnapshot(t, program).content, "same hover is visually stable")
				program.Send(tea.MouseMotionMsg{X: 0, Y: 30})
				require.Eventually(t, func() bool {
					s := sidebarProgramSnapshot(t, program)
					return s.active == 0 && s.content == frame.content
				}, time.Second, time.Millisecond, "leaving sidebar fades back to its idle frame")
				before := handle.discoveries.Load()
				program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
				diagnostic := sidebarProgramSnapshot(t, program)
				t.Logf("sidebar picker position=%s label=%q discoveries=%d modal=%t frame:\n%s", position, label, handle.discoveries.Load(), diagnostic.open, ansi.Strip(diagnostic.content))
				require.Eventually(t, func() bool {
					frame := sidebarProgramSnapshot(t, program)
					return frame.open && strings.Contains(ansi.Strip(frame.content), "PickerChoice")
				}, time.Second, time.Millisecond)
				require.Equal(t, before+1, handle.discoveries.Load(), "sidebar reuses existing asynchronous discovery once")
				require.Equal(t, "DRAFT-UNCHANGED", sidebarProgramSnapshot(t, program).editor)
				program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
				require.Eventually(t, func() bool { return !sidebarProgramSnapshot(t, program).open }, time.Second, time.Millisecond)
			}
		})
	}
}

func TestActualProgramSidebarMissingCanonicalRootDoesNotInventAttachTarget(t *testing.T) {
	for _, kind := range []string{"missing", "empty-node", "empty-identity"} {
		t.Run(kind, func(t *testing.T) {
			parent := session.New(session.WithID("missing-parent-full-session"), session.WithAgentName("director"))
			parent.Title = "KNOWN-PARENT"
			child := session.New(session.WithID("missing-child-full-session"), session.WithAgentName("worker"))
			child.Title = "EMPTY-CHILD"
			child.AddMessage(session.UserMessage("EMPTY-CANONICAL-CHILD"))
			node := subagent.NodeID("missing-full-canonical-node")
			if kind == "empty-node" {
				node = ""
			}
			tree := subagent.Snapshot{Root: subagent.SessionRootID(parent.ID), Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: subagent.SessionRootID(parent.ID), SessionID: parent.ID, Agent: "director"}}}}
			if kind == "empty-identity" {
				tree.Nodes[0].Children = []subagent.NodeSnapshot{{Node: subagent.Node{ID: node, SessionID: child.ID}}}
			}
			child.SetSubagentTree(&tree)
			services := &sidebarAttachRuntime{closeTabRuntime: newCloseTabRuntime("director", false), infos: map[subagent.NodeID]runtime.SubagentAttachInfo{}}
			info := runtime.SubagentAttachInfo{NodeID: node, Agent: "worker", Session: child, ParentAgent: "director", ParentSessionID: parent.ID}
			application := app.New(t.Context(), nil, child, runtime.SessionBinding{AgentName: "worker"}, app.WithRuntimeServices(services), app.WithSubagentAttach(info))
			root := newSidebarProgramRoot(t, application)
			parentApp := app.New(t.Context(), nil, parent, runtime.SessionBinding{AgentName: "director"}, app.WithRuntimeServices(services))
			_, err := root.supervisor.AddSession(t.Context(), parentApp, parent, "", nil)
			require.NoError(t, err)
			_, _ = root.Update(runtime.TeamInfo([]runtime.AgentDetails{{Name: "root"}, {Name: "worker"}}, "worker"))
			program := startTestProgram(t, root, &shellProgramModel{root: root}, tea.WithOutput(&cacheProgramWriter{}))
			root.supervisor.SetProgram(program)
			program.Send(runtime.SubagentTree(tree))
			selected := sidebarProgramSnapshot(t, program)
			require.Contains(t, ansi.Strip(selected.content), "EMPTY-CANONICAL-CHILD")
			x, y := sidebarProgramPoint(t, selected.content, "parent: director")
			require.NotContains(t, ansi.Strip(selected.content), "root", "attached missing identity must not borrow configured root")
			lookups := services.lookups.Load()
			// The next pane row used to offer a bogus configured-root attachment.
			program.Send(tea.MouseMotionMsg{X: x, Y: y + 1})
			program.Send(tea.MouseClickMsg{X: x, Y: y + 1, Button: tea.MouseLeft})
			after := sidebarProgramSnapshot(t, program)
			require.Equal(t, child.ID, after.activeID)
			require.Equal(t, lookups, services.lookups.Load())
			require.NotContains(t, ansi.Strip(after.content), "no session to open")
			require.NotContains(t, ansi.Strip(after.content), "Session not found")
			program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			require.Eventually(t, func() bool { return sidebarProgramSnapshot(t, program).activeID == parent.ID }, time.Second, time.Millisecond)
		})
	}
}

func TestRootSidebarDirectoryClickEmitsFullPathFeedback(t *testing.T) {
	sess := session.New(session.WithID("directory-action-session"), session.WithWorkingDir("/workspace/full/path/sidebar-workspace"))
	application := app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	root := newSidebarProgramRoot(t, application)
	root.editor.SetValue("DRAFT-UNCHANGED")
	x, y := sidebarProgramPoint(t, root.View().Content, "sidebar-workspace")
	_, cmd := root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.NotNil(t, cmd)
	sequence := reflect.ValueOf(cmd())
	require.Equal(t, reflect.Slice, sequence.Kind())
	require.Equal(t, reflect.TypeFor[tea.Cmd](), sequence.Type().Elem())
	require.Equal(t, 3, sequence.Len(), "directory click retains platform clipboard, OSC52, then feedback sequence")
	// Do not execute the platform clipboard closure in a host-independent test.
	require.False(t, sequence.Index(0).IsNil())
	osc52, ok := sequence.Index(1).Interface().(tea.Cmd)
	require.True(t, ok)
	require.Equal(t, tea.SetClipboard(sess.WorkingDir)(), osc52(), "terminal clipboard receives the full path")
	feedback, ok := sequence.Index(2).Interface().(tea.Cmd)
	require.True(t, ok)
	note, ok := feedback().(notification.ShowMsg)
	require.True(t, ok)
	require.Equal(t, notification.TypeSuccess, note.Type)
	require.Equal(t, "Working directory copied to clipboard: "+sess.WorkingDir, note.Text)
	_, _ = root.Update(note)
	require.Equal(t, "DRAFT-UNCHANGED", root.editor.Value(), "path feedback never enters composer")
	require.Contains(t, ansi.Strip(root.View().Content), sess.WorkingDir)
}

type sidebarDoubleClickMsg struct{ click tea.MouseClickMsg }

type sidebarDoubleClickProgram struct{ *shellProgramModel }

func (m *sidebarDoubleClickProgram) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if pair, ok := msg.(sidebarDoubleClickMsg); ok {
		// Both accepted clicks run before any View, tick, or motion can rebuild hit geometry.
		_, first := m.root.Update(pair.click)
		_, second := m.root.Update(pair.click)
		return m, tea.Batch(first, second)
	}
	_, cmd := m.shellProgramModel.Update(msg)
	return m, cmd
}

func TestActualProgramSidebarBranchToggleTwiceRetainsControlIdentity(t *testing.T) {
	sess := session.New(session.WithID("branch-root-session"), session.WithAgentName("root"))
	tree := subagent.Snapshot{Root: subagent.SessionRootID(sess.ID), Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: subagent.SessionRootID(sess.ID), Agent: "root", SessionID: sess.ID}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "branch-full-node", Agent: "worker", Name: "BranchWorker", SessionID: "branch-child", State: subagent.NodeIdle}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "leaf-full-node", Agent: "reviewer", Name: "VisibleNestedLeaf", SessionID: "branch-grand", State: subagent.NodeCompleted}}}}}}}}
	sess.SetSubagentTree(&tree)
	child := session.New(session.WithID("branch-child"), session.WithAgentName("worker"))
	child.SetSubagentTree(&tree)
	services := &sidebarAttachRuntime{closeTabRuntime: newCloseTabRuntime("root", false), infos: map[subagent.NodeID]runtime.SubagentAttachInfo{"branch-full-node": {NodeID: "branch-full-node", Session: child, Agent: "worker", ParentSessionID: sess.ID, ParentAgent: "root"}}}
	views := services.preparedViews(sess)
	application := app.New(t.Context(), views, sess, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(services))
	root := newSidebarProgramRoot(t, application)
	coalescer := tuiinput.NewMouseCoalescer()
	t.Cleanup(coalescer.Stop)
	program := startTestProgram(t, root, &sidebarDoubleClickProgram{shellProgramModel: &shellProgramModel{root: root}}, tea.WithOutput(&cacheProgramWriter{}), tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg { return coalescer.Filter(msg) }))
	coalescer.SetSender(program.Send)
	frame := expandProgramSubagents(t, program, "BranchWorker")
	x, y := sidebarProgramPoint(t, frame.content, "BranchWorker")
	program.Send(tea.MouseMotionMsg{X: x, Y: y})
	var index int
	var row string
	require.Eventually(t, func() bool {
		hovered := sidebarProgramSnapshot(t, program)
		row = ansi.Strip(strings.Split(hovered.content, "\n")[y])
		index = strings.LastIndex(row, "⌄")
		return index >= 0
	}, time.Second, time.Millisecond, "hover reveals trailing branch control without moving its name")
	chevronX := ansi.StringWidth(row[:index])
	require.Greater(t, chevronX, x)
	program.Send(tea.MouseMotionMsg{X: chevronX, Y: y})
	require.Eventually(t, func() bool {
		s := sidebarProgramSnapshot(t, program)
		cells := layoutTerminalCells(s.content)
		return s.active == 0 && cells[y][x].Style.Fg != nil && color.NRGBAModel.Convert(cells[y][x].Style.Fg) != color.NRGBAModel.Convert(layoutTerminalCells(frame.content)[y][x].Style.Fg)
	}, time.Second, time.Millisecond)
	hovered := sidebarProgramSnapshot(t, program)
	beforeCells, hoverCells := layoutTerminalCells(frame.content), layoutTerminalCells(hovered.content)
	require.NotEqual(t, beforeCells[y][x].Style.Fg, hoverCells[y][x].Style.Fg, "chevron hover highlights its own name")
	require.Equal(t, beforeCells[y][x].Style.Bg, hoverCells[y][x].Style.Bg)
	beforeLookups := services.lookups.Load()
	program.Send(sidebarDoubleClickMsg{click: tea.MouseClickMsg{X: chevronX, Y: y, Button: tea.MouseLeft}})
	after := sidebarProgramSnapshot(t, program)
	require.Contains(t, ansi.Strip(after.content), "VisibleNestedLeaf", "same-coordinate double toggle reopens branch")
	require.Equal(t, beforeLookups, services.lookups.Load(), "control cannot fall through to an attach request")
	require.NotContains(t, ansi.Strip(after.content), "no session to open")
	require.Equal(t, sess.ID, after.activeID)
	root.supervisor.SetProgram(program)
	program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.Eventually(t, func() bool { return sidebarProgramSnapshot(t, program).activeID == child.ID }, time.Second, time.Millisecond, "name remains the full canonical attach target after both toggles")
	require.Equal(t, beforeLookups+1, services.lookups.Load())
}
