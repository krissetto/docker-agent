package sidebar

import (
	"image/color"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestAgentBelowUsageAndRightFooterPillReserved(t *testing.T) {
	for _, width := range []int{1, 2, 4, 8, 20, 60} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			m := newVisibilityTestSidebar(t).model
			m.SetAgentInfo("root", "provider/界e\u0301-long-model-name", "", 0, "", 0)
			m.sessionState.SetYoloMode(true)
			m.SetSize(width, 12)
			lines := strings.Split(m.View(), "\n")
			require.Len(t, lines, 12)
			footer := lines[11]
			require.LessOrEqual(t, ansi.StringWidth(footer), width)
			for x := range width {
				result, _ := m.HandleClickType(x, 11)
				require.Equal(t, ClickNone, result, "canonical identity and pills are passive")
			}
			if width == 60 {
				require.NotContains(t, ansi.Strip(footer), "root")
				require.Equal(t, "root", strings.TrimSpace(ansi.Strip(lines[m.modelStart-1])))
				require.Greater(t, m.agentIdentityRow, m.usageReadingLine)
				require.Equal(t, m.agentIdentityRow+1, m.modelStart)
				require.True(t, strings.HasSuffix(strings.TrimSpace(ansi.Strip(footer)), "YOLO"))
				require.NotContains(t, ansi.Strip(footer), "long-model-name")
				require.Contains(t, ansi.Strip(strings.Join(lines[:11], "\n")), "long-model-name")
			}
		})
	}
}

func TestIdentityBlockFollowsActiveSessionAgentWithoutLeases(t *testing.T) {
	m := newVisibilityTestSidebar(t).model
	m.SetTeamInfo([]runtime.AgentDetails{
		{Name: "root", Model: "RootModel", Provider: "provider-one"},
		{Name: "worker", Model: "WorkerModel", Provider: "provider-two"},
	})
	m.SetSize(70, 20)
	m.SetPresentationActive(false)
	before := m.View()
	require.Contains(t, strings.Split(ansi.Strip(before), "\n")[m.agentIdentityRow], "root")
	m.sessionState.SetCurrentAgentName("worker")
	after := m.View()
	identity := strings.Split(ansi.Strip(after), "\n")[m.agentIdentityRow]
	require.Contains(t, identity, "worker")
	require.NotContains(t, strings.Split(ansi.Strip(after), "\n")[19], "worker")
	afterLines := strings.Split(ansi.Strip(after), "\n")
	require.Contains(t, afterLines[m.modelStart], "WorkerModel")
	require.Contains(t, afterLines[m.modelStart+1], "provider-two")
	require.NotContains(t, after, "RootModel")
	require.Zero(t, m.ar.ActiveCount())
	m.SetAgentInfo("worker", "fallback/NewModel", "", 0, "", 0)
	updated := strings.Split(ansi.Strip(m.View()), "\n")
	require.Contains(t, updated[m.modelStart], "NewModel")
	require.Contains(t, updated[m.modelStart+1], "fallback")
	require.Zero(t, m.ar.ActiveCount())
}

func TestCompactModelFooterLeftAndPillRight(t *testing.T) {
	m := newVisibilityTestSidebar(t).model
	m.SetMode(ModeCollapsed)
	m.SetSize(120, 20)
	m.sessionState.SetYoloMode(true)
	lines := strings.Split(ansi.Strip(m.View()), "\n")
	require.Len(t, lines, 2)
	require.True(t, strings.HasPrefix(strings.TrimLeft(lines[1], " "), "root  openai"))
	require.True(t, strings.HasSuffix(strings.TrimSpace(lines[1]), "YOLO"))
	require.Contains(t, lines[0], "gpt-4")
	require.NotContains(t, lines[1], "gpt-4")
	result, _ := m.HandleClickType(m.layoutCfg.PaddingLeft, 1)
	require.Equal(t, ClickNone, result)
	result, _ = m.HandleClickType(m.layoutCfg.PaddingLeft+ansi.StringWidth("root  "), 1)
	require.Equal(t, ClickModel, result)
	result, _ = m.HandleClickType(119, 1)
	require.Equal(t, ClickNone, result)
}

