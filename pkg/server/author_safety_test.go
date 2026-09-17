package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
)

func authorSafetyStore(t *testing.T) session.Store {
	t.Helper()
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func authorSafetyFactory(store session.Store, agentSafety, runtimeSafety latest.SafetyMode) SessionRuntimeFactory {
	return func(ctx context.Context, _ config.Source, workingDir string) (runtime.SessionRuntimeSupervisor, error) {
		root := agent.New("root", "prompt", agent.WithModel(sessionHTTPProvider{}), agent.WithSafety(agentSafety))
		other := agent.New("other", "prompt", agent.WithModel(sessionHTTPProvider{}))
		rt, err := runtime.NewLocalRuntime(ctx, team.New(team.WithAgents(root, other), team.WithRuntimeSafety(runtimeSafety)),
			runtime.WithSessionStore(store), runtime.WithWorkingDir(workingDir))
		if err != nil {
			return nil, err
		}
		return runtime.NewSessionRuntimeSupervisor(rt), nil
	}
}

func newAuthorSafetyServer(t *testing.T, store session.Store, factory SessionRuntimeFactory) *Server {
	t.Helper()
	sm := NewSessionManager(t.Context(), config.Sources{"agent": &memorySource{data: "agents: {}"}}, store, 0,
		&config.RuntimeConfig{}, WithSessionRuntimeFactory(factory))
	t.Cleanup(func() { require.NoError(t, sm.Shutdown(context.WithoutCancel(t.Context()))) })
	return NewWithManager(sm, "")
}

func authorSafetySnapshot(t *testing.T, srv *Server, id string) *session.Session {
	t.Helper()
	rec := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/"+id+"/snapshot", "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var snapshot sessionSnapshotDTO
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snapshot))
	require.NotNil(t, snapshot.Session)
	return snapshot.Session
}

func TestCanonicalAuthorSafetyDefaultPrecedenceAndPersistence(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want session.SafetyPolicy
	}{
		{name: "selected agent wins", body: `{"agent_name":"root"}`, want: session.SafetyPolicyBalanced},
		{name: "runtime fallback", body: `{"agent_name":"other"}`, want: session.SafetyPolicyStrict},
		{name: "explicit policy wins", body: `{"agent_name":"root","safety_policy":"autonomous"}`, want: session.SafetyPolicyAutonomous},
		{name: "legacy approval wins", body: `{"agent_name":"root","tools_approved":true}`, want: session.SafetyPolicyAutonomous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := authorSafetyStore(t)
			srv := newAuthorSafetyServer(t, store, authorSafetyFactory(store, latest.SafetyModeBalanced, latest.SafetyModeStrict))
			metadata, code := createSessionVia(t, srv, tc.body)
			require.Equal(t, http.StatusCreated, code)
			assert.Equal(t, tc.want, authorSafetySnapshot(t, srv, metadata.SessionID).GetSafetyPolicy())
			stored, err := store.GetSession(t.Context(), metadata.SessionID)
			require.NoError(t, err)
			assert.Equal(t, tc.want, stored.GetSafetyPolicy())
			assert.Equal(t, tc.want == session.SafetyPolicyAutonomous, stored.IsToolsApproved())
		})
	}
}

func TestCanonicalAuthorSafetyResumeDoesNotRedefault(t *testing.T) {
	for _, policy := range []session.SafetyPolicy{"", session.SafetyPolicyBalanced, session.SafetyPolicyAutonomous} {
		t.Run("stored_"+string(policy), func(t *testing.T) {
			store := authorSafetyStore(t)
			first := newAuthorSafetyServer(t, store, authorSafetyFactory(store, "", ""))
			body, err := json.Marshal(sessionCreateRequest{AgentName: "root", SafetyPolicy: policy})
			require.NoError(t, err)
			metadata, code := createSessionVia(t, first, string(body))
			require.Equal(t, http.StatusCreated, code)
			require.NoError(t, first.sm.Shutdown(t.Context()))

			resumed := newAuthorSafetyServer(t, store, authorSafetyFactory(store, latest.SafetyModeStrict, latest.SafetyModeStrict))
			assert.Equal(t, policy, authorSafetySnapshot(t, resumed, metadata.SessionID).GetSafetyPolicy(),
				"restoring must preserve even an empty mode consumed by a no-default creation")
			stored, err := store.GetSession(t.Context(), metadata.SessionID)
			require.NoError(t, err)
			assert.Equal(t, policy, stored.GetSafetyPolicy())
		})
	}
}

