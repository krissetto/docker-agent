package root

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui"
)

func TestRemoteBackendResumeAndBorrowedRestore(t *testing.T) {
	for _, ref := range []string{"saved", "-1"} {
		t.Run(ref, func(t *testing.T) {
			sess := session.New(session.WithID("saved"), session.WithAgentName("root"), session.WithWorkingDir("/server/workspace"), session.WithSafetyPolicy(session.SafetyPolicyAutonomous), session.WithAttributes(map[string]string{sessionActorSourceAttribute: "team.yaml"}))
			sess.Title = "Durable title"
			sess.AddMessage(session.UserMessage("previous question"))
			var requests []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "Bearer private-token", r.Header.Get("Authorization"))
				requests = append(requests, r.Method+" "+r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case api.SessionAPIPath:
					require.Equal(t, http.MethodGet, r.Method)
					require.Equal(t, "summary", r.URL.Query().Get("view"))
					rows := []runtime.SessionSummaryEntry{
						{SessionID: "wrong-source", Source: "elsewhere.yaml", WorkingDir: sess.WorkingDir, CreatedAt: time.Now()},
						{SessionID: "wrong-workspace", Source: "team.yaml", WorkingDir: "/elsewhere", CreatedAt: time.Now()},
						{SessionID: sess.ID, Source: "team.yaml", WorkingDir: sess.WorkingDir, CreatedAt: time.Unix(1, 0)},
					}
					require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"version": 2, "view": "summary", "sessions": rows}))
				case api.SessionAPIPath + "/saved/status":
					fmt.Fprint(w, `{"metadata":{"session_id":"saved","agent_name":"root","model":"test/model"},"status":{"session_id":"saved","agent_name":"root","state":"settled"}}`)
				case api.SessionAPIPath + "/saved/snapshot":
					require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"session": sess, "status": map[string]any{"session_id": sess.ID, "state": "settled"}}))
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			tokenFile := filepath.Join(t.TempDir(), "token")
			require.NoError(t, os.WriteFile(tokenFile, []byte("private-token\n"), 0600))
			flags := &runExecFlags{remoteAddress: server.URL, remoteAuthTokenFile: tokenFile, remoteWorkingDir: sess.WorkingDir, sessionID: ref, defaultSafety: session.SafetyPolicyStrict}
			b := &remoteBackend{flags: flags, agentFileName: "team.yaml"}
			req := b.CreateSessionRequest("/arbitrary/client/checkout")
			require.Equal(t, sess.WorkingDir, req.WorkingDir)
			services, sessions, got, cleanup, err := b.CreateSession(t.Context(), nil, req)
			require.NoError(t, err)
			require.Equal(t, "Durable title", got.Title)
			require.Equal(t, session.SafetyPolicyAutonomous, got.GetSafetyPolicy())
			require.Len(t, got.GetAllMessages(), 1)
			assert.Equal(t, chat.MessageRoleUser, got.GetAllMessages()[0].Message.Role)
			assert.Equal(t, "root", got.AgentName)
			assert.Contains(t, requests, "GET "+api.SessionAPIPath+"/saved/status")
			n := len(requests)
			_, err = b.Restorer(services, sessions)(t.Context(), "", "")
			require.Error(t, err)
			require.Len(t, requests, n)
			restored, err := b.Restorer(services, sessions)(t.Context(), "saved", "/client/does-not-exist")
			require.NoError(t, err)
			assert.Equal(t, tui.RuntimeBorrowed, restored.Ownership)
			require.NotNil(t, restored.App.SessionHandle())
			n = len(requests)
			cleanup()
			require.NoError(t, b.Close())
			require.Len(t, requests, n, "borrowed cleanup must not contact/cancel/shutdown the server")
			for _, request := range requests {
				assert.True(t, strings.HasPrefix(request, "GET "), request)
			}
		})
	}
}

