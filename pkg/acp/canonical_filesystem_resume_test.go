package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type canonicalFilesystemProbe struct {
	fs      *FilesystemToolset
	results chan string
}

func (p *canonicalFilesystemProbe) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{{Name: "probe", Parameters: map[string]any{"type": "object"}, Handler: func(ctx context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
		for _, call := range []struct{ name, args string }{{"read_file", `{"path":"file.txt"}`}, {"write_file", `{"path":"file.txt","content":"before"}`}, {"edit_file", `{"path":"file.txt","edits":[{"oldText":"before","newText":"after"}]}`}} {
			var result *tools.ToolCallResult
			var err error
			tc := tools.ToolCall{Function: tools.FunctionCall{Name: call.name, Arguments: call.args}}
			switch call.name {
			case "read_file":
				result, err = p.fs.handleReadFile(ctx, tc, nil)
			case "write_file":
				result, err = p.fs.handleWriteFile(ctx, tc, nil)
			case "edit_file":
				result, err = p.fs.handleEditFile(ctx, tc, nil)
			}
			if err != nil {
				return nil, err
			}
			if result.IsError {
				p.results <- result.Output
				return result, nil
			}
		}
		p.results <- "ok"
		return tools.ResultSuccess("ok"), nil
	}}}, nil
}

type filesystemRoute struct {
	Method        string
	SessionID     acpsdk.SessionId
	Path, Content string
}
type canonicalFilesystemClient struct {
	peer   io.Writer
	mu     sync.Mutex
	routes []filesystemRoute
}

func (p *canonicalFilesystemClient) Write(b []byte) (int, error) {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(b, &msg); err != nil {
		return 0, err
	}
	if len(msg.ID) == 0 {
		return len(b), nil
	}
	var result any
	route := filesystemRoute{Method: msg.Method}
	switch msg.Method {
	case acpsdk.ClientMethodFsReadTextFile:
		var req acpsdk.ReadTextFileRequest
		if err := json.Unmarshal(msg.Params, &req); err != nil {
			return 0, err
		}
		route.SessionID, route.Path = req.SessionId, req.Path
		result = acpsdk.ReadTextFileResponse{Content: "before"}
	case acpsdk.ClientMethodFsWriteTextFile:
		var req acpsdk.WriteTextFileRequest
		if err := json.Unmarshal(msg.Params, &req); err != nil {
			return 0, err
		}
		route.SessionID, route.Path, route.Content = req.SessionId, req.Path, req.Content
		result = acpsdk.WriteTextFileResponse{}
	default:
		return 0, fmt.Errorf("unexpected method %s", msg.Method)
	}
	p.mu.Lock()
	p.routes = append(p.routes, route)
	p.mu.Unlock()
	response, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{"2.0", msg.ID, result})
	if err != nil {
		return 0, err
	}
	_, err = p.peer.Write(append(response, '\n'))
	return len(b), err
}

func (p *canonicalFilesystemClient) recorded() []filesystemRoute {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]filesystemRoute(nil), p.routes...)
}

func TestACPCanonicalFilesystemPromptRoutes(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	a := NewAgent(nil, nil, session.NewInMemorySessionStore())
	a.clientFS = acpsdk.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true}
	reader, writer := io.Pipe()
	client := &canonicalFilesystemClient{peer: writer}
	conn := acpsdk.NewAgentSideConnection(a, client, reader)
	conn.SetLogger(slog.New(slog.DiscardHandler))
	a.SetAgentConnection(conn)
	t.Cleanup(func() { _ = writer.Close(); <-conn.Done() })
	for _, id := range []string{"workspace-one", "workspace-two"} {
		previousRoutes := len(client.recorded())
		wd := t.TempDir()
		probe := &canonicalFilesystemProbe{results: make(chan string, 1)}
		probe.fs = NewFilesystemToolset(a, wd)
		rt, err := runtime.NewLocalRuntime(ctx, team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(&realACPProvider{}), agent.WithToolSets(probe), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})), agent.New("worker", "prompt", agent.WithModel(&realACPProvider{}), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})))), runtime.WithSessionStore(a.sessionStore), runtime.WithWorkingDir(wd))
		require.NoError(t, err)
		owner := runtime.NewSessionRuntimeSupervisor(rt)
		t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(ctx))) })
		sess := session.New(session.WithID(id), session.WithWorkingDir(wd), session.WithToolsApproved(true))
		h, err := owner.Runtime().CreateSession(ctx, sess, runtime.SessionBinding{AgentName: "root"})
		require.NoError(t, err)
		a.sessions[id] = &Session{id: id, sess: sess, session: h, rt: owner.Runtime(), workingDir: wd}
		_, err = a.Prompt(ctx, acpsdk.PromptRequest{SessionId: acpsdk.SessionId(id), Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("use probe")}})
		require.NoError(t, err)
		require.Equal(t, "ok", <-probe.results)
		routes := client.recorded()
		require.Len(t, routes, previousRoutes+4)
		for _, route := range routes[len(routes)-4:] {
			require.Equal(t, acpsdk.SessionId(id), route.SessionID)
			realWD, err := filepath.EvalSymlinks(wd)
			require.NoError(t, err)
			require.Equal(t, filepath.Join(realWD, "file.txt"), route.Path)
		}
		require.Equal(t, "before", routes[len(routes)-3].Content)
		require.Equal(t, "after", routes[len(routes)-1].Content)
		child := session.New(session.WithID(id + "-child"))
		_, err = owner.Runtime().CreateSession(ctx, child, runtime.SessionBinding{AgentName: "worker", ParentSessionID: id})
		require.NoError(t, err)
		nested := session.New(session.WithID(id + "-nested"))
		_, err = owner.Runtime().CreateSession(ctx, nested, runtime.SessionBinding{AgentName: "worker", ParentSessionID: child.ID})
		require.NoError(t, err)
		childCtx := httpclient.ContextWithSessionID(ctx, nested.ID)
		resolved, err := probe.fs.resolvePathForSession(childCtx, "nested.txt")
		require.NoError(t, err)
		realWD, err := filepath.EvalSymlinks(wd)
		require.NoError(t, err)
		require.Equal(t, filepath.Join(realWD, "nested.txt"), resolved)
		_, err = probe.fs.resolvePathForSession(childCtx, filepath.Join(t.TempDir(), "blocked.txt"))
		require.ErrorContains(t, err, "escapes the working directory")
		result, err := probe.fs.handleReadFile(childCtx, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"path":"child.txt"}`}}, nil)
		require.NoError(t, err)
		require.False(t, result.IsError, result.Output)
		require.Equal(t, acpsdk.SessionId(id), client.recorded()[len(client.recorded())-1].SessionID)
		unknown := httpclient.ContextWithSessionID(ctx, "unknown")
		before := len(client.recorded())
		result, err = probe.fs.handleReadFile(unknown, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"path":"file.txt"}`}}, nil)
		require.NoError(t, err)
		require.True(t, result.IsError)
		require.Contains(t, result.Output, "no registered ACP workspace")
		require.Len(t, client.recorded(), before)
	}
}

