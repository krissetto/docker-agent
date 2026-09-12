package tool

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
	shelltool "github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// withCleanToolRegistry snapshots the package-global custom renderer registry and
// restores it when the test finishes, so Register calls don't leak across tests.
// Tests using it must not run in parallel: they would race on the shared registry.
func withCleanToolRegistry(t *testing.T) {
	t.Helper()
	customMu.Lock()
	saved := custom
	custom = map[string]Builder{}
	customMu.Unlock()
	t.Cleanup(func() {
		customMu.Lock()
		defer customMu.Unlock()
		custom = saved
	})
}

func TestRegisterAndResolve(t *testing.T) {
	withCleanToolRegistry(t)

	// Unknown, unregistered key resolves to nothing.
	_, ok := resolve("add")
	assert.False(t, ok)

	// A new tool name resolves to its registered renderer.
	customCalled := false
	Register("add", func(*animation.Runtime, *types.Message, service.SessionStateReader) layout.Model {
		customCalled = true
		return nil
	})
	b, ok := resolve("add")
	assert.True(t, ok)
	b(animation.NewRuntime(), nil, nil)
	assert.True(t, customCalled)

	// A custom renderer takes precedence over a built-in one for the same key.
	overrodeBuiltin := false
	Register(shelltool.ToolNameShell, func(*animation.Runtime, *types.Message, service.SessionStateReader) layout.Model {
		overrodeBuiltin = true
		return nil
	})
	b, ok = resolve(shelltool.ToolNameShell)
	assert.True(t, ok)
	b(animation.NewRuntime(), nil, nil)
	assert.True(t, overrodeBuiltin)

	// A built-in with no custom override still resolves to its built-in renderer.
	_, ok = resolve(filesystem.ToolNameReadFile)
	assert.True(t, ok)
}

func TestCustomSubagentRendererOverridesInjectedBuiltin(t *testing.T) {
	withCleanToolRegistry(t)
	called := false
	Register(subagent.ToolSpawnSubagent, func(*animation.Runtime, *types.Message, service.SessionStateReader) layout.Model {
		called = true
		return nil
	})
	msg := &types.Message{ToolCall: tools.ToolCall{Function: tools.FunctionCall{Name: subagent.ToolSpawnSubagent}}}
	_ = New(animation.NewRuntime(), msg, service.StaticSessionState{})
	assert.True(t, called)
}

// TestNew_Dispatch verifies New's renderer selection: a registered renderer is
// chosen by exact tool name first, then by "category:<category>", with the exact
// name winning when both match, and an unregistered tool falling through to the
// default. The factory is origin-agnostic — it keys only on the tool-call name
// and category — so this holds for built-in, Go-SDK, and MCP tools alike. (For an
// end-to-end custom renderer over a real MCP tool, see examples/golibrary/renderer.)
func TestNew_Dispatch(t *testing.T) {
	ss := service.StaticSessionState{}

	newMsg := func() *types.Message {
		return &types.Message{
			ToolCall:       tools.ToolCall{Function: tools.FunctionCall{Name: "weather_report"}},
			ToolDefinition: tools.Tool{Name: "weather_report", Category: "external"},
		}
	}

	t.Run("by exact tool name", func(t *testing.T) {
		withCleanToolRegistry(t)
		called := false
		Register("weather_report", func(*animation.Runtime, *types.Message, service.SessionStateReader) layout.Model {
			called = true
			return nil
		})
		New(animation.NewRuntime(), newMsg(), ss)
		assert.True(t, called, "renderer registered under the exact tool name should be selected")
	})

	t.Run("by category", func(t *testing.T) {
		withCleanToolRegistry(t)
		called := false
		Register("category:external", func(*animation.Runtime, *types.Message, service.SessionStateReader) layout.Model {
			called = true
			return nil
		})
		New(animation.NewRuntime(), newMsg(), ss)
		assert.True(t, called, "a category renderer should match any tool in that category")
	})

	t.Run("exact name wins over category", func(t *testing.T) {
		withCleanToolRegistry(t)
		exactCalled, categoryCalled := false, false
		Register("weather_report", func(*animation.Runtime, *types.Message, service.SessionStateReader) layout.Model {
			exactCalled = true
			return nil
		})
		Register("category:external", func(*animation.Runtime, *types.Message, service.SessionStateReader) layout.Model {
			categoryCalled = true
			return nil
		})
		New(animation.NewRuntime(), newMsg(), ss)
		assert.True(t, exactCalled, "exact-name renderer should take precedence")
		assert.False(t, categoryCalled, "category renderer should not run when an exact-name match exists")
	})

	t.Run("unregistered tool falls through to default", func(t *testing.T) {
		withCleanToolRegistry(t)
		_, byName := resolve("weather_report")
		_, byCategory := resolve("category:external")
		assert.False(t, byName, "no per-tool renderer registered")
		assert.False(t, byCategory, "no category renderer registered")
	})
}

// TestPlanToolsRouting locks in which plan tools get the status-surfacing
// renderer: the single-plan write/status tools do, while read_plan (shows the
// full body), list_plans (many plans) and delete_plan (no status) intentionally
// fall through to the default renderer.
func TestNewRendersResultImagesWithinToolWidth(t *testing.T) {
	msg := &types.Message{
		ToolCall: tools.ToolCall{Function: tools.FunctionCall{Name: "image_tool"}},
		Images: []tuiimage.Inline{{
			Name: "screenshot.png", MIME: "image/png", PNGData: []byte("png"), Width: 1600, Height: 900,
		}},
	}

	view := New(animation.NewRuntime(), msg, service.StaticSessionState{})
	view.SetSize(40, 0)
	rendered := view.View()

	assert.Contains(t, rendered, "cagent-image")
	assert.Contains(t, rendered, ";36;", "image must stay inside the tool's available width")
	assert.LessOrEqual(t, len(strings.Split(rendered, "\n")), 22, "image height must remain bounded")
}

func TestPlanToolsRouting(t *testing.T) {
	withCleanToolRegistry(t)

	for _, name := range []string{
		plan.ToolNameWritePlan,
		plan.ToolNameSetPlanStatus,
		plan.ToolNameGetPlanStatus,
		plan.ToolNameUpdatePlanFromFile,
		plan.ToolNameExportPlanToFile,
	} {
		_, ok := resolve(name)
		assert.True(t, ok, "%q should have a dedicated plan renderer", name)
	}

	for _, name := range []string{plan.ToolNameReadPlan, plan.ToolNameListPlans, plan.ToolNameDeletePlan} {
		_, ok := resolve(name)
		assert.False(t, ok, "%q should fall through to the default renderer", name)
	}
}
