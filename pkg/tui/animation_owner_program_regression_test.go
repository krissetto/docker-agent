package tui

import (
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	chatmsg "github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/tabbar"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

// Only executed commands enter pending. Merely creating (and then losing) a
// timer command must NOT make a frame available to this actual-program harness.
// The gate controls delivery, not Runtime.Accept, registration, or continuation.
type ownerProgramScheduler struct {
	mu          sync.Mutex
	now         time.Time
	allocations int
	pending     chan chan struct{}
	stop        chan struct{}
}

func (s *ownerProgramScheduler) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now
}

func (s *ownerProgramScheduler) Tick(delay time.Duration, create func(time.Time) tea.Msg) tea.Cmd {
	s.mu.Lock()
	s.allocations++
	s.mu.Unlock()
	return func() tea.Msg {
		release := make(chan struct{})
		select {
		case s.pending <- release:
		case <-s.stop:
			return nil
		}
		select {
		case <-release:
		case <-s.stop:
			return nil
		}
		s.mu.Lock()
		s.now = s.now.Add(delay)
		now := s.now
		s.mu.Unlock()
		return create(now)
	}
}

func (s *ownerProgramScheduler) allocated() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allocations
}

type ownerProgramFrame struct {
	active  int32
	slack   int
	focus   string
	tab     string
	content string
	ticks   int
	tabs    int
	hidden  bool
}

type ownerProgramModel struct {
	root    *appModel
	initial tea.Cmd
	mu      sync.Mutex
	frame   ownerProgramFrame
}

// Like the existing actual-program harnesses, startup omits external watchers
// and provider initialization. All fixture page Init commands are preserved;
// Bubble Tea's real WindowSizeMsg enters the root's owner scheduling boundary.
func (m *ownerProgramModel) Init() tea.Cmd { return m.initial }

func (m *ownerProgramModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	before := m.root.ar.Now()
	_, cmd := m.root.Update(msg)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, tick := msg.(animation.TickMsg); tick && before != m.root.ar.Now() {
		m.frame.ticks++
	}
	if _, tabs := msg.(messages.TabsUpdatedMsg); tabs {
		m.frame.tabs++
	}
	m.record()
	return m, cmd
}

func (m *ownerProgramModel) View() tea.View {
	view := m.root.View()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.frame.content = view.Content
	m.record()
	return view
}

func (m *ownerProgramModel) record() {
	m.frame.active = m.root.ar.ActiveCount()
	m.frame.focus = m.root.supervisor.ActiveID()
	m.frame.tab = m.root.tabBar.View()
	// Read-only observation of real retained state: no unsafe, field writes,
	// fake message list, or animation registration in the fixture.
	page := reflect.ValueOf(m.root.chatPages["profile"]).Elem()
	list := page.FieldByName("messages").Elem().Elem()
	m.frame.slack = int(list.FieldByName("bottomSlack").Int())
	m.frame.hidden = page.FieldByName("presentationHidden").Bool()
}

func (m *ownerProgramModel) snapshot() ownerProgramFrame {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.frame
}

