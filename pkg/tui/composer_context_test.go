package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/tabbar"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service/tuistate"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestComposerContextRetargetsAcrossTabs(t *testing.T) {
	scheduler := &composerScheduler{rootImmediateScheduler: rootImmediateScheduler{now: time.Unix(1, 0)}}
	root, _, _ := harnessRoot(t, 96, 28, animation.NewRuntimeWithScheduler(scheduler))
	const first = "profile"
	add := func(name string) string {
		sess := session.New(session.WithTitle(name), session.WithAgentName("root"))
		application := app.New(t.Context(), stubRuntime{}, sess)
		return root.supervisor.AddSession(t.Context(), application, sess, "", nil)
	}
	second := add("Second")
	unknown := add("Unknown")
	zero := add("Zero")
	usage := func(id string, used int64) {
		root.Update(messages.RoutedMsg{SessionID: id, Inner: runtime.NewTokenUsageEvent(id, "root", &runtime.Usage{ContextLength: used, ContextLimit: 100})})
	}
	frame := func() string { return strings.Split(root.View().Content, "\n")[root.composerDock().stripTop] }
	label := func(want string) {
		t.Helper()
		require.Contains(t, ansi.Strip(frame()), "Context "+want)
	}
	settle := func() {
		for range 60 {
			if !root.ar.HasActive() {
				break
			}
			scheduler.step(t, root)
		}
		require.Zero(t, root.ar.ActiveCount())
	}
	switchBy := func(msg tea.Msg, target string) {
		root.View()
		_, cmd := root.Update(msg)
		switchMsg, ok := firstOfType[messages.SwitchTabMsg](collectMsgs(cmd))
		require.True(t, ok)
		require.Equal(t, target, switchMsg.SessionID)
		root.Update(switchMsg)
		root.Update(messages.TabsUpdatedMsg{})
		require.Equal(t, target, root.supervisor.ActiveID())
	}
	next := tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl}
	prev := tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl}
	usage(first, 60)
	usage(second, 35) // Pre-existing usage on a tab with no mounted page.
	usage(zero, 0)
	root.Update(messages.TabsUpdatedMsg{})
	settle()
	label("60%")

	row := ansi.Strip(strings.Split(root.View().Content, "\n")[root.composerDock().tabsTop])
	firstCell, _, found := strings.Cut(row, "Second")
	require.True(t, found)
	x := ansi.StringWidth(firstCell)
	y := root.composerDock().tabsTop
	h := &contextHarness{t: t, root: root, scheduler: scheduler}
	start := h.fill()
	from := h.filled()
	root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	switchBy(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft}, second)
	require.Equal(t, start, h.fill(), "the first frame keeps the outgoing tab's fill")
	label("35%")
	scheduler.step(t, root)
	down := h.filled()
	require.Less(t, down, from)
	label("35%")
	middle := h.fill()
	usage(second, 35)
	root.Update(messages.SwitchTabMsg{SessionID: second})
	require.Equal(t, middle, h.fill(), "an unchanged target must preserve the current fill")

	// Reverse before reaching 35%, then reverse again during the rise.
	switchBy(prev, first)
	require.Equal(t, middle, h.fill())
	label("60%")
	scheduler.step(t, root)
	require.Greater(t, h.filled(), down)
	require.Less(t, h.filled(), from)
	middle = h.fill()
	switchBy(next, second)
	require.Equal(t, middle, h.fill())
	label("35%")
	settle()
	label("35%")

	usage(first, 35)
	start = frame()
	switchBy(prev, first)
	require.Equal(t, start, frame(), "equal percentages must not replay the animation")
	require.Zero(t, root.ar.ActiveCount())

	switchBy(next, second)
	switchBy(next, unknown)
	label("—")
	settle()
	switchBy(next, zero)
	label("0%")
	switchBy(next, first)
	label("35%")
	require.Zero(t, h.filled())
	settle()
	from = h.filled()
	switchBy(prev, zero)
	label("0%")
	scheduler.step(t, root)
	require.Positive(t, h.filled())
	require.Less(t, h.filled(), from)
	settle()
	label("0%")
}

