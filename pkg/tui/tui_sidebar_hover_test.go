package tui

import (
	"context"
	"image/color"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type sidebarHoverFrame struct {
	content        string
	sidebar        string
	active         int32
	ticks          int
	modal, closing bool
}
type sidebarHoverProgram struct {
	root   *appModel
	mu     sync.Mutex
	frames []sidebarHoverFrame
	ticks  int
}

func (*sidebarHoverProgram) Init() tea.Cmd { return nil }
func (m *sidebarHoverProgram) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	before := m.root.ar.Now()
	_, cmd := m.root.Update(msg)
	if _, ok := msg.(animation.TickMsg); ok && m.root.ar.Now() != before {
		m.ticks++
	}
	return m, cmd
}

func (m *sidebarHoverProgram) View() tea.View {
	view := m.root.View()
	m.mu.Lock()
	defer m.mu.Unlock()
	sidebar := ""
	if page, ok := m.root.chatPage.(interface{ SidebarView() string }); ok {
		sidebar = page.SidebarView()
	}
	m.frames = append(m.frames, sidebarHoverFrame{sidebar: sidebar, content: view.Content, active: m.root.ar.ActiveCount(), ticks: m.ticks, modal: m.root.dialogMgr.Open(), closing: m.root.dialogMgr.Closing()})
	return view
}

func (m *sidebarHoverProgram) snapshot() []sidebarHoverFrame {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]sidebarHoverFrame(nil), m.frames...)
}

func assertSidebarHoverCells(t *testing.T, frames []sidebarHoverFrame, base string, x, y int) {
	t.Helper()
	expected := layoutTerminalCells(base)
	colors := map[color.NRGBA]bool{}
	for _, frame := range frames {
		cells := layoutTerminalCells(frame.content)
		require.Len(t, cells, len(expected), "hover never changes frame height")
		for row := range cells {
			require.Len(t, cells[row], len(expected[row]))
			for col := range cells[row] {
				require.Equal(t, expected[row][col].Content, cells[row][col].Content, "hover geometry row%d col%d", row, col)
				require.Equal(t, expected[row][col].Style.Bg, cells[row][col].Style.Bg, "hover must not paint a row background")
			}
		}
		fg := cells[y][x].Style.Fg
		require.NotNil(t, fg)
		colors[color.NRGBAModel.Convert(fg).(color.NRGBA)] = true
	}
	require.GreaterOrEqual(t, len(colors), 3, "shared elapsed hover publishes intermediate foreground colors")
}

func TestActualProgramSidebarHoverEntryExitCellsAndIdle(t *testing.T) {
	for _, position := range []messages.SidebarPosition{messages.SidebarRight, messages.SidebarTop, messages.SidebarBottom} {
		t.Run(string(position), func(t *testing.T) {
			sess := session.New(session.WithID("hover-session"), session.WithAgentName("worker"))
			sess.Title = "HOVER-TAB"
			application := app.New(t.Context(), nil, sess, runtime.SessionBinding{AgentName: "worker"}, app.WithRuntimeServices(stubRuntime{}))
			root := newSidebarProgramRoot(t, application)
			root.layoutSettings.SidebarPosition = position
			root.chatPage.SetLayoutSettings(root.layoutSettings)
			root.resizeAll()
			root.editor.Blur()
			root.focusedPanel = PanelContent
			model := &sidebarHoverProgram{root: root}
			writer := &cacheProgramWriter{}
			program := startTestProgram(t, root, model, tea.WithOutput(writer))
			program.Send(runtime.TeamInfo([]runtime.AgentDetails{{Name: "worker", Model: "HOVER-MODEL", Provider: "hover-provider"}}, "worker"))
			require.Eventually(t, func() bool {
				f := model.snapshot()
				return len(f) > 0 && f[len(f)-1].active == 0 && strings.Contains(ansi.Strip(f[len(f)-1].content), "HOVER-MODEL")
			}, time.Second, time.Millisecond)
			require.Eventually(t, func() bool { return len(model.snapshot()) > 0 }, time.Second, time.Millisecond)
			initial := model.snapshot()
			base := initial[len(initial)-1].content
			x, y := sidebarProgramPoint(t, base, "HOVER-MODEL")
			program.Send(tea.MouseMotionMsg{X: x, Y: y})
			require.Eventually(t, func() bool {
				frames := model.snapshot()
				last := frames[len(frames)-1]
				return last.active == 0 && last.content != base && last.ticks > 0
			}, time.Second, time.Millisecond)
			entered := model.snapshot()
			assertSidebarHoverCells(t, entered[len(initial):], base, x, y)
			settled := entered[len(entered)-1]
			select {
			case <-time.After(80 * time.Millisecond):
			case <-t.Context().Done():
				t.Fatal("cancelled waiting final flush")
			}
			writes := len(writer.snapshot())
			require.Never(t, func() bool {
				frames := model.snapshot()
				last := frames[len(frames)-1]
				return last.ticks != settled.ticks || len(writer.snapshot()) != writes
			}, 80*time.Millisecond, time.Millisecond, "stationary settled hover owns no ticking or terminal writes")
			program.Send(tea.MouseMotionMsg{X: 0, Y: 0})
			require.Eventually(t, func() bool {
				frames := model.snapshot()
				last := frames[len(frames)-1]
				return last.active == 0 && last.content == base
			}, time.Second, time.Millisecond)
			exited := model.snapshot()
			assertSidebarHoverCells(t, exited[len(entered):], base, x, y)
		})
	}
}

