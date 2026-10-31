package chat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMessageDeltaServerFallbackWireFormat(t *testing.T) {
	t.Parallel()

	delta := MessageDelta{ServerFallback: &ServerFallback{From: "claude-a", To: "claude-b"}}
	data, err := json.Marshal(delta)
	require.NoError(t, err)
	assert.JSONEq(t, `{"server_fallback":{"from":"claude-a","to":"claude-b"}}`, string(data))

	var decoded MessageDelta
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, delta.ServerFallback, decoded.ServerFallback)

	var plain MessageDelta
	data, err = json.Marshal(plain)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "server_fallback", "a normal turn carries no fallback marker")
}
