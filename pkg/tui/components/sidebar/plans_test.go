package sidebar

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func newPlanSidebar(t *testing.T) *model {
	t.Helper()
	// A deterministic clock lets placement entrance animations settle through
	// planView instead of depending on wall-clock time.
	clock := &placementClock{now: time.Unix(1, 0), step: 50 * time.Millisecond}
	m := New(animation.NewRuntimeWithScheduler(clock), t.Context(), &service.SessionState{}).(*model)
	m.scrollview.SetAnimationRuntime(nil)
	m.plansCollapsed = false // Existing row fixtures exercise the expanded body.
	t.Cleanup(m.StopAnimation)
	m.sessionTitle = "T"
	m.workingDirectory = ""
	m.SetSize(40, 80)
	m.SetSectionVisibility(SectionVisibility{
		ShowPlans: true, HideUsage: true, HideAgents: true, HideTools: true, HideTodos: true,
	})
	return m
}

func TestPlanSidebar_RecentOrderAndLimit(t *testing.T) {
	t.Parallel()
	for _, count := range []int{0, 1, 5, 8} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			t.Parallel()
			m := newPlanSidebar(t)
			var input []plans.Plan
			for i := range count {
				input = append(input, plans.Plan{
					Name: fmt.Sprintf("plan-%d", i), UpdatedAt: time.Unix(int64(i+1), 0), Version: new(i + 1),
				})
			}
			m.setPlans(messages.PlanSidebarDataMsg{Result: plans.ListResult{Plans: input}})
			require.Len(t, m.recentPlans, min(count, recentPlanLimit))
			for i, p := range m.recentPlans {
				assert.Equal(t, fmt.Sprintf("plan-%d", count-i-1), p.Name)
			}
			for i, p := range input {
				assert.Equal(t, fmt.Sprintf("plan-%d", i), p.Name, "do not reorder the service/browser snapshot")
			}
			view := planView(t, m)
			if count == 0 {
				assert.NotContains(t, view, "plans")
				assert.Empty(t, m.planClickZones)
			} else {
				if count == 1 {
					assert.Contains(t, view, "1 plan")
				} else {
					assert.Contains(t, view, fmt.Sprintf("%d plans", count))
				}
			}
		})
	}
}

func TestPlanSidebar_TiesAndUnknownDates(t *testing.T) {
	t.Parallel()
	m := newPlanSidebar(t)
	stamp := time.Unix(100, 0)
	m.setPlans(messages.PlanSidebarDataMsg{Result: plans.ListResult{Plans: []plans.Plan{
		{Name: "unknown"}, {Name: "z", UpdatedAt: stamp}, {Name: "a", UpdatedAt: stamp},
	}}})
	require.Len(t, m.recentPlans, 3)
	assert.Equal(t, "a", m.recentPlans[0].Name)
	assert.Equal(t, "z", m.recentPlans[1].Name)
	assert.Equal(t, "unknown", m.recentPlans[2].Name)
}

