package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

func TestRecentChatHTTPBoundedPagedAndReadOnly(t *testing.T) {
	for name, open := range map[string]func(*testing.T) session.Store{
		"memory": func(*testing.T) session.Store { return session.NewInMemorySessionStore() },
		"sqlite": func(t *testing.T) session.Store {
			db, err := sql.Open("sqlite", ":memory:")
			require.NoError(t, err)
			db.SetMaxOpenConns(1)
			store, err := session.NewSQLiteSessionStoreFromDB(t.Context(), db)
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			return store
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			root := session.New(session.WithID("recent-http"), session.WithWorkingDir("/workspace"), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "root"}))
			root.AddMessage(session.UserMessage("first"))
			root.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "answer", ReasoningContent: "private reasoning"}))
			root.AddMessage(session.UserMessage(strings.Repeat("é", 4000)))
			require.NoError(t, store.AddSession(t.Context(), root))
			registry := &httpSessionRegistry{sessions: map[string]*httpSession{}}
			srv := NewWithManager(NewSessionManager(t.Context(), nil, store, 0, nil, WithSessionRuntime(registry)), "secret")
			response := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/recent-http/recent-chat?limit=1", "", "secret")
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.LessOrEqual(t, response.Body.Len(), api.RecentChatMaxResponseBytes)
			var first api.RecentChat
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &first))
			require.Len(t, first.Messages, 1)
			require.True(t, first.Messages[0].Truncated)
			assert.True(t, utf8.ValidString(first.Messages[0].Text))
			require.True(t, first.TruncatedBefore)
			require.NotEmpty(t, first.BeforeCursor)
			second := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/recent-http/recent-chat?limit=2&cursor="+url.QueryEscape(first.BeforeCursor), "", "secret")
			require.Equal(t, http.StatusOK, second.Code, second.Body.String())
			var older api.RecentChat
			require.NoError(t, json.Unmarshal(second.Body.Bytes(), &older))
			require.Len(t, older.Messages, 2)
			assert.Equal(t, "first", older.Messages[0].Text)
			assert.Equal(t, "answer", older.Messages[1].Text)
			assert.NotContains(t, second.Body.String(), "private reasoning")
			assert.Equal(t, root.ID, older.RootSessionID)
			assert.False(t, older.ExecutionAvailable)
			assert.Zero(t, registry.createCount, "read must not restore/attach")
			assert.Equal(t, http.StatusBadRequest, sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/recent-http/recent-chat?limit=99", "", "secret").Code)
			assert.Equal(t, http.StatusUnauthorized, sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/recent-http/recent-chat", "", "").Code)
		})
	}
}

func TestRecentChatRejectsChildRowsWithoutCanonicalMembership(t *testing.T) {
	base := session.NewInMemorySessionStore()
	root := session.New(session.WithID("recent-auth-root"), session.WithWorkingDir("/workspace"), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "root"}))
	require.NoError(t, base.AddSession(t.Context(), root))
	valid := session.New(session.WithID("recent-auth-child"), session.WithParentID(root.ID), session.WithWorkingDir("/workspace"), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "worker", runtime.SessionParentAgentAttribute: "root"}))
	require.NoError(t, base.(session.CoordinationStore).AdmitChild(t.Context(), session.ChildAdmission{Child: valid, Record: session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: subagent.Node{ID: "node-a", Parent: subagent.SessionRootID(root.ID), SessionID: valid.ID, Agent: "worker"}}}))
	orphan := session.New(session.WithID("recent-auth-orphan"), session.WithParentID(root.ID), session.WithWorkingDir("/workspace"), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "worker", runtime.SessionParentAgentAttribute: "root"}))
	require.NoError(t, base.AddSession(t.Context(), orphan))
	registry := &httpSessionRegistry{sessions: map[string]*httpSession{}}
	srv := NewWithManager(NewSessionManager(t.Context(), nil, base, 0, nil, WithSessionRuntime(registry)), "secret")
	allowed := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/recent-auth-child/recent-chat", "", "secret")
	require.Equal(t, http.StatusOK, allowed.Code, allowed.Body.String())
	denied := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/recent-auth-orphan/recent-chat", "", "secret")
	require.Equal(t, http.StatusNotFound, denied.Code, denied.Body.String())
	require.Zero(t, registry.createCount, "authorization must not attach or restore")
}

func TestRecentChatRejectsAncestrySourceAndWorkspaceMismatch(t *testing.T) {
	for name, mutate := range map[string]func(*session.Session, *session.Session){
		"source": func(_ *session.Session, child *session.Session) {
			child.SetAttribute(sessionSourceAttribute, "other-source")
		},
		"workspace": func(_ *session.Session, child *session.Session) { child.WorkingDir = "/other-workspace" },
	} {
		t.Run(name, func(t *testing.T) {
			base := session.NewInMemorySessionStore()
			root := session.New(session.WithID("recent-mismatch-root"), session.WithWorkingDir("/workspace"), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "root"}))
			require.NoError(t, base.AddSession(t.Context(), root))
			child := session.New(session.WithID("recent-mismatch-child"), session.WithParentID(root.ID), session.WithWorkingDir("/workspace"), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "worker", runtime.SessionParentAgentAttribute: "root"}))
			mutate(root, child)
			require.NoError(t, base.(session.CoordinationStore).AdmitChild(t.Context(), session.ChildAdmission{Child: child, Record: session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: subagent.Node{ID: "node-b", Parent: subagent.SessionRootID(root.ID), SessionID: child.ID, Agent: "worker"}}}))
			registry := &httpSessionRegistry{sessions: map[string]*httpSession{}}
			srv := NewWithManager(NewSessionManager(t.Context(), nil, base, 0, nil, WithSessionRuntime(registry)), "secret")
			response := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/recent-mismatch-child/recent-chat", "", "secret")
			require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
			require.Zero(t, registry.createCount)
		})
	}
}
