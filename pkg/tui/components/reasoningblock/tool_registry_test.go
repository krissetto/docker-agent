package reasoningblock

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

type blockLabelView struct{ label string }

func (v *blockLabelView) Init() tea.Cmd                          { return nil }
func (v *blockLabelView) Update(tea.Msg) (layout.Model, tea.Cmd) { return v, nil }
func (v *blockLabelView) View() string                           { return v.label }
func (v *blockLabelView) SetSize(int, int) tea.Cmd               { return nil }

// Nested tool calls in a reasoning block use the block's own registry only.
func TestReasoningBlockToolRenderersAreInstanceScoped(t *testing.T) {
	t.Parallel()

	registry := tool.NewRegistry()
	registry.Register("weather_report", func(*animation.Runtime, *types.Message, service.SessionStateReader) layout.Model {
		return &blockLabelView{label: "block-registry-view"}
	})
	call := types.ToolCallMessage("agent", tools.ToolCall{ID: "call-1", Function: tools.FunctionCall{Name: "weather_report", Arguments: "{}"}},
		tools.Tool{Name: "weather_report"}, types.ToolStatusCompleted)
	ar := animation.NewRuntime()

	scoped := New(ar, "scoped", "agent", service.StaticSessionState{})
	scoped.SetToolRenderers(registry)
	scoped.AddToolCall(call)
	scoped.SetExpandedImmediately(true)
	scoped.SetSize(80, 0)
	assert.Contains(t, ansi.Strip(scoped.View()), "block-registry-view")

	plain := New(ar, "plain", "agent", service.StaticSessionState{})
	plain.AddToolCall(call)
	plain.SetExpandedImmediately(true)
	plain.SetSize(80, 0)
	assert.NotContains(t, ansi.Strip(plain.View()), "block-registry-view")
}