func TestPlanSidebar_ClicksTrackCanonicalPlanAndVersion(t *testing.T) {
	t.Parallel()
	m := newPlanSidebar(t)
	m.SetSize(80, 80)
	m.setPlans(messages.PlanSidebarDataMsg{Result: plans.ListResult{Plans: []plans.Plan{
		{Name: "first", Title: "Same title", Status: "awaiting-special-review", Version: new(4)},
		{Name: "second", Title: "Same title", Version: new(8)},
	}}})
	view := planView(t, m)
	assert.Contains(t, view, "awaiting-special-review")
	found := map[ClickResult]int{}
	for y, line := range strings.Split(view, "\n") {
		kind, name := m.HandleClickType(m.layoutCfg.PaddingLeft+1, y)
		if kind == ClickNone {
			continue
		}
		found[kind]++
		if kind == ClickPlan {
			msg, ok := m.EditPlan(name, "tab")().(messages.EditSidebarPlanMsg)
			require.True(t, ok)
			assert.Equal(t, plans.SharedRef(name), msg.Ref)
			assert.Equal(t, "tab", msg.TabID)
			if name == "first" {
				assert.Equal(t, 4, msg.ExpectedVersion)
			} else {
				assert.Equal(t, "second", name)
				assert.Equal(t, 8, msg.ExpectedVersion)
			}
		}
		assert.NotEmpty(t, strings.TrimSpace(line))
	}
	assert.Equal(t, 2, found[ClickPlan], "each compact row belongs to its canonical plan")
	assert.Equal(t, 1, found[ClickPlanBrowser])
	assert.Zero(t, found[ClickPlanRefresh], "healthy plans refresh live; manual refresh remains in the browser")
	before := m.VisualGeneration()
	m.SetSectionVisibility(SectionVisibility{})
	assert.Greater(t, m.VisualGeneration(), before)
	// Update reconciles the layout after every message; do the same here so the
	// hit test sees the rows leaving rather than the stale placement.
	m.ReconcileLayout()
	for y := range strings.Split(view, "\n") {
		kind, _ := m.HandleClickType(m.layoutCfg.PaddingLeft+1, y)
		assert.NotEqual(t, ClickPlan, kind, "hiding must immediately disable old click zones")
	}
	assert.NotContains(t, planView(t, m), "Same title")
}

func TestPlanSidebar_StateAndCompactGeometry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		data messages.PlanSidebarDataMsg
		text string
	}{
		{"loading", messages.PlanSidebarDataMsg{Loading: true, Result: plans.ListResult{Plans: []plans.Plan{{Name: "release"}}}}, "release"},
		{"error", messages.PlanSidebarDataMsg{Err: errors.New("unavailable")}, "Plans unavailable"},
		{"warning", messages.PlanSidebarDataMsg{Result: plans.ListResult{Warnings: []string{"corrupt"}}}, "could not be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newPlanSidebar(t)
			m.setPlans(tc.data)
			assert.Contains(t, planView(t, m), tc.text)
			m.SetMode(ModeCollapsed)
			for _, width := range []int{10, 40, 100} {
				m.SetSize(width, 80)
				view := planView(t, m)
				lines := strings.Split(view, "\n")
				assert.Len(t, lines, m.CollapsedHeight(width), "the collapsed band keeps its two-row geometry")
				// The collapsed Plans link shares the info row; wherever it lands,
				// clicking inside its text opens the browser.
				found := false
				for y, line := range lines {
					x := strings.Index(line, "Plans")
					if x < 0 || !strings.Contains(line, "open /plans") && !strings.Contains(line, "Loading plans") && !strings.Contains(line, "Plans unavailable") {
						continue
					}
					found = true
					kind, _ := m.HandleClickType(m.layoutCfg.PaddingLeft+x, y)
					assert.Equal(t, ClickPlanBrowser, kind)
				}
				if !found {
					t.Skipf("the collapsed band at width %d has no room for the Plans link", width)
				}
			}
		})
	}
}

func TestPlanSidebar_ScrollbarAndScrolledRows(t *testing.T) {
	t.Parallel()
	m := newPlanSidebar(t)
	m.SetSize(30, 5)
	m.setPlans(messages.PlanSidebarDataMsg{Result: plans.ListResult{Plans: []plans.Plan{
		{Name: "one", Version: new(1)}, {Name: "two", Version: new(2)},
	}}})
	planView(t, m)
	require.True(t, m.cachedNeedsScrollbar)
	for contentY, zone := range m.planClickZones {
		if zone.kind != ClickPlan {
			continue
		}

		m.scrollview.SetScrollOffset(contentY)
		viewportY := contentY - m.scrollview.ScrollOffset()
		kind, name := m.HandleClickType(m.layoutCfg.PaddingLeft, viewportY)
		assert.Equal(t, ClickPlan, kind)
		assert.Equal(t, zone.name, name)
		kind, _ = m.HandleClickType(m.layoutCfg.PaddingLeft+m.contentWidth(true), viewportY)
		assert.Equal(t, ClickNone, kind, "scrollbar must not edit plans")
	}
}

