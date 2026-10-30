package messages

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/tool"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type labelView struct{ label string }

func (v *labelView) Init() tea.Cmd                          { return nil }
func (v *labelView) Update(tea.Msg) (layout.Model, tea.Cmd) { return v, nil }
func (v *labelView) View() string                           { return v.label }
func (v *labelView) SetSize(int, int) tea.Cmd               { return nil }

// A message list built with WithToolRenderers renders tools through its own
// registry, not through renderers registered elsewhere.
func TestWithToolRenderersRendersToolsThroughInstanceRegistry(t *testing.T) {
	t.Parallel()

	registry := tool.NewRegistry()
	registry.Register("weather_report", func(*animation.Runtime, *types.Message, service.SessionStateReader) layout.Model {
		return &labelView{label: "instance-renderer"}
	})

	m := NewScrollableView(animation.NewRuntime(), 80, 24, &service.SessionState{}, WithToolRenderers(registry))
	m.AddOrUpdateToolCall("root", tools.ToolCall{ID: "call-1", Function: tools.FunctionCall{Name: "weather_report", Arguments: "{}"}},
		tools.Tool{Name: "weather_report"}, types.ToolStatusCompleted)
	m.SetSize(80, 24)

	assert.Contains(t, ansi.Strip(m.View()), "instance-renderer")
}