func TestCanonicalAuthorSafetyClientChoiceBeforeFirstTurnWins(t *testing.T) {
	store := authorSafetyStore(t)
	srv := newAuthorSafetyServer(t, store, authorSafetyFactory(store, latest.SafetyModeBalanced, latest.SafetyModeStrict))
	metadata, code := createSessionVia(t, srv, `{"agent_name":"root"}`)
	require.Equal(t, http.StatusCreated, code)
	require.Equal(t, session.SafetyPolicyBalanced, authorSafetySnapshot(t, srv, metadata.SessionID).GetSafetyPolicy())

	updated := sessionRequest(t, srv, http.MethodPatch, "/api/v2/sessions/"+metadata.SessionID+"/safety-policy", `{"safety_policy":"strict"}`, "")
	require.Equal(t, http.StatusOK, updated.Code, updated.Body.String())
	assert.Equal(t, session.SafetyPolicyStrict, authorSafetySnapshot(t, srv, metadata.SessionID).GetSafetyPolicy())
	submitted := sessionRequest(t, srv, http.MethodPost, "/api/v2/sessions/"+metadata.SessionID+"/messages", `{"content":"hello"}`, "")
	require.Equal(t, http.StatusAccepted, submitted.Code, submitted.Body.String())
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		stored, err := store.GetSession(t.Context(), metadata.SessionID)
		if assert.NoError(c, err) {
			assert.GreaterOrEqual(c, len(stored.GetAllMessages()), 2)
			assert.Equal(c, session.SafetyPolicyStrict, stored.GetSafetyPolicy())
		}
	}, 5*time.Second, time.Millisecond)
	require.NoError(t, srv.sm.Shutdown(t.Context()))
	stored, err := store.GetSession(t.Context(), metadata.SessionID)
	require.NoError(t, err)
	assert.Equal(t, session.SafetyPolicyStrict, stored.GetSafetyPolicy())
}

func TestCanonicalAuthorSafetyFailedBuildRetryUsesCurrentDefault(t *testing.T) {
	store := authorSafetyStore(t)
	buildErr := errors.New("synthetic runtime construction failure")
	fail := true
	build := authorSafetyFactory(store, latest.SafetyModeBalanced, latest.SafetyModeStrict)
	srv := newAuthorSafetyServer(t, store, func(ctx context.Context, source config.Source, workingDir string) (runtime.SessionRuntimeSupervisor, error) {
		if fail {
			return nil, buildErr
		}
		return build(ctx, source, workingDir)
	})

	failed := sessionRequest(t, srv, http.MethodPost, "/api/v2/sessions", `{"agent_name":"root"}`, "")
	require.Equal(t, http.StatusInternalServerError, failed.Code, failed.Body.String())
	stored, err := store.GetSessions(t.Context())
	require.NoError(t, err)
	require.Empty(t, stored, "failed canonical creation must not leave a defaulted session behind")

	fail = false
	build = authorSafetyFactory(store, latest.SafetyModeStrict, latest.SafetyModeBalanced)
	metadata, code := createSessionVia(t, srv, `{"agent_name":"root"}`)
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, session.SafetyPolicyStrict, authorSafetySnapshot(t, srv, metadata.SessionID).GetSafetyPolicy())
	stored, err = store.GetSessions(t.Context())
	require.NoError(t, err)
	require.Len(t, stored, 1)
	assert.Equal(t, session.SafetyPolicyStrict, stored[0].GetSafetyPolicy(), "the retry uses the current author's default")
}
