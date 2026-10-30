package chat

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/todo"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func BenchmarkSidebarTodoScroll(b *testing.B) {
	for _, n := range []int{50, 300, 1000} {
		b.Run(fmt.Sprintf("todos=%d", n), func(b *testing.B) {
			ss := &service.SessionState{}
			ar := animation.NewRuntime()
			p := &chatPage{
				sidebar:      sidebar.New(ar, b.Context(), ss),
				messages:     messages.New(ar, ss),
				sessionState: ss,
			}
			p.SetSize(160, 50)
			p.messages.AppendToLastMessage("root", strings.Repeat("A message with **formatting** and `code` to fill the chat viewport.\n", 100))
			items := make([]todo.Todo, n)
			for i := range items {
				items[i] = todo.Todo{ID: strconv.Itoa(i), Status: "pending", Description: fmt.Sprintf("Task %d: refactor the rendering pipeline and add coverage for the new code path", i)}
			}
			if err := p.sidebar.SetTodos(&tools.ToolCallResult{Meta: items}); err != nil {
				b.Fatal(err)
			}
			p.messages.ScrollToBottom()
			_ = p.View()
			sl := p.computeSidebarLayout()
			wheel := msgtypes.WheelCoalescedMsg{Delta: 1, X: styles.AppPadding + sl.sidebarStartX + 5, Y: 10}
			steps := 0
			b.ReportAllocs()
			for b.Loop() {
				_, _ = p.Update(wheel)
				_ = p.View()
				steps++
				if steps == n*2 {
					wheel.Delta = -wheel.Delta
					steps = 0
				}
			}
		})
	}
}

func TestChatPaneCacheMatchesRender(t *testing.T) {
	t.Parallel()
	p := &chatPage{}
	for _, content := range []string{"", "short", "one\ntwo", "日本語 👨‍👩‍👧 é", "\x1b[31mred\x1b[0m", "\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\", "a\tb\r\nc", strings.Repeat("word ", 80)} {
		for _, width := range []int{1, 20, 100} {
			for _, height := range []int{1, 10} {
				want := styles.ChatStyle.Height(height).Width(width).Render(content)
				require.Equal(t, want, p.renderChatPane(content, width, height))
				require.Equal(t, want, p.renderChatPane(content, width, height), "cached formatting")
			}
		}
	}
}

func TestSidebarScrollKeepsChatPaneFresh(t *testing.T) {
	t.Parallel()
	for _, position := range []msgtypes.SidebarPosition{msgtypes.SidebarRight, msgtypes.SidebarLeft} {
		t.Run(string(position), func(t *testing.T) {
			p := newSidebarScrollPage(t, position)
			p.SetSize(p.width, p.height)
			require.NoError(t, p.sidebar.SetTodos(&tools.ToolCallResult{Meta: overflowingSidebarTodos()}))
			// Branch adaptation: the page reconciles sidebar layout after todo results (runtime_events.go:705).
			_ = p.reconcileSidebarLayout()
			child := &countingMessages{Model: p.messages}
			p.messages = child
			p.messages.AddUserMessage("first message")
			// Branch adaptation: the branch sidebar keeps todos collapsed and fades new rows in
			// on animation ticks. Expand the todo summary through its mouse click, then settle.
			expandSidebarTodos(t, p)
			child.views = 0 // count only the frames this test composes
			before := p.View()
			cached := p.chatPaneCache.rendered
			sl := p.computeSidebarLayout()
			_, _ = p.Update(msgtypes.WheelCoalescedMsg{Delta: 3, X: styles.AppPadding + sl.sidebarStartX + 5, Y: 10})
			after := p.View()
			require.NotEqual(t, before, after)
			require.Equal(t, 2, child.views, "child View side effects must still run")
			require.Equal(t, cached, p.chatPaneCache.rendered)
			require.Equal(t, uncachedVerticalView(p), after)

			p.messages.AddUserMessage("new message")
			got := p.View()
			require.Equal(t, uncachedVerticalView(p), got)
			require.NotEqual(t, cached, p.chatPaneCache.rendered)
			p.SetSize(140, 20)
			got = p.View()
			require.Equal(t, uncachedVerticalView(p), got)

			_, _ = p.Update(msgtypes.ThemeChangedMsg{})
			require.False(t, p.chatPaneCache.valid)
			got = p.View()
			require.Equal(t, uncachedVerticalView(p), got)
		})
	}
}

