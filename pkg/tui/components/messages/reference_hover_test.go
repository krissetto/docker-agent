package messages

import (
	"fmt"
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
	"github.com/docker/docker-agent/pkg/tui/components/agentidentity"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/subagentindex"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type referenceScheduler struct {
	now  time.Time
	step time.Duration
}

func (s *referenceScheduler) Now() time.Time { return s.now }
func (s *referenceScheduler) Tick(_ time.Duration, create func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg { s.now = s.now.Add(s.step); return create(s.now) }
}
func referenceTick(t *testing.T, m *model) {
	t.Helper()
	cmd := m.ar.Continue()
	require.NotNil(t, cmd)
	tick, ok := m.ar.Accept(cmd().(animation.TickMsg))
	require.True(t, ok)
	m.Update(tick)
	require.True(t, tick.Dirty())
}
func referenceFixture(t *testing.T, kind string, width int) *model {
	t.Helper()
	index := subagentindex.New()
	index.Reset(subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "abcde-full-canonical-node", SessionID: "child-session", Agent: "worker", Name: "Café 👩‍💻 long worker 界"}}}})
	ar := animation.NewRuntimeWithScheduler(&referenceScheduler{now: time.Unix(1, 0), step: 50 * time.Millisecond})
	m := newModel(ar, width, 40, &service.SessionState{}, index)
	t.Cleanup(m.StopAnimations)
	switch kind {
	case "header", "reply", "completion":
		input := session.UserMessage("unchanged delivered **body**")
		input.InputOrigin, input.SenderID, input.SenderName = session.InputOriginAgent, "child-session", "worker"
		if kind == "reply" {
			input.InputMode = "steer"
		}
		if kind == "completion" {
			input.InputOrigin = session.InputOriginRuntime
		}
		m.AddInputMessage(input, 0)
	default:
		msg := types.ToolCallMessage("root", tools.ToolCall{ID: kind, Function: tools.FunctionCall{Name: kind, Arguments: `{"to":"abcde-full-canonical-node","subagent_id":"abcde-full-canonical-node"}`}}, tools.Tool{}, types.ToolStatusCompleted)
		if kind == subagent.ToolSpawnSubagent {
			msg.Content = `Spawned subagent "Café 👩‍💻 long worker 界" (abcde-full-canonical-node).`
		}
		m.messages = append(m.messages, msg)
		m.views = append(m.views, m.createToolCallView(msg))
	}
	m.SetPosition(7, 3)
	m.View()
	return m
}
func hoverLastReferenceCell(t *testing.T, m *model) {
	t.Helper()
	for y := len(m.renderedLines) - 1; y >= 0; y-- {
		spans := extractOSC8Links(m.renderedLines[y])
		for i := len(spans) - 1; i >= 0; i-- {
			span := spans[i]
			if span.url == agentidentity.Link {
				ref, ok := m.InputReferenceAt(m.xPos+span.endCol-1, m.yPos+y-m.scrollOffset)
				require.True(t, ok)
				require.Equal(t, "abcde-full-canonical-node", ref.ID)
				m.handleMouseMotion(tea.MouseMotionMsg{X: m.xPos + span.endCol - 1, Y: m.yPos + y - m.scrollOffset})
				return
			}
		}
	}
	t.Fatal("identity missing")
}
func TestReferenceHoverRenderedFramesShareFiniteTransition(t *testing.T) {
	for _, kind := range []string{"header", "reply", "completion", subagent.ToolSpawnSubagent, subagent.ToolSendMessage, subagent.ToolReadSubagent, subagent.ToolStopSubagent} {
		for _, width := range []int{80, 18} {
			t.Run(fmt.Sprintf("%s/%d", kind, width), func(t *testing.T) {
				m := referenceFixture(t, kind, width)
				base := m.View()
				raw := strings.Join(m.renderedLines, "\n")
				hoverLastReferenceCell(t, m)
				require.Equal(t, base, m.View(), "entry begins at base color")
				rebuilds, misses, renders := m.ResizeCacheStats()
				referenceTick(t, m)
				intermediate := m.View()
				require.NotEqual(t, base, intermediate)
				require.Equal(t, ansi.Strip(base), ansi.Strip(intermediate))
				require.InDelta(t, 1.0/3, m.referenceHoverValues[m.referenceHoverTarget].value, 1e-8)
				referenceTick(t, m)
				referenceTick(t, m)
				settled := m.View()
				require.NotEqual(t, intermediate, settled)
				require.False(t, m.ar.HasActive())
				require.Nil(t, m.ar.Continue(), "settled hover schedules no idle work")
				require.Equal(t, settled, m.View())
				require.Equal(t, raw, strings.Join(m.renderedLines, "\n"), "presentation never mutates cached lines")
				referenceLines := append([]string(nil), m.renderedLines...)
				m.applyReferenceHover(referenceLines, 0)
				nameRows := 0
				for i, line := range m.renderedLines {
					if strings.Contains(line, "id=docker-agent-identity-name") {
						nameRows++
						require.NotEqual(t, line, referenceLines[i], "suffix hover lights every wrapped name row")
					}
					for _, span := range extractOSC8Links(line) {
						if span.url == agentidentity.Link && !strings.Contains(ansi.Cut(line, span.startCol, span.endCol), "id=docker-agent-identity-name") {
							require.Equal(t, ansi.Cut(line, span.startCol, span.endCol), ansi.Cut(referenceLines[i], span.startCol, span.endCol), "neutral suffix is unchanged")
						}
					}
				}
				require.Positive(t, nameRows)
				m.ClearReferenceHover()
				referenceTick(t, m)
				fade := m.View()
				require.NotEqual(t, settled, fade)
				require.NotEqual(t, base, fade)
				hoverLastReferenceCell(t, m)
				referenceTick(t, m)
				require.Equal(t, settled, m.View(), "reverse takes the same constant-speed step")
				m.ClearReferenceHover()
				referenceTick(t, m)
				referenceTick(t, m)
				referenceTick(t, m)
				require.Equal(t, base, m.View())
				require.Empty(t, m.referenceHoverValues)
				require.False(t, m.ar.HasActive())
				r, miss, render := m.ResizeCacheStats()
				require.Equal(t, rebuilds, r)
				require.Equal(t, misses, miss)
				require.Equal(t, renders, render)
				row := 0
				for i, line := range strings.Split(base, "\n") {
					if strings.Contains(line, "id=docker-agent-identity-name") {
						row = i
						break
					}
				}
				t.Logf("frames %s row %d: base=%q intermediate=%q settled=%q fade=%q", kind, row, strings.Split(base, "\n")[row], strings.Split(intermediate, "\n")[row], strings.Split(settled, "\n")[row], strings.Split(fade, "\n")[row])
			})
		}
	}
}
func TestReferenceHoverRebasesThemeAndCancelsPointerLifetimes(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	m := referenceFixture(t, "reply", 35)
	hoverLastReferenceCell(t, m)
	referenceTick(t, m)
	progress := m.referenceHoverValues[m.referenceHoverTarget].value
	before := m.View()
	theme := *original
	theme.Colors.Accent = "#123456"
	theme.Colors.TextMuted = "#445566"
	styles.ApplyTheme(&theme)
	m.Update(msgtypes.ThemeChangedMsg{})
	after := m.View()
	require.NotEqual(t, before, after)
	require.Equal(t, ansi.Strip(before), ansi.Strip(after))
	require.Equal(t, progress, m.referenceHoverValues[m.referenceHoverTarget].value)
	for _, cancel := range []func(){func() { m.SetSize(36, 40) }, func() { m.SetPosition(8, 4) }, m.CancelReferenceHover, func() { m.SetReferencePresentationActive(false) }, m.StopAnimations} {
		m.SetReferencePresentationActive(true)
		hoverLastReferenceCell(t, m)
		referenceTick(t, m)
		cancel()
		require.False(t, m.ar.HasActive())
		require.Empty(t, m.referenceHoverValues)
		m.View()
	}
}
func TestReferenceHoverOccurrenceAndCanonicalCollisionIsolation(t *testing.T) {
	m := referenceFixture(t, "completion", 80)
	tree := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{
		{Node: subagent.Node{ID: "abcde-full-canonical-node", SessionID: "child-session", Agent: "worker", Name: "same"}},
		{Node: subagent.Node{ID: "abcde-other-canonical-node", SessionID: "other-session", Agent: "worker", Name: "same"}},
	}}
	m.subagents.Reset(tree)
	m.RefreshInputReferences()
	for _, sender := range []string{"child-session", "other-session"} {
		input := session.UserMessage("raw")
		input.InputOrigin, input.SenderID = session.InputOriginRuntime, sender
		m.AddInputMessage(input, 0)
	}
	m.View()
	for _, span := range extractOSC8Links(m.renderedLines[0]) {
		if span.url == agentidentity.Link {
			m.updateHoveredURL(0, span.startCol)
			break
		}
	}
	referenceTick(t, m)
	lines := append([]string(nil), m.renderedLines...)
	m.applyReferenceHover(lines, 0)
	require.NotEqual(t, m.renderedLines[0], lines[0])
	for i := m.lineOffsets[1]; i < len(lines); i++ {
		require.Equal(t, m.renderedLines[i], lines[i], "other occurrences and short-ID collisions stay unhighlighted")
	}
	require.Len(t, m.referenceHoverValues, 1)
}