func newOwnerProgram(t *testing.T) (*ownerProgramModel, *ownerProgramScheduler, *tea.Program) {
	t.Helper()
	scheduler := &ownerProgramScheduler{now: time.Unix(1, 0), pending: make(chan chan struct{}, 16), stop: make(chan struct{})}
	root, _, _ := harnessRoot(t, 156, 48, animation.NewRuntimeWithScheduler(scheduler))
	// harnessRoot replaces the runtime after New. Give the REAL tabbar the
	// same runtime as the chat pages instead of its old wall-clock runtime.
	root.tabBar = tabbar.New(root.ar, 20)
	root.sessionState.SetExpandThinking(true)
	root.application.Session().Messages = []session.Item{session.NewMessageItem(&session.Message{
		AgentName: "root",
		Message:   chatmsg.Message{Role: chatmsg.MessageRoleAssistant, Content: "Completed answer.", ReasoningContent: strings.Repeat("A completed reasoning paragraph.\n\n", 8)},
	})}
	initial := root.chatPage.Init()
	root.editors["profile"] = root.editor
	second := &session.Session{ID: "second", Title: "B"}
	a := app.New(t.Context(), nil, second, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	_, err := root.supervisor.AddSession(t.Context(), a, second, "", nil)
	require.NoError(t, err)
	page, state, editor, application := root.chatPage, root.sessionState, root.editor, root.application
	root.initSessionComponents("second", a, second)
	initial = tea.Batch(initial, root.chatPage.Init())
	root.chatPage, root.sessionState, root.editor, root.application = page, state, editor, application
	tabs, active := root.supervisor.GetTabs()
	initial = tea.Batch(initial, func() tea.Msg { return messages.TabsUpdatedMsg{Tabs: tabs, ActiveIdx: active} })
	model := &ownerProgramModel{root: root, initial: initial}
	program := startTestProgram(t, root, model, tea.WithOutput(io.Discard), tea.WithWindowSize(156, 48))
	// This is the production notifier, not filtered/fabricated switch snapshots.
	root.supervisor.SetProgram(program)
	t.Cleanup(func() { close(scheduler.stop) })
	require.Eventually(t, func() bool { f := model.snapshot(); return f.tabs > 0 && strings.Contains(f.content, "Thinking") }, 3*time.Second, time.Millisecond)
	return model, scheduler, program
}

func ownerProgramStep(t *testing.T, model *ownerProgramModel, scheduler *ownerProgramScheduler) {
	t.Helper()
	before := model.snapshot().ticks
	select {
	case release := <-scheduler.pending:
		close(release)
	case <-time.After(time.Second):
		t.Fatalf("active animation has no executed timer command: %+v; allocations=%d", model.snapshot(), scheduler.allocated())
	}
	require.Eventually(t, func() bool { return model.snapshot().ticks > before }, time.Second, time.Millisecond, "delivered frame must reach the real root Accept path")
}

func ownerProgramSettle(t *testing.T, model *ownerProgramModel, scheduler *ownerProgramScheduler) {
	t.Helper()
	for range 120 {
		if model.snapshot().active == 0 {
			return
		}
		ownerProgramStep(t, model, scheduler)
	}
	t.Fatalf("finite animation did not settle: %+v", model.snapshot())
}

func ownerProgramPlus(t *testing.T, frame ownerProgramFrame) (int, int) {
	t.Helper()
	for y, line := range strings.Split(frame.content, "\n") {
		plain := ansi.Strip(line)
		if strings.Contains(plain, "profile") && strings.Contains(plain, "+") {
			i := strings.LastIndex(plain, "+")
			return ansi.StringWidth(plain[:i]), y
		}
	}
	t.Fatal("cannot find real tabbar plus button")
	return 0, 0
}

func TestActualProgramKeyboardReturnResumesRetainedSlackAndIndependentHover(t *testing.T) {
	model, scheduler, program := newOwnerProgram(t)
	ownerProgramSettle(t, model, scheduler)
	program.Send(tea.KeyboardEnhancementsMsg{Flags: 1})
	before := model.snapshot()
	require.Zero(t, before.active, "completed short-title tabs begin idle")
	require.Zero(t, before.slack)

	// A real disclosure click shrinks completed reasoning. View's production
	// updateScrollState creates slack; this does not seed private state or run
	// a provider/tool, and needs no wall-clock reasoning fade delay.
	x, y := -1, -1
	for row, line := range strings.Split(before.content, "\n") {
		plain := ansi.Strip(line)
		if i := strings.Index(plain, "Thinking"); i >= 0 {
			x, y = ansi.StringWidth(plain[:i]), row
			break
		}
	}
	require.GreaterOrEqual(t, x, 0)
	program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	program.Send(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.Eventually(t, func() bool { return model.snapshot().slack > 0 }, time.Second, time.Millisecond, "real content shrink must retain bottom slack")
	retained := model.snapshot().slack
	program.Send(tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl})
	require.Eventually(t, func() bool { f := model.snapshot(); return f.focus == "second" && f.hidden && f.tabs > before.tabs }, time.Second, time.Millisecond)
	ownerProgramSettle(t, model, scheduler)
	idle := model.snapshot()
	require.Equal(t, retained, idle.slack, "hidden transcript retains its scroll state")
	require.Zero(t, idle.active, "no running indicator, scrolling title, or hidden lease may rescue the return")
	allocations := scheduler.allocated()
	neverInTestLoop(t, func() bool { return scheduler.allocated() != allocations || len(scheduler.pending) != 0 }, 50*time.Millisecond, "idle hub must not allocate timers")

	program.Send(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	require.Eventually(t, func() bool { f := model.snapshot(); return f.focus == "profile" && !f.hidden && f.tabs > idle.tabs }, time.Second, time.Millisecond)
	returned := model.snapshot()
	require.Positive(t, returned.active, "showing retained slack reacquires its real subscription")
	require.Equal(t, retained, returned.slack, "no timer has been delivered yet")

	// Independent finite, visible owner: a real hover (not a running tab DTO
	// or synthetic rescue subscription). The pre-fix lost resume timer pins
	// the entire shared hub, so this transition cannot advance either.
	plusX, plusY := ownerProgramPlus(t, returned)
	program.Send(tea.MouseMotionMsg{X: plusX, Y: plusY})
	require.Eventually(t, func() bool { return model.snapshot().active >= 2 }, time.Second, time.Millisecond)
	hoverStart := model.snapshot()
	for range 5 {
		ownerProgramStep(t, model, scheduler)
	}
	advanced := model.snapshot()
	require.Less(t, advanced.slack, returned.slack, "retained transcript resumes its 14Hz decay on shared 60Hz elapsed time")
	require.NotEqual(t, hoverStart.tab, advanced.tab, "independent real plus hover advances without another input")
	ownerProgramSettle(t, model, scheduler)
	require.Zero(t, model.snapshot().slack)
	program.Send(tea.MouseMotionMsg{X: 0, Y: 0})
	require.Eventually(t, func() bool { return model.snapshot().active > 0 }, time.Second, time.Millisecond)
	ownerProgramSettle(t, model, scheduler)
	final := model.snapshot()
	allocations = scheduler.allocated()
	neverInTestLoop(t, func() bool {
		return model.snapshot().ticks != final.ticks || scheduler.allocated() != allocations || len(scheduler.pending) != 0
	}, 80*time.Millisecond, "settled owners must leave no idle timer chain")
}

// Expose only the sidebar seam on an otherwise real page: the unsupported
// split layout must return commands already collected before validation.
type ownerSidebarCommandPage struct {
	chat.Page

	delivered chan struct{}
}

func (p *ownerSidebarCommandPage) SetSplitSidebarSettings(*chat.SidebarSettings) tea.Cmd {
	return func() tea.Msg { close(p.delivered); return nil }
}

func (*ownerSidebarCommandPage) SplitSidebarSettings() (chat.SidebarSettings, bool) {
	return chat.SidebarSettings{}, false
}

func TestResizePanesUnsupportedLayoutPreservesCollectedSidebarCommand(t *testing.T) {
	root, _, _ := wallClockRoot(t, 156, 48)
	collected := &ownerSidebarCommandPage{Page: root.chatPage, delivered: make(chan struct{})}
	root.chatPages["unsupported"] = collected
	layout, ok := newSplitLayout("profile").Insert("unsupported", "profile", splitRight)
	require.True(t, ok)
	root.panes = layout
	require.True(t, root.panePresentationEnabled(), "focused real page enters the measured branch")
	require.False(t, root.supportsPanes(layout), "second page lacks the optional split seam")
	cmd := root.resizePanes()
	require.NotNil(t, cmd, "validation must not discard collected sidebar commands")
	// Tea itself executes the returned batch, including nested batches.
	model := &ownerSidebarCommandProgram{cmd: cmd}
	startTestProgram(t, root, model, tea.WithOutput(io.Discard))
	select {
	case <-collected.delivered:
	case <-time.After(time.Second):
		t.Fatal("collected sidebar command was not delivered by the program")
	}
}

type ownerSidebarCommandProgram struct {
	cmd tea.Cmd
}

func (m *ownerSidebarCommandProgram) Init() tea.Cmd                       { return m.cmd }
func (m *ownerSidebarCommandProgram) Update(tea.Msg) (tea.Model, tea.Cmd) { return m, nil }
func (m *ownerSidebarCommandProgram) View() tea.View {
	return tea.NewView("sidebar command preservation")
}