type contextHarness struct {
	t         *testing.T
	root      *appModel
	scheduler *composerScheduler
}

func newContextHarness(t *testing.T) *contextHarness {
	t.Helper()
	scheduler := &composerScheduler{rootImmediateScheduler: rootImmediateScheduler{now: time.Unix(1, 0)}}
	root, _, _ := harnessRoot(t, 96, 28, animation.NewRuntimeWithScheduler(scheduler))
	return &contextHarness{t: t, root: root, scheduler: scheduler}
}

// warmTab opens a live tab whose session has already loaded.
func (h *contextHarness) warmTab(title string) string {
	sess := session.New(session.WithTitle(title), session.WithAgentName("root"))
	application := app.New(h.t.Context(), stubRuntime{}, sess)
	return h.root.supervisor.AddSession(h.t.Context(), application, sess, "", nil)
}

// coldTab mirrors tab restore: a placeholder runtime whose saved session is
// only read from the store on first activation.
func (h *contextHarness) coldTab(input, output int64) (tabID, savedID string) {
	store := session.NewInMemorySessionStore()
	saved := session.New(session.WithTitle("Saved"), session.WithAgentName("root"))
	saved.AddMessage(session.UserMessage("saved conversation"))
	saved.SetUsage(input, output)
	require.NoError(h.t, store.AddSession(h.t.Context(), saved))
	placeholder := session.New(session.WithAgentName("root"))
	application := app.New(h.t.Context(), storeRuntime{store: store}, placeholder)
	tabID = h.root.supervisor.AddSession(h.t.Context(), application, placeholder, "", nil)
	h.root.ensureTab(tabID).pendingRestore = new(saved.ID)
	h.root.supervisor.SetRunnerTitle(tabID, "Saved")
	return tabID, saved.ID
}

func (h *contextHarness) route(tabID string, event tea.Msg) {
	h.root.Update(messages.RoutedMsg{SessionID: tabID, Inner: event})
}

func (h *contextHarness) usage(tabID, sessionID string, used, limit int64) {
	h.route(tabID, runtime.NewTokenUsageEvent(sessionID, "root", &runtime.Usage{ContextLength: used, ContextLimit: limit}))
}

func (h *contextHarness) switchTo(tabID string) {
	h.root.View()
	h.root.Update(messages.SwitchTabMsg{SessionID: tabID})
	h.root.Update(messages.TabsUpdatedMsg{})
	require.Equal(h.t, tabID, h.root.supervisor.ActiveID())
}

func (h *contextHarness) frame() string {
	return strings.Split(h.root.View().Content, "\n")[h.root.composerDock().stripTop]
}

// fill is the rendered fill and gradient, excluding the track and reading,
// whose widths follow the label.
func (h *contextHarness) fill() string {
	line := h.frame()
	line = line[:strings.LastIndex(line, "▄")+len("▄")]
	track, _, _ := strings.Cut(lipgloss.NewStyle().Background(styles.EditorBg).Foreground(styles.Background).Render("▄"), "▄")
	if i := strings.Index(line, track); i >= 0 {
		line = line[:i]
	}
	return line
}

// filled counts the rendered cells ahead of the unfilled track.
func (h *contextHarness) filled() int { return strings.Count(h.fill(), "▄") }

func filledComposerCells(line, track string) int {
	if i := strings.Index(line, track); i >= 0 {
		line = line[:i]
	}
	return strings.Count(line, "▄")
}

func (h *contextHarness) label() string {
	_, label, ok := strings.Cut(ansi.Strip(h.frame()), "Context ")
	require.True(h.t, ok)
	return strings.TrimSpace(label)
}

func (h *contextHarness) step() {
	h.t.Helper()
	h.scheduler.step(h.t, h.root)
}

func (h *contextHarness) settle() {
	h.t.Helper()
	for range 60 {
		if !h.root.ar.HasActive() {
			break
		}
		h.step()
	}
	require.Zero(h.t, h.root.ar.ActiveCount())
}

