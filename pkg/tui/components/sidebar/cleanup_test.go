package sidebar

import (
	"fmt"
	"image/color"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestCleanSidebarLayoutAndCanonicalRoot(t *testing.T) {
	t.Parallel()
	m := newVisibilityTestSidebar(t).model
	m.SetSize(45, 70)
	m.workingDirectory = "/private/parent/workspace"
	m.gitBranchName = "feature/cleanup"
	m.sessionTitle = "Session title"
	m.sessionState.SetYoloMode(true)
	m.SetTeamInfo([]runtime.AgentDetails{{Name: "root", Provider: "provider-one", Model: "Original display"}, {Name: "worker", Provider: "provider-two", Model: "Worker display"}})
	first := ansi.Strip(m.View())
	assert.NotContains(t, first, "/private/parent")
	assert.Contains(t, first, "workspace")
	assert.NotContains(t, first, "Agents")
	assert.NotContains(t, first, "Subagents")
	assert.NotContains(t, first, "Tools")
	assert.NotContains(t, first, "worker")
	assert.Contains(t, first, "YOLO")
	require.Len(t, m.agentClickZones, 1)

	m.sessionState.SetCurrentAgentName("worker")
	m.SetAgentInfo("worker", "fallback/Current display", "", 0, "", 0)
	// Late static team metadata must not erase the active fallback display.
	m.SetTeamInfo([]runtime.AgentDetails{{Name: "root", Model: "Original display"}, {Name: "worker", Provider: "stale", Model: "Stale display"}})
	second := ansi.Strip(m.View())
	assert.Contains(t, second, "Current display")
	assert.Contains(t, second, "fallback")
	assert.NotContains(t, second, "Stale display")
	assert.Contains(t, second, "▶ root")
	lines := strings.Split(second, "\n")
	row := func(text string) int {
		for i, line := range lines {
			if strings.Contains(line, text) {
				return i
			}
		}
		t.Fatalf("missing %q", text)
		return -1
	}
	assert.Equal(t, row("workspace")+1, row("feature/cleanup"))
	assert.Equal(t, row("Current display")+1, row("fallback"))
	assert.Less(t, row("Session"), row("Token Usage"))
	assert.Less(t, row("fallback"), row("▶ root"))
}

func TestCleanSidebarRecursiveIDsMouseAndSmallGeometry(t *testing.T) {
	t.Parallel()
	m := newSubagentTestModel(t)
	m.sessionState.SetCurrentAgentName("root")
	m.rootSessionID = "sess"
	const fullID = "worker-node-with-full-identity-[REDACTED]"
	m.SetSubagentTree(subagent.Snapshot{Root: "root:sess", Nodes: []subagent.NodeSnapshot{{
		Node:     subagent.Node{ID: "root:sess", Agent: "root", State: subagent.NodeIdle},
		Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: fullID, Agent: "worker", Name: "Named worker", State: subagent.NodeRunning, CreatedAt: time.Now()}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "nested-full-id", Agent: "reviewer", State: subagent.NodeFailed}}}}},
	}}})
	m.SetSize(100, 70)
	m.View()
	var workerY int
	for y, id := range m.subagentHoverZone {
		if id == fullID {
			workerY = y
		}
	}
	require.Positive(t, workerY)
	m.Update(tea.MouseMotionMsg{X: m.layoutCfg.PaddingLeft, Y: workerY})
	assert.Contains(t, ansi.Strip(m.View()), fullID)
	result, payload := m.HandleClickType(m.layoutCfg.PaddingLeft, workerY)
	assert.Equal(t, ClickSubagent, result)
	assert.Equal(t, fullID, payload)
	assert.Contains(t, ansi.Strip(m.View()), "failed")

	for _, width := range []int{8, 16, 20, 40} {
		for _, height := range []int{3, 8, 20} {
			t.Run(fmt.Sprintf("%dx%d", width, height), func(t *testing.T) {
				m.SetSize(width, height)
				view := m.View()
				require.Len(t, strings.Split(view, "\n"), height)
				for line := range strings.SplitSeq(view, "\n") {
					assert.LessOrEqual(t, lipgloss.Width(line), width)
				}
				result, _ := m.HandleClickType(width, 0)
				assert.Equal(t, ClickNone, result)
				result, _ = m.HandleClickType(0, -1)
				assert.Equal(t, ClickNone, result)
				result, _ = m.HandleClickType(0, height)
				assert.Equal(t, ClickNone, result)
				for line, id := range m.subagentHoverZone {
					if id == fullID {
						workerY = line
					}
				}
				m.scrollview.SetScrollOffset(workerY)
				m.View()
				y := workerY - m.scrollview.ScrollOffset()
				if y >= 0 && y < height {
					result, payload = m.HandleClickType(m.layoutCfg.PaddingLeft, y)
					assert.Equal(t, ClickSubagent, result)
					assert.Equal(t, fullID, payload)
				}
			})
		}
	}
}

type sidebarCell struct {
	glyph  string
	fg, bg color.Color
}

