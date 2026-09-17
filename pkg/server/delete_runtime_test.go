package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
)

func TestCanonicalDeleteWithRealRuntimeHandlesSharedStoreDeletion(t *testing.T) {
	store := session.NewInMemorySessionStore()
	srv, _ := newCanonicalLocalServer(t, store, agent.New("root", "prompt", agent.WithModel(sessionHTTPProvider{})))
	created := sessionRequest(t, srv, http.MethodPost, "/api/v2/sessions", `{"agent_name":"root"}`, "")
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var metadata sessionMetadataDTO
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &metadata))

	deleted := sessionRequest(t, srv, http.MethodDelete, "/api/v2/sessions/"+metadata.SessionID, "", "")
	require.Equal(t, http.StatusNoContent, deleted.Code, deleted.Body.String())
	_, err := store.GetSession(t.Context(), metadata.SessionID)
	assert.ErrorIs(t, err, session.ErrNotFound)
}
