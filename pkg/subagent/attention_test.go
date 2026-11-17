package subagent

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestAttentionIdentityCanonicalAndPortable(t *testing.T) {
	id := AttentionIdentity("interaction:b", "interaction:a", "interaction:b", "")
	require.Equal(t, id, AttentionIdentity("interaction:a", "interaction:b"))
	node := Node{AttentionID: id}
	encoded, err := json.Marshal(node)
	require.NoError(t, err)
	var restored Node
	require.NoError(t, json.Unmarshal(encoded, &restored))
	require.Equal(t, node, restored)
	require.Equal(t, []string{"interaction:a", "interaction:b"}, AttentionOccurrences(restored.AttentionID))
	require.Empty(t, AttentionIdentity())
	require.Empty(t, AttentionOccurrences("legacy-or-malformed"))
}