func TestModelUsesThemeForegroundAcrossTruncation(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	m := newVisibilityTestSidebar(t).model
	m.SetSize(60, 60)
	for _, foreground := range []string{"#f1e2d3", "#182838"} {
		theme := *original
		theme.Colors.TextPrimary = foreground
		styles.ApplyTheme(&theme)
		for _, modelRef := range []string{"provider/LongModelIdentity", "other/AnotherModelIdentity"} {
			m.SetAgentInfo("root", modelRef, "", 0, "", 0)
			for _, width := range []int{8, 60} {
				m.SetSize(width, 60)
				m.CancelPresentation() // Placement interpolation has separate coverage.
				row := strings.Split(m.View(), "\n")[m.modelStart]
				cells := sidebarCells(row)
				require.Greater(t, len(cells), m.layoutCfg.PaddingLeft)
				expected := styles.BaseStyle.GetForeground()
				require.NotNil(t, cells[m.layoutCfg.PaddingLeft].fg, "settled model glyph retains ordinary theme foreground")
				require.Equal(t, color.NRGBAModel.Convert(expected), color.NRGBAModel.Convert(cells[m.layoutCfg.PaddingLeft].fg))
			}
		}
	}
}

func TestProjectedFriendlyModelAndThinkingTargets(t *testing.T) {
	for _, mode := range []Mode{ModeVertical, ModeCollapsed} {
		m := newVisibilityTestSidebar(t).model
		m.SetTeamInfo([]runtime.AgentDetails{{Name: "root", Provider: "provider", Model: "configured alias", ModelID: "canonical-model", ModelName: "Friendly model", ThinkingMode: "effort", ThinkingLevel: "high", ThinkingLevels: []string{"low", "high"}, CanCycleThinking: true}})
		m.SetMode(mode)
		m.SetSize(120, 12)
		view := m.View()
		lines := strings.Split(ansi.Strip(view), "\n")
		modelY, y := m.modelStart, m.modelStart+1
		if mode == ModeCollapsed {
			modelY, y = 0, 1
		}
		require.Contains(t, lines[modelY], "Friendly model")
		require.NotContains(t, lines[modelY], "provider")
		require.Contains(t, lines[y], "provider (high)")
		require.NotContains(t, lines[y], "Friendly model")
		require.NotContains(t, view, "canonical-model")
		for _, target := range []struct {
			text string
			kind ClickResult
		}{{"Friendly", ClickModel}, {"provider", ClickModel}, {"(high)", ClickThinkingLevel}} {
			targetY := y
			if target.text == "Friendly" {
				targetY = modelY
			}
			prefix, _, found := strings.Cut(lines[targetY], target.text)
			require.True(t, found)
			x := ansi.StringWidth(prefix)
			result, _ := m.HandleClickType(x, targetY)
			require.Equal(t, target.kind, result)
		}
		agent, model, enabled := m.ThinkingTarget()
		require.Equal(t, "root", agent)
		require.Equal(t, "provider/canonical-model", model)
		require.True(t, enabled)
		m.SetSize(8, 12)
		m.View()
		for x := range 8 {
			result, _ := m.HandleClickType(x, y)
			require.NotEqual(t, ClickThinkingLevel, result, "a clipped level never retains a hit target")
		}
	}
}

func TestProjectedThinkingModesStayHonestAndPassiveWhenUnsupported(t *testing.T) {
	m := newVisibilityTestSidebar(t).model
	m.SetSize(100, 12)
	for _, mode := range []string{"default", "off", "auto", "adaptive", "unknown", "unsupported"} {
		m.SetTeamInfo([]runtime.AgentDetails{{Name: "root", Provider: "provider", ModelID: "canonical", ModelName: "Friendly", ThinkingMode: mode, Thinking: "off"}})
		lines := strings.Split(ansi.Strip(m.View()), "\n")
		line := lines[m.modelStart+1]
		prefix, _, found := strings.Cut(line, "("+mode+")")
		require.True(t, found, "projection must not collapse %s into off", mode)
		result, _ := m.HandleClickType(ansi.StringWidth(prefix), m.modelStart+1)
		require.Equal(t, ClickNone, result)
		_, _, enabled := m.ThinkingTarget()
		require.False(t, enabled)
	}
}