func TestSidebarHoverThemeRebasesAndHiddenPageReleasesOnlyHover(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	sess := session.New(session.WithID("hover-theme-session"), session.WithAgentName("worker"))
	application := app.New(t.Context(), nil, sess, runtime.SessionBinding{AgentName: "worker"}, app.WithRuntimeServices(stubRuntime{}))
	root := newSidebarProgramRoot(t, application)
	scheduler := &rootImmediateScheduler{now: time.Unix(1, 0)}
	root.ar.Stop()
	root.ar = animation.NewRuntimeWithScheduler(scheduler)
	root.initSessionComponents(sess.ID, application, sess)
	root.chatPage.Init()
	root.resizeAll()
	_, setupCmd := root.Update(runtime.TeamInfo([]runtime.AgentDetails{{Name: "worker", Model: "THEME-HOVER", Provider: "provider"}}, "worker"))
	setupMsgs := collectMsgs(setupCmd)
	for step := 0; step < 30 && root.ar.ActiveCount() > 0; step++ {
		var next []tea.Msg
		for _, msg := range setupMsgs {
			if _, ok := msg.(animation.TickMsg); ok {
				_, cmd := root.Update(msg)
				next = append(next, collectMsgs(cmd)...)
			}
		}
		setupMsgs = next
	}
	x, y := sidebarProgramPoint(t, root.View().Content, "THEME-HOVER")
	_, cmd := root.Update(tea.MouseMotionMsg{X: x, Y: y})
	pending := collectMsgs(cmd)
	for range 4 {
		var next []tea.Msg
		for _, msg := range pending {
			if _, ok := msg.(animation.TickMsg); ok {
				_, cmd := root.Update(msg)
				next = append(next, collectMsgs(cmd)...)
			}
		}
		pending = next
	}
	before := root.View().Content
	target := "default-light"
	if styles.CurrentTheme().Ref == target {
		target = "default"
	}
	theme, err := styles.LoadTheme(target)
	require.NoError(t, err)
	styles.ApplyTheme(theme)
	_, themeCmd := root.Update(messages.ThemeChangedMsg{})
	_ = collectMsgs(themeCmd)
	after := root.View().Content
	require.NotEqual(t, before, after)
	require.Contains(t, ansi.Strip(after), "THEME-HOVER")
	require.Equal(t, ansi.Strip(before), ansi.Strip(after), "theme changes palette, not hovered geometry")
	second := session.New(session.WithID("hover-next-session"))
	secondApp := app.New(t.Context(), nil, second, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	_, err = root.supervisor.AddSession(t.Context(), secondApp, second, "", nil)
	require.NoError(t, err)
	root.handleSwitchTab(second.ID)
	require.Zero(t, root.ar.ActiveCount(), "hidden sidebar hover cannot strand a shared runtime lease")
	require.NotContains(t, root.editor.Value(), "THEME-HOVER")
}

func TestActualProgramInitialTeamInfoColorsTabWithoutInput(t *testing.T) {
	styles.SetAgentOrder(nil)
	t.Cleanup(func() { styles.SetAgentOrder(nil) })
	root, _, _ := wallClockRoot(t, 120, 40)
	root.editor.Blur()
	root.focusedPanel = PanelContent
	root.sessionState.SetCurrentAgentName("root")
	root.syncTabAgents()
	root.viewCacheValid = false
	tabY := root.contentHeight + 1
	model := &sidebarHoverProgram{root: root}
	program := startTestProgram(t, root, model, tea.WithOutput(&cacheProgramWriter{}))
	require.Eventually(t, func() bool { return len(model.snapshot()) > 0 }, time.Second, time.Millisecond)
	initial := model.snapshot()
	before := initial[len(initial)-1].content
	tabRows := strings.Split(before, "\n")
	x := -1
	for col, cell := range chromeCells(tabRows[tabY]) {
		if cell.glyph == "R" {
			x = col
			break
		}
	}
	require.GreaterOrEqual(t, x, 0, "initial bound-agent pill exists before roster delivery")
	program.Send(runtime.TeamInfo([]runtime.AgentDetails{{Name: "root"}, {Name: "worker"}}, "root"))
	require.Eventually(t, func() bool {
		frames := model.snapshot()
		cells := layoutTerminalCells(frames[len(frames)-1].content)
		expected := styles.AgentIdentityStyle("root", false).GetForeground()
		return cells[tabY][x].Style.Bg != nil && color.NRGBAModel.Convert(cells[tabY][x].Style.Bg) == color.NRGBAModel.Convert(expected)
	}, time.Second, time.Millisecond, "TeamInfo alone refreshes the bound agent pill palette")
	after := model.snapshot()
	require.NotEqual(t, before, after[len(after)-1].content)
	require.Zero(t, after[len(after)-1].ticks, "first identity palette needs no animation or user-input wakeup")
}

func TestActualProgramForegroundDialogOcclusionFadesSidebarHoverWithoutMotion(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	root.hideSidebar = false
	root.initSessionComponents("profile", root.application, root.application.Session())
	root.chatPage.Init()
	root.resizeAll()
	root.editor.Blur()
	model := &shellProgramModel{root: root}
	program := startTestProgram(t, root, model, tea.WithOutput(&cacheProgramWriter{}))
	program.Send(runtime.TeamInfo([]runtime.AgentDetails{{Name: "root", Model: "OCCLUDED-MODEL", Provider: "provider"}}, "root"))
	require.Eventually(t, func() bool {
		s := sidebarProgramSnapshot(t, program)
		return s.active == 0 && strings.Contains(ansi.Strip(s.content), "OCCLUDED-MODEL")
	}, time.Second, time.Millisecond)
	initial := sidebarProgramSnapshot(t, program)
	x, y := sidebarProgramPoint(t, initial.content, "OCCLUDED-MODEL")
	program.Send(tea.MouseMotionMsg{X: x, Y: y})
	require.Eventually(t, func() bool {
		s := sidebarProgramSnapshot(t, program)
		return s.active == 0 && s.content != initial.content
	}, time.Second, time.Millisecond)
	program.Send(tea.KeyPressMsg{Code: 'h', Mod: tea.ModCtrl})
	require.Eventually(t, func() bool { return sidebarProgramSnapshot(t, program).open }, time.Second, time.Millisecond)
	// No pointer motion follows the opening dialog; accepted shared ticks must fade the occluded target.
	require.Eventually(t, func() bool { s := sidebarProgramSnapshot(t, program); return s.open && s.active == 0 }, time.Second, time.Millisecond)
	program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Eventually(t, func() bool { s := sidebarProgramSnapshot(t, program); return !s.open && s.active == 0 }, time.Second, time.Millisecond)
	returned := sidebarProgramSnapshot(t, program)
	require.Equal(t, initial.content, returned.content, "closed foreground dialog cannot restore stale underlying hover")
}

type occlusionContextHandle struct{ *sidebarModelHandle }

func (*occlusionContextHandle) ContextBreakdown(context.Context) (*runtime.ContextBreakdown, error) {
	return &runtime.ContextBreakdown{ContextLimit: 100}, nil
}

type occlusionSessions struct {
	*openSubagentSessions

	handle *occlusionContextHandle
}

func (s *occlusionSessions) SessionByID(string) (runtime.SessionHandle, error) { return s.handle, nil }

func TestActualProgramStationarySidebarActionClickFadesOccludedTarget(t *testing.T) {
	for _, label := range []string{"OCCLUSION-MODEL", "$", "◉"} {
		t.Run(label, func(t *testing.T) {
			sess := session.New(session.WithID("occlusion-action-session"), session.WithAgentName("worker"))
			sess.Title = "OCCLUSION-ACTIONS"
			handle := &occlusionContextHandle{sidebarModelHandle: &sidebarModelHandle{lifecycleHandle: &lifecycleHandle{id: sess.ID}, id: sess.ID}}
			application := app.New(t.Context(), &occlusionSessions{openSubagentSessions: &openSubagentSessions{}, handle: handle}, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
			root := newSidebarProgramRoot(t, application)
			root.editor.Blur()
			model := &sidebarHoverProgram{root: root}
			writer := &cacheProgramWriter{}
			program := startTestProgram(t, root, model, tea.WithOutput(writer))
			program.Send(runtime.TeamInfo([]runtime.AgentDetails{{Name: "worker", Model: "OCCLUSION-MODEL", Provider: "provider"}}, "worker"))
			require.Eventually(t, func() bool {
				f := model.snapshot()
				return len(f) > 0 && f[len(f)-1].active == 0 && strings.Contains(ansi.Strip(f[len(f)-1].content), "OCCLUSION-MODEL")
			}, time.Second, time.Millisecond)
			require.Eventually(t, func() bool { f := model.snapshot(); return len(f) > 0 && f[len(f)-1].active == 0 }, time.Second, time.Millisecond)
			frames := model.snapshot()
			base := frames[len(frames)-1]
			x, y := sidebarProgramPoint(t, base.content, label)
			localX, localY := sidebarProgramPoint(t, base.sidebar, label)
			program.Send(tea.MouseMotionMsg{X: x, Y: y})
			require.Eventually(t, func() bool {
				f := model.snapshot()
				last := f[len(f)-1]
				return last.active == 0 && last.sidebar != base.sidebar
			}, time.Second, time.Millisecond)
			frames = model.snapshot()
			entered := len(frames)
			program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			require.Eventually(t, func() bool {
				f := model.snapshot()
				last := f[len(f)-1]
				return last.modal && last.active == 0 && last.sidebar == base.sidebar
			}, time.Second, time.Millisecond, "stationary click fades the occluded sidebar without another input")
			frames = model.snapshot()
			colors := map[color.NRGBA]bool{}
			for _, frame := range frames[entered:] {
				if !frame.modal {
					continue
				}
				cells := layoutTerminalCells(frame.sidebar)
				fg := cells[localY][localX].Style.Fg
				if fg != nil {
					colors[color.NRGBAModel.Convert(fg).(color.NRGBA)] = true
				}
				require.Equal(t, layoutTerminalCells(base.sidebar)[localY][localX].Style.Bg, cells[localY][localX].Style.Bg)
			}
			require.GreaterOrEqual(t, len(colors), 3, "occlusion exits through intermediate foreground cells behind the modal")
			select {
			case <-time.After(80 * time.Millisecond):
			case <-t.Context().Done():
				t.Fatal("cancelled waiting terminalflush")
			}
			writes := len(writer.snapshot())
			require.Never(t, func() bool { return len(writer.snapshot()) != writes }, 80*time.Millisecond, time.Millisecond)
			program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
			require.Eventually(t, func() bool {
				f := model.snapshot()
				last := f[len(f)-1]
				return !last.modal && last.active == 0 && last.sidebar == base.sidebar
			}, time.Second, time.Millisecond, "modal close does not restore stale hover")
		})
	}
}