func TestACPResumeSQLiteCanonicalBindings(t *testing.T) {
	for _, mode := range []string{"worker-root", "handoff", "child", "legacy", "legacy-default", "legacy-nondefault", "missing-agent", "source-mismatch", "source-match", "orphan-child"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			store, err := sqlitestore.New(ctx, filepath.Join(t.TempDir(), "owned.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			tm := team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(&realACPProvider{}), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})), agent.New("worker", "prompt", agent.WithModel(&realACPProvider{}), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"}))))
			rt, err := runtime.NewLocalRuntime(ctx, tm, runtime.WithSessionStore(store))
			require.NoError(t, err)
			owner := runtime.NewSessionRuntimeSupervisor(rt)
			binding := "worker"
			if mode == "handoff" || mode == "child" {
				binding = "root"
			}
			sess := session.New(session.WithWorkingDir(t.TempDir()))
			selected := sess
			if mode == "legacy" || mode == "legacy-default" || mode == "legacy-nondefault" {
				if mode == "legacy-default" {
					sess.AgentName = "root"
				}
				if mode == "legacy-nondefault" {
					sess.AgentName = "worker"
				}
				require.NoError(t, store.AddSession(ctx, sess))
			} else {
				_, err = owner.Runtime().CreateSession(ctx, sess, runtime.SessionBinding{AgentName: binding})
				require.NoError(t, err)
				if mode == "child" {
					selected = session.New()
					_, err = owner.Runtime().CreateSession(ctx, selected, runtime.SessionBinding{AgentName: "worker", ParentSessionID: sess.ID})
					require.NoError(t, err)
				}
			}
			require.NoError(t, owner.Shutdown(context.WithoutCancel(ctx)))
			selected, err = store.GetSession(ctx, selected.ID)
			require.NoError(t, err)
			if mode == "handoff" {
				selected.AgentName = "worker"
			}
			if mode == "missing-agent" {
				selected.SetAttribute(runtime.SessionAgentAttribute, "absent")
			}
			if mode == "source-match" {
				selected.SetAttribute("docker-agent.actor.source", "configured.yaml")
			}
			if mode == "source-mismatch" {
				selected.SetAttribute("docker-agent.actor.source", "other.yaml")
			}
			if mode == "orphan-child" {
				parent := session.New()
				require.NoError(t, store.AddSession(ctx, parent))
				selected.ParentID = parent.ID
			}
			require.NoError(t, store.UpdateSession(ctx, selected))
			a := NewAgent(config.NewBytesSource("configured.yaml", nil), &config.RuntimeConfig{}, store)
			a.team = tm
			t.Cleanup(func() { a.Stop(context.WithoutCancel(ctx)) })
			_, err = a.ResumeSession(ctx, acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(selected.ID)})
			if mode == "missing-agent" || mode == "source-mismatch" || mode == "orphan-child" || mode == "legacy-nondefault" {
				require.Error(t, err)
				require.Empty(t, a.sessions)
				return
			}
			require.NoError(t, err)
			snapshot, err := a.sessions[selected.ID].session.Snapshot(ctx)
			require.NoError(t, err)
			wantBinding := binding
			if mode == "legacy" || mode == "legacy-default" {
				wantBinding = "root"
			}
			if mode == "child" {
				wantBinding = "worker"
				require.Equal(t, sess.ID, snapshot.ParentID)
			}
			require.Equal(t, wantBinding, snapshot.AttributesSnapshot()[runtime.SessionAgentAttribute])
			if mode == "handoff" {
				require.Equal(t, "worker", snapshot.AgentName)
			}
		})
	}
}
