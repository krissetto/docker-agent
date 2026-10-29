package session

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

func newSchemaRegressionMap() map[string]any {
	return map[string]any{
		"type":       "object",
		"required":   []any{"cmd"},
		"properties": map[string]any{"cmd": map[string]any{"type": "string"}},
	}
}

// schemaRegressionSession has two assistant messages whose tool schemas are one
// shared source map (shared=true) or two equal but distinct source maps.
func schemaRegressionSession(shared bool) *Session {
	sess := New(WithID("schema-regression"))
	common := newSchemaRegressionMap()
	for range 2 {
		params := common
		if !shared {
			params = newSchemaRegressionMap()
		}
		sess.AddMessage(&Message{AgentName: "root", Message: chat.Message{
			Role:            chat.MessageRoleAssistant,
			ToolDefinitions: []tools.Tool{{Name: "shell", Parameters: params}},
		}})
	}
	return sess
}

func schemaRegressionParams(t *testing.T, sess *Session, index int) map[string]any {
	t.Helper()
	params, ok := sess.Messages[index].Message.Message.ToolDefinitions[0].Parameters.(map[string]any)
	require.True(t, ok)
	return params
}

func schemaIdentity(v any) uintptr { return reflect.ValueOf(v).Pointer() }

// Baseline contract: a clone shares no mutable schema state with its source or
// with other clones, even where the source shares one schema map across messages.
func TestCloneSchemaOccurrencesStayIndependentAsInBaseline(t *testing.T) {
	t.Parallel()

	source := schemaRegressionSession(true)
	clone := source.Clone()
	first, second := schemaRegressionParams(t, clone, 0), schemaRegressionParams(t, clone, 1)
	assert.NotEqual(t, schemaIdentity(first), schemaIdentity(second), "clone occurrences must not alias each other")

	first["within-clone"] = true
	assert.NotContains(t, second, "within-clone", "mutating one cloned message must not change another cloned message")
	assert.NotContains(t, schemaRegressionParams(t, source, 0), "within-clone")
	assert.NotContains(t, schemaRegressionParams(t, source, 1), "within-clone")
}

func TestCloneSchemaSourceAndCloneAreIndependent(t *testing.T) {
	t.Parallel()

	source := schemaRegressionSession(false)
	clone := source.Clone()
	schemaRegressionParams(t, clone, 0)["type"] = "mutated-in-clone"
	assert.Equal(t, "object", schemaRegressionParams(t, source, 0)["type"], "clone mutation must not reach the source")

	schemaRegressionParams(t, source, 1)["source-only"] = true
	assert.NotContains(t, schemaRegressionParams(t, clone, 1), "source-only", "source mutation must not reach an existing clone")

	second := source.Clone()
	assert.NotContains(t, schemaRegressionParams(t, second, 0), "mutated-in-clone", "a new clone must not see another clone's mutation")
}

func TestCloneSchemaSnapshotItemsAreIndependent(t *testing.T) {
	t.Parallel()

	source := schemaRegressionSession(true)
	items := source.ItemsSnapshot()
	items[0].Message.Message.ToolDefinitions[0].Parameters.(map[string]any)["snapshot-only"] = true
	assert.NotContains(t, schemaRegressionParams(t, source, 0), "snapshot-only", "snapshot mutation must not reach the source")
	assert.NotContains(t, schemaRegressionParams(t, source, 1), "snapshot-only")

	again := source.ItemsSnapshot()
	assert.NotContains(t, again[1].Message.Message.ToolDefinitions[0].Parameters, "snapshot-only")
}

