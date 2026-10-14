package root

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui"
	"github.com/docker/docker-agent/pkg/tui/service/tuistate"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestManagedReattachInitialTabSelection(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		flags                      *runExecFlags
		args                       []string
		interactive, restore, want bool
	}{
		{name: "passive reattach", flags: &runExecFlags{managedAPIAttach: true}, interactive: true, restore: true, want: true},
		{name: "lean reattach", flags: &runExecFlags{managedAPIAttach: true, lean: true}, interactive: true, restore: true, want: true},
		{name: "exec", flags: &runExecFlags{managedAPIAttach: true}, restore: true},
		{name: "restore disabled", flags: &runExecFlags{managedAPIAttach: true}, interactive: true},
		{name: "generic remote", flags: &runExecFlags{}, interactive: true, restore: true},
		{name: "explicit session", flags: &runExecFlags{managedAPIAttach: true, sessionID: "chosen"}, interactive: true, restore: true},
		{name: "explicit agent", flags: &runExecFlags{managedAPIAttach: true, agentName: "worker"}, interactive: true, restore: true},
		{name: "explicit working directory", flags: &runExecFlags{managedAPIAttach: true, workingDirChanged: true}, interactive: true, restore: true},
		{name: "prompt", flags: &runExecFlags{managedAPIAttach: true}, args: []string{"team.yaml", "prompt"}, interactive: true, restore: true},
		{name: "stdin", flags: &runExecFlags{managedAPIAttach: true}, args: []string{"team.yaml", "-"}, interactive: true, restore: true},
		{name: "attachment", flags: &runExecFlags{managedAPIAttach: true, attachmentPath: "image.png"}, interactive: true, restore: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.flags.shouldRestoreInitialTab(tc.interactive, tc.args, &userconfig.Settings{RestoreTabs: &tc.restore}))
		})
	}
}

