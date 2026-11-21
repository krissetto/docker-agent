package sidebar

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestCollapsedBandBoundedRowsKeepTheirActions(t *testing.T) {
	t.Parallel()
	m := newVisibilityTestSidebar(t)
	m.sessionHasContent = true
	m.titleGenerated = true
	m.sessionTitle = "alpha " + strings.Repeat("界", 70) + "\nend"
	m.workingDirectory = "/workspace/project"
	m.gitBranchName = "topic"
	m.agentCompactionModel = "some/model"
	m.SetTeamInfo([]runtime.AgentDetails{
		{Name: "root", Model: "gpt-4", Provider: "openai", Thinking: "high"},
		{Name: "idle", Model: "not-active", Provider: "other"},
	})
	m.sessionState.SetYoloMode(true)
	m.SetSectionVisibility(SectionVisibility{ShowPlans: true})
	m.setPlans(messages.PlanSidebarDataMsg{Result: plans.ListResult{Plans: []plans.Plan{{Name: "release"}}}})
	m.SetMode(ModeCollapsed)
	m.SetPosition(5, 10)
	for width := 1; width <= 120; width++ {
		m.SetSize(width, 2)
		lines := strings.Split(ansi.Strip(m.View()), "\n")
		require.Len(t, lines, m.CollapsedHeight(width))
		require.LessOrEqual(t, len(lines), 2)
		for _, line := range lines {
			assert.LessOrEqual(t, ansi.StringWidth(line), width)
			assert.NotContains(t, line, "not-active")
		}
	}
	lines := strings.Split(ansi.Strip(m.View()), "\n")
	require.Len(t, lines, 2)
	assert.NotContains(t, lines[1], "alpha", "title never wraps into metadata")
	for _, target := range []struct {
		text string
		y    int
		kind ClickResult
	}{
		{"☆", 0, ClickStar},
		{"lpha", 0, ClickTitle},
		{"gpt-4", 0, ClickModel},
		{"root", 1, ClickNone},
		{"openai", 1, ClickModel},
		{"(high)", 1, ClickThinkingLevel},
		{"project", 1, ClickWorkingDir},
		{"topic", 1, ClickNone},
		{"Plans", 1, ClickPlanBrowser},
		{"⚠", 1, ClickUsageContext},
		{"$", 1, ClickUsage},
		{"YOLO", 1, ClickNone},
	} {
		before, _, found := strings.Cut(lines[target.y], target.text)
		require.True(t, found, "%q in %q", target.text, lines[target.y])
		x := ansi.StringWidth(before)
		kind, payload := m.HandleClickType(x, target.y)
		assert.Equal(t, target.kind, kind, "text %q at row %d", target.text, target.y)
		if kind == ClickThinkingLevel {
			assert.Equal(t, "openai/gpt-4", payload)
			beforeHover := m.View()
			m.Update(tea.MouseMotionMsg{X: m.xPos + x, Y: m.yPos + target.y})
			assert.NotEqual(t, beforeHover, m.View(), "hover changes only presentation")
			m.Update(tea.MouseMotionMsg{X: -1, Y: -1})
			assert.Equal(t, beforeHover, m.View(), "pointer leave restores the exact frame")
		}
	}
	for _, point := range [][2]int{{0, 0}, {120, 0}, {m.layoutCfg.PaddingLeft, 2}} {
		kind, _ := m.HandleClickType(point[0], point[1])
		assert.Equal(t, ClickNone, kind, "outside the band or in padding")
	}
}