func TestReferenceHoverClippedScrollDragAndImmediateExit(t *testing.T) {
	m := referenceFixture(t, "reply", 18)
	hoverLastReferenceCell(t, m)
	m.ClearReferenceHover()
	require.False(t, m.ar.HasActive(), "exit before the first frame has nothing to animate")
	require.Empty(t, m.referenceHoverValues)
	m.SetSize(18, 4)
	m.setScrollOffset(1)
	m.userHasScrolled = true
	base := m.View()
	found := false
	for y := m.scrollOffset; y < min(m.scrollOffset+m.height, len(m.renderedLines)); y++ {
		for _, span := range extractOSC8Links(m.renderedLines[y]) {
			if span.url != agentidentity.Link {
				continue
			}
			ref, ok := m.InputReferenceAt(m.xPos+span.startCol, m.yPos+y-m.scrollOffset)
			require.True(t, ok)
			require.Equal(t, "abcde-full-canonical-node", ref.ID)
			m.handleMouseMotion(tea.MouseMotionMsg{X: m.xPos + span.startCol, Y: m.yPos + y - m.scrollOffset})
			found = true
			break
		}
		if found {
			break
		}
	}
	require.True(t, found)
	referenceTick(t, m)
	require.Equal(t, ansi.Strip(base), ansi.Strip(m.View()))
	m.selection.mouseButtonDown, m.selection.active = true, true
	m.handleMouseMotion(tea.MouseMotionMsg{X: m.xPos, Y: m.yPos})
	require.False(t, m.ar.HasActive())
	require.Empty(t, m.referenceHoverValues)
	m.selection.mouseButtonDown, m.selection.active = false, false
	m.updateHoveredURL(m.scrollOffset, 0)
	m.setScrollOffset(0)
	require.False(t, m.ar.HasActive())
}