// requirePending asserts the selected session's usage is pending: the label
// is not a percentage and the bar still shows the held fill.
func (h *contextHarness) requirePending(heldCells int) {
	h.t.Helper()
	require.Equal(h.t, "—", h.label(), "pending usage is not labelled as a percentage")
	require.Positive(h.t, h.filled())
	require.Equal(h.t, heldCells, h.filled(), "pending keeps the last visible fill")
	require.Zero(h.t, h.root.ar.ActiveCount())
}

func TestComposerContextStripSpansEditorColumn(t *testing.T) {
	h := newContextHarness(t)
	h.root.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	freshID := h.warmTab("Fresh")
	// The track starts at the editor's left edge and the reading ends at its
	// right edge, one space after the track, whatever the number of digits.
	strip := func(reading string) string {
		label := " Context " + reading
		return "  " + strings.Repeat("▄", 76-ansi.StringWidth(label)) + label + "  "
	}
	require.Equal(t, strip("—"), ansi.Strip(h.frame()))
	for _, used := range []int64{0, 5, 42, 100} {
		h.usage("profile", "profile", used, 100)
		h.settle()
		require.Equal(t, strip(fmt.Sprintf("%d%%", used)), ansi.Strip(h.frame()))
	}
	h.switchTo(freshID)
	h.settle()
	require.Equal(t, strip("—"), ansi.Strip(h.frame()))
}

func TestComposerContextColdRestoreHoldsFillUntilOwnUsage(t *testing.T) {
	h := newContextHarness(t)
	h.usage("profile", "profile", 60, 100)
	h.settle()
	require.Equal(t, "60%", h.label())
	held, heldCells := h.fill(), h.filled()

	coldID, savedID := h.coldTab(20, 10)
	h.switchTo(coldID)
	require.Equal(t, savedID, h.root.application.Session().ID, "the saved session is attached on first activation")
	h.requirePending(heldCells)
	pending := h.frame()

	// Startup metadata without a context window, unrelated updates and a
	// late event from the replaced placeholder keep the bar pending.
	h.route(coldID, runtime.AgentInfo("root", "model", "", ""))
	h.root.Update(messages.TabsUpdatedMsg{})
	h.root.Update(tea.WindowSizeMsg{Width: 96, Height: 28})
	h.usage(coldID, "placeholder-session", 90, 100)
	require.Equal(t, pending, h.frame())

	// The restored session's own usage eases from the held fill.
	h.usage(coldID, savedID, 30, 100)
	require.Equal(t, held, h.fill())
	require.Equal(t, "30%", h.label())
	h.step()
	require.Less(t, h.filled(), heldCells)
	require.Equal(t, "30%", h.label())
	h.settle()
	require.Equal(t, "30%", h.label())
}

func TestComposerContextColdRestoreFromAgentInfo(t *testing.T) {
	h := newContextHarness(t)
	h.usage("profile", "profile", 60, 100)
	h.settle()
	held, heldCells := h.fill(), h.filled()

	coldID, _ := h.coldTab(20, 10)
	h.switchTo(coldID)
	h.requirePending(heldCells)

	// The startup context window confirms the restored session's own totals.
	h.route(coldID, runtime.AgentInfo("root", "model", "", "", 100))
	require.Equal(t, held, h.fill())
	require.Equal(t, "30%", h.label())
	h.step()
	require.Less(t, h.filled(), heldCells)
	h.settle()
	require.Equal(t, "30%", h.label())
}