func TestRemoteBackendMissingExplicitIDCreatesAndHydrates(t *testing.T) {
	var created api.SessionCreateRequest
	sess := session.New(session.WithID("chosen"), session.WithAgentName("root"), session.WithWorkingDir("/server"), session.WithAttributes(map[string]string{sessionActorSourceAttribute: "team.yaml"}))
	sess.Title = "canonical"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == api.ServerInfoPath:
			fmt.Fprint(w, `{"version":1,"session_api_version":2,"instance_id":"test-server","capabilities":["session_explicit_ids"]}`)
		case strings.HasSuffix(r.URL.Path, "/status"):
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"not_found"}`)
		case r.Method == http.MethodPost && r.URL.Path == api.SessionAPIPath:
			require.NoError(t, json.NewDecoder(r.Body).Decode(&created))
			fmt.Fprint(w, `{"session_id":"chosen","agent_name":"root"}`)
		case strings.HasSuffix(r.URL.Path, "/snapshot"):
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"session": sess, "status": map[string]any{"session_id": sess.ID}}))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	b := &remoteBackend{flags: &runExecFlags{remoteAddress: server.URL, sessionID: "chosen", agentName: "root", remoteWorkingDir: "/server", autoApprove: true}, agentFileName: "team.yaml"}
	_, _, got, _, err := b.CreateSession(t.Context(), nil, b.CreateSessionRequest("/client"))
	require.NoError(t, err)
	assert.Equal(t, "chosen", created.SessionID)
	assert.Equal(t, "team.yaml", created.Source)
	assert.Equal(t, "/server", created.WorkingDir)
	assert.True(t, created.ToolsApproved)
	assert.Equal(t, "canonical", got.Title)
}

func TestRemoteAuthTokenFileMustBePrivate(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(file, []byte("secret"), 0644))
	_, err := newRemoteClient("http://localhost", file)
	require.ErrorContains(t, err, "private")
	assert.NotContains(t, err.Error(), "secret")
	require.NoError(t, os.Chmod(file, 0600))
	_, err = newRemoteClient("http://localhost", file)
	require.NoError(t, err)
}

func TestRemoteRequestDoesNotSendLocalWorkingDir(t *testing.T) {
	b := &remoteBackend{flags: &runExecFlags{}}
	assert.Empty(t, b.CreateSessionRequest("/local/check/out").WorkingDir)
	_, ok := b.ResumeWorkingDir(t.Context())
	assert.False(t, ok)
}

func TestRemoteStartupModelOrderedAndPolicySafe(t *testing.T) {
	for _, tc := range []struct {
		name      string
		overrides []string
		policy    bool
		want, err string
	}{
		{name: "ordered", overrides: []string{"root=test/first", "root=test/last"}, want: "test/last"},
		{name: "named", overrides: []string{"root=named"}, want: "named"},
		{name: "team-wide", overrides: []string{"test/all"}, err: "selected root"},
		{name: "other-agent", overrides: []string{"worker=test/other"}, err: "selected root"},
		{name: "policy", overrides: []string{"root=test/custom"}, policy: true, err: "policy inheritance"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := ""
			if tc.policy {
				policy = `,"parallel_tool_calls":false`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/agents/team.yaml", r.URL.Path)
				fmt.Fprintf(w, `{"agents":[{"name":"root","model":"base"},{"name":"worker","model":"base"}],"models":{"base":{"provider":"test","model":"base"%s},"named":{"provider":"test","model":"named"}}}`, policy)
			}))
			defer server.Close()
			client, err := runtime.NewClient(server.URL)
			require.NoError(t, err)
			b := &remoteBackend{flags: &runExecFlags{modelOverrides: tc.overrides}, agentFileName: "team.yaml"}
			got, err := b.remoteStartupModel(t.Context(), client, "root")
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestRemoteResumeExplicitSafetyUsesCanonicalEdit(t *testing.T) {
	sess := session.New(session.WithID("saved"), session.WithSafetyPolicy(session.SafetyPolicyAutonomous), session.WithAttributes(map[string]string{sessionActorSourceAttribute: "team.yaml"}))
	var order []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/status"):
			fmt.Fprint(w, `{"metadata":{"session_id":"saved","agent_name":"root"},"status":{"session_id":"saved","agent_name":"root","state":"settled"}}`)
		case strings.HasSuffix(r.URL.Path, "/snapshot"):
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"session": sess, "status": map[string]any{"session_id": sess.ID}}))
		case r.Method == http.MethodPatch && r.URL.Path == api.SessionAPIPath+"/saved":
			var edit runtime.SessionEdit
			require.NoError(t, json.NewDecoder(r.Body).Decode(&edit))
			require.Equal(t, runtime.SessionEditPolicy, edit.Kind)
			require.NotNil(t, edit.SafetyPolicy)
			session.WithSafetyPolicy(*edit.SafetyPolicy)(sess)
			require.NoError(t, json.NewEncoder(w).Encode(sess))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	b := &remoteBackend{flags: &runExecFlags{remoteAddress: server.URL, sessionID: "saved", safety: "strict", safetyChanged: true}, agentFileName: "team.yaml"}
	_, _, got, _, err := b.CreateSession(t.Context(), nil, b.CreateSessionRequest(""))
	require.NoError(t, err)
	assert.Equal(t, session.SafetyPolicyStrict, got.GetSafetyPolicy())
	assert.False(t, got.ToolsApproved)
	require.Equal(t, []string{"GET " + api.SessionAPIPath + "/saved/status", "GET " + api.SessionAPIPath + "/saved/snapshot", "PATCH " + api.SessionAPIPath + "/saved", "GET " + api.SessionAPIPath + "/saved/snapshot"}, order)
}

func TestRemoteMissingExplicitIDRejectsOldPeerWithoutCreation(t *testing.T) {
	for _, identityAvailable := range []bool{false, true} {
		t.Run(fmt.Sprint(identityAvailable), func(t *testing.T) {
			creates := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					creates++
					http.Error(w, "must not create", http.StatusBadRequest)
					return
				}
				if r.URL.Path == api.ServerInfoPath && identityAvailable {
					fmt.Fprint(w, `{"version":1,"session_api_version":2,"instance_id":"old-server","capabilities":[]}`)
					return
				}
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"error":"not_found"}`)
			}))
			defer server.Close()
			b := &remoteBackend{flags: &runExecFlags{remoteAddress: server.URL, sessionID: "chosen", agentName: "root"}, agentFileName: "team.yaml"}
			_, _, _, _, err := b.CreateSession(t.Context(), nil, b.CreateSessionRequest(""))
			require.Error(t, err)
			assert.Zero(t, creates, "unsupported explicit identities must never fall back to creating another session")
		})
	}
}

func TestRemoteSpawnerRequiresBorrowedResources(t *testing.T) {
	for _, b := range []*remoteBackend{nil, {}, {flags: &runExecFlags{}}} {
		assert.Nil(t, b.Spawner(nil, nil))
		_, err := b.Restorer(nil, nil)(t.Context(), "saved", "")
		require.ErrorIs(t, err, runtime.ErrUnsupported)
	}
}