func TestThinkingProjectionMatchesConfiguredAliasAndRejectsReplacement(t *testing.T) {
	for _, modelRef := range []string{"provider/canonical", "configured-alias", "provider/configured-alias"} {
		m := newVisibilityTestSidebar(t).model
		m.SetAgentInfo("root", modelRef, "", 0, "", 0)
		m.SetTeamInfo([]runtime.AgentDetails{{Name: "root", Provider: "provider", Model: "configured-alias", ModelID: "canonical", ModelName: "Friendly", ThinkingMode: "adaptive", ThinkingLevel: "high", CanCycleThinking: true}})
		agent, canonical, enabled := m.ThinkingTarget()
		require.Equal(t, "root", agent)
		require.Equal(t, "provider/canonical", canonical)
		require.True(t, enabled)
		require.Equal(t, "(high)", ansi.Strip(m.activeThinkingLabel()), "adaptive mode preserves its actual effort")
		m.SetAgentInfo("root", "other/new-model", "", 0, "", 0)
		_, canonical, enabled = m.ThinkingTarget()
		require.Equal(t, "other/new-model", canonical)
		require.False(t, enabled, "replacement cannot reuse the old model's cycle capability")
		require.Empty(t, m.activeThinkingLabel())
	}
}

func TestCanonicalAgentBlockColorFocusAndTheme(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original); styles.SetAgentOrder(nil) })
	styles.SetAgentOrder([]string{"root", "worker"})
	m := newVisibilityTestSidebar(t).model
	m.SetTeamInfo([]runtime.AgentDetails{{Name: "root", Provider: "p", ModelID: "raw-a", ModelName: "Friendly A"}, {Name: "worker", Provider: "p", ModelID: "raw-b", ModelName: "Friendly B"}})
	m.SetSize(60, 20)
	m.sessionState.SetYoloMode(true)
	for _, agent := range []string{"root", "worker"} {
		m.sessionState.SetCurrentAgentName(agent)
		for _, themeChange := range []bool{false, true} {
			if themeChange {
				theme := *original
				theme.Colors.AgentHues = []float64{15, 170}
				styles.ApplyTheme(&theme)
			}
			lines := strings.Split(m.View(), "\n")
			require.Len(t, lines, 20)
			footer := lines[19]
			identity := lines[m.agentIdentityRow]
			require.Equal(t, m.agentIdentityRow+1, m.modelStart)
			require.Greater(t, m.agentIdentityRow, m.usageReadingLine)
			require.True(t, strings.HasPrefix(strings.TrimLeft(ansi.Strip(identity), " "), agent))
			require.NotContains(t, ansi.Strip(footer), agent)
			require.True(t, strings.HasSuffix(strings.TrimSpace(ansi.Strip(footer)), "YOLO"))
			cells := sidebarCells(identity)
			require.Equal(t, color.NRGBAModel.Convert(styles.AgentIdentityStyle(agent, false).GetForeground()), color.NRGBAModel.Convert(cells[m.layoutCfg.PaddingLeft].fg))
			require.Equal(t, 1, m.footerHeight(), "focus and model changes cannot change footer geometry")
		}
	}
}

func TestModelAndReasoningHoverTargetsStayIndependentAndSettle(t *testing.T) {
	for _, mode := range []Mode{ModeVertical, ModeCollapsed} {
		m := newHoverSidebar(t)
		m.SetTeamInfo([]runtime.AgentDetails{{Name: "root", Provider: "provider", ModelID: "canonical-model", ModelName: "Friendly model", ThinkingLevel: "high", CanCycleThinking: true}})
		m.sessionState.SetCurrentAgentName("root")
		m.SetMode(mode)
		m.SetSize(120, 30)
		m.ReconcileLayout()
		m.CancelPresentation()
		lines := strings.Split(ansi.Strip(m.View()), "\n")
		modelY, reasoningY := m.modelStart, m.modelStart+1
		if mode == ModeCollapsed {
			modelY, reasoningY = 0, 1
		}
		for _, target := range []struct {
			text, key string
			y         int
			kind      ClickResult
		}{{"Friendly", "model", modelY, ClickModel}, {"(high)", "thinking-level", reasoningY, ClickThinkingLevel}} {
			prefix, _, found := strings.Cut(lines[target.y], target.text)
			require.True(t, found)
			x := ansi.StringWidth(prefix)
			result, _ := m.HandleClickType(x, target.y)
			require.Equal(t, target.kind, result)
			_, cmd := m.Update(tea.MouseMotionMsg{X: x, Y: target.y})
			settleSidebarHover(t, m, cmd)
			require.Equal(t, target.key, m.hoverTarget)
			require.InDelta(t, 1.0, m.hoverValues[target.key].value, 0, "settled hover endpoint is exactly one")
			other := "model"
			if target.key == other {
				other = "thinking-level"
			}
			require.Zero(t, m.hoverValues[other].value)
			require.Zero(t, m.ar.ActiveCount())
			require.Equal(t, lines, strings.Split(ansi.Strip(m.View()), "\n"), "hover cannot change geometry or labels")
		}
		settleSidebarHover(t, m, m.ClearSubagentHover())
		require.Empty(t, m.hoverValues)
		require.Zero(t, m.ar.ActiveCount())
	}
}

