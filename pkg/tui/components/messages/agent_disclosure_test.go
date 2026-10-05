package messages

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/subagentindex"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type disclosureClock struct{ now time.Time }

func (c *disclosureClock) Now() time.Time { return c.now }
func (c *disclosureClock) Tick(delay time.Duration, create func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg { c.now = c.now.Add(delay); return create(c.now) }
}

func TestAgentDisclosureTicksInvalidateTranscriptAndKeyboardToggle(t *testing.T) {
	ar := animation.NewRuntimeWithScheduler(&disclosureClock{now: time.Unix(1, 0)})
	m := newModel(ar, 100, 10, &service.SessionState{}, subagentindex.New())
	t.Cleanup(m.StopAnimations)
	input := session.UserMessage(strings.Repeat("literal body row\n", 20))
	input.InputOrigin, input.SenderName, input.SenderID = session.InputOriginAgent, "director", "parent-session"
	m.AddInputMessage(input, 0)
	m.View()
	m.selectedMessageIndex = 0
	for _, code := range []rune{tea.KeyEnter, tea.KeySpace} {
		m.handleKeyPress(tea.KeyPressMsg{Code: code})
		require.False(t, m.itemNeedsTick(0), "unfocused transcript cannot steal composer keys")
	}
	m.focused = true
	m.handleKeyPress(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.True(t, m.itemNeedsTick(0))
	require.NotContains(t, ansi.Strip(m.View()), "literal body")
	heights := map[int]bool{}
	for m.itemNeedsTick(0) {
		cmd := ar.Continue()
		require.NotNil(t, cmd)
		tick, ok := ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		m.Update(tick)
		m.View()
		heights[m.totalHeight] = true
	}
	require.Greater(t, len(heights), 2, "transcript follows multiple intermediate heights")
	require.Contains(t, ansi.Strip(strings.Join(m.renderedLines, "\n")), "literal body")
	m.scrollOffset, m.userHasScrolled = 2, true
	m.handleKeyPress(tea.KeyPressMsg{Code: tea.KeySpace})
	require.True(t, m.itemNeedsTick(0))
	for m.itemNeedsTick(0) {
		tick, ok := ar.Accept(ar.Continue()().(animation.TickMsg))
		require.True(t, ok)
		m.Update(tick)
		m.View()
	}
	require.NotContains(t, ansi.Strip(m.View()), "literal body")
	require.LessOrEqual(t, m.scrollOffset, max(0, m.totalScrollableHeight()-m.height))
	require.Equal(t, input.Message.Content, m.messages[0].ReceivedBody)
	require.Equal(t, input.Message.Content, copyableMessageContent(m.messages[0]))
}

func TestSendRequestNestedChevronAndStatusReplacement(t *testing.T) {
	ar := animation.NewRuntime()
	m := newModel(ar, 100, 40, &service.SessionState{}, subagentindex.New())
	t.Cleanup(m.StopAnimations)
	m.AppendReasoning("root", "Thinking")
	call := tools.ToolCall{ID: "send", Function: tools.FunctionCall{Name: subagent.ToolSendMessage, Arguments: `{"to":"parent","message":"nested request"}`}}
	m.AddOrUpdateToolCall("root", call, tools.Tool{}, types.ToolStatusCompleted)
	block := m.views[0].(interface {
		Toggle()
		IsToggleAt(line, col int) bool
		ToggleAt(line, col int)
	})
	block.Toggle()
	m.invalidateItem(0)
	m.ensureAllItemsRendered()
	found := false
	for y, line := range m.renderedLines {
		plain := strings.TrimRight(ansi.Strip(line), " ")
		if strings.HasSuffix(plain, "Messaged parent >") {
			col := ansi.StringWidth(plain) - 1
			require.True(t, block.IsToggleAt(y, col))
			block.ToggleAt(y, col)
			found = true
			break
		}
	}
	require.True(t, found)
	require.True(t, m.itemNeedsTick(0))
	// Child identity remains navigation, not the disclosure target.
	for y, line := range m.renderedLines {
		plain := ansi.Strip(line)
		if pos := strings.Index(plain, "Messaged parent"); pos >= 0 {
			require.False(t, block.IsToggleAt(y, pos+len("Messaged ")))
		}
	}
	// Collapsing the outer block releases the hidden child's transition.
	block.Toggle()
	require.False(t, m.itemNeedsTick(0))
	require.Zero(t, ar.ActiveCount())
}
