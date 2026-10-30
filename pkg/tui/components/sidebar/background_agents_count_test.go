package sidebar

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestBackgroundAgentStatusCountsRunningAgents(t *testing.T) {
	t.Parallel()
	m := newPlanSidebar(t)
	m.SetSectionVisibility(SectionVisibility{HideUsage: true, HideAgents: true, HideTools: true, HideTodos: true})
	assert.NotContains(t, planView(t, m), "Background:")

	m.Update(messages.BackgroundAgentStatusMsg{ChildSessionID: "child-a", AgentName: "reviewer", Running: true})
	m.Update(messages.BackgroundAgentStatusMsg{ChildSessionID: "child-b", AgentName: "tester", Running: true})
	m.Update(messages.BackgroundAgentStatusMsg{ChildSessionID: "child-a", AgentName: "reviewer", Running: true})
	assert.Contains(t, planView(t, m), "Background: 2 running", "a repeated start is counted once")

	m.Update(messages.BackgroundAgentStatusMsg{ChildSessionID: "child-a", Running: false})
	assert.Contains(t, planView(t, m), "Background: 1 running")

	m.Update(messages.BackgroundAgentStatusMsg{ChildSessionID: "child-b", Running: false})
	m.Update(messages.BackgroundAgentStatusMsg{ChildSessionID: "", Running: true})
	assert.NotContains(t, planView(t, m), "Background:", "an empty count hides the line")
}
