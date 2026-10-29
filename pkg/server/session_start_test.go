package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
)

func TestSessionStartConcurrentRetryAndConflict(t *testing.T) {
	store := session.NewInMemorySessionStore()
	srv, owner := newCanonicalLocalServer(t, store, agent.New("root", "prompt", agent.WithModel(sessionHTTPProvider{})))
	body := `{"session_id":"idempotent-start","agent_name":"root","input":{"request_id":"first","content":"hello"}}`
	var wg sync.WaitGroup
	results := make(chan sessionSubmissionDTO, 2)
	for range 2 {
		wg.Go(func() {
			response := sessionRequest(t, srv, http.MethodPost, "/api/v2/sessions/start", body, "")
			require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
			var submitted sessionSubmissionDTO
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &submitted))
			results <- submitted
		})
	}
	wg.Wait()
	close(results)
	var first string
	for submitted := range results {
		if first == "" {
			first = submitted.TurnID
		}
		require.Equal(t, first, submitted.TurnID)
	}
	handle, err := owner.Runtime().SessionByID("idempotent-start")
	require.NoError(t, err)
	require.NoError(t, handle.AwaitTurn(t.Context(), first))
	snapshot, err := handle.Snapshot(t.Context())
	require.NoError(t, err)
	users := 0
	for _, item := range snapshot.MessagesSnapshot() {
		if item.Message != nil && item.Message.TurnID == first {
			users++
		}
	}
	require.Equal(t, 1, users)
	conflict := sessionRequest(t, srv, http.MethodPost, "/api/v2/sessions/start", `{"session_id":"idempotent-start","agent_name":"root","input":{"request_id":"first","content":"different"}}`, "")
	require.Equal(t, http.StatusConflict, conflict.Code, conflict.Body.String())
}

func TestSessionStartRequestsTitleOnlyWhenTitleIsBlank(t *testing.T) {
	for _, tc := range []struct {
		name  string
		title string
		want  string
	}{
		{name: "blank title generates", want: "HTTP title"},
		{name: "explicit title wins", title: "Keep my title", want: "Keep my title"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &presentationTitleProvider{entered: make(chan struct{}), release: make(chan struct{})}
			store := session.NewInMemorySessionStore()
			srv, owner := newCanonicalLocalServer(t, store,
				agent.New("root", "prompt", agent.WithModel(sessionHTTPProvider{}), agent.WithTitleModel(provider)))
			sessionID := "start-title-blank"
			if tc.title != "" {
				sessionID = "start-title-explicit"
			}
			body, err := json.Marshal(map[string]any{
				"session_id": sessionID, "agent_name": "root", "title": tc.title,
				"input": map[string]any{"request_id": "first", "content": "first prompt", "generate_title": true},
			})
			require.NoError(t, err)
			response := sessionRequest(t, srv, http.MethodPost, "/api/v2/sessions/start", string(body), "")
			require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
			var submitted sessionSubmissionDTO
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &submitted))
			handle, err := owner.Runtime().SessionByID(sessionID)
			require.NoError(t, err)
			if tc.title == "" {
				select {
				case <-provider.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("blank first turn did not start title generation")
				}
				close(provider.release)
			} else {
				require.NoError(t, handle.AwaitTurn(t.Context(), submitted.TurnID))
				require.Zero(t, provider.calls.Load(), "the server must preserve an explicit title")
			}
			require.Eventually(t, func() bool {
				stored, getErr := store.GetSession(t.Context(), sessionID)
				return getErr == nil && stored.TitleSnapshot() == tc.want
			}, 5*time.Second, 10*time.Millisecond)
		})
	}
}

func TestSessionStartRetriesAfterCreationWithoutSubmission(t *testing.T) {
	registry := &httpSessionRegistry{sessions: map[string]*httpSession{}}
	srv, store := newSessionHTTPServer(t, registry)
	body := `{"session_id":"partial-start","agent_name":"root","input":{"request_id":"first","content":"hello"}}`
	// Fail the initial submit after creation, leaving the immutable proof durable.
	registry.submitErr = errors.New("injected submit failure")
	first := sessionRequest(t, srv, http.MethodPost, "/api/v2/sessions/start", body, "")
	require.Equal(t, http.StatusInternalServerError, first.Code, first.Body.String())
	registry.sessions["partial-start"].submitErr = nil
	require.Equal(t, 1, registry.createCount)
	persisted, err := store.GetSession(t.Context(), "partial-start")
	require.NoError(t, err)
	require.NotEmpty(t, persisted.AttributesSnapshot()[sessionStartProofAttribute])
	// Simulate a lost response: retry must not recreate the persisted identity.
	retry := sessionRequest(t, srv, http.MethodPost, "/api/v2/sessions/start", body, "")
	require.Equal(t, http.StatusAccepted, retry.Code, retry.Body.String())
	require.Equal(t, 1, registry.createCount)
	unrelated := session.New(session.WithID("unrelated"), session.WithAgentName("root"))
	require.NoError(t, store.AddSession(t.Context(), unrelated))
	conflict := sessionRequest(t, srv, http.MethodPost, "/api/v2/sessions/start", `{"session_id":"unrelated","agent_name":"root","input":{"request_id":"first","content":"hello"}}`, "")
	require.Equal(t, http.StatusConflict, conflict.Code)
}
