package runtime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/session"
)

func confirmedChildInfo() PreparedSessionViewInfo {
	child := session.New(session.WithID("child"), session.WithAgentName("worker"), session.WithAttributes(map[string]string{"docker-agent.actor.source": "team.yaml"}))
	child.ParentID = "root"
	return PreparedSessionViewInfo{SessionID: child.ID, RootSessionID: "root", Session: child, ActiveAgentName: "worker", Binding: SessionBinding{AgentName: "worker", ParentSessionID: "root"}, Attach: &SubagentAttachInfo{NodeID: "node", Agent: "worker", ParentSessionID: "root", ParentAgent: "director", Session: child.Clone()}}
}

func TestRemoteConfirmedViewRejectsForeignIdentity(t *testing.T) {
	tests := map[string]func(*PreparedSessionViewInfo){
		"selected":           func(i *PreparedSessionViewInfo) { i.SessionID = "other" },
		"snapshot":           func(i *PreparedSessionViewInfo) { i.Session.ID = "other" },
		"root":               func(i *PreparedSessionViewInfo) { i.RootSessionID = "" },
		"self root":          func(i *PreparedSessionViewInfo) { i.RootSessionID = "child" },
		"binding":            func(i *PreparedSessionViewInfo) { i.Binding.AgentName = "" },
		"parent":             func(i *PreparedSessionViewInfo) { i.Binding.ParentSessionID = "other" },
		"workspace":          func(i *PreparedSessionViewInfo) { i.WorkingDir = "/other" },
		"source":             func(i *PreparedSessionViewInfo) { i.Session.SetAttribute("docker-agent.actor.source", "foreign.yaml") },
		"missing attachment": func(i *PreparedSessionViewInfo) { i.Attach = nil },
		"attachment node":    func(i *PreparedSessionViewInfo) { i.Attach.NodeID = "" },
		"attachment session": func(i *PreparedSessionViewInfo) { i.Attach.Session.ID = "other" },
		"attachment parent":  func(i *PreparedSessionViewInfo) { i.Attach.ParentSessionID = "other" },
		"attachment agent":   func(i *PreparedSessionViewInfo) { i.Attach.Agent = "other" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			info := confirmedChildInfo()
			mutate(&info)
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "prepare-info", r.URL.Query().Get("view"))
				_ = json.NewEncoder(w).Encode(map[string]any{"version": api.SessionAPIVersion, "view": "prepare-info", "info": info})
			}))
			defer srv.Close()
			client, err := NewClient(srv.URL, WithHTTPClient(srv.Client()))
			require.NoError(t, err)
			transport, err := NewSessionTransport(client, WithSessionTransportSource("team.yaml"))
			require.NoError(t, err)
			_, err = transport.ConfirmedSessionViewInfo(t.Context(), "child")
			require.Error(t, err)
			_, err = transport.PrepareSessionView(t.Context(), "child")
			require.Error(t, err)
			assert.Equal(t, 2, calls, "no commit or hydration fallback")
		})
	}
}

func TestRemotePreparedViewRevalidatesCommit(t *testing.T) {
	for _, change := range []string{"session", "parent", "source", "agent", "abort"} {
		t.Run(change, func(t *testing.T) {
			info := confirmedChildInfo()
			patches := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Query().Get("view") == "prepare-info":
					_ = json.NewEncoder(w).Encode(map[string]any{"version": api.SessionAPIVersion, "view": "prepare-info", "info": info})
				case r.Method == http.MethodPatch:
					patches++
					snapshot := info.Session.Clone()
					switch change {
					case "session":
						snapshot.ID = "other"
					case "parent":
						info.Binding.ParentSessionID = "other"
						info.Attach.ParentSessionID = "other"
					case "source":
						snapshot.SetAttribute("docker-agent.actor.source", "other.yaml")
					}
					_ = json.NewEncoder(w).Encode(snapshot)
				case r.URL.Path == api.SessionAPIPath+"/child/status":
					_ = json.NewEncoder(w).Encode(map[string]any{"metadata": SessionMetadata{SessionID: "child", AgentName: "other"}, "status": SessionStatus{SessionID: "child"}})
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			client, err := NewClient(srv.URL, WithHTTPClient(srv.Client()))
			require.NoError(t, err)
			transport, err := NewSessionTransport(client, WithSessionTransportSource("team.yaml"))
			require.NoError(t, err)
			prepared, err := transport.PrepareSessionView(t.Context(), "child")
			require.NoError(t, err)
			if change == "abort" {
				prepared.Abort()
			}
			_, err = prepared.Commit(t.Context())
			require.Error(t, err)
			prepared.Abort()
			if change == "abort" {
				assert.Zero(t, patches)
			} else {
				assert.Equal(t, 1, patches)
			}
		})
	}
}

func TestRemoteConfirmedViewPreservesActiveAgentAndAncestry(t *testing.T) {
	info := confirmedChildInfo()
	info.ActiveAgentName = "active"
	info.Session.AgentName = "active"
	info.Attach.Agent = "active"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		_ = json.NewEncoder(w).Encode(map[string]any{"version": api.SessionAPIVersion, "view": "prepare-info", "info": info})
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL, WithHTTPClient(srv.Client()))
	require.NoError(t, err)
	transport, err := NewSessionTransport(client, WithSessionTransportSource("team.yaml"))
	require.NoError(t, err)
	prepared, err := transport.PrepareSessionView(t.Context(), "child")
	require.NoError(t, err)
	defer prepared.Abort()
	got := prepared.Info()
	assert.Equal(t, "worker", got.Binding.AgentName)
	assert.Equal(t, "active", got.Session.AgentName)
	assert.Equal(t, "active", got.Attach.Agent)
	assert.Equal(t, "root", got.Session.ParentID)
	assert.Equal(t, "root", got.Attach.Session.ParentID)
	got.Session.SetTitle("mutated")
	got.Attach.Session.SetTitle("mutated")
	assert.Empty(t, prepared.Info().Session.TitleSnapshot())
	assert.Empty(t, prepared.Info().Attach.Session.TitleSnapshot())
}

func TestRemoteConfirmedViewLegacyIdentity(t *testing.T) {
	for _, child := range []bool{false, true} {
		t.Run(map[bool]string{false: "root", true: "child"}[child], func(t *testing.T) {
			info := confirmedChildInfo()
			info.ActiveAgentName = ""
			expected := "worker"
			if child {
				info.Attach.Agent = "active-worker"
				expected = "active-worker"
			} else {
				info.RootSessionID = info.SessionID
				info.Binding.ParentSessionID = ""
				info.Session.ParentID = ""
				info.Attach = nil
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"version": api.SessionAPIVersion, "view": "prepare-info", "info": info})
			}))
			defer srv.Close()
			client, err := NewClient(srv.URL, WithHTTPClient(srv.Client()))
			require.NoError(t, err)
			transport, err := NewSessionTransport(client)
			require.NoError(t, err)
			got, err := transport.ConfirmedSessionViewInfo(t.Context(), "child")
			require.NoError(t, err)
			assert.Equal(t, expected, got.Session.AgentName)
			assert.Equal(t, "worker", got.Binding.AgentName)
		})
	}
}
