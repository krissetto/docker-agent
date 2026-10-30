package transcript

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

type transcriptLabelView struct{ label string }

func (v *transcriptLabelView) Init() tea.Cmd                          { return nil }
func (v *transcriptLabelView) Update(tea.Msg) (layout.Model, tea.Cmd) { return v, nil }
func (v *transcriptLabelView) View() string                           { return v.label }
func (v *transcriptLabelView) SetSize(int, int) tea.Cmd               { return nil }

// A transcript built with WithToolRenderers renders tool calls through its own
// registry; a transcript without one keeps the shared default.
func TestTranscriptToolRenderersAreInstanceScoped(t *testing.T) {
	t.Parallel()

	registry := tool.NewRegistry()
	registry.Register("weather_report", func(*animation.Runtime, *types.Message, service.SessionStateReader) layout.Model {
		return &transcriptLabelView{label: "transcript-registry-view"}
	})
	call := types.ToolCallMessage("agent", tools.ToolCall{ID: "call-1", Function: tools.FunctionCall{Name: "weather_report", Arguments: "{}"}},
		tools.Tool{Name: "weather_report"}, types.ToolStatusCompleted)
	ar := animation.NewRuntime()

	scoped := New(ar, service.StaticSessionState{}, WithToolRenderers(registry))
	scoped.Append(call)
	assert.Contains(t, ansi.Strip(scoped.Render(80)), "transcript-registry-view")

	plain := New(ar, service.StaticSessionState{})
	plain.Append(call)
	assert.NotContains(t, ansi.Strip(plain.Render(80)), "transcript-registry-view")
}