func sidebarCells(line string) []sidebarCell {
	p := ansi.GetParser()
	defer ansi.PutParser(p)
	var fg, bg color.Color
	var state byte
	var cells []sidebarCell
	for line != "" {
		seq, width, n, next := ansi.DecodeSequence(line, state, p)
		if n == 0 {
			break
		}
		if ansi.HasCsiPrefix(seq) && p.Command() == 'm' {
			params := p.Params()
			if len(params) == 0 {
				fg, bg = nil, nil
			}
			for i := 0; i < len(params); i++ {
				switch param := params[i].Param(0); param {
				case 0:
					fg, bg = nil, nil
				case 39:
					fg = nil
				case 49:
					bg = nil
				case 38, 48, 58:
					var c color.Color
					if consumed := ansi.ReadStyleColor(params[i:], &c); consumed > 0 {
						if param == 38 {
							fg = c
						}
						if param == 48 {
							bg = c
						}
						i += consumed - 1
					}
				}
			}
		}
		for range width {
			cells = append(cells, sidebarCell{seq, fg, bg})
		}
		state, line = next, line[n:]
	}
	return cells
}

func TestSidebarWarmCacheThemeColorsAndLayout(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original); styles.SetAgentOrder(nil) })
	styles.SetAgentOrder([]string{"root", "worker"})
	m := newVisibilityTestSidebar(t).model
	m.SetSize(45, 60)
	m.SetTeamInfo([]runtime.AgentDetails{{Name: "root", Provider: "provider", Model: "Display"}})
	m.sessionTitle = "Title"
	before := m.View()
	generation := m.VisualGeneration()
	theme := *original
	theme.Colors.Background = "#102030"
	theme.Colors.TextMuted = "#7391af"
	theme.Colors.TextPrimary = "#f1e2d3"
	styles.ApplyTheme(&theme)
	after := m.View() // no ThemeChangedMsg: warm caches must notice generation
	assert.Greater(t, m.VisualGeneration(), generation)
	assert.Equal(t, ansi.Strip(before), ansi.Strip(after))
	assert.NotEqual(t, before, after)
	assert.Equal(t, after, m.View(), "warm render is stable")
	for _, needle := range []string{"Display", "provider", "root"} {
		var found bool
		for line := range strings.SplitSeq(after, "\n") {
			text := ansi.Strip(line)
			offset := strings.Index(text, needle)
			if offset < 0 {
				continue
			}
			cells := sidebarCells(line)
			cellX := lipgloss.Width(text[:offset])
			require.Less(t, cellX, len(cells))
			expected := styles.TabPrimaryStyle.GetForeground()
			if needle == "provider" {
				expected = styles.MutedStyle.GetForeground()
			}
			if needle == "root" {
				expected = styles.AgentIdentityStyle("root", false).GetForeground()
			}
			require.NotNil(t, cells[cellX].fg)
			assert.Equal(t, color.NRGBAModel.Convert(expected), color.NRGBAModel.Convert(cells[cellX].fg))
			assert.Nil(t, cells[cellX].bg, "unheaded model/tree cells retain the transparent sidebar surface")
			found = true
		}
		assert.True(t, found, "missing %s", needle)
	}
	m.Update(messages.ThemeChangedMsg{})
	assert.Equal(t, after, m.View(), "explicit theme message agrees with lazy invalidation")
}

func TestActiveModelDisplayLocalAndRemoteFallback(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, modelID, provider, display string }{
		{"local nested model", "dmr/ai/llama3.2", "dmr", "ai/llama3.2"},
		{"remote configured display", "anthropic/claude-sonnet", "anthropic", "claude-sonnet"},
		{"remote fallback", "openai/Fallback display", "openai", "Fallback display"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newVisibilityTestSidebar(t).model
			m.SetAgentInfo("root", tc.modelID, "", 0, "", 0)
			m.SetTeamInfo([]runtime.AgentDetails{{Name: "root", Provider: "stale", Model: "Old model"}})
			lines := strings.Split(ansi.Strip(m.activeModelInfo(60)), "\n")
			require.Equal(t, []string{tc.display, tc.provider}, lines)
		})
	}
}

func TestCollapsedBranchStaysDirectlyBelowWorkspace(t *testing.T) {
	t.Parallel()
	m := newVisibilityTestSidebar(t).model
	m.SetMode(ModeCollapsed)
	m.SetSize(60, 20)
	m.workingDirectory = "/parent/workspace"
	m.gitBranchName = "branch-name"
	vm := m.computeCollapsedViewModel(m.contentWidth(false))
	view := ansi.Strip(RenderCollapsedView(vm))
	lines := strings.Split(view, "\n")
	require.Len(t, lines, vm.LineCount()-1)
	for i, line := range lines {
		if !strings.Contains(line, "workspace") {
			continue
		}
		require.Less(t, i+1, len(lines))
		assert.Equal(t, "branch-name", strings.TrimSpace(lines[i+1]))
		result, _ := m.HandleClickType(m.layoutCfg.PaddingLeft, i)
		assert.Equal(t, ClickWorkingDir, result)
		result, _ = m.HandleClickType(m.layoutCfg.PaddingLeft, i+1)
		assert.Equal(t, ClickNone, result)
		return
	}
	t.Fatal("workspace not rendered")
}
