package openai

import (
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"testing"

	"github.com/openai/openai-go/v3/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/chatgpt"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/tools"
)

func responseStateOutput() []json.RawMessage {
	return []json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"opaque"}`),
		json.RawMessage(`{"type":"message","id":"msg_1","role":"assistant","status":"completed","phase":"commentary","content":[{"type":"output_text","text":"Checking.","annotations":[]}]}`),
		json.RawMessage(`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"{}","status":"completed"}`),
	}
}

func responseStateMessage() chat.Message {
	return chat.Message{
		Role: chat.MessageRoleAssistant, Content: "Checking.",
		ToolCalls:      []tools.ToolCall{{ID: "call_1", Type: "function", Function: tools.FunctionCall{Name: "read", Arguments: "{}"}}},
		OpenAIResponse: &chat.OpenAIResponse{ID: "resp_1", Source: "openai/gpt-5.6", SessionID: "session_1", Output: responseStateOutput(), ReasoningContent: new(string)},
	}
}

func responseStateClient() *Client {
	return &Client{Config: base.Config{ModelConfig: latest.ModelConfig{Provider: "openai", Model: "gpt-5.6", ProviderOpts: map[string]any{"preserve_reasoning": true}}}}
}

func TestResponseStateReplay(t *testing.T) {
	t.Parallel()
	client := responseStateClient()
	message := responseStateMessage()
	items := client.convertMessagesToResponseInput(t.Context(), []chat.Message{message, {
		Role: chat.MessageRoleTool, ToolCallID: "call_1", Content: "ok",
	}})
	require.Len(t, items, 4)
	for i, want := range message.OpenAIResponse.Output {
		got, err := json.Marshal(items[i])
		require.NoError(t, err)
		assert.JSONEq(t, string(want), string(got))
	}
	assert.Equal(t, "call_1", items[3].OfFunctionCallOutput.CallID.Value)
}

func TestResponseStateDoesNotBypassEdits(t *testing.T) {
	t.Parallel()
	for name, edit := range map[string]func(*chat.Message, *Client){
		"text redaction":     func(m *chat.Message, _ *Client) { m.Content = "redacted" },
		"tool argument edit": func(m *chat.Message, _ *Client) { m.ToolCalls[0].Function.Arguments = `{"safe":true}` },
		"tool removal":       func(m *chat.Message, _ *Client) { m.ToolCalls = nil },
		"model switch":       func(_ *chat.Message, c *Client) { c.ModelConfig.Model = "gpt-6-astra" },
		"vendor switch":      func(_ *chat.Message, c *Client) { c.ModelConfig.Provider = "xai" },
		"custom endpoint":    func(_ *chat.Message, c *Client) { c.ModelConfig.BaseURL = "https://example.com/v1" },
		"non assistant":      func(m *chat.Message, _ *Client) { m.Role = chat.MessageRoleUser },
		"attachment added": func(m *chat.Message, _ *Client) {
			m.MultiContent = []chat.MessagePart{{Type: chat.MessagePartTypeText, Text: "attached"}}
		},
		"legacy function added":              func(m *chat.Message, _ *Client) { m.FunctionCall = &tools.FunctionCall{Name: "changed"} },
		"reasoning edit without raw summary": func(m *chat.Message, _ *Client) { m.ReasoningContent = "edited" },
		"invalid item":                       func(m *chat.Message, _ *Client) { m.OpenAIResponse.Output[0] = json.RawMessage(`{`) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m, c := responseStateMessage(), responseStateClient()
			edit(&m, c)
			assert.Empty(t, c.replayResponse(m))
		})
	}
}

func TestResponseStateStream(t *testing.T) {
	t.Parallel()
	for _, completedOutput := range []bool{false, true} {
		t.Run(strconv.FormatBool(completedOutput), func(t *testing.T) {
			t.Parallel()
			var events []map[string]any
			for i, item := range responseStateOutput() {
				events = append(events, map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
			}
			// A repeated done event must not duplicate opaque state.
			events = append(events, events[0])
			response := map[string]any{"id": "resp_1", "output": []any{}}
			if completedOutput {
				response["output"] = responseStateOutput()
			}
			events = append(events, map[string]any{"type": "response.completed", "response": response})
			client := responseStateClient()
			adapter := client.responseAdapter(httpclient.ContextWithSessionID(t.Context(), "session_1"), &fakeEventStream{events: decodeEvents(t, events)})
			var state *chat.OpenAIResponse
			for {
				r, err := adapter.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				require.NoError(t, err)
				for _, c := range r.Choices {
					if c.Delta.OpenAIResponse != nil {
						state = c.Delta.OpenAIResponse
					}
				}
			}
			require.NotNil(t, state)
			assert.Equal(t, "resp_1", state.ID)
			assert.Equal(t, "session_1", state.SessionID)
			require.Len(t, state.Output, 3)
			for i, want := range responseStateOutput() {
				assert.JSONEq(t, string(want), string(state.Output[i]))
			}
		})
	}
}

func TestResponseStateRejectsRedactedReasoning(t *testing.T) {
	t.Parallel()
	message := responseStateMessage()
	message.OpenAIResponse.Output[0] = json.RawMessage(`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"secret"}],"encrypted_content":"opaque"}`)
	message.ReasoningContent = "secret"
	message.OpenAIResponse.ReasoningContent = nil
	client := responseStateClient()
	require.Len(t, client.replayResponse(message), 3)
	message.ReasoningContent = "[REDACTED]"
	assert.Nil(t, client.replayResponse(message))
}

func TestResponseStateUsesResolvedModel(t *testing.T) {
	t.Parallel()
	client := responseStateClient()
	client.ModelConfig.DisplayModel = "friendly-alias"
	message := responseStateMessage()
	require.Len(t, client.replayResponse(message), 3, "display aliases do not scope opaque state")
	client.ModelConfig.Model = "gpt-6-astra"
	assert.Nil(t, client.replayResponse(message), "re-pinning an alias must not reuse another model's state")
}

func TestResponseStatePreservesMultipleAssistantPhases(t *testing.T) {
	t.Parallel()
	message := responseStateMessage()
	message.ToolCalls = nil
	message.Content = "Checking.Done."
	message.OpenAIResponse.Output = append(message.OpenAIResponse.Output[:2], json.RawMessage(`{"type":"message","id":"msg_2","role":"assistant","status":"completed","phase":"final_answer","content":[{"type":"output_text","text":"Done.","annotations":[]}]}`))
	items := responseStateClient().replayResponse(message)
	require.Len(t, items, 3)
	assert.Equal(t, responses.ResponseOutputMessagePhaseCommentary, items[1].OfOutputMessage.Phase)
	assert.Equal(t, responses.ResponseOutputMessagePhaseFinalAnswer, items[2].OfOutputMessage.Phase)
}

func TestResponseStateOrphanFunctionCall(t *testing.T) {
	t.Parallel()
	items := responseStateClient().convertMessagesToResponseInput(t.Context(), []chat.Message{responseStateMessage()})
	require.Len(t, items, 4)
	assert.Equal(t, "call_1", items[3].OfFunctionCallOutput.CallID.Value)
	assert.Contains(t, items[3].OfFunctionCallOutput.Output.OfString.Value, "not executed")
}

func TestResponseStateKeepsCompletedReasoningItem(t *testing.T) {
	t.Parallel()
	events := decodeEvents(t, []map[string]any{
		{"type": "response.output_item.done", "output_index": 0, "item": responseStateOutput()[0]},
		{"type": "response.completed", "response": map[string]any{"id": "resp_1", "output": []any{map[string]any{"type": "reasoning", "id": "rs_1", "summary": []any{}}}}},
	})
	adapter := responseStateClient().responseAdapter(t.Context(), &fakeEventStream{events: events})
	_, err := adapter.Recv()
	require.NoError(t, err)
	result, err := adapter.Recv()
	require.NoError(t, err)
	state := result.Choices[0].Delta.OpenAIResponse
	require.NotNil(t, state)
	require.Len(t, state.Output, 1)
	assert.JSONEq(t, string(responseStateOutput()[0]), string(state.Output[0]))
}

func TestResponseStateChatGPTDefaultEndpoint(t *testing.T) {
	t.Parallel()
	client := responseStateClient()
	client.ModelConfig.Provider = chatgpt.ProviderName
	client.ModelConfig.BaseURL = chatgpt.BaseURL
	message := responseStateMessage()
	message.OpenAIResponse.Source = "chatgpt/gpt-5.6"
	require.Len(t, client.replayResponse(message), 3)
	client.ModelConfig.BaseURL = "https://other.example/codex"
	assert.Nil(t, client.replayResponse(message))
}

func TestResponseStateEndpointEligibility(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		provider, endpoint string
		gateway            bool
		want               bool
	}{
		{"openai", "", false, true}, {"openai", "https://api.openai.com/v1/", false, true},
		{"openai", "http://api.openai.com/v1", false, false}, {"openai", "https://api.openai.com/v1?x=1", false, false},
		{"openai", "https://api.openai.com.evil.example/v1", false, false}, {"openai", "https://user@api.openai.com/v1", false, false},
		{"openai", "", true, false}, {"vercel", "https://api.openai.com/v1", false, false},
		{"chatgpt", "", false, true}, {"chatgpt", chatgpt.BaseURL, false, true},
	} {
		t.Run(tc.provider+tc.endpoint+strconv.FormatBool(tc.gateway), func(t *testing.T) {
			c := responseStateClient()
			c.ModelConfig.Provider = tc.provider
			c.ModelConfig.BaseURL = tc.endpoint
			if tc.gateway {
				options.WithGateway("https://gateway.example")(&c.ModelOptions)
			}
			assert.Equal(t, tc.want, c.preservesResponseState())
		})
	}
}

func TestResponseStateRequestGatesReasoningInclude(t *testing.T) {
	t.Parallel()
	c := responseStateClient()
	c.ModelConfig.Model = "gpt-4.1"
	var params responses.ResponseNewParams
	c.configureResponseState(&params)
	assert.Empty(t, params.Include)
	c.ModelConfig.Model = "gpt-5.6"
	c.configureResponseState(&params)
	assert.Equal(t, []responses.ResponseIncludable{"reasoning.encrypted_content"}, params.Include)
}

func TestResponseStateRejectsUnsupportedOutput(t *testing.T) {
	t.Parallel()
	m := responseStateMessage()
	m.OpenAIResponse.Output = append(m.OpenAIResponse.Output, json.RawMessage(`{"type":"tool_search_output","execution":"server"}`))
	assert.Nil(t, responseStateClient().replayResponse(m), "bundled native search is not part of preservation")
}

func TestResponseStateDoesNotUseCiphertextFromDifferentItem(t *testing.T) {
	t.Parallel()
	events := decodeEvents(t, []map[string]any{
		{"type": "response.output_item.done", "output_index": 0, "item": responseStateOutput()[0]},
		{"type": "response.completed", "response": map[string]any{"output": []any{map[string]any{"type": "reasoning", "id": "rs_other", "summary": []any{}}}}},
	})
	a := responseStateClient().responseAdapter(t.Context(), &fakeEventStream{events: events})
	_, err := a.Recv()
	require.NoError(t, err)
	response, err := a.Recv()
	require.NoError(t, err)
	assert.NotContains(t, string(response.Choices[0].Delta.OpenAIResponse.Output[0]), "opaque")
}

func TestResponseStateCapturesVisibleReasoningForEditGuard(t *testing.T) {
	t.Parallel()
	events := decodeEvents(t, []map[string]any{
		{"type": "response.reasoning_summary_text.delta", "delta": "visible"},
		{"type": "response.output_item.done", "output_index": 0, "item": responseStateOutput()[0]},
		{"type": "response.completed", "response": map[string]any{"id": "resp_1", "output": []any{}}},
	})
	a := responseStateClient().responseAdapter(t.Context(), &fakeEventStream{events: events})
	for range 2 {
		_, err := a.Recv()
		require.NoError(t, err)
	}
	r, err := a.Recv()
	require.NoError(t, err)
	state := r.Choices[0].Delta.OpenAIResponse
	require.NotNil(t, state.ReasoningContent)
	assert.Equal(t, "visible", *state.ReasoningContent)
	m := chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "visible", OpenAIResponse: state}
	require.Len(t, responseStateClient().replayResponse(m), 1)
	m.ReasoningContent = "redacted"
	assert.Nil(t, responseStateClient().replayResponse(m))
}

func TestResponseStateUsesCapturedEndpoint(t *testing.T) {
	t.Parallel()
	c := responseStateClient()
	c.BaseURL = "https://redirect.example/v1"
	assert.False(t, c.preservesResponseState(), "SDK environment redirects must not receive opaque state")
	c.BaseURL = "https://api.openai.com/v1"
	assert.True(t, c.preservesResponseState())
}
