package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
	"github.com/docker/docker-agent/pkg/tui/components/panel"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// panelPlansFixture is a bottom-panel root with temp-backed plan storage, so
// the Plans element never reads the user's plan directory.
func panelPlansFixture(t *testing.T) (*appModel, plans.Service) {
	t.Helper()
	root := panelFixture(t)
	svc := plans.NewService(plan.NewFilesystemStorage(t.TempDir()))
	WithPlansService(svc)(root)
	return root, svc
}

func plansPanelZone(t *testing.T, root *appModel) panel.Zone {
	t.Helper()
	for _, zone := range root.panelFrame().Zones {
		if zone.ID == messages.PanelPlans {
			return zone
		}
	}
	require.FailNow(t, "the Plans element has no panel zone")
	return panel.Zone{}
}

func TestPlansPanelElementCountsAndOpensBrowserByMouse(t *testing.T) {
	root, svc := panelPlansFixture(t)
	mustCreatePlan(t, svc, "release", "first")
	mustCreatePlan(t, svc, "migration", "second")
	_, cmd := root.applyPanelSettings(messages.PanelSettings{Elements: []messages.PanelElement{messages.PanelWorkspace, messages.PanelPlans}})
	drainPlanFlow(t, root, cmd)

	require.Contains(t, ansi.Strip(root.renderMessageBar()), "2 plans")
	zone := plansPanelZone(t, root)
	_, cmd = root.handleMouseClick(tea.MouseClickMsg{X: zone.Start + messageBarOrigin(root.width), Y: 39, Button: tea.MouseLeft})
	_, opened := firstOfType[messages.ShowPlanBrowserMsg](collectMsgs(cmd))
	assert.True(t, opened, "a click on the Plans zone opens the plan browser")
}

func TestPlansPanelElementRefreshesWhenEnabledWithoutSidebar(t *testing.T) {
	root, svc := panelPlansFixture(t)
	mustCreatePlan(t, svc, "release", "first")
	root.layoutSettings.ShowPlans = false
	root.hideSidebar = true

	_, cmd := root.applyPanelSettings(messages.PanelSettings{Elements: []messages.PanelElement{messages.PanelPlans}})
	require.NotNil(t, cmd, "enabling the Plans element starts its first read")
	drainPlanFlow(t, root, cmd)
	assert.Len(t, root.planSidebarData.Result.Plans, 1, "the panel reads the shared snapshot even with the sidebar hidden")
	assert.Equal(t, "1 plan", root.planPanelLabel())
}

func TestPlansPanelElementStaysOffInLeanMode(t *testing.T) {
	root, _ := panelPlansFixture(t)
	root.leanMode = true
	root.panelSettings = messages.PanelSettings{Elements: []messages.PanelElement{messages.PanelPlans}}
	assert.False(t, root.planPanelEnabled())
	assert.Nil(t, root.refreshPlanSidebarCmd(), "lean mode loads no plan rows")
}