type countingMessages struct {
	messages.Model

	views int
}

func (m *countingMessages) View() string { m.views++; return m.Model.View() }
func (m *countingMessages) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	updated, cmd := m.Model.Update(msg)
	m.Model = updated.(messages.Model)
	return m, cmd
}

// Keep the original composition as a byte-for-byte oracle, including ANSI and padding.
func uncachedVerticalView(p *chatPage) string {
	sl := p.computeSidebarLayout()
	chatView := styles.ChatStyle.Height(sl.chatHeight).Width(sl.chatWidth).Render(p.messagesView(sl))
	toggle := p.renderSidebarHandle(sl.chatHeight)
	sidebarView := lipgloss.NewStyle().Width(sl.sidebarWidth-toggleColumnWidth).Height(sl.chatHeight).Align(lipgloss.Left, lipgloss.Top).Render(p.sidebar.View())
	var body string
	if sl.sidebarOnLeft {
		body = lipgloss.JoinHorizontal(lipgloss.Left, sidebarView, toggle, chatView)
	} else {
		body = lipgloss.JoinHorizontal(lipgloss.Left, chatView, toggle, sidebarView)
	}
	return styles.AppStyle.Height(p.height).Render(body)
}

// expandSidebarTodos clicks the collapsed todo summary and runs the placement animation to rest.
func expandSidebarTodos(t *testing.T, p *chatPage) {
	t.Helper()
	// Rows are built by View and fade in on ticks; the summary is not painted until they settle.
	_ = p.View()
	settleChatPageAnimation(t, p)
	sl := p.computeSidebarLayout()
	y := -1
	for i, line := range strings.Split(ansi.Strip(p.View()), "\n") {
		if strings.Contains(ansi.Strip(ansi.Cut(line, sl.sidebarStartX, sl.sidebarStartX+sl.sidebarWidth)), "todos") {
			y = i
			break
		}
	}
	require.GreaterOrEqual(t, y, 0, "collapsed todo summary is visible in the sidebar")
	x := styles.AppPadding + sl.sidebarStartX + 5
	_, _ = p.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	_, _ = p.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	_ = p.View()
	settleChatPageAnimation(t, p)
}

func settleChatPageAnimation(t *testing.T, p *chatPage) {
	t.Helper()
	for range 200 {
		if !p.ar.HasActive() {
			return
		}
		cmd := p.ar.Continue()
		require.NotNil(t, cmd)
		tick, ok := p.ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		_, _ = p.Update(tick)
	}
	t.Fatal("sidebar animation did not settle")
}

// newSidebarScrollPage shares one animation runtime between the page, sidebar and
// transcript, so the test can settle sidebar animation through the page.
func newSidebarScrollPage(t *testing.T, position msgtypes.SidebarPosition) *chatPage {
	t.Helper()
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	sessionState := &service.SessionState{}
	p := &chatPage{
		ar:                ar,
		sidebar:           sidebar.New(ar, t.Context(), sessionState),
		messages:          messages.New(ar, sessionState),
		sessionState:      sessionState,
		width:             160,
		height:            40,
		showStartupBanner: true,
	}
	p.layoutSettings = msgtypes.LayoutSettings{SidebarPosition: position}
	return p
}

// overflowingSidebarTodos gives the expanded todo list enough wrapped rows to scroll the sidebar.
func overflowingSidebarTodos() []todo.Todo {
	items := make([]todo.Todo, 12)
	for i := range items {
		items[i] = todo.Todo{ID: strconv.Itoa(i), Status: "pending", Description: fmt.Sprintf("long TODO %d %s", i, strings.Repeat("overflowing sidebar content ", 6))}
	}
	return items
}