func TestCloneSchemaSubSessionOccurrencesStayIndependent(t *testing.T) {
	t.Parallel()

	shared := newSchemaRegressionMap()
	root := New(WithID("schema-root"))
	root.AddMessage(&Message{AgentName: "root", Message: chat.Message{Role: chat.MessageRoleAssistant, ToolDefinitions: []tools.Tool{{Name: "shell", Parameters: shared}}}})
	child := New(WithID("schema-child"), WithParentID(root.ID))
	child.AddMessage(&Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, ToolDefinitions: []tools.Tool{{Name: "shell", Parameters: shared}}}})
	root.AddSubSession(child)

	clone := root.Clone()
	rootParams := schemaRegressionParams(t, clone, 0)
	childParams := clone.Messages[1].SubSession.Messages[0].Message.Message.ToolDefinitions[0].Parameters.(map[string]any)
	assert.NotEqual(t, schemaIdentity(rootParams), schemaIdentity(childParams))
	assert.NotEqual(t, schemaIdentity(shared), schemaIdentity(rootParams))
	childParams["child-only"] = true
	assert.NotContains(t, rootParams, "child-only")
}

func TestCloneSchemaPresentationIsIndependentFromToolDefinition(t *testing.T) {
	t.Parallel()

	schema := newSchemaRegressionMap()
	call := tools.ToolCall{ID: "call-1", Function: tools.FunctionCall{Name: "shell"}}
	sess := New(WithID("schema-presentation"))
	sess.AddMessage(&Message{AgentName: "root", Message: chat.Message{
		Role:            chat.MessageRoleAssistant,
		ToolCalls:       []tools.ToolCall{call},
		ToolDefinitions: []tools.Tool{{Name: "shell", Parameters: schema}},
		Presentation: []chat.AssistantPart{{
			Type: chat.AssistantPartToolCall, ToolCallID: call.ID,
			Tool: &chat.AssistantTool{Call: call, Definition: tools.Tool{Name: "shell", Parameters: schema}},
		}},
	}})
	clone := sess.Clone()
	message := clone.Messages[0].Message.Message
	presented := message.Presentation[0].Tool.Definition.Parameters.(map[string]any)
	assert.NotEqual(t, schemaIdentity(message.ToolDefinitions[0].Parameters), schemaIdentity(presented))
	presented["presented-only"] = true
	assert.NotContains(t, message.ToolDefinitions[0].Parameters, "presented-only")
	assert.NotContains(t, schemaRegressionParams(t, sess, 0), "presented-only")
}

func TestCloneTypedSchemaContainersAreIsolated(t *testing.T) {
	t.Parallel()

	sess := New(WithID("schema-typed"))
	sess.AddMessage(&Message{AgentName: "root", Message: chat.Message{
		Role: chat.MessageRoleAssistant,
		ToolDefinitions: []tools.Tool{{Name: "shell", Parameters: map[string]any{
			"required": []string{"cmd"},
			"enum":     map[string]string{"mode": "fast"},
		}}},
	}})
	clone := sess.Clone()
	cloned := schemaRegressionParams(t, clone, 0)
	cloned["required"].([]string)[0] = "mutated"
	cloned["enum"].(map[string]string)["mode"] = "mutated"
	source := schemaRegressionParams(t, sess, 0)
	assert.Equal(t, []string{"cmd"}, source["required"])
	assert.Equal(t, map[string]string{"mode": "fast"}, source["enum"])
}

func TestCloneGenericTypedSchemaContainersKeepTypeAndIsolation(t *testing.T) {
	t.Parallel()

	sess := New(WithID("schema-generic"))
	sess.AddMessage(&Message{AgentName: "root", Message: chat.Message{
		Role: chat.MessageRoleAssistant,
		ToolDefinitions: []tools.Tool{{Name: "shell", Parameters: map[string]any{
			"groups":  map[string][]string{"a": {"x"}},
			"choices": []map[string]any{{"label": "one"}},
		}}},
	}})
	clone := sess.Clone()
	cloned := schemaRegressionParams(t, clone, 0)
	groups, ok := cloned["groups"].(map[string][]string)
	require.True(t, ok, "concrete map type must be preserved")
	require.IsType(t, []map[string]any{}, cloned["choices"])
	groups["a"][0] = "mutated"
	groups["b"] = []string{"added"}
	cloned["choices"].([]map[string]any)[0]["label"] = "mutated"

	source := schemaRegressionParams(t, sess, 0)
	assert.Equal(t, map[string][]string{"a": {"x"}}, source["groups"])
	assert.Equal(t, []map[string]any{{"label": "one"}}, source["choices"])
}