func TestCanonicalIdentityPassiveWithHiddenUsageAndStableStatusRow(t *testing.T) {
	m := newVisibilityTestSidebar(t).model
	m.SetSize(60, 20)
	m.SetSectionVisibility(SectionVisibility{HideUsage: true})
	m.ReconcileLayout()
	m.CancelPresentation()
	lines := strings.Split(ansi.Strip(m.View()), "\n")
	require.Equal(t, m.agentIdentityRow+1, m.modelStart)
	require.Contains(t, lines[m.agentIdentityRow], "root")
	for x := range 60 {
		result, _ := m.HandleClickType(x, m.agentIdentityRow)
		require.Equal(t, ClickNone, result)
	}
	height := m.viewportHeight()
	m.sessionState.SetYoloMode(true)
	m.View()
	require.Equal(t, height, m.viewportHeight())
	m.sessionState.SetYoloMode(false)
	m.View()
	require.Equal(t, height, m.viewportHeight())
	require.Zero(t, m.ar.ActiveCount())
}

func TestFallbackPrimaryReasoningLabelAndHitTargets(t *testing.T) {
	for _, mode := range []Mode{ModeVertical, ModeCollapsed} {
		for _, capable := range []bool{true, false} {
			m := newVisibilityTestSidebar(t).model
			m.SetMode(mode)
			m.SetSize(120, 20)
			m.SetAgentInfo("root", "provider/fallback", "", 0, "", 0)
			level := "high"
			if !capable {
				level = "unsupported"
			}
			m.SetTeamInfo([]runtime.AgentDetails{{
				Name: "root", Provider: "provider", ModelID: "fallback", ModelName: "Friendly Fallback",
				ThinkingMode: "effort", ThinkingLevel: "low", CanCycleThinking: !capable,
				PrimaryThinking: &runtime.ThinkingDetails{ModelRef: "provider/primary", Mode: level, Level: level, CanCycle: capable},
			}})
			m.ReconcileLayout()
			lines := strings.Split(ansi.Strip(m.View()), "\n")
			row := m.modelStart + 1
			if mode == ModeCollapsed {
				row = 1
				require.LessOrEqual(t, len(lines), 2)
			}
			require.Contains(t, strings.Join(lines, "\n"), "Friendly Fallback")
			prefix, _, found := strings.Cut(lines[row], "(primary: "+level+")")
			require.True(t, found)
			require.NotContains(t, lines[row], "(low)", "an F label must never silently control P")
			click, _ := m.HandleClickType(ansi.StringWidth(prefix), row)
			wantClick := ClickNone
			if capable {
				wantClick = ClickThinkingLevel
			}
			require.Equal(t, wantClick, click)
			agent, target, enabled := m.ThinkingTarget()
			require.Equal(t, "root", agent)
			require.Equal(t, "provider/primary", target)
			require.Equal(t, capable, enabled)
			require.Equal(t, "provider/fallback", m.ThinkingDisplayReference())
			m.SetSize(12, 20)
			m.ReconcileLayout()
			for y, line := range strings.Split(ansi.Strip(m.View()), "\n") {
				for x := range ansi.StringWidth(line) {
					click, _ := m.HandleClickType(x, y)
					require.NotEqual(t, ClickThinkingLevel, click, "a clipped primary label must not leave a hidden hit target")
				}
			}
			m.SetAgentInfo("root", "provider/replacement", "", 0, "", 0)
			_, _, enabled = m.ThinkingTarget()
			require.False(t, enabled)
			require.Empty(t, m.ThinkingDisplayReference())
			require.Empty(t, m.activeThinkingLabel())
		}
	}
}
