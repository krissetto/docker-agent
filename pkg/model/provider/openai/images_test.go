package openai

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testImageBytes = []byte("\x89PNG\r\n\x1a\n" + "image payload")

func TestImagesEndpoint(t *testing.T) {
	for _, model := range []string{"flux-local", "gpt-image-1", "chatgpt-image-latest", "dall-e-3"} {
		t.Run(model, func(t *testing.T) {
			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/v1/images/generations", r.URL.Path)
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"data":[{"b64_json":%q},{"b64_json":%q}]}`, base64.StdEncoding.EncodeToString(testImageBytes), base64.StdEncoding.EncodeToString(testImageBytes))
			}))
			defer server.Close()
			client, err := NewClient(t.Context(), &latest.ModelConfig{OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(true)}, Provider: "local", Model: model, BaseURL: server.URL + "/v1", ImageGeneration: map[string]any{"size": "1024x1024", "steps": 4}, ProviderOpts: map[string]any{"api_type": "openai_images"}}, environment.NewNoEnvProvider())
			require.NoError(t, err)
			stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleSystem, Content: "use watercolors"}, {Role: chat.MessageRoleUser, Content: "old request"}, {Role: chat.MessageRoleAssistant, Content: "old response", MultiContent: []chat.MessagePart{{Type: chat.MessagePartTypeImageURL}}}, {Role: chat.MessageRoleUser, Content: "draw cat", MultiContent: []chat.MessagePart{{Type: chat.MessagePartTypeText, Text: "in a hat"}}}}, nil)
			require.NoError(t, err)
			defer stream.Close()
			response, err := stream.Recv()
			require.NoError(t, err)
			require.Len(t, response.Choices[0].Delta.Media, 2)
			assert.Equal(t, testImageBytes, response.Choices[0].Delta.Media[0].Data)
			assert.Equal(t, "image/png", response.Choices[0].Delta.Media[0].MimeType)
			assert.Empty(t, response.Choices[0].Delta.Content)
			assert.Equal(t, "use watercolors\n\ndraw cat\nin a hat", body["prompt"])
			assert.Equal(t, model, body["model"])
			assert.Equal(t, float64(4), body["steps"])
			if model == "gpt-image-1" || model == "chatgpt-image-latest" {
				assert.NotContains(t, body, "response_format")
			} else {
				assert.Equal(t, "b64_json", body["response_format"])
			}
			_, err = stream.Recv()
			require.ErrorIs(t, err, io.EOF)
		})
	}
}

func TestImagesValidation(t *testing.T) {
	client, err := NewClient(t.Context(), &latest.ModelConfig{OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(true)}, Provider: "local", Model: "flux", ProviderOpts: map[string]any{"api_type": "openai_images"}}, environment.NewNoEnvProvider())
	require.NoError(t, err)
	_, err = client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "cat"}}, []tools.Tool{{Name: "shell"}})
	require.ErrorContains(t, err, "does not support function tools")
	_, err = client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleAssistant, Content: "cat"}}, nil)
	require.ErrorContains(t, err, "nonempty user text")
	_, err = client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "cat", MultiContent: []chat.MessagePart{{Type: chat.MessagePartTypeImageURL}}}}, nil)
	require.ErrorContains(t, err, "image edits are unsupported")
	for _, fields := range []map[string]any{{"stream": true}, {"prompt": "override"}, {"type": "override"}, {"model": "override"}} {
		cfg := latest.ModelConfig{Provider: "local", OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(true)}, ProviderOpts: map[string]any{"api_type": "openai_images"}, ImageGeneration: fields}
		require.Error(t, cfg.ValidateResolvedImageGeneration())
	}

}

func TestResponseImages(t *testing.T) {
	image := map[string]any{"type": "image_generation_call", "id": "ig_1", "status": "completed", "result": base64.StdEncoding.EncodeToString(testImageBytes)}
	for _, terminalOnly := range []bool{false, true} {
		events := []map[string]any{}
		if !terminalOnly {
			events = append(events, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": image})
		}
		events = append(events, map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{image}}})
		adapter := newResponseStreamAdapter(&fakeEventStream{events: decodeEvents(t, events)}, false)
		adapter.imageOutputEnabled = true
		count := 0
		for {
			response, err := adapter.Recv()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			for _, choice := range response.Choices {
				count += len(choice.Delta.Media)
				assert.Empty(t, choice.Delta.Content)
			}
		}
		assert.Equal(t, 1, count)
	}
}

func TestResponseImageTool(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/responses", r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		tool := body["tools"].([]any)[0].(map[string]any)
		assert.Equal(t, "image_generation", tool["type"])
		assert.Equal(t, "gpt-image-1", tool["model"])
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{}}\n\n")
	}))
	defer server.Close()
	client, err := NewClient(t.Context(), &latest.ModelConfig{OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(true)}, Provider: "local", Model: "gpt-4.1", BaseURL: server.URL, ImageGeneration: map[string]any{"model": "gpt-image-1"}, ProviderOpts: map[string]any{"api_type": "openai_responses"}}, environment.NewNoEnvProvider())
	require.NoError(t, err)
	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "cat"}}, nil)
	require.NoError(t, err)
	defer stream.Close()
	_, err = stream.Recv()
	require.NoError(t, err)
}

func TestImagesBuiltinAuth(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-openai-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer test-openai-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":[{"b64_json":%q}]}`, base64.StdEncoding.EncodeToString(testImageBytes))
	}))
	defer server.Close()
	client, err := NewClient(t.Context(), &latest.ModelConfig{OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(true)}, Provider: "openai", Model: "gpt-image-1", BaseURL: server.URL, ProviderOpts: map[string]any{"api_type": "openai_images"}}, environment.NewNoEnvProvider())
	require.NoError(t, err)
	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "cat"}}, nil)
	require.NoError(t, err)
	stream.Close()
}

