package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type attachmentWarmupProvider struct {
	sessionHTTPProvider
	requests chan []chat.Message
}

func (p *attachmentWarmupProvider) CreateChatCompletionStream(ctx context.Context, messages []chat.Message, ts []tools.Tool) (chat.MessageStream, error) {
	p.requests <- messages
	return p.sessionHTTPProvider.CreateChatCompletionStream(ctx, messages, ts)
}

func TestSessionHTTPAttachmentWarmsOnceWithoutConversation(t *testing.T) {
	provider := &attachmentWarmupProvider{requests: make(chan []chat.Message, 4)}
	store := session.NewInMemorySessionStore()
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(provider)))), runtime.WithSessionStore(store))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	handle, err := owner.Runtime().CreateSession(t.Context(), session.New(), runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	sm := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntime(owner.Runtime()))
	handler := NewWithManager(sm, "")
	srv := httptest.NewServer(handler.e)
	defer srv.Close()
	attach := func(query string) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v2/sessions/"+handle.ID()+"/events"+query, http.NoBody)
		require.NoError(t, err)
		response, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, response.StatusCode)
		require.NoError(t, response.Body.Close())
	}
	// Cursor reconnects are not new attachment warmups.
	attach("?since=0")
	select {
	case <-provider.requests:
		t.Fatal("cursor reconnect warmed cache")
	case <-time.After(25 * time.Millisecond):
	}
	attach("")
	select {
	case messages := <-provider.requests:
		require.NotEmpty(t, messages)
		assert.Equal(t, "hi", messages[len(messages)-1].Content)
	case <-time.After(5 * time.Second):
		t.Fatal("attachment did not warm cache")
	}
	attach("")
	select {
	case <-provider.requests:
		t.Fatal("repeat attachment warmed cache again")
	case <-time.After(25 * time.Millisecond):
	}
	snapshot, err := handle.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Empty(t, snapshot.MessagesSnapshot())
}