func TestComposerContextInterruptedSwitchHoldsMidAnimation(t *testing.T) {
	h := newContextHarness(t)
	warmID := h.warmTab("Warm")
	h.usage("profile", "profile", 60, 100)
	h.usage(warmID, warmID, 35, 100)
	h.settle()
	from := h.filled()

	h.switchTo(warmID)
	h.step()
	mid := h.filled()
	require.Less(t, mid, from)
	require.Equal(t, "35%", h.label())
	held, heldCells := h.fill(), h.filled()

	coldID, savedID := h.coldTab(10, 10)
	h.switchTo(coldID)
	h.requirePending(heldCells)
	pending := h.frame()
	for len(h.scheduler.pending) > 0 {
		h.step() // stale ticks from the interrupted sweep
	}
	require.Equal(t, pending, h.frame())

	// Leave before the cold tab's usage is known: the next owner eases from
	// the held fill, and the late background usage does not drive the bar.
	h.switchTo("profile")
	require.Equal(t, held, h.fill())
	require.Equal(t, "60%", h.label())
	h.step()
	require.Greater(t, h.filled(), mid)
	h.usage(coldID, savedID, 20, 100)
	h.settle()
	require.Equal(t, "60%", h.label())
	settled := h.frame()
	settledFill, settledCells := h.fill(), h.filled()
	h.usage(coldID, savedID, 20, 100)
	require.Equal(t, settled, h.frame())
	require.Zero(t, h.root.ar.ActiveCount())

	// Returning is now a warm switch to the cold tab's own usage.
	h.switchTo(coldID)
	require.Equal(t, settledFill, h.fill())
	require.Equal(t, "20%", h.label())
	h.step()
	require.Less(t, h.filled(), settledCells)
	h.settle()
	require.Equal(t, "20%", h.label())
}

func TestComposerContextConfirmedZeroIsNotPending(t *testing.T) {
	h := newContextHarness(t)
	h.usage("profile", "profile", 40, 100)
	h.settle()
	held, heldCells := h.fill(), h.filled()

	coldID, savedID := h.coldTab(0, 0)
	h.switchTo(coldID)
	h.route(coldID, runtime.AgentInfo("root", "model", "", "", 100))
	h.requirePending(heldCells) // zero restored totals are not confirmed usage

	h.usage(coldID, savedID, 0, 100)
	require.Equal(t, held, h.fill())
	require.Equal(t, "0%", h.label())
	h.step()
	require.Positive(t, h.filled())
	require.Less(t, h.filled(), heldCells)
	h.settle()
	require.Equal(t, "0%", h.label())
	require.Zero(t, h.filled())
}

func TestComposerContextStartupWithoutPreviousFill(t *testing.T) {
	h := newContextHarness(t)
	require.Equal(t, "—", h.label())
	require.Zero(t, h.filled())
	h.root.Update(messages.TabsUpdatedMsg{})
	require.Equal(t, "—", h.label())
	require.Zero(t, h.filled())
	require.Zero(t, h.root.ar.ActiveCount())

	h.usage("profile", "profile", 40, 100)
	require.Equal(t, "40%", h.label(), "the first reading is shown before the fill sweeps")
	require.Zero(t, h.filled())
	h.step()
	require.Positive(t, h.filled())
	require.Equal(t, "40%", h.label())
	h.settle()
	require.Equal(t, "40%", h.label())
}

func TestComposerContextEmptyConversationEasesToEmptyTrack(t *testing.T) {
	h := newContextHarness(t)
	warmID := h.warmTab("Warm")
	freshID := h.warmTab("Fresh")
	h.usage("profile", "profile", 60, 100)
	h.usage(warmID, warmID, 35, 100)
	h.settle()

	// Interrupt a sweep: the empty conversation eases from the on-screen fill.
	h.switchTo(warmID)
	h.step()
	held, heldCells := h.fill(), h.filled()
	h.switchTo(freshID)
	require.Equal(t, held, h.fill(), "the first frame keeps the outgoing fill")
	require.Equal(t, "—", h.label(), "an empty conversation has no usage to wait for")
	h.step()
	require.Positive(t, h.filled())
	require.Less(t, h.filled(), heldCells)

	// Reverse mid-sweep, then return and let the empty track settle.
	middle, middleCells := h.fill(), h.filled()
	h.switchTo("profile")
	require.Equal(t, middle, h.fill())
	require.Equal(t, "60%", h.label())
	h.step()
	require.Greater(t, h.filled(), middleCells)
	h.switchTo(freshID)
	h.settle()
	require.Equal(t, "—", h.label())
	require.Zero(t, h.filled())
}

func TestComposerContextUsageWithoutWindowIsNotPending(t *testing.T) {
	h := newContextHarness(t)
	h.usage("profile", "profile", 60, 100)
	h.settle()
	heldCells := h.filled()

	coldID, savedID := h.coldTab(20, 10)
	h.switchTo(coldID)
	h.requirePending(heldCells)

	// Harness agents report usage without a context window.
	h.usage(coldID, savedID, 30, 0)
	require.Equal(t, "—", h.label())
	require.Equal(t, heldCells, h.filled())
	h.step()
	require.Less(t, h.filled(), heldCells)
	h.settle()
	require.Equal(t, "—", h.label())
	require.Zero(t, h.filled())
}

