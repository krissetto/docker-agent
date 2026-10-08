package compaction

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/chat"
)

func TestRetainedOpenAIReasoningTokenEstimate(t *testing.T) {
	t.Parallel()
	msg := &chat.Message{Role: chat.MessageRoleAssistant, Usage: &chat.Usage{OutputTokens: 100, ReasoningTokens: 80}}
	assert.Equal(t, int64(25), EstimateMessageTokens(msg))
	msg.OpenAIResponse = &chat.OpenAIResponse{Output: []json.RawMessage{json.RawMessage(`{"type":"reasoning","encrypted_content":"opaque"}`)}}
	assert.Equal(t, int64(105), EstimateMessageTokens(msg))
	msg.OpenAIResponse.Output = nil
	assert.Equal(t, int64(25), EstimateMessageTokens(msg), "empty metadata must not inflate estimates")
}
