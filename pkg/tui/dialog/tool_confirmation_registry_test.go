package dialog

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/tool"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	tuimessages "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// registeredPreviewView is an embedder renderer used as the confirmation preview.
type registeredPreviewView struct{ lines []string }

func (v *registeredPreviewView) Init() tea.Cmd                          { return nil }
func (v *registeredPreviewView) Update(tea.Msg) (layout.Model, tea.Cmd) { return v, nil }
func (v *registeredPreviewView) View() string                           { return strings.Join(v.lines, "\n") }
func (v *registeredPreviewView) SetSize(int, int) tea.Cmd               { return nil }

func weatherConfirmationEvent() *runtime.ToolCallConfirmationEvent {
	return &runtime.ToolCallConfirmationEvent{
		ToolCall:       tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "weather_report", Arguments: `{"city":"Paris"}`}},
		ToolDefinition: tools.Tool{Name: "weather_report"},
	}
}

// An application-registered renderer replaces the static argument summary in the
// confirmation, and the decision buttons keep their painted, clickable layout.
func TestToolConfirmationPreviewUsesRegisteredRenderer(t *testing.T) {
	t.Parallel()

	registry := tool.NewRegistry()
	registry.Register("weather_report", func(*animation.Runtime, *types.Message, service.SessionStateReader) layout.Model {
		return &registeredPreviewView{lines: []string{"REGISTERED-PREVIEW one", "two", "three"}}
	})

	dialog := NewToolConfirmationDialog(animation.NewRuntime(), weatherConfirmationEvent(), &service.SessionState{}, registry)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	d, ok := dialog.(*toolConfirmationDialog)
	require.True(t, ok)

	view := ansi.Strip(d.View())
	assert.Contains(t, view, "REGISTERED-PREVIEW one")
	assert.NotContains(t, view, "Proposed: weather_report")

	y, x, found := locateInView(d, "Yes, once")
	require.True(t, found, "the decision buttons must stay painted under the registered preview")
	response, ok := findMsg[tuimessages.InteractionResponseMsg](collectMsgs(clickCell(d, y, x)))
	require.True(t, ok, "clicking 'Yes, once' must still dispatch a decision")
	assert.Equal(t, runtime.ResumeApprove(), response.Response.Resume)

	// The layout reserves the registered preview's height, so the decisions sit below it.
	assert.Contains(t, view, "three", "all registered preview lines must be visible")
	assert.Greater(t, y, dialogRowOf(d, "three"), "decisions render below the preview")
}

// Without a registered renderer, and for built-in tools, the static summary stays.
func TestToolConfirmationPreviewKeepsStaticSummaryWithoutCustomRenderer(t *testing.T) {
	t.Parallel()

	for name, registry := range map[string]*tool.Registry{
		"no registry":      nil,
		"empty registry":   tool.NewRegistry(),
		"builtin-only app": tool.DefaultRegistry(),
	} {
		dialog := NewToolConfirmationDialog(animation.NewRuntime(), weatherConfirmationEvent(), &service.SessionState{}, registry)
		_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
		view := ansi.Strip(dialog.View())
		assert.Contains(t, view, "Proposed: weather_report", name)
	}
}

// dialogRowOf returns the screen row of the first line containing needle.
func dialogRowOf(d *toolConfirmationDialog, needle string) int {
	row, _ := d.Position()
	for i, line := range strings.Split(ansi.Strip(d.View()), "\n") {
		if strings.Contains(line, needle) {
			return row + i
		}
	}
	return -1
}
