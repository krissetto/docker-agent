package anthropic

import (
	"log/slog"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

// Anthropic allows at most 4 cache_control breakpoints per request; a
// request exceeding the limit is rejected outright. The budget is
// allocated in one place — this file — as follows:
//
//	tool list       0 or 1 (only when deferred tools are in play)
//	system blocks   up to maxSystemCacheBreakpoints, honoring upstream
//	                CacheControl marks (excess marks are dropped)
//	message tail    the remainder (messageCacheBreakpoints)
const maxSystemCacheBreakpoints = 2

// messageCacheBreakpoints returns how many message-level cache_control
// breakpoints a request may use: what remains of the budget after the
// tool list and system blocks take their share.
func messageCacheBreakpoints(requestTools []tools.Tool) int {
	if containsDeferredTool(requestTools) {
		return 1
	}
	return 2
}

// markSystemBlockCacheControl marks the block as a cache breakpoint if the
// system-block budget allows it, returning the updated count of marked
// blocks. Re-marking an already-marked block is a no-op so that duplicate
// upstream marks don't burn budget; over-budget marks are dropped (and
// logged) so a request can never exceed Anthropic's breakpoint limit.
func markSystemBlockCacheControl(block *anthropic.TextBlockParam, marked int) int {
	if block.CacheControl.Type != "" {
		return marked
	}
	if marked >= maxSystemCacheBreakpoints {
		slog.Debug("Dropping over-budget system cache breakpoint mark",
			"max", maxSystemCacheBreakpoints)
		return marked
	}
	block.CacheControl = anthropic.NewCacheControlEphemeralParam()
	return marked + 1
}

// extractSystemBlocks converts any system-role messages into Anthropic system text blocks
// to be set on the top-level MessageNewParams.System field.
func extractSystemBlocks(messages []chat.Message) []anthropic.TextBlockParam {
	var systemBlocks []anthropic.TextBlockParam
	marked := 0
	for i := range messages {
		msg := &messages[i]
		if msg.Role != chat.MessageRoleSystem {
			continue
		}

		if len(msg.MultiContent) > 0 {
			for _, part := range msg.MultiContent {
				if part.Type == chat.MessagePartTypeText {
					if txt := strings.TrimSpace(part.Text); txt != "" {
						systemBlocks = append(systemBlocks, anthropic.TextBlockParam{Text: txt})
					}
				}
			}
		} else if txt := strings.TrimSpace(msg.Content); txt != "" {
			// Trim system-message content: YAML literal blocks (instruction: |) always
			// append a trailing newline, and we don't want that in API payloads.
			systemBlocks = append(systemBlocks, anthropic.TextBlockParam{
				Text: txt,
			})
		}

		if msg.CacheControl && len(systemBlocks) > 0 {
			marked = markSystemBlockCacheControl(&systemBlocks[len(systemBlocks)-1], marked)
		}
	}

	return systemBlocks
}

// extractBetaSystemBlocks extracts system messages for Beta API format
func extractBetaSystemBlocks(messages []chat.Message) []anthropic.BetaTextBlockParam {
	regularBlocks := extractSystemBlocks(messages)

	betaBlocks := make([]anthropic.BetaTextBlockParam, len(regularBlocks))
	for i, block := range regularBlocks {
		betaBlocks[i] = anthropic.BetaTextBlockParam{Text: block.Text}

		// Copy over cache control from regular blocks (already set on first 2)
		if block.CacheControl.Type != "" {
			betaBlocks[i].CacheControl = anthropic.BetaCacheControlEphemeralParam{
				Type: block.CacheControl.Type,
				TTL:  anthropic.BetaCacheControlEphemeralTTL(block.CacheControl.TTL),
			}
		}
	}

	return betaBlocks
}

// applyMessageCacheControl adds ephemeral cache control to the last content block
// of the last `breakpoints` messages for prompt caching.
func applyMessageCacheControl(messages []anthropic.MessageParam, breakpoints int) {
	for i := len(messages) - 1; i >= 0 && i >= len(messages)-breakpoints; i-- {
		content := messageContent(messages[i])
		if len(content) == 0 {
			continue
		}
		// nil for block kinds without cache control (e.g. thinking blocks).
		if cc := content[len(content)-1].GetCacheControl(); cc != nil {
			*cc = anthropic.NewCacheControlEphemeralParam()
		}
	}
}

// applyBetaMessageCacheControl adds ephemeral cache control to the last content block
// of the last `breakpoints` messages for prompt caching.
func applyBetaMessageCacheControl(messages []anthropic.BetaMessageParam, breakpoints int) {
	for i := len(messages) - 1; i >= 0 && i >= len(messages)-breakpoints; i-- {
		if messages[i].OfSystem != nil {
			sys := messages[i].OfSystem.Content
			if len(sys) == 0 {
				continue
			}
			if cc := sys[len(sys)-1].GetCacheControl(); cc != nil {
				*cc = anthropic.NewBetaCacheControlEphemeralParam()
			}
			continue
		}
		content := betaMessageContent(messages[i])
		if len(content) == 0 {
			continue
		}
		// nil for block kinds without cache control (e.g. thinking blocks).
		if cc := content[len(content)-1].GetCacheControl(); cc != nil {
			*cc = anthropic.NewBetaCacheControlEphemeralParam()
		}
	}
}