// Emit the same types of startup events as a local runtime, without a provider
// call or allowing the placeholder startup to stand in for the saved session.
type contextRestoreRuntime struct{ storeRuntime }

func (contextRestoreRuntime) EmitStartupInfo(_ context.Context, sess *session.Session, sink runtime.EventSink) {
	input, output := sess.Usage()
	if input+output == 0 {
		return
	}
	sink.Emit(runtime.AgentInfo("root", "test/model", "", "", 100))
	sink.Emit(runtime.NewTokenUsageEvent(sess.ID, "root", runtime.SessionUsage(sess, 100)))
}

func TestComposerContextAgreesOnPersistedStartupRestore(t *testing.T) {
	setupSettingsConfigTest(t)
	paths.SetDataDir(t.TempDir())
	t.Cleanup(func() { paths.SetDataDir("") })
	require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
		cfg.Settings = &userconfig.Settings{RestoreTabs: new(true)}
		return nil
	}))
	ctx := t.Context()
	sessions := session.NewInMemorySessionStore()
	saved := session.New(session.WithAgentName("root"), session.WithTitle("Saved"))
	saved.AddMessage(session.UserMessage("synthetic restore conversation"))
	saved.SetUsage(20, 10)
	require.NoError(t, sessions.AddSession(ctx, saved))
	tabs, err := tuistate.New(ctx)
	require.NoError(t, err)
	require.NoError(t, tabs.AddTab(ctx, saved.ID, ""))
	require.NoError(t, tabs.SetActiveTab(ctx, saved.ID))
	require.NoError(t, tabs.ToggleSidebarCollapsed(ctx, saved.ID))
	require.NoError(t, tabs.Close())

	placeholder := session.New(session.WithAgentName("root"))
	a := app.New(ctx, contextRestoreRuntime{storeRuntime{store: sessions}}, placeholder)
	scheduler := &composerScheduler{rootImmediateScheduler: rootImmediateScheduler{now: time.Unix(1, 0)}}
	root := New(ctx, nil, a, "", func() {}, func(m *appModel) {
		m.ar = animation.NewRuntimeWithScheduler(scheduler)
		m.tabBar = tabbar.New(m.ar, userconfig.Get().GetTabTitleMaxLength())
	}).(*appModel)
	t.Cleanup(root.Shutdown)
	require.Equal(t, saved.ID, *root.activeTab.pendingRestore)
	startup := make(chan tea.Msg, 2)
	ready := make(chan struct{})
	go a.SubscribeReliable(ctx, func(msg tea.Msg) { startup <- msg }, app.WithSubscriptionReady(func() { close(ready) }))
	<-ready
	root.Init()
	root.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	require.Equal(t, saved.ID, root.application.Session().ID)
	require.Contains(t, ansi.Strip(root.View().Content), "synthetic restore conversation")
	h := &contextHarness{t: t, root: root, scheduler: scheduler}
	require.Equal(t, "—", h.label())
	nextStartup := func() tea.Msg {
		t.Helper()
		select {
		case msg := <-startup:
			return msg
		case <-time.After(time.Second):
			t.Fatal("missing startup event for restored session")
			return nil
		}
	}
	info := nextStartup()
	require.IsType(t, &runtime.AgentInfoEvent{}, info)
	h.route(placeholder.ID, info)
	h.requireReadouts("30%", "30%")
	settled := h.filled()
	require.Equal(t, 31, settled)
	usage := nextStartup()
	require.Equal(t, saved.ID, usage.(*runtime.TokenUsageEvent).SessionID)
	h.route(placeholder.ID, usage)
	require.Equal(t, settled, h.filled(), "the matching snapshot does not start a sweep")
	require.Zero(t, root.ar.ActiveCount())

	h.usage(placeholder.ID, saved.ID, 25, 200)
	h.requireReadouts("12%", "12%")
	require.Equal(t, settled, h.filled(), "retargeting does not move the fill before the first tick")
	for range 60 {
		if !root.ar.HasActive() {
			break
		}
		h.step()
		h.requireReadouts("12%", "12%")
	}
	require.Zero(t, root.ar.ActiveCount())
	t.Logf("25/200: rendered sidebar (12%%), composer Context 12%%; gradient cells: %d → %d", settled, h.filled())
}

