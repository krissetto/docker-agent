package dialog

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func testAgentPicker(t *testing.T, count, width, height int) *agentPickerDialog {
	t.Helper()
	agents := make([]runtime.AgentDetails, count)
	for i := range agents {
		agents[i] = runtime.AgentDetails{Name: fmt.Sprintf("agent-%03d", i), Provider: "provider", Model: "model", Thinking: "high", Description: "A helpful agent with a detailed description for the team."}
	}
	d := NewAgentPickerDialog(agents, "agent-000", "/same/directory").(*agentPickerDialog)
	d.SetSize(width, height)
	return d
}

func TestAgentPickerLargeTeamNavigationAndSelection(t *testing.T) {
	d := testAgentPicker(t, 137, 120, 30)
	require.Greater(t, d.columns, 1)
	require.Contains(t, d.View(), "provider/model")
	require.Contains(t, d.View(), "Thinking: high")
	d.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	require.Equal(t, 136, d.selected)
	require.Positive(t, d.BodyScrollOffset())
	require.Contains(t, d.View(), "agent-136")
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Contains(t, collectMsgs(cmd), tea.Msg(messages.NewAgentSessionMsg{AgentName: "agent-136", WorkingDir: "/same/directory"}))
	d.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	require.Zero(t, d.BodyScrollOffset())
}

func TestAgentPickerClickGridAndScrolledRows(t *testing.T) {
	d := testAgentPicker(t, 50, 120, 30)
	d.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	x, y, _, _ := d.BodyScrollBounds()
	line := d.BodyScrollOffset()
	index := line/(agentCardHeight+1)*d.columns + 1
	_, cmd := d.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: x + d.cardWidth + 2, Y: y})
	require.Contains(t, collectMsgs(cmd), tea.Msg(messages.NewAgentSessionMsg{AgentName: fmt.Sprintf("agent-%03d", index), WorkingDir: "/same/directory"}))
}

func TestAgentPickerResponsiveBoundsAndEmpty(t *testing.T) {
	for _, size := range [][2]int{{40, 18}, {80, 24}, {160, 50}} {
		d := testAgentPicker(t, 17, size[0], size[1])
		for _, line := range strings.Split(d.View(), "\n") {
			require.LessOrEqual(t, lipgloss.Width(line), size[0])
		}
		require.LessOrEqual(t, lipgloss.Height(d.View()), size[1])
		if size[0] == 40 {
			require.Equal(t, 1, d.columns)
		}
	}
	d := testAgentPicker(t, 0, 80, 24)
	require.Contains(t, d.View(), "No agents available")
	require.Nil(t, d.choose())
}

func TestAgentPickerGridGapsAndUnusedCellsDoNotSelect(t *testing.T) {
	d := testAgentPicker(t, 4, 120, 40)
	x, y, _, _ := d.BodyScrollBounds()
	for _, point := range [][2]int{{x + d.cardWidth, y}, {x, y + agentCardHeight}, {x + 2*(d.cardWidth+1), y + agentCardHeight + 1}} {
		_, cmd := d.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: point[0], Y: point[1]})
		require.Nil(t, cmd)
	}
}

func TestAgentPickerResizeAndLongDetailsKeepCardGeometry(t *testing.T) {
	d := testAgentPicker(t, 2, 120, 30)
	d.agents[0].Name = strings.Repeat("界", 100)
	d.agents[0].Model = strings.Repeat("model", 100)
	d.agents[0].Description = strings.Repeat("details ", 100)
	d.SetSize(40, 18)
	require.Equal(t, agentCardHeight, lipgloss.Height(d.renderCard(0)))
	require.Equal(t, d.cardWidth, lipgloss.Width(d.renderCard(0)))
}

func TestAgentPickerWheelScrollDoesNotSnapToSelection(t *testing.T) {
	d := testAgentPicker(t, 137, 100, 30)
	x, y, _, _ := d.BodyScrollBounds()
	d.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: x, Y: y})
	require.Positive(t, d.BodyScrollOffset())
	require.Zero(t, d.selected)
}
