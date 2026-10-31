package bedrock

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	smithydocument "github.com/aws/smithy-go/document"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
)

func TestToolSchemaPreservesDeclarationOrder(t *testing.T) {
	t.Parallel()
	type child struct {
		Zulu  string `json:"zulu,omitempty"`
		Alpha string `json:"alpha"`
	}
	type args struct {
		Zulu  string  `json:"zulu,omitempty"`
		Alpha []child `json:"alpha"`
	}
	schema := convertToolSchema(tools.MustSchemaFor[args]())
	// The Converse serializer writes documents that implement Marshaler verbatim.
	_, ok := schema.(smithydocument.Marshaler)
	require.True(t, ok)
	data, err := schema.MarshalSmithyDocument()
	require.NoError(t, err)
	assert.NotContains(t, string(data), "x-docker-agent-property-order")
	assert.Equal(t, 2, bytes.Count(data, []byte(`"properties":{"zulu":`)))
}

// The Converse request must carry WriteFileArgs with path declared before content.
func TestConverseRequestToolSchemaPathFirst(t *testing.T) {
	t.Parallel()

	var body []byte
	client, err := NewClient(t.Context(), &latest.ModelConfig{
		Provider:     "amazon-bedrock",
		Model:        "anthropic.claude-v2",
		TokenKey:     "BEDROCK_TOKEN",
		ProviderOpts: map[string]any{"region": "eu-west-1"},
	}, environment.NewMapEnvProvider(map[string]string{"BEDROCK_TOKEN": "test-api-key-12345"}),
		options.WithHTTPTransportWrapper(func(http.RoundTripper) http.RoundTripper {
			return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				body, _ = io.ReadAll(req.Body)
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"message":"stop"}`)),
					Request:    req,
				}, nil
			})
		}))
	require.NoError(t, err)

	_, err = client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}},
		[]tools.Tool{{Name: "write_file", Description: "write", Parameters: tools.MustSchemaFor[filesystem.WriteFileArgs]()}})
	require.Error(t, err)
	require.NotEmpty(t, body, "request must reach the transport")

	var req struct {
		ToolConfig struct {
			Tools []struct {
				ToolSpec struct {
					InputSchema struct {
						JSON json.RawMessage `json:"json"`
					} `json:"inputSchema"`
				} `json:"toolSpec"`
			} `json:"tools"`
		} `json:"toolConfig"`
	}
	require.NoError(t, json.Unmarshal(body, &req))
	require.Len(t, req.ToolConfig.Tools, 1)
	schema := req.ToolConfig.Tools[0].ToolSpec.InputSchema.JSON
	assert.Contains(t, string(schema), `"properties":{"path":`)
	assert.Less(t, bytes.Index(schema, []byte(`"path":`)), bytes.Index(schema, []byte(`"content":`)))
}