func (h *contextHarness) requireReadouts(sidebar, composer string) {
	h.t.Helper()
	require.Contains(h.t, ansi.Strip(h.root.View().Content), "("+sidebar+")")
	require.Equal(h.t, composer, h.label())
}

func TestComposerContextAgreesOnSessionAndWindowChanges(t *testing.T) {
	h := newContextHarness(t)
	h.root.hideSidebar = false
	h.root.activeTab.chatPage = chat.New(h.root.ar, t.Context(), h.root.application, h.root.activeTab.sessionState)
	h.root.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	const root, child, detached = "profile", "child", "detached"
	h.route(root, runtime.StreamStarted(root, "root"))
	h.route(root, runtime.NewTokenUsageEvent(root, "root", &runtime.Usage{InputTokens: 20, OutputTokens: 10, ContextLength: 30, ContextLimit: 100}))
	h.requireReadouts("30%", "30%")
	h.route(root, runtime.AgentInfo("root", "test/model", "", "", 100))
	h.route(root, runtime.AgentInfo("root", "test/model", "", "", 200))
	h.requireReadouts("15%", "15%")
	h.route(root, runtime.NewTokenUsageEvent("", "root", &runtime.Usage{InputTokens: 99, ContextLength: 99}))
	h.requireReadouts("15%", "15%")
	input, output := h.root.application.Session().Usage()
	require.Equal(t, int64(20), input)
	require.Equal(t, int64(10), output)

	// Reusing an agent name in distinct sessions does not alias the readings.
	h.route(root, runtime.StreamStarted(child, "root"))
	require.Equal(t, "—", h.label())
	h.usage(root, child, 50, 100)
	h.requireReadouts("50%", "50%")
	h.usage(root, detached, 98, 100)
	h.requireReadouts("50%", "50%")
	h.route(root, runtime.StreamStopped(child, "root", "normal"))
	h.requireReadouts("15%", "15%")
	h.route(root, runtime.StreamStopped(root, "root", "normal"))
	h.route(root, runtime.AgentInfo("other", "test/larger", "", "", 300))
	h.requireReadouts("10%", "10%")
	h.usage(root, root, 130, 100)
	h.requireReadouts("130%", "100%")
}

type composerRestoreProgram struct {
	*appModel

	mu           sync.Mutex
	latest       tea.View
	idle         bool
	firstReading tea.View
}

func (m *composerRestoreProgram) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	_, cmd := m.appModel.Update(msg)
	return m, cmd
}

func (m *composerRestoreProgram) View() tea.View {
	view := m.appModel.View()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.latest = view
	m.idle = !m.ar.HasActive()
	if m.firstReading.Content == "" && strings.Contains(ansi.Strip(view.Content), "Context 6%") {
		m.firstReading = view
	}
	return view
}

func (m *composerRestoreProgram) frames() (latest, first tea.View, idle bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.latest, m.firstReading, m.idle
}

