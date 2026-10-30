package tool

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// probeView is a minimal layout.Model used to observe which builder ran.
type probeView struct{ label string }

func (v *probeView) Init() tea.Cmd                          { return nil }
func (v *probeView) Update(tea.Msg) (layout.Model, tea.Cmd) { return v, nil }
func (v *probeView) View() string                           { return v.label }
func (v *probeView) SetSize(int, int) tea.Cmd               { return nil }

func probeBuilder(label string) Builder {
	return func(*animation.Runtime, *types.Message, service.SessionStateReader) layout.Model {
		return &probeView{label: label}
	}
}

func weatherMessage() *types.Message {
	return &types.Message{
		ToolCall:       tools.ToolCall{Function: tools.FunctionCall{Name: "weather_report"}},
		ToolDefinition: tools.Tool{Name: "weather_report"},
	}
}

// Renderers registered on one registry stay out of every other registry and out
// of the shared default, so embedders cannot leak views into each other.
func TestRegistriesAreIndependent(t *testing.T) {
	t.Parallel()

	first, second := NewRegistry(), NewRegistry()
	first.Register("weather_report", probeBuilder("first"))

	ar := animation.NewRuntime()
	assert.Equal(t, "first", first.New(ar, weatherMessage(), service.StaticSessionState{}).View())
	assert.NotEqual(t, "first", second.New(ar, weatherMessage(), service.StaticSessionState{}).View())
	assert.NotEqual(t, "first", DefaultRegistry().New(ar, weatherMessage(), service.StaticSessionState{}).View())
}

// Theme changes run every invalidator registered on the instance.
func TestRegistryInvalidatesCachesOnThemeChange(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	calls := 0
	r.RegisterCacheInvalidator(func() { calls++ })
	r.RegisterCacheInvalidator(func() { calls += 10 })
	r.InvalidateCaches()
	assert.Equal(t, 11, calls)
}
