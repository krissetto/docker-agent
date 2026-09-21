package sidebar

import (
	"image/color"
	"strconv"
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
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestContinuousPaneOrderWithRecapsAndBoundedSpacing(t *testing.T) {
	t.Parallel()
	m := newVisibilityTestSidebar(t).model
	m.sessionTitle = "Title"
	m.workingDirectory = "/full/path/project"
	m.gitBranchName = "main"
	m.sessionState.SetYoloMode(true)
	m.SetSectionGap(5)
	lines := m.renderSections(50)
	plain := ansi.Strip(strings.Join(lines, "\n"))
	for _, heading := range []string{"Session ─", "Token Usage", "Agents ─", "Tools ─", "──"} {
		assert.NotContains(t, plain, heading)
	}
	assert.NotContains(t, plain, "\n\n\n", "breathing room is modest, not inherited section padding")
	assert.Contains(t, ansi.Strip(lines[0]), "Title")
	assert.Empty(t, lines[1])
	assert.Equal(t, "project", strings.TrimSpace(ansi.Strip(ansi.Cut(lines[2], 0, 50-directoryReserve()))))
	pathCells := sidebarCells(lines[2])
	require.Len(t, pathCells, 50)
	assert.Equal(t, directoryIcon, pathCells[48].glyph)
	assert.Equal(t, color.NRGBAModel.Convert(styles.Background), color.NRGBAModel.Convert(pathCells[48].fg), "idle arrow is visually hidden, not removed from reserved cells")
	assert.Equal(t, "main", strings.TrimSpace(ansi.Strip(lines[3])))
	assert.Empty(t, lines[4])
	assert.NotContains(t, plain, "YOLO", "pills belong only to pinned footer")
	assert.Contains(t, m.footerView(50), "YOLO")
	assert.Equal(t, 5, m.usageReadingLine)
	assert.Equal(t, m.modelStart+2, m.modelEnd, "model and provider occupy distinct rows next to usage")
	assert.Empty(t, m.subagentHoverZone, "empty descendants do not render recap")
	assert.Equal(t, "/full/path/project", m.WorkingDirectory())

	// Section recaps replace decorative headings; populated sections have an
	// explicit separator and share the same two-cell body indentation.
	m.SetSubagentTree(collapseFixture())
	require.NoError(t, m.SetTodos(makeTodos(1)))
	lines = m.renderSections(50)
	require.Contains(t, ansi.Strip(lines[m.summaryLine]), "subagents")
	require.Greater(t, m.todoSummaryLine, m.summaryLine)
	assert.Empty(t, lines[m.todoSummaryLine-1], "tree and todos are separated even without old section padding")
	assert.Contains(t, ansi.Strip(lines[m.todoSummaryLine]), "1/1 todos")
	assert.Contains(t, ansi.Strip(lines[m.todoSummaryLine]), "⌄")
	for row := range m.subagentHoverZone {
		assert.True(t, strings.HasPrefix(ansi.Strip(lines[row]), "  "), "tree results are indented under the recap")
	}
	for row := m.todoSummaryLine + 2; row < m.todoEnd; row++ {
		assert.True(t, strings.HasPrefix(ansi.Strip(lines[row]), "  "), "every wrapped todo row is indented")
	}
	for _, line := range lines {
		assert.LessOrEqual(t, ansi.StringWidth(line), 50)
	}
	assert.NotContains(t, ansi.Strip(strings.Join(lines, "\n")), "\n\n\n", "recaps do not restore oversized legacy padding")
}

func TestPaneRegionsSharedHoverCellsAndNoopMotion(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	m := New(animation.NewRuntimeWithScheduler(&immediateScheduler{now: time.Unix(1, 0)}), t.Context(), &service.SessionState{}).(*model)
	m.sessionState.SetCurrentAgentName("root")
	m.SetTeamInfo([]runtime.AgentDetails{{Name: "root", Model: "Display", Provider: "provider"}})
	m.SetSize(50, 30)
	m.SetPosition(4, 2)
	m.View()
	initialGeometry := strings.ReplaceAll(strings.ReplaceAll(ansi.Strip(m.View()), directoryIcon, strings.Repeat(" ", directoryIconWidth())), directoryCopyIcon, strings.Repeat(" ", ansi.StringWidth(directoryCopyIcon)))
	wantAnchors := []int{m.workingDirRow, m.usageReadingLine, m.usageSectionEnd, m.modelStart, m.modelEnd, m.treeSectionStart}
	assertStable := func() {
		t.Helper()
		actualGeometry := ansi.Strip(m.View())
		actualGeometry = strings.ReplaceAll(strings.ReplaceAll(actualGeometry, directoryIcon, strings.Repeat(" ", directoryIconWidth())), directoryCopyIcon, strings.Repeat(" ", ansi.StringWidth(directoryCopyIcon)))
		assert.Equal(t, initialGeometry, actualGeometry, "only reserved folder glyph cells may change on hover")
		assert.Equal(t, wantAnchors, []int{m.workingDirRow, m.usageReadingLine, m.usageSectionEnd, m.modelStart, m.modelEnd, m.treeSectionStart})
	}
	targets := []struct {
		kind ClickResult
		x, y int
	}{
		{ClickWorkingDir, 0, m.workingDirRow},
		{ClickUsageContext, 0, m.usageReadingLine},
		{ClickUsage, m.usageContextSegWidth, m.usageReadingLine},
		{ClickModel, 0, m.modelStart},
		{ClickModel, 0, m.modelStart + 1},
	}
	for _, target := range targets {
		x := m.layoutCfg.PaddingLeft + target.x
		settleSidebarHover(t, m, m.ClearSubagentHover())
		assertStable()
		before := sidebarCells(strings.Split(m.View(), "\n")[target.y])
		_, cmd := m.Update(tea.MouseMotionMsg{X: m.xPos + x, Y: m.yPos + target.y})
		settleSidebarHover(t, m, cmd)
		assertStable()
		result, _ := m.HandleClickType(x, target.y)
		assert.Equal(t, target.kind, result)
		after := sidebarCells(strings.Split(m.View(), "\n")[target.y])
		require.Less(t, x, len(after))
		assert.Equal(t, before[x].bg, after[x].bg, "text hover never adds a row background")
		base := before[x].fg
		if base == nil {
			base = styles.TextPrimary
		}
		require.NotNil(t, after[x].fg, "hover target must remain on its text cell after cached re-render")
		assert.Equal(t, color.NRGBAModel.Convert(styles.Brighten(base, .25)), color.NRGBAModel.Convert(after[x].fg))
		assert.Zero(t, m.ar.ActiveCount(), "settled hover holds no animation lease")
		gen := m.VisualGeneration()
		for range 100 {
			m.Update(tea.MouseMotionMsg{X: m.xPos + x, Y: m.yPos + target.y})
			assertStable()
		}
		assert.Equal(t, gen, m.VisualGeneration(), "100 unchanged motions must not invalidate")
	}
	theme := *original
	theme.Colors.TextMuted = "#314159"
	styles.ApplyTheme(&theme)
	assertStable()
	after := sidebarCells(strings.Split(m.View(), "\n")[m.modelStart+1])
	assert.Nil(t, after[m.layoutCfg.PaddingLeft].bg)
	assert.Equal(t, color.NRGBAModel.Convert(styles.Brighten(styles.MutedStyle.GetForeground(), .25)), color.NRGBAModel.Convert(after[m.layoutCfg.PaddingLeft].fg), "warm hover rebases original current theme")
}

func TestTrailingBranchChevronStableNameAndRightStatus(t *testing.T) {
	t.Parallel()
	m := newCollapseSidebar(t)
	m.SetSize(60, 30)
	before := strings.Split(ansi.Strip(m.View()), "\n")
	var row int
	for y, id := range m.subagentHoverZone {
		if id == "branch-full-identity" {
			row = y
		}
	}
	require.Positive(t, row)
	nameByte := strings.Index(before[row], "planner")
	require.Positive(t, nameByte)
	nameX := lipgloss.Width(before[row][:nameByte])
	m.Update(tea.MouseMotionMsg{X: m.xPos + nameX, Y: m.yPos + row})
	after := strings.Split(ansi.Strip(m.View()), "\n")
	afterByte := strings.Index(after[row], "planner")
	require.Positive(t, afterByte)
	assert.Equal(t, nameX, lipgloss.Width(after[row][:afterByte]), "UTF-8 chevron bytes never change the identity cell column")
	assert.Contains(t, after[row], "⌄")
	controls := m.treeControls[row-m.treeSectionStart]
	require.Len(t, controls, 1)
	assert.Equal(t, nameX+ansi.StringWidth("planner")+1, m.layoutCfg.PaddingLeft+controls[0].x, "expanded row reserves no hidden count space")
	assert.True(t, strings.HasPrefix(strings.TrimLeft(before[row], " "), "planner"), "first-degree descendants have no connector prefix")
	result, id := m.HandleClickType(nameX, row)
	assert.Equal(t, ClickSubagent, result)
	assert.Equal(t, "branch-full-identity", id)
	// Status text ends at the same content edge on every node, not before controls.
	m.ClearSubagentHover()
	view := strings.Split(ansi.Strip(m.View()), "\n")
	for y := range m.subagentHoverZone {
		assert.Equal(t, m.layoutCfg.PaddingLeft+m.contentWidth(false), lipgloss.Width(strings.TrimRight(view[y], " ")))
	}
}

func TestAttachedPerspectiveNeverFabricatesMainRoot(t *testing.T) {
	t.Parallel()
	m := newCollapseSidebar(t)
	m.SetTeamInfo([]runtime.AgentDetails{{Name: "root"}, {Name: "child"}})
	m.SetSubagentContext("child-full-id", "parent", "parent-session")
	m.SetSubagentTree(subagent.Snapshot{})
	assert.Empty(t, m.subagentNodes)
	assert.Empty(t, m.delegationRootName())
	assert.NotContains(t, ansi.Strip(m.View()), "root")
	assert.Empty(t, m.agentClickZones)
	assert.Empty(t, m.subagentHoverZone)
	empty := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child-full-id"}}}}
	m.SetSubagentTree(empty)
	assert.Empty(t, m.delegationRootName())
	leaf := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child-full-id", Agent: "child", State: subagent.NodeIdle}}}}
	m.SetSubagentTree(leaf)
	m.View()
	assert.Empty(t, m.agentClickZones, "current leaf is never its own child row")
	assert.Empty(t, m.subagentHoverZone)
	assert.NotContains(t, ansi.Strip(m.View()), "0 total")
	assert.Empty(t, m.treeControls, "actual leaf has no branch chevron")
	leaf.Nodes[0].Children = []subagent.NodeSnapshot{{Node: subagent.Node{ID: "grandchild-full-id", Agent: "grandchild", State: subagent.NodeIdle}}}
	m.SetSubagentTree(leaf)
	m.View()
	assert.Empty(t, m.treeControls, "a leaf grandchild has no branch control")
	for y, id := range m.subagentHoverZone {
		if id == "grandchild-full-id" {
			result, payload := m.HandleClickType(m.layoutCfg.PaddingLeft+2, y)
			assert.Equal(t, ClickSubagent, result)
			assert.Equal(t, string(id), payload)
		}
	}
	m.SetSubagentContext("", "parent", "parent-session")
	m.SetSubagentTree(collapseFixture())
	assert.Empty(t, m.delegationRootName(), "empty attached ID must not use configured main root")
}

func TestContinuousPaneTinyHeightsAndClippedHover(t *testing.T) {
	t.Parallel()
	for _, width := range []int{4, 8, 16, 40} {
		for _, height := range []int{1, 2, 4} {
			t.Run(strconv.Itoa(width)+"x"+strconv.Itoa(height), func(t *testing.T) {
				m := newVisibilityTestSidebar(t).model
				m.SetSize(width, height)
				view := m.View()
				require.Len(t, strings.Split(view, "\n"), height)
				for line := range strings.SplitSeq(view, "\n") {
					assert.LessOrEqual(t, lipgloss.Width(line), width)
				}
				m.Update(tea.MouseMotionMsg{X: width, Y: height})
				assert.Equal(t, ClickNone, m.hoveredRegion)
				result, _ := m.HandleClickType(width, height)
				assert.Equal(t, ClickNone, result)
			})
		}
	}
}

func TestCollapsedPaneYoloAndModelHoverClickParity(t *testing.T) {
	t.Parallel()
	m := newVisibilityTestSidebar(t).model
	m.SetMode(ModeCollapsed)
	m.SetSize(120, 20)
	m.sessionState.SetYoloMode(true)
	m.workingDirectory = "/absolute/workspace"
	m.gitBranchName = "branch"
	lines := strings.Split(ansi.Strip(m.View()), "\n")
	require.Len(t, lines, 2)
	for _, text := range []string{"workspace", "branch", "YOLO", "openai"} {
		require.Contains(t, lines[1], text)
	}
	require.Contains(t, lines[0], "gpt-4")
	require.NotContains(t, lines[1], "gpt-4")
	for _, text := range []string{"gpt-4", "openai"} {
		y := 1
		if text == "gpt-4" {
			y = 0
		}
		prefix, _, found := strings.Cut(lines[y], text)
		require.True(t, found)
		x := ansi.StringWidth(prefix)
		result, _ := m.HandleClickType(x, y)
		assert.Equal(t, ClickModel, result)
		m.Update(tea.MouseMotionMsg{X: x, Y: y})
		assert.Equal(t, ClickModel, m.hoveredRegion)
	}
}

func TestPaneBreathingRowsYieldToSmallHeights(t *testing.T) {
	t.Parallel()
	m := newVisibilityTestSidebar(t).model
	m.SetSize(50, 30)
	roomy := m.renderSections(49)
	require.Empty(t, roomy[m.usageReadingLine-1])
	require.Empty(t, roomy[m.agentIdentityRow-1], "identity/model block has breathing room below usage")
	require.Equal(t, m.usageSectionEnd+1, m.agentIdentityRow, "exactly one affordable row separates usage from identity")
	require.Equal(t, "root", ansi.Strip(roomy[m.agentIdentityRow]))
	require.Equal(t, m.agentIdentityRow+1, m.modelStart, "model immediately follows canonical identity")
	require.Contains(t, ansi.Strip(roomy[m.modelStart]), "gpt-4")
	require.Contains(t, ansi.Strip(roomy[m.modelStart+1]), "openai")
	assert.Empty(t, m.subagentHoverZone, "empty tree contributes no summary or spacer")
	m.SetSize(50, 5)
	compact := m.renderSections(49)
	for _, line := range compact {
		assert.NotEmpty(t, strings.TrimSpace(ansi.Strip(line)), "small viewports spend rows on content")
	}
	for row, name := range m.agentClickZones {
		assert.Contains(t, ansi.Strip(compact[row]), name)
	}
	assert.Contains(t, ansi.Strip(compact[m.usageReadingLine]), "$0.00")
	assert.Equal(t, m.usageSectionEnd, m.agentIdentityRow, "small viewports omit the breathing row, not identity")
	assert.Equal(t, "root", ansi.Strip(compact[m.agentIdentityRow]))
	assert.Equal(t, m.agentIdentityRow+1, m.modelStart)
	assert.Contains(t, ansi.Strip(compact[m.modelStart]), "gpt-4")
	assert.Contains(t, ansi.Strip(compact[m.modelStart+1]), "openai")
}

func TestBreathingRowsStableAcrossWarmAnimationFrames(t *testing.T) {
	t.Parallel()
	m := newVisibilityTestSidebar(t).model
	m.SetSize(50, 30)
	m.sessionState.SetYoloMode(true)
	m.gitBranchName = "branch"
	first := ansi.Strip(m.View())
	directory, usage, modelStart, modelEnd, tree := m.workingDirRow, m.usageReadingLine, m.modelStart, m.modelEnd, m.treeSectionStart
	for range 100 {
		m.invalidateAnimation()
		assert.Equal(t, first, ansi.Strip(m.View()), "warm session section must reuse raw anchors before adding gaps")
		assert.Equal(t, directory, m.workingDirRow)
		assert.Equal(t, usage, m.usageReadingLine)
		assert.Equal(t, modelStart, m.modelStart)
		assert.Equal(t, modelEnd, m.modelEnd)
		assert.Equal(t, tree, m.treeSectionStart)
	}
}
