package anthropic

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/stretchr/testify/assert"
)

// applyMessageCacheControl and applyBetaMessageCacheControl rely on the
// SDK's GetCacheControl union accessors to decide which block kinds can
// carry a breakpoint. Pin that contract for every block kind our
// converters produce, so an SDK bump that changes accessor coverage
// (e.g. adding cache control to thinking blocks) fails here instead of
// silently changing which requests get cached.
func TestSDKCacheControlAccessorCoverage(t *testing.T) {
	t.Parallel()

	t.Run("standard", func(t *testing.T) {
		t.Parallel()
		cacheable := map[string]anthropic.ContentBlockParamUnion{
			"text":        anthropic.NewTextBlock("x"),
			"tool_use":    {OfToolUse: &anthropic.ToolUseBlockParam{ID: "1", Name: "t"}},
			"tool_result": anthropic.NewToolResultBlock("1", "ok", false),
			"image":       anthropic.NewImageBlock(anthropic.Base64ImageSourceParam{Data: "d"}),
			"document":    {OfDocument: &anthropic.DocumentBlockParam{}},
		}
		for name, block := range cacheable {
			assert.NotNil(t, block.GetCacheControl(), "%s blocks must accept cache control", name)
		}

		uncacheable := map[string]anthropic.ContentBlockParamUnion{
			"thinking":          anthropic.NewThinkingBlock("sig", "why"),
			"redacted_thinking": anthropic.NewRedactedThinkingBlock("sig"),
		}
		for name, block := range uncacheable {
			assert.Nil(t, block.GetCacheControl(), "%s blocks must not accept cache control", name)
		}
	})

	t.Run("beta", func(t *testing.T) {
		t.Parallel()
		cacheable := map[string]anthropic.BetaContentBlockParamUnion{
			"text":        {OfText: &anthropic.BetaTextBlockParam{Text: "x"}},
			"tool_use":    {OfToolUse: &anthropic.BetaToolUseBlockParam{ID: "1", Name: "t"}},
			"tool_result": {OfToolResult: &anthropic.BetaToolResultBlockParam{ToolUseID: "1"}},
			"image":       {OfImage: &anthropic.BetaImageBlockParam{}},
			"document":    {OfDocument: &anthropic.BetaRequestDocumentBlockParam{}},
		}
		for name, block := range cacheable {
			assert.NotNil(t, block.GetCacheControl(), "beta %s blocks must accept cache control", name)
		}

		uncacheable := map[string]anthropic.BetaContentBlockParamUnion{
			"thinking":          anthropic.NewBetaThinkingBlock("sig", "why"),
			"redacted_thinking": anthropic.NewBetaRedactedThinkingBlock("sig"),
		}
		for name, block := range uncacheable {
			assert.Nil(t, block.GetCacheControl(), "beta %s blocks must not accept cache control", name)
		}
	})
}