func TestPlanSidebar_MultilineMetadataKeepsClickGeometry(t *testing.T) {
	t.Parallel()
	m := newPlanSidebar(t)
	m.setPlans(messages.PlanSidebarDataMsg{Result: plans.ListResult{Plans: []plans.Plan{
		{Name: "release", Title: "first\nsecond\tthird", Status: "pending\r\nreview", Version: new(1)},
	}}})
	view := planView(t, m)
	assert.Contains(t, view, "first second")
	assert.Contains(t, view, "pending review")
	for y, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "first second") || strings.Contains(line, "pending review") {
			kind, name := m.HandleClickType(m.layoutCfg.PaddingLeft, y)
			assert.Equal(t, ClickPlan, kind)
			assert.Equal(t, "release", name)
		}
	}
}

// planView settles the placement presentation, then returns the plain view.
// Plan rows fade in through the placement animation, so unsettled frames
// would hide them from both the paint and the hit assertions.
func planView(t *testing.T, m *model) string {
	t.Helper()
	settlePlacement(t, m, m.ReconcileLayout())
	return ansi.Strip(m.View())
}

func TestPlanSidebar_UnusedHiddenUntilDataArrives(t *testing.T) {
	t.Parallel()
	m := newPlanSidebar(t)
	for _, mode := range []Mode{ModeVertical, ModeCollapsed} {
		m.SetMode(mode)
		for _, loading := range []bool{false, true} {
			m.Update(messages.PlanSidebarDataMsg{Loading: loading})
			assert.NotContains(t, planView(t, m), "plans")
			assert.Empty(t, m.plansSummary())
			assert.Empty(t, m.planClickZones)
		}
	}

	m.SetMode(ModeVertical)
	m.SetSize(80, 80)
	for _, status := range []string{"", "draft", "done", "awaiting-special-review", "🚀 shipping"} {
		m.Update(messages.PlanSidebarDataMsg{Result: plans.ListResult{Plans: []plans.Plan{{
			Name: "release", Status: status, Version: new(1),
		}}}})
		view := planView(t, m)
		assert.Contains(t, view, "release", "status must not filter out a plan")
		if status != "" {
			assert.Contains(t, view, status, "status is a free-form label, not a state enum")
		}
		assert.NotEmpty(t, m.plansSummary())
	}
	m.Update(messages.PlanSidebarDataMsg{})
	assert.NotContains(t, planView(t, m), "plans", "deleting the last plan removes the section")
	assert.Empty(t, m.planClickZones)
	assert.Empty(t, m.plansSummary())
}

func TestPlanSidebar_CompactLayoutAndRecoveryActions(t *testing.T) {
	t.Parallel()
	m := newPlanSidebar(t)
	require.NoError(t, m.SetTodos(makeTodos(1)))
	m.SetSectionVisibility(SectionVisibility{ShowPlans: true, HideUsage: true, HideTools: true})
	var input []plans.Plan
	for i := range 8 {
		input = append(input, plans.Plan{Name: fmt.Sprintf("canonical-%d", i), Title: "A lengthy human readable title", Status: "awaiting input", Version: new(i + 1)})
	}
	for _, width := range []int{1, 2, 8, 30, 60} {
		m.SetSize(width, 80)
		m.Update(messages.PlanSidebarDataMsg{Result: plans.ListResult{Plans: input}})
		view := planView(t, m)
		for _, line := range strings.Split(view, "\n") {
			assert.LessOrEqual(t, ansi.StringWidth(line), width)
		}
		assert.NotContains(t, view, "──")
		if width < 30 {
			continue
		}
		assert.Contains(t, view, "+3 more")
		browserIDs := map[string]bool{}
		for _, row := range m.placement.rows {
			if row.target && row.action == ClickPlanBrowser {
				assert.False(t, browserIDs[row.id], "browser and overflow rows must have distinct placement identities")
				browserIDs[row.id] = true
			}
		}
		assert.Len(t, browserIDs, 2)
		assert.Contains(t, view, "awaiting inp")
		assert.NotContains(t, view, "Refresh plans")
		assert.Empty(t, strings.TrimSpace(ansi.Strip(m.cachedLines[m.planStart-1])))
		assert.Empty(t, strings.TrimSpace(ansi.Strip(m.cachedLines[m.planStart+1])))
		for y, zone := range m.planClickZones {
			if zone.kind == ClickPlan {
				assert.True(t, strings.HasPrefix(ansi.Strip(m.cachedLines[y]), "  "))
				assert.Contains(t, zone.name, "canonical-")
			}
		}
		before := append([]string(nil), m.cachedLines...)
		m.Update(messages.PlanSidebarDataMsg{Loading: true, Result: plans.ListResult{Plans: input}})
		planView(t, m)
		assert.Equal(t, before, m.cachedLines, "refresh does not shift rows or click geometry")
	}
	m.SetSize(60, 80)
	m.Update(messages.PlanSidebarDataMsg{Err: errors.New("unavailable"), Result: plans.ListResult{Plans: input}})
	view := planView(t, m)
	assert.Contains(t, view, "Showing stale plans")
	assert.Contains(t, view, "Refresh plans")
	found := false
	for y, zone := range m.planClickZones {
		if zone.kind == ClickPlanRefresh {
			kind, _ := m.HandleClickType(m.layoutCfg.PaddingLeft, y-m.scrollview.ScrollOffset())
			assert.Equal(t, ClickPlanRefresh, kind)
			found = true
		}
	}
	assert.True(t, found)
}

