package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestMessageAddedBoundaryWireMetadata(t *testing.T) {
	client, err := NewClient("http://localhost")
	require.NoError(t, err)
	for _, role := range []chat.MessageRole{chat.MessageRoleAssistant, chat.MessageRoleUser, chat.MessageRoleTool} {
		t.Run(string(role), func(t *testing.T) {
			message := session.NewAgentMessage("root", &chat.Message{Role: role, Content: "private body", ToolCalls: []tools.ToolCall{{ID: "committed-call"}}})
			encoded, err := json.Marshal(MessageAddedAt("s", message, "root", 7))
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), "private body")
			var wire map[string]any
			require.NoError(t, json.Unmarshal(encoded, &wire))
			assert.Equal(t, string(role), wire["message_role"])
			assert.Equal(t, []any{"committed-call"}, wire["tool_call_ids"])
			decoded, err := client.decodeSessionEvent(encoded)
			require.NoError(t, err)
			boundary, ok := decoded.(*MessageAddedEvent)
			require.True(t, ok)
			assert.Nil(t, boundary.Message)
			assert.Equal(t, 7, boundary.SessionPosition)
			roundtrip, err := json.Marshal(boundary)
			require.NoError(t, err)
			assert.JSONEq(t, string(encoded), string(roundtrip))
		})
	}
}

func TestMessageAddedBoundaryHTTPReplay(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, api.SessionAPIPath+"/s/events", r.URL.Path)
		assert.Equal(t, "4", r.URL.Query().Get("since"))
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":{\"session\":{\"id\":\"s\"},\"status\":{\"session_id\":\"s\",\"agent_name\":\"root\",\"state\":\"running\"},\"cursor\":6,\"transcript_position\":2}}\n\n")
		for i, role := range []chat.MessageRole{chat.MessageRoleAssistant, chat.MessageRoleUser} {
			message := session.NewAgentMessage("root", &chat.Message{Role: role, Content: "private body", ToolCalls: []tools.ToolCall{{ID: "published"}}})
			encoded, err := json.Marshal(MessageAddedAt("s", message, "root", i))
			if !assert.NoError(t, err) {
				return
			}
			assert.NotContains(t, string(encoded), "private body")
			fmt.Fprintf(w, "data: {\"version\":2,\"type\":\"event\",\"envelope\":{\"version\":2,\"session_id\":\"s\",\"turn_id\":\"turn\",\"sequence\":%d,\"transcript_position\":%d,\"event\":%s}}\n\n", i+5, i, encoded)
		}
		fmt.Fprint(w, "data: {\"version\":2,\"type\":\"ready\",\"cursor\":6}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := NewSessionTransport(client)
	require.NoError(t, err)
	handle, err := transport.SessionByID("s")
	require.NoError(t, err)
	since := uint64(4)
	observation, err := handle.Observe(t.Context(), ObserveOptions{Since: &since})
	require.NoError(t, err)
	defer observation.Cancel()
	require.Len(t, observation.Replay, 2)
	for i, envelope := range observation.Replay {
		boundary, ok := envelope.Event.(*MessageAddedEvent)
		require.True(t, ok)
		assert.Nil(t, boundary.Message)
		assert.Equal(t, []chat.MessageRole{chat.MessageRoleAssistant, chat.MessageRoleUser}[i], boundary.CommittedRole())
		assert.Equal(t, []string{"published"}, boundary.CommittedToolCallIDs())
		assert.Equal(t, i, boundary.SessionPosition)
		assert.Equal(t, uint64(i+5), envelope.Sequence)
	}
}
