package anthropic

import (
	"fmt"
	"log/slog"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

// betaStreamAdapter adapts the Anthropic Beta stream to our interface
type betaStreamAdapter struct {
	retryableStream[anthropic.BetaRawMessageStreamEventUnion]

	usage      anthropic.BetaUsage
	trackUsage bool
	toolCall   bool
	stopReason anthropic.BetaStopReason
	// toolIDByBlock maps a content block index to its tool_use block ID.
	// See streamAdapter.toolIDByBlock for the same rationale (parallel
	// tool calls require per-block routing of input_json deltas).
	toolIDByBlock map[int64]string
}

// newBetaStreamAdapter creates a new Beta stream adapter
func (c *Client) newBetaStreamAdapter(stream *ssestream.Stream[anthropic.BetaRawMessageStreamEventUnion], trackUsage bool) *betaStreamAdapter {
	return &betaStreamAdapter{
		retryableStream: retryableStream[anthropic.BetaRawMessageStreamEventUnion]{stream: stream},
		trackUsage:      trackUsage,
		toolIDByBlock:   map[int64]string{},
	}
}

// Recv gets the next completion chunk from the Beta stream
func (a *betaStreamAdapter) Recv() (chat.MessageStreamResponse, error) {
	ok, err := a.next()
	if !ok {
		return chat.MessageStreamResponse{}, wrapAnthropicError(err)
	}

	event := a.stream.Current()

	response := chat.MessageStreamResponse{
		ID:     event.Message.ID,
		Object: "chat.completion.chunk",
		Model:  event.Message.Model,
		Choices: []chat.MessageStreamChoice{
			{
				Index: 0,
				Delta: chat.MessageDelta{
					Role: string(chat.MessageRoleAssistant),
				},
			},
		},
	}

	// Handle different event types
	switch eventVariant := event.AsAny().(type) {
	case anthropic.BetaRawContentBlockStartEvent:
		switch block := eventVariant.ContentBlock.AsAny().(type) {
		case anthropic.BetaToolUseBlock:
			a.toolIDByBlock[eventVariant.Index] = block.ID
			a.toolCall = true
			toolCall := tools.ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: tools.FunctionCall{
					Name: block.Name,
				},
			}
			response.Choices[0].Delta.ToolCalls = []tools.ToolCall{toolCall}
		case anthropic.BetaThinkingBlock:
			if block.Thinking != "" {
				response.Choices[0].Delta.ReasoningContent = block.Thinking
				slog.Debug("Received thinking", "thinking", block.Thinking)
			}
			if block.Signature != "" {
				response.Choices[0].Delta.ThinkingSignature = block.Signature
			}
		}
	case anthropic.BetaRawContentBlockDeltaEvent:
		switch deltaVariant := eventVariant.Delta.AsAny().(type) {
		case anthropic.BetaTextDelta:
			response.Choices[0].Delta.Content = deltaVariant.Text
		case anthropic.BetaThinkingDelta:
			response.Choices[0].Delta.ReasoningContent = deltaVariant.Thinking
		case anthropic.BetaInputJSONDelta:
			inputBytes := deltaVariant.PartialJSON
			toolCall := tools.ToolCall{
				ID:   a.toolIDByBlock[eventVariant.Index],
				Type: "function",
				Function: tools.FunctionCall{
					Arguments: inputBytes,
				},
			}
			response.Choices[0].Delta.ToolCalls = []tools.ToolCall{toolCall}
		case anthropic.BetaSignatureDelta:
			// Signature delta is for thinking blocks - capture it so we can replay thinking in history
			response.Choices[0].Delta.ThinkingSignature = deltaVariant.Signature
		default:
			return response, fmt.Errorf("unknown delta type: %T", deltaVariant)
		}
	case anthropic.BetaRawMessageStartEvent:
		if a.trackUsage {
			a.usage = eventVariant.Message.Usage
		}
	case anthropic.BetaRawMessageDeltaEvent:
		a.stopReason = eventVariant.Delta.StopReason
		if a.trackUsage {
			a.accumulateUsage(eventVariant.Usage)
			response.Usage = betaUsageFromMessage(a.usage)
		}
	case anthropic.BetaRawMessageStopEvent:
		response.Choices[0].FinishReason = finishReason(anthropic.StopReason(a.stopReason), a.toolCall)
	}

	return response, nil
}

// Close closes the Beta stream
func (a *betaStreamAdapter) Close() {
	a.close()
}

// betaUsageFromMessage maps cumulative usage; reasoning is already included in output.
func betaUsageFromMessage(u anthropic.BetaUsage) *chat.Usage {
	return &chat.Usage{
		InputTokens:       u.InputTokens,
		OutputTokens:      u.OutputTokens,
		CachedInputTokens: u.CacheReadInputTokens,
		CacheWriteTokens:  u.CacheCreationInputTokens,
		ReasoningTokens:   u.OutputTokensDetails.ThinkingTokens,
	}
}

// Counts are cumulative: omitted/null fields preserve previous values, including zero.
func (a *betaStreamAdapter) accumulateUsage(u anthropic.BetaMessageDeltaUsage) {
	if u.JSON.InputTokens.Valid() {
		a.usage.InputTokens = u.InputTokens
	}
	if u.JSON.OutputTokens.Valid() {
		a.usage.OutputTokens = u.OutputTokens
	}
	if u.JSON.CacheReadInputTokens.Valid() {
		a.usage.CacheReadInputTokens = u.CacheReadInputTokens
	}
	if u.JSON.CacheCreationInputTokens.Valid() {
		a.usage.CacheCreationInputTokens = u.CacheCreationInputTokens
	}
	if u.JSON.OutputTokensDetails.Valid() && u.OutputTokensDetails.JSON.ThinkingTokens.Valid() {
		a.usage.OutputTokensDetails.ThinkingTokens = u.OutputTokensDetails.ThinkingTokens
	}
}