func TestPlanSidebar_DefaultCollapsedClickTogglesAndUpdatesPreserveExpansion(t *testing.T) {
	t.Parallel()
	m := newPlanSidebar(t)
	// Use the constructor default rather than the expanded row fixture.
	fresh := New(m.ar, t.Context(), &service.SessionState{}).(*model)
	t.Cleanup(fresh.StopAnimation)
	m.plansCollapsed = fresh.plansCollapsed
	require.True(t, m.plansCollapsed)
	require.NoError(t, m.SetTodos(makeTodos(1)))
	m.SetSectionVisibility(SectionVisibility{ShowPlans: true, HideUsage: true, HideAgents: true, HideTools: true})
	data := messages.PlanSidebarDataMsg{Result: plans.ListResult{Plans: []plans.Plan{{Name: "release", Status: "draft", Version: new(1)}}}}
	m.Update(data)
	view := planView(t, m)
	assert.Contains(t, view, "1 plan")
	assert.NotContains(t, view, "release")
	assert.NotContains(t, view, "Open /plans")
	assert.Equal(t, m.planStart+1, m.planEnd)
	assert.Empty(t, m.planClickZones)

	clickSummary := func() {
		t.Helper()
		x, y := m.layoutCfg.PaddingLeft+1, m.planStart-m.scrollview.ScrollOffset()
		kind, _ := m.HandleClickType(x, y)
		require.Equal(t, ClickNone, kind, "summary is disclosure, not browser navigation")
		_, cmd := m.Update(tea.MouseClickMsg{X: m.xPos + x, Y: m.yPos + y, Button: tea.MouseLeft})
		settlePlacement(t, m, cmd)
	}
	clickSummary()
	view = planView(t, m)
	assert.False(t, m.plansCollapsed)
	assert.True(t, m.todosCollapsed, "Plans disclosure is independent of Todos")
	assert.Contains(t, view, "release")
	assert.Contains(t, view, "Open /plans")
	assert.Contains(t, view, "⌄")

	data.Result.Plans[0].Status = "awaiting review"
	data.Result.Plans[0].Version = new(2)
	m.Update(data)
	view = planView(t, m)
	assert.False(t, m.plansCollapsed)
	assert.Contains(t, view, "awaiting review")
	msg := m.EditPlan("release", "tab")().(messages.EditSidebarPlanMsg)
	assert.Equal(t, 2, msg.ExpectedVersion)
	clickSummary()
	assert.True(t, m.plansCollapsed)
	assert.NotContains(t, planView(t, m), "release")
	m.Update(data)
	assert.True(t, m.plansCollapsed, "refresh never expands a collapsed section")
	m.Update(messages.PlanSidebarDataMsg{})
	assert.NotContains(t, planView(t, m), "plans")
	assert.Empty(t, m.planClickZones)
}
