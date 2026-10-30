package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/tool"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type appLabelView struct{ label string }

func (v *appLabelView) Init() tea.Cmd                          { return nil }
func (v *appLabelView) Update(tea.Msg) (layout.Model, tea.Cmd) { return v, nil }
func (v *appLabelView) View() string                           { return v.label }
func (v *appLabelView) SetSize(int, int) tea.Cmd               { return nil }

// Each application keeps its own tool renderers: a renderer registered for one
// app must not change what another app renders.
func TestToolRendererOptionsAreAppScoped(t *testing.T) {
	t.Parallel()

	first, _ := newTestModel(t)
	second, _ := newTestModel(t)
	WithToolRenderers(map[string]tool.Builder{"weather_report": labelBuilder("first")})(first)
	WithToolRenderers(map[string]tool.Builder{"weather_report": labelBuilder("second")})(second)

	msg := &types.Message{
		ToolCall:       tools.ToolCall{Function: tools.FunctionCall{Name: "weather_report"}},
		ToolDefinition: tools.Tool{Name: "weather_report"},
	}
	ar := animation.NewRuntime()
	assert.Equal(t, "first", first.toolRenderers.New(ar, msg, service.StaticSessionState{}).View())
	assert.Equal(t, "second", second.toolRenderers.New(ar, msg, service.StaticSessionState{}).View())
	assert.NotEqual(t, "first", tool.DefaultRegistry().New(ar, msg, service.StaticSessionState{}).View())
}

func labelBuilder(label string) tool.Builder {
	return func(*animation.Runtime, *types.Message, service.SessionStateReader) layout.Model {
		return &appLabelView{label: label}
	}
}