func TestImageToolSelectsResponses(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-openai-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/responses", r.URL.Path)
		assert.Equal(t, "Bearer test-openai-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{}}\n\n")
	}))
	defer server.Close()
	client, err := NewClient(t.Context(), &latest.ModelConfig{OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(true)}, Provider: "openai", Model: "gpt-4o", BaseURL: server.URL, ImageGeneration: map[string]any{}}, environment.NewNoEnvProvider())
	require.NoError(t, err)
	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "cat"}}, nil)
	require.NoError(t, err)
	defer stream.Close()
	_, err = stream.Recv()
	require.NoError(t, err)
}

func TestImageToolPreservesExplicitToken(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "wrong-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer explicit-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":[{"b64_json":%q}]}`, base64.StdEncoding.EncodeToString(testImageBytes))
	}))
	defer server.Close()
	client, err := NewClient(t.Context(), &latest.ModelConfig{OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(true)}, Provider: "openai", Model: "gpt-image-1", TokenKey: "CUSTOM_KEY", BaseURL: server.URL, ProviderOpts: map[string]any{"api_type": "openai_images"}}, environment.NewMapEnvProvider(map[string]string{"CUSTOM_KEY": "explicit-key"}))
	require.NoError(t, err)
	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "cat"}}, nil)
	require.NoError(t, err)
	stream.Close()
}

func TestResponseMixedImageDeduplication(t *testing.T) {
	image := map[string]any{"type": "image_generation_call", "id": "ig_1", "status": "completed", "result": base64.StdEncoding.EncodeToString(testImageBytes)}
	events := []map[string]any{
		{"type": "response.output_text.delta", "item_id": "msg_1", "delta": "Here is your cat."},
		{"type": "response.output_item.done", "output_index": 1, "item": image},
		{"type": "response.output_item.done", "output_index": 1, "item": image},
		{"type": "response.completed", "response": map[string]any{"output": []any{image}}},
	}
	adapter := newResponseStreamAdapter(&fakeEventStream{events: decodeEvents(t, events)}, false)
	adapter.imageOutputEnabled = true
	text := ""
	count := 0
	for {
		response, err := adapter.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		for _, choice := range response.Choices {
			text += choice.Delta.Content
			count += len(choice.Delta.Media)
			assert.Empty(t, choice.Delta.ToolCalls)
			assert.Nil(t, choice.Delta.FunctionCall)
		}
	}
	assert.Equal(t, "Here is your cat.", text)
	assert.Equal(t, 1, count)
}

func TestNativeImageExplicitGateAndAuxiliaryRequests(t *testing.T) {
	for _, tc := range []struct {
		name     string
		image    *bool
		opts     []options.Opt
		settings map[string]any
		wantTool bool
	}{
		{name: "omitted"},
		{name: "false", image: new(false)},
		{name: "true without settings", image: new(true), wantTool: true},
		{name: "true with settings", image: new(true), settings: map[string]any{"size": "1024x1024"}, wantTool: true},
		{name: "title", image: new(true), opts: []options.Opt{options.WithGeneratingTitle()}},
		{name: "compaction", image: new(true), opts: []options.Opt{options.WithCompacting()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/responses", r.URL.Path)
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{}}\n\n")
			}))
			defer server.Close()
			cfg := &latest.ModelConfig{Provider: "local", Model: "test", BaseURL: server.URL, ProviderOpts: map[string]any{"api_type": "openai_responses"}, OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: tc.image}, ImageGeneration: tc.settings}
			client, err := NewClient(t.Context(), cfg, environment.NewNoEnvProvider(), tc.opts...)
			require.NoError(t, err)
			stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "cat"}}, nil)
			require.NoError(t, err)
			defer stream.Close()
			_, err = stream.Recv()
			require.NoError(t, err)
			if tc.wantTool {
				require.Len(t, body["tools"], 1)
			} else {
				assert.Empty(t, body["tools"])
			}
		})
	}
}

func TestImagesRequireExplicitEnablementBeforeDispatch(t *testing.T) {
	for _, enabled := range []*bool{nil, new(false)} {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("disabled image request reached endpoint") }))
		cfg := &latest.ModelConfig{Provider: "local", Model: "flux", BaseURL: server.URL, ProviderOpts: map[string]any{"api_type": "openai_images"}, OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: enabled}}
		_, err := NewClient(t.Context(), cfg, environment.NewNoEnvProvider())
		require.ErrorContains(t, err, "output_capabilities.image: true")
		server.Close()
	}
}

func TestResponsesImageDecodingRequiresEnablement(t *testing.T) {
	events := []map[string]any{{"type": "response.output_item.done", "item": map[string]any{"type": "image_generation_call", "id": "image", "status": "completed", "result": base64.StdEncoding.EncodeToString(testImageBytes)}}}
	adapter := newResponseStreamAdapter(&fakeEventStream{events: decodeEvents(t, events)}, false)
	response, err := adapter.Recv()
	require.NoError(t, err)
	for _, choice := range response.Choices {
		assert.Empty(t, choice.Delta.Media)
	}
}