func TestComposerContextIdleStartupRestore(t *testing.T) {
	setupSettingsConfigTest(t)
	paths.SetDataDir(t.TempDir())
	t.Cleanup(func() { paths.SetDataDir("") })
	require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
		cfg.Settings = &userconfig.Settings{RestoreTabs: new(true)}
		return nil
	}))
	ctx := t.Context()
	sessionsPath := filepath.Join(paths.GetDataDir(), "sessions.db")
	sessions, err := sqlitestore.New(ctx, sessionsPath)
	require.NoError(t, err)
	tabs, err := tuistate.New(ctx)
	require.NoError(t, err)
	for i, tab := range []struct {
		title string
		used  int64
	}{{"Left", 91}, {"Middle", 6}, {"Right", 91}} {
		saved := session.New(session.WithAgentName("root"), session.WithTitle(tab.title))
		saved.AddMessage(session.UserMessage(saved.Title + " synthetic restore conversation"))
		saved.SetUsage(tab.used, 0)
		require.NoError(t, sessions.AddSession(ctx, saved))
		require.NoError(t, tabs.AddTab(ctx, saved.ID, ""))
		if i == 1 {
			require.NoError(t, tabs.SetActiveTab(ctx, saved.ID))
		}
	}
	require.NoError(t, tabs.Close())
	require.NoError(t, sessions.Close())
	sessions, err = sqlitestore.New(ctx, sessionsPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sessions.Close()) })

	spawn := func(ctx context.Context, _ string) (*app.App, *session.Session, func(), error) {
		placeholder := session.New(session.WithAgentName("root"))
		return app.New(ctx, contextRestoreRuntime{storeRuntime{store: sessions}}, placeholder), placeholder, func() {}, nil
	}
	a, _, cleanup, err := spawn(ctx, "")
	require.NoError(t, err)
	root := New(ctx, spawn, a, "", cleanup, WithStartupBannerAnimation()).(*appModel)
	t.Cleanup(root.Shutdown)
	model := &composerRestoreProgram{appModel: root}
	writer := &cacheProgramWriter{}
	program := tea.NewProgram(model, tea.WithInput(nil), tea.WithOutput(writer), tea.WithColorProfile(colorprofile.TrueColor), tea.WithWindowSize(120, 40), tea.WithoutSignals())
	root.SetProgram(program)
	done := make(chan error, 1)
	go func() { _, err := program.Run(); done <- err }()
	t.Cleanup(func() {
		program.Quit()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			program.Kill()
			t.Error("program did not exit")
		}
	})

	type meter struct {
		x, y, track, filled int
		gradient            string
	}
	track, _, _ := strings.Cut(lipgloss.NewStyle().Background(styles.EditorBg).Foreground(styles.Background).Render("▄"), "▄")
	read := func(view tea.View, label string) (meter, bool) {
		for y, row := range strings.Split(view.Content, "\n") {
			plain := ansi.Strip(row)
			if !strings.HasSuffix(strings.TrimSpace(plain), "Context "+label) {
				continue
			}
			fill := row[:strings.LastIndex(row, "▄")+len("▄")]
			return meter{strings.Index(plain, "▄"), y, strings.Count(plain, "▄"), filledComposerCells(fill, track), fill}, true
		}
		return meter{}, false
	}
	settled := func(label, title string) meter {
		t.Helper()
		var result meter
		// Wait with no input for the real timer to settle.
		require.Eventually(t, func() bool {
			view, _, idle := model.frames()
			var ok bool
			result, ok = read(view, label)
			plain := ansi.Strip(view.Content)
			return idle && ok && strings.Contains(plain, "("+label+")") &&
				strings.Contains(plain, title+" synthetic restore conversation")
		}, 3*time.Second, 5*time.Millisecond)
		t.Logf("%s %s: origin=(%d,%d) track=%d tinted=%d active=%d", title, label, result.x, result.y, result.track, result.filled, root.ar.ActiveCount())
		return result
	}
	startup := settled("6%", "Middle")
	// Subsequent frames use incremental terminal writes rather than repeating the full label.
	require.Eventually(t, func() bool {
		return strings.Contains(ansi.Strip(writer.snapshot()), "Context 6%")
	}, 3*time.Second, 5*time.Millisecond)
	_, first, _ := model.frames()
	firstMeter, ok := read(first, "6%")
	require.True(t, ok)
	t.Logf("first 6%% frame: origin=(%d,%d) track=%d tinted=%d", firstMeter.x, firstMeter.y, firstMeter.track, firstMeter.filled)
	program.Send(tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl})
	right := settled("91%", "Right")
	program.Send(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	returned := settled("6%", "Middle")
	require.Equal(t, startup, returned)
	require.Equal(t, startup, firstMeter, "first visible reading paints the restored gradient without a sweep")
	require.Equal(t, 6, startup.filled)
	require.Equal(t, 94, right.filled)
}