func TestManagedReattachRestoresBeforeCreatingStartupSession(t *testing.T) {
	for _, mode := range []string{"active", "child-active", "stale-active", "wrong-source", "first-run", "all-stale", "forbidden"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			paths.SetRoot(dir)
			t.Cleanup(func() { paths.SetRoot("") })
			require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
				cfg.Settings = &userconfig.Settings{RestoreTabs: new(true), ShowBanner: new(false)}
				return nil
			}))
			const source = "team.yaml"
			infos := map[string]runtime.PreparedSessionViewInfo{}
			for _, id := range []string{"first", "second", "child", "foreign", "fresh"} {
				sess := session.New(session.WithID(id), session.WithAgentName("root"), session.WithWorkingDir("/server/saved-directory"), session.WithAttributes(map[string]string{sessionActorSourceAttribute: source, runtime.SessionAgentAttribute: "root"}))
				if id != "fresh" {
					sess.AddMessage(session.UserMessage("saved history " + id))
				}
				infos[id] = runtime.PreparedSessionViewInfo{SessionID: id, RootSessionID: id, Session: sess, ActiveAgentName: "root", Binding: runtime.SessionBinding{AgentName: "root"}, WorkingDir: sess.WorkingDir}
			}
			foreign := infos["foreign"]
			foreign.Session.SetAttribute(sessionActorSourceAttribute, "another.yaml")
			child := infos["child"]
			child.RootSessionID = "first"
			child.Binding.ParentSessionID = "first"
			child.Attach = &runtime.SubagentAttachInfo{NodeID: "child-node", Agent: "root", Session: child.Session.Clone(), ParentSessionID: "first", ParentAgent: "root"}
			infos["child"] = child

			ids, active := []string{"first", "second"}, "second"
			switch mode {
			case "child-active":
				ids, active = []string{"first", "child"}, "child"
			case "stale-active":
				ids, active = []string{"first", "missing"}, "missing"
			case "wrong-source":
				ids, active = []string{"foreign", "first"}, "foreign"
			case "first-run":
				ids, active = nil, ""
			case "all-stale":
				ids, active = []string{"missing"}, "missing"
			}
			store, err := tuistate.New(t.Context())
			require.NoError(t, err)
			for _, id := range ids {
				require.NoError(t, store.AddTab(t.Context(), id, "/server/saved-directory"))
			}
			require.NoError(t, store.SetActiveTab(t.Context(), active))
			require.NoError(t, store.Close())

			var creates, destructive atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete || strings.Contains(r.URL.Path, "/cancel") || strings.Contains(r.URL.Path, "/stop") || strings.Contains(r.URL.Path, "/release") {
					destructive.Add(1)
				}
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/agents/team.yaml" {
					fmt.Fprint(w, `{"agents":[{"name":"root"}]}`)
					return
				}
				if r.Method == http.MethodPost && r.URL.Path == api.SessionAPIPath {
					creates.Add(1)
					fmt.Fprint(w, `{"session_id":"fresh","agent_name":"root"}`)
					return
				}
				id := strings.Split(strings.TrimPrefix(r.URL.Path, api.SessionAPIPath+"/"), "/")[0]
				info, found := infos[id]
				if mode == "forbidden" {
					w.WriteHeader(http.StatusForbidden)
					fmt.Fprint(w, `{"error":"forbidden"}`)
					return
				}
				if !found {
					w.WriteHeader(http.StatusNotFound)
					fmt.Fprint(w, `{"error":"not_found"}`)
					return
				}
				switch {
				case r.URL.Query().Get("view") == "prepare-info":
					assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"version": 2, "view": "prepare-info", "info": info}))
				case r.Method == http.MethodPatch && r.URL.Query().Get("view") == "open-view":
					writeReadyOpening(t, w, info)
				case strings.HasSuffix(r.URL.Path, "/snapshot"):
					assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"session": info.Session, "status": runtime.SessionStatus{SessionID: id, AgentName: "root", State: runtime.SessionStateSettled}}))
				case strings.HasSuffix(r.URL.Path, "/status"):
					assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"metadata": api.SessionMetadata{SessionID: id, AgentName: "root"}, "status": runtime.SessionStatus{SessionID: id, AgentName: "root", State: runtime.SessionStateSettled}}))
				default:
					w.WriteHeader(http.StatusNotFound)
					fmt.Fprint(w, `{"error":"not_found"}`)
				}
			}))
			defer server.Close()
			passes := 1
			if mode == "active" || mode == "child-active" {
				passes = 2
			}
			for range passes {
				flags := &runExecFlags{managedAPI: true, managedAPIAttach: true, remoteAddress: server.URL, remoteWorkingDir: "/server/connection-subdirectory"}
				backend := &remoteBackend{flags: flags, agentFileName: source, restoreInitialTab: flags.shouldRestoreInitialTab(true, []string{source}, userconfig.Get())}
				services, sessions, initial, cleanup, err := backend.CreateSession(t.Context(), nil, backend.CreateSessionRequest("ignored-client-directory"))
				if mode == "forbidden" {
					require.Error(t, err)
					require.Zero(t, creates.Load(), "failed authenticated restoration must not create a replacement")
					return
				}
				require.NoError(t, err)
				want, wantCreates := "second", int32(0)
				if mode == "first-run" || mode == "all-stale" {
					want, wantCreates = "fresh", 1
				} else {
					if mode == "child-active" {
						want = "child"
					} else if mode != "active" {
						want = "first"
					}
					require.Equal(t, "/server/saved-directory", initial.WorkingDir, "connection cwd must not overwrite the restored workspace")
					require.Len(t, initial.MessagesSnapshot(), 1)
				}
				require.Equal(t, want, initial.ID)
				require.NoError(t, flags.configureSessionViewHost(t.Context(), backend, services, sessions, initial, backend.Spawner(services, sessions), cleanup))
				application := app.New(t.Context(), sessions, initial, sessionSessionBinding(t.Context(), services, initial), app.WithRuntimeServices(services))
				model := tui.New(t.Context(), backend.Spawner(services, sessions), application, initial.WorkingDir, flags.sessionViewHost.Shutdown, tui.WithSupervisor(flags.sessionViewHost), tui.WithSessionRestorer(backend.Restorer(services, sessions)))
				tabs, _ := flags.sessionViewHost.GetTabs()
				count := 1
				if mode == "active" || mode == "child-active" {
					count = 2
				}
				require.Len(t, tabs, count, "reattach must not add an empty startup tab")
				if count == 2 {
					require.Equal(t, "first", tabs[0].SessionID, "saved tab ordering is preserved")
					require.Equal(t, want, tabs[1].SessionID)
				}
				require.Equal(t, want, flags.sessionViewHost.ActiveID())
				require.Equal(t, wantCreates, creates.Load())
				if mode == "child-active" {
					require.NotNil(t, application.AttachedSubagent())
				}
				model.(interface{ Shutdown() }).Shutdown()
				require.Zero(t, destructive.Load(), "detaching never cancels/releases server execution")
			}
			if mode == "active" {
				flags := &runExecFlags{remoteAddress: server.URL, remoteWorkingDir: "/server/new-directory"}
				backend := &remoteBackend{flags: flags, agentFileName: source}
				client, err := runtime.NewClient(server.URL)
				require.NoError(t, err)
				services, err := runtime.NewRemoteServices(client, runtime.WithRemoteCurrentAgent("root"), runtime.WithRemoteAgentFilename(source))
				require.NoError(t, err)
				transport, err := runtime.NewSessionTransport(client, runtime.WithSessionTransportSource(source))
				require.NoError(t, err)
				spawned, err := backend.Spawner(services, transport)(t.Context(), flags.remoteWorkingDir)
				require.NoError(t, err)
				require.Equal(t, "fresh", spawned.Session.ID)
				require.Equal(t, int32(1), creates.Load(), "explicit new-tab actions still create sessions")
				spawned.App.Close()
			}
		})
	}
}
