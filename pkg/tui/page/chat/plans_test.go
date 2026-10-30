package chat

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// planTestClock drives the sidebar's animation runtime deterministically so
// placement entrances settle in bounded frames regardless of wall time.
type planTestClock struct {
	now time.Time
}

func (c *planTestClock) Now() time.Time { return c.now }
func (c *planTestClock) Tick(_ time.Duration, f func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		c.now = c.now.Add(50 * time.Millisecond)
		return f(c.now)
	}
}

// newPlansLayoutPage builds a layout test page whose sidebar animates on a
// deterministic clock. The caller settles it with settlePlansSidebar.
func newPlansLayoutPage(t *testing.T, position msgtypes.SidebarPosition) (*chatPage, *animation.Runtime) {
	t.Helper()
	p := newLayoutTestPage(t, position)
	ar := animation.NewRuntimeWithScheduler(&planTestClock{now: time.Unix(1, 0)})
	p.sidebar = sidebar.New(ar, t.Context(), p.sessionState)
	return p, ar
}

func settlePlansSidebar(t *testing.T, p *chatPage, ar *animation.Runtime) {
	t.Helper()
	for steps := 0; ar.HasActive(); steps++ {
		require.Less(t, steps, 100, "plan sidebar animations must settle")
		cmd := ar.Continue()
		require.NotNil(t, cmd)
		tick, ok := cmd().(animation.TickMsg)
		require.True(t, ok)
		accepted, ok := ar.Accept(tick)
		require.True(t, ok)
		p.Update(accepted)
	}
}

func TestPlanSidebar_ClickRoutesDirectEdit(t *testing.T) {
	t.Parallel()
	for _, position := range []msgtypes.SidebarPosition{msgtypes.SidebarLeft, msgtypes.SidebarRight} {
		t.Run(string(position), func(t *testing.T) {
			t.Parallel()
			p, ar := newPlansLayoutPage(t, position)
			p.SetRoutingID("origin")
			p.SetLayoutSettings(msgtypes.LayoutSettings{
				SidebarPosition: position, ShowPlans: true,
				HideUsage: true, HideAgents: true, HideTools: true, HideTodos: true,
			})
			p.SetSize(160, 40)
			p.Update(msgtypes.PlanSidebarDataMsg{Result: plans.ListResult{Plans: []plans.Plan{
				{Name: "release", Version: new(7)},
			}}})
			settlePlansSidebar(t, p, ar)
			sl := p.computeSidebarLayout()
			lines := strings.Split(ansi.Strip(p.sidebar.View()), "\n")
			found := false
			for y, line := range lines {
				if !strings.Contains(line, "release") {
					continue
				}
				found = true
				x := styles.AppPadding + sl.sidebarStartX + strings.Index(line, "release")
				hit := NewHitTest(p)
				require.Equal(t, TargetSidebarPlan, hit.At(x, y))
				assert.Equal(t, "release", hit.PlanName)
				_, cmd := p.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
				require.NotNil(t, cmd)
				msg, ok := cmd().(msgtypes.EditSidebarPlanMsg)
				require.True(t, ok)
				assert.Equal(t, plans.SharedRef("release"), msg.Ref)
				assert.Equal(t, 7, msg.ExpectedVersion)
				assert.Equal(t, "origin", msg.TabID)
				_, cmd = p.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseRight})
				assert.Nil(t, cmd)
			}
			assert.True(t, found)
		})
	}
}

func TestPlanSidebar_BandClickOpensBrowser(t *testing.T) {
	t.Parallel()
	for _, position := range []msgtypes.SidebarPosition{msgtypes.SidebarTop, msgtypes.SidebarBottom} {
		t.Run(string(position), func(t *testing.T) {
			t.Parallel()
			p, ar := newPlansLayoutPage(t, position)
			p.SetLayoutSettings(msgtypes.LayoutSettings{
				SidebarPosition: position, ShowPlans: true, HideAgents: true, HideTools: true, HideTodos: true,
			})
			p.SetSize(90, 40)
			p.Update(msgtypes.PlanSidebarDataMsg{Result: plans.ListResult{Plans: []plans.Plan{{Name: "release"}}}})
			settlePlansSidebar(t, p, ar)
			found := false
			// Locate the link in the page as rendered, so the band's real
			// offset (including the bottom band's chat above it) is measured.
			for y, line := range strings.Split(ansi.Strip(p.View()), "\n") {
				if !strings.Contains(line, "Plans (1)") {
					continue
				}
				found = true
				x := strings.Index(line, "Plans")
				require.Equal(t, TargetSidebarPlanBrowser, NewHitTest(p).At(x, y))
				_, cmd := p.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
				require.NotNil(t, cmd)
				assert.IsType(t, msgtypes.ShowPlanBrowserMsg{}, cmd())
			}
			assert.True(t, found)
		})
	}
}

func TestBackgroundAgentStatusReachesSidebarCount(t *testing.T) {
	t.Parallel()
	p, ar := newPlansLayoutPage(t, msgtypes.SidebarRight)
	p.SetSize(160, 40)
	p.Update(msgtypes.BackgroundAgentStatusMsg{ChildSessionID: "child", AgentName: "reviewer", Running: true})
	settlePlansSidebar(t, p, ar)
	assert.Contains(t, ansi.Strip(p.sidebar.View()), "Background: 1 running")
}
