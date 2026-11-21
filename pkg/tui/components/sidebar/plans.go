package sidebar

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const recentPlanLimit = 5

type planClickZone struct {
	kind ClickResult
	name string
}

func (m *model) setPlans(data messages.PlanSidebarDataMsg) {
	m.planData = data
	m.recentPlans = slices.Clone(data.Result.Plans)
	slices.SortFunc(m.recentPlans, func(a, b plans.Plan) int {
		if order := b.UpdatedAt.Compare(a.UpdatedAt); order != 0 {
			return order
		}
		return cmp.Compare(a.Name, b.Name)
	})
	m.recentPlans = m.recentPlans[:min(len(m.recentPlans), recentPlanLimit)]
	m.invalidateCache()
}

func (m *model) EditPlan(name, tabID string) tea.Cmd {
	for _, p := range m.recentPlans {
		if p.Name == name && p.Version != nil {
			return core.CmdHandler(messages.EditSidebarPlanMsg{
				TabID: tabID, Ref: plans.SharedRef(p.Name), ExpectedVersion: *p.Version,
			})
		}
	}
	return notification.WarningCmd("Plan is no longer available for editing. Refresh plans and retry.")
}

// hasPlansSection keeps unused plans quiet without hiding read failures.
func (m *model) hasPlansSection() bool {
	return m.sectionVisibility.ShowPlans && (len(m.recentPlans) > 0 ||
		len(m.planData.Result.Warnings) > 0 || m.planData.Err != nil)
}

func (m *model) planSummary(width int) string {
	label := fmt.Sprintf("%d plans", len(m.planData.Result.Plans))
	if len(m.planData.Result.Plans) == 1 {
		label = "1 plan"
	}
	if m.planData.Err != nil && len(m.recentPlans) == 0 {
		label = "Plans unavailable"
	}
	if len(m.planData.Result.Warnings) > 0 || m.planData.Err != nil && len(m.recentPlans) > 0 {
		label += " (warnings)"
	}
	glyph := "⌄"
	if m.plansCollapsed {
		glyph = "›"
	}
	if width <= 1 {
		return styles.MutedStyle.Render(glyph)
	}
	text := styles.MutedStyle.Render(ansi.Truncate(label, width-2, "…"))
	return m.hoverText(text+strings.Repeat(" ", max(1, width-ansi.StringWidth(text)-1))+styles.MutedStyle.Render(glyph), ClickPlanSummary)
}

// plansSection renders the Plans section body. Zones are relative to the
// section's first line; renderSections maps them to content lines after
// breathing rows are inserted.
func (m *model) plansSection(width int) (string, []planClickZone) {
	lines := []string{m.planSummary(width)}
	zones := []planClickZone{{kind: ClickPlanSummary}}
	if m.plansCollapsed {
		return strings.Join(lines, "\n"), zones
	}
	lines = append(lines, "")
	zones = append(zones, planClickZone{})
	indent := min(2, max(0, width-1))
	bodyWidth := max(1, width-indent)
	add := func(text string, zone planClickZone) {
		lines = append(lines, strings.Repeat(" ", indent)+ansi.Truncate(text, bodyWidth, "…"))
		zones = append(zones, zone)
	}
	if m.planData.Err != nil && len(m.recentPlans) > 0 {
		add(styles.MutedStyle.Render("Showing stale plans"), planClickZone{})
	}
	if n := len(m.planData.Result.Warnings); n > 0 {
		add(styles.MutedStyle.Render(fmt.Sprintf("%d plan(s) could not be read", n)), planClickZone{})
	}
	for _, p := range m.recentPlans {
		label := strings.Join(strings.Fields(p.Title), " ")
		if label == "" {
			label = p.Name
		}
		status := strings.Join(strings.Fields(p.Status), " ")
		status = toolcommon.TruncateText(status, bodyWidth/2)
		statusWidth := ansi.StringWidth(status)
		if bodyWidth < 3 {
			status, statusWidth = "", 0
		}
		labelWidth := bodyWidth
		if statusWidth > 0 {
			labelWidth -= statusWidth + 1
		}
		label = toolcommon.TruncateText(label, max(1, labelWidth))
		row := styles.BaseStyle.Render(label)
		if statusWidth > 0 {
			row += strings.Repeat(" ", max(1, bodyWidth-ansi.StringWidth(label)-statusWidth)) + styles.MutedStyle.Render(status)
		}
		add(row, planClickZone{kind: ClickPlan, name: p.Name})
	}
	add(styles.MutedStyle.Render("Open /plans"), planClickZone{kind: ClickPlanBrowser})
	if remaining := len(m.planData.Result.Plans) - len(m.recentPlans); remaining > 0 {
		add(styles.MutedStyle.Render(fmt.Sprintf("+%d more", remaining)), planClickZone{kind: ClickPlanBrowser})
	}
	if m.planData.Err != nil || len(m.planData.Result.Warnings) > 0 {
		add(styles.MutedStyle.Render("Refresh plans"), planClickZone{kind: ClickPlanRefresh})
	}
	return strings.Join(lines, "\n"), zones
}

// plansSummary is the one-line Plans entry for the collapsed band. It is a
// link to the plan browser, so the band never grows beyond its two rows.
func (m *model) plansSummary() string {
	if !m.hasPlansSection() {
		return ""
	}
	label := fmt.Sprintf("Plans (%d) - open /plans", len(m.planData.Result.Plans))
	switch {
	case m.planData.Err != nil:
		label = "Plans unavailable - open /plans"
	case m.planData.Loading:
		label = "Loading plans - open /plans"
	case len(m.planData.Result.Warnings) > 0:
		label += " (warnings)"
	}
	return styles.MutedStyle.Render(label)
}
