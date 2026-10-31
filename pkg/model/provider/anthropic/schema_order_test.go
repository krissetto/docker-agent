package anthropic

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
)

type orderedChild struct {
	Zulu  string `json:"zulu"`
	Alpha string `json:"alpha"`
}

type orderedArgs struct {
	Zulu  string         `json:"zulu"`
	Alpha []orderedChild `json:"alpha"`
}

// wireToolSchema returns the raw input_schema bytes of the named tool as they
// appear in a captured request body, so key order is observable.
func wireToolSchema(t *testing.T, raw, name string) (schema json.RawMessage, strict bool) {
	t.Helper()
	var body struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"input_schema"`
			Strict      bool            `json:"strict"`
		} `json:"tools"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &body))
	for _, tool := range body.Tools {
		if tool.Name == name {
			return tool.InputSchema, tool.Strict
		}
	}
	require.FailNow(t, "tool not found in request", name)
	return nil, false
}

func TestWriteFileWireSchemaPathFirst(t *testing.T) {
	t.Parallel()
	schema, err := ConvertParametersToSchema(tools.MustSchemaFor[filesystem.WriteFileArgs]())
	require.NoError(t, err)
	wire, err := json.Marshal(anthropic.ToolParam{Name: "write_file", InputSchema: schema})
	require.NoError(t, err)
	assert.Contains(t, string(wire), `"properties":{"path":`)
	assert.Less(t, bytes.Index(wire, []byte(`"path":`)), bytes.Index(wire, []byte(`"content":`)))
}

func TestBetaToolsRetainNestedDeclarationOrder(t *testing.T) {
	t.Parallel()
	converted, err := convertBetaTools([]tools.Tool{{Name: "ordered", Parameters: tools.MustSchemaFor[orderedArgs]()}})
	require.NoError(t, err)
	data, err := json.Marshal(converted)
	require.NoError(t, err)
	assert.Equal(t, 2, bytes.Count(data, []byte(`"properties":{"zulu":`)))
	assert.NotContains(t, string(data), "x-docker-agent-property-order")
}

// Strict mode restores SDK-dropped keywords through ExtraFields; the property
// order must survive on both the standard and the Beta request paths.
func TestStrictToolWireKeepsDeclarationOrder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		providerOpts map[string]any
	}{
		{name: "standard", providerOpts: map[string]any{"strict_tools": true}},
		// Server-side fallbacks route the request through the Beta Messages API.
		{name: "beta", providerOpts: map[string]any{"strict_tools": true, "fallbacks": []string{"claude-haiku-4-5"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newScriptedServer(t, textReply("msg_strict", "ok"))
			client := newScriptedClient(srv, latest.ModelConfig{
				Provider: "anthropic", Model: "claude-sonnet-4-6", ProviderOpts: tc.providerOpts,
			})
			reqTools := []tools.Tool{
				{Name: "write_file", Description: "write", Parameters: tools.MustSchemaFor[filesystem.WriteFileArgs]()},
				{Name: "ordered", Description: "ordered", Parameters: tools.MustSchemaFor[orderedArgs]()},
			}
			chatTurn(t, client, []chat.Message{user("hello")}, reqTools)

			req := srv.request(t, 0)
			schema, strict := wireToolSchema(t, req.raw, "write_file")
			assert.True(t, strict, "write_file must be sent as a strict tool")
			assert.Less(t, bytes.Index(schema, []byte(`"path":`)), bytes.Index(schema, []byte(`"content":`)))

			schema, strict = wireToolSchema(t, req.raw, "ordered")
			assert.True(t, strict, "ordered must be sent as a strict tool")
			assert.Equal(t, 2, bytes.Count(schema, []byte(`"properties":{"zulu":`)))
		})
	}
}
