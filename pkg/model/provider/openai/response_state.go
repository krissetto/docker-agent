package openai

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/openai/openai-go/v3/responses"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/chatgpt"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/tools"
)

// Opaque state must never reach a compatible endpoint or a gateway.
func (c *Client) firstPartyResponses() bool {
	if c.ModelOptions.Gateway() != "" {
		return false
	}
	endpoint := strings.TrimRight(cmp.Or(c.BaseURL, c.ModelConfig.BaseURL), "/")
	switch c.ModelConfig.Provider {
	case "openai":
		return endpoint == "" || endpoint == "https://api.openai.com/v1"
	case chatgpt.ProviderName:
		return endpoint == "" || endpoint == chatgpt.BaseURL
	default:
		return false
	}
}

func (c *Client) preservesResponseState() bool {
	enabled, _ := c.ModelConfig.ProviderOpts["preserve_reasoning"].(bool)
	return enabled && c.firstPartyResponses()
}

func (c *Client) responseSource() string { return c.ModelConfig.Provider + "/" + c.ModelConfig.Model }

func (c *Client) configureResponseState(params *responses.ResponseNewParams) {
	if c.preservesResponseState() && modelinfo.UsesReasoningEffort(c.ModelConfig.Model) {
		params.Include = append(params.Include, responses.ResponseIncludable("reasoning.encrypted_content"))
	}
}

func (c *Client) responseAdapter(ctx context.Context, stream responseEventStream) *ResponseStreamAdapter {
	adapter := newResponseStreamAdapter(stream, c.TrackUsageEnabled())
	if c.preservesResponseState() {
		adapter.responseState = &chat.OpenAIResponse{Source: c.responseSource(), SessionID: httpclient.SessionIDFromContext(ctx)}
	}
	return adapter
}

func replayableResponseItem(item responses.ResponseOutputItemUnion) bool {
	switch item.Type {
	case "reasoning":
		return item.EncryptedContent != ""
	case "message":
		return item.Role == "assistant" && slices.ContainsFunc(item.Content, func(part responses.ResponseOutputMessageContentUnion) bool { return part.Type == "output_text" })
	case "function_call":
		return item.CallID != "" && item.Name != ""
	default:
		return false
	}
}

func responseItemJSON(item responses.ResponseOutputItemUnion) json.RawMessage {
	return json.RawMessage(item.RawJSON())
}

// Replay preserves provider ordering and phase, but never bypasses edits or attachments.
func (c *Client) replayResponse(msg chat.Message) []responses.ResponseInputItemUnionParam {
	state := msg.OpenAIResponse
	if !c.preservesResponseState() || state == nil || state.Source != c.responseSource() || msg.Role != chat.MessageRoleAssistant || len(msg.MultiContent) != 0 || msg.FunctionCall != nil {
		return nil
	}
	if state.ReasoningContent != nil && *state.ReasoningContent != msg.ReasoningContent {
		return nil
	}
	var text, reasoning strings.Builder
	var calls []tools.ToolCall
	hasState := false
	input := make([]responses.ResponseInputItemUnionParam, 0, len(state.Output))
	for _, raw := range state.Output {
		var output responses.ResponseOutputItemUnion
		if err := json.Unmarshal(raw, &output); err != nil || !replayableResponseItem(output) {
			return nil
		}
		switch output.Type {
		case "reasoning":
			hasState = true
			for _, summary := range output.Summary {
				reasoning.WriteString(summary.Text)
			}
			for _, part := range output.Content {
				reasoning.WriteString(part.Text)
			}
		case "message":
			hasState = hasState || output.Phase != ""
			for _, part := range output.Content {
				if part.Type != "output_text" {
					return nil
				}
				text.WriteString(part.Text)
			}
		case "function_call":
			calls = append(calls, tools.ToolCall{ID: output.CallID, Type: "function", Function: tools.FunctionCall{Name: output.Name, Arguments: output.Arguments.OfString}})
		}
		var item responses.ResponseInputItemUnionParam
		if output.Type == "message" {
			item.OfOutputMessage = new(responses.ResponseOutputMessageParam)
			if err := json.Unmarshal(raw, item.OfOutputMessage); err != nil {
				return nil
			}
		} else if err := json.Unmarshal(raw, &item); err != nil {
			return nil
		}
		input = append(input, item)
	}
	if state.ReasoningContent == nil && reasoning.String() != msg.ReasoningContent {
		return nil
	}
	if !hasState || text.String() != msg.Content || !slices.Equal(calls, msg.ToolCalls) {
		return nil
	}
	return input
}
