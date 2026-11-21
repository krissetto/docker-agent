package acp

import (
	"context"
	"errors"
	"fmt"
	"github.com/docker/docker-agent/pkg/chat"
	runtimeclient "github.com/docker/docker-agent/pkg/runtime/client"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/server"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// Embedding the interface makes unexpected filesystem/terminal requests fail
// the test, rather than installing permissive fallback implementations.
type managedTestClient struct {
	acpsdk.Client
	mu        sync.Mutex
	updates   []acpsdk.SessionNotification
	approvals int
	decision  string
}

func (c *managedTestClient) SessionUpdate(_ context.Context, p acpsdk.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updates = append(c.updates, p)
	return nil
}
func (c *managedTestClient) RequestPermission(_ context.Context, p acpsdk.RequestPermissionRequest) (acpsdk.RequestPermissionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.approvals++
	if c.decision == "failure" {
		return acpsdk.RequestPermissionResponse{}, errors.New("permission client disconnected")
	}
	return acpsdk.RequestPermissionResponse{Outcome: acpsdk.RequestPermissionOutcome{Selected: &acpsdk.RequestPermissionOutcomeSelected{OptionId: acpsdk.PermissionOptionId(c.decision)}}}, nil
}

type managedProbe struct {
	executed       atomic.Int32
	modal          bool
	modalCompleted atomic.Bool
}

func (p *managedProbe) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{{Name: "probe", Parameters: map[string]any{"type": "object"}, Handler: func(ctx context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
		p.executed.Add(1)
		if p.modal {
			_, err := tools.ScopedElicitationHandler(ctx, &mcp.ElicitParams{Message: "enter value"})
			if err != nil {
				return nil, err
			}
			p.modalCompleted.Store(true)
		}
		return tools.ResultSuccess("executed"), nil
	}}}, nil
}

func managedLoopback(t *testing.T, modal bool, token string) (*runtime.Client, session.Store, *managedProbe, string, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	wd, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	store, err := sqlitestore.New(ctx, filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	probe := &managedProbe{modal: modal}
	source := config.NewBytesSource("managed-team", []byte("agents:\n  root:\n    model: test/model\n    instruction: test\n"))
	sm := server.NewSessionManager(ctx, config.Sources{"managed-team": source}, store, 0, &config.RuntimeConfig{Config: config.Config{WorkingDir: wd}},
		server.WithSessionWorkingDirRoot(wd), server.WithSessionRuntimeFactory(func(ctx context.Context, _ config.Source, workingDir string) (runtime.SessionRuntimeSupervisor, error) {
			// realACPProvider is an in-process scripted fake returning two fixed streams;
			// no inference, provider network or model credentials are used.
			rt, err := runtime.NewLocalRuntime(ctx, team.New(team.WithAgents(agent.New("root", "test", agent.WithModel(&managedProvider{}), agent.WithToolSets(probe), agent.WithSafety(latest.SafetyModeAutonomous)))), runtime.WithSessionStore(store), runtime.WithWorkingDir(workingDir))
			if err != nil {
				return nil, err
			}
			return runtime.NewSessionRuntimeSupervisor(rt), nil
		}))
	srv := server.NewWithManager(sm, "loopback-test-token", server.WithCacheWarmup(false))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
		require.NoError(t, sm.Shutdown(context.Background()))
		require.NoError(t, store.Close())
	})
	client, err := runtime.NewClient("http://"+listener.Addr().String(), runtime.WithAuthToken(token))
	require.NoError(t, err)
	return client, store, probe, wd, "managed-team"
}

// Every turn requests one action then completes, including follow-up turns.
type managedProvider struct{ realACPProvider }

func (p *managedProvider) CreateChatCompletionStream(ctx context.Context, messages []chat.Message, available []tools.Tool) (chat.MessageStream, error) {
	fake := &realACPProvider{}
	if p.calls.Add(1)%2 == 0 {
		fake.calls.Store(1)
	}
	return fake.CreateChatCompletionStream(ctx, messages, available)
}

func managedProtocol(t *testing.T, remote *runtime.Client, source, wd string, peer *managedTestClient) *acpsdk.ClientSideConnection {
	t.Helper()
	client, _ := managedProtocolConnection(t, remote, source, wd, peer)
	return client
}

func managedProtocolConnection(t *testing.T, remote *runtime.Client, source, wd string, peer *managedTestClient) (*acpsdk.ClientSideConnection, func()) {
	t.Helper()
	bridge, err := NewManagedAgent(remote, source, wd)
	require.NoError(t, err)
	agentRead, clientWrite := io.Pipe()
	clientRead, agentWrite := io.Pipe()
	agentConn := acpsdk.NewAgentSideConnection(bridge, agentWrite, agentRead)
	bridge.SetAgentConnection(agentConn)
	client := acpsdk.NewClientSideConnection(peer, clientWrite, clientRead)
	logger := slog.New(slog.DiscardHandler)
	agentConn.SetLogger(logger)
	client.SetLogger(logger)
	var once sync.Once
	disconnect := func() {
		once.Do(func() {
			_ = clientWrite.Close()
			_ = agentWrite.Close()
			_ = agentRead.Close()
			_ = clientRead.Close()
			bridge.Stop(context.Background())
			<-agentConn.Done()
			<-client.Done()
		})
	}
	t.Cleanup(disconnect)
	return client, disconnect
}

func TestManagedACPProtocolLoopback(t *testing.T) {
	for _, tc := range []struct {
		name, decision string
		modal          bool
		executions     int32
	}{
		{"allow", "allow", false, 1}, {"reject", "reject", false, 0}, {"permission failure", "failure", false, 0}, {"unknown decision", "invalid", false, 0}, {"unsupported modal", "allow", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			remote, store, probe, wd, source := managedLoopback(t, tc.modal, "loopback-test-token")
			peer := &managedTestClient{decision: tc.decision}
			client := managedProtocol(t, remote, source, wd, peer)
			init, err := client.Initialize(ctx, acpsdk.InitializeRequest{ProtocolVersion: acpsdk.ProtocolVersionNumber})
			require.NoError(t, err)
			require.True(t, init.AgentCapabilities.LoadSession)
			require.NotNil(t, init.AgentCapabilities.SessionCapabilities.List)
			require.NotNil(t, init.AgentCapabilities.SessionCapabilities.Resume)
			require.Nil(t, init.AgentCapabilities.SessionCapabilities.AdditionalDirectories)
			created, err := client.NewSession(ctx, acpsdk.NewSessionRequest{McpServers: []acpsdk.McpServer{}, Cwd: wd})
			require.NoError(t, err)
			_, err = client.SetSessionMode(ctx, acpsdk.SetSessionModeRequest{SessionId: created.SessionId, ModeId: "autonomous"})
			require.Error(t, err, "ACP mode changes must not bypass approvals")
			_, err = client.SetSessionConfigOption(ctx, acpsdk.SetSessionConfigOptionRequest{ValueId: &acpsdk.SetSessionConfigOptionValueId{SessionId: created.SessionId, ConfigId: "safety", Value: "autonomous"}})
			require.Error(t, err, "ACP options are unsupported")
			_, err = client.Prompt(ctx, acpsdk.PromptRequest{SessionId: created.SessionId, Prompt: []acpsdk.ContentBlock{{ResourceLink: &acpsdk.ContentBlockResourceLink{Type: "resource_link", Uri: "file:///untrusted", Name: "untrusted"}}}})
			require.Error(t, err, "unsupported input must fail rather than be silently dropped")
			stored, err := store.GetSession(ctx, string(created.SessionId))
			require.NoError(t, err)
			require.Equal(t, wd, stored.WorkingDir)
			require.Equal(t, source, stored.AttributesSnapshot()["docker-agent.actor.source"])
			require.Equal(t, session.SafetyPolicyStrict, stored.GetSafetyPolicy())
			require.False(t, stored.IsToolsApproved())
			response, err := client.Prompt(ctx, acpsdk.PromptRequest{SessionId: created.SessionId, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("use probe")}})
			if tc.modal || tc.decision == "failure" || tc.decision == "invalid" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, acpsdk.StopReasonEndTurn, response.StopReason)
			}
			require.Equal(t, tc.executions, probe.executed.Load())
			require.False(t, probe.modalCompleted.Load())
			transport, err := runtime.NewSessionTransport(remote)
			require.NoError(t, err)
			handle, err := transport.SessionByID(string(created.SessionId))
			require.NoError(t, err)
			status, err := handle.Status(ctx)
			require.NoError(t, err)
			require.Empty(t, status.TurnID, "every prompt, including unsupported modal, settles its API turn")
			peer.mu.Lock()
			approvals := peer.approvals
			updates := append([]acpsdk.SessionNotification(nil), peer.updates...)
			peer.mu.Unlock()
			require.Equal(t, 1, approvals)
			if !tc.modal && tc.decision == "allow" {
				var text strings.Builder
				for _, u := range updates {
					if u.Update.AgentMessageChunk != nil && u.Update.AgentMessageChunk.Content.Text != nil {
						text.WriteString(u.Update.AgentMessageChunk.Content.Text.Text)
					}
				}
				require.Contains(t, text.String(), "finished")
			}
			_, err = client.LoadSession(ctx, acpsdk.LoadSessionRequest{McpServers: []acpsdk.McpServer{}, SessionId: created.SessionId, Cwd: wd})
			require.NoError(t, err)
			listed, err := client.ListSessions(ctx, acpsdk.ListSessionsRequest{})
			require.NoError(t, err)
			require.Len(t, listed.Sessions, 1)
			require.Equal(t, created.SessionId, listed.Sessions[0].SessionId)
			_, err = client.ResumeSession(ctx, acpsdk.ResumeSessionRequest{McpServers: []acpsdk.McpServer{}, SessionId: created.SessionId, Cwd: wd})
			require.NoError(t, err)
			_, err = client.Prompt(ctx, acpsdk.PromptRequest{SessionId: "unowned", Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("hello")}})
			require.Error(t, err)
			_, err = client.NewSession(ctx, acpsdk.NewSessionRequest{McpServers: []acpsdk.McpServer{}, Cwd: t.TempDir()})
			require.Error(t, err)
			_, err = client.NewSession(ctx, acpsdk.NewSessionRequest{McpServers: []acpsdk.McpServer{}, Cwd: wd, AdditionalDirectories: []string{wd}})
			require.Error(t, err)
			_, err = client.CloseSession(ctx, acpsdk.CloseSessionRequest{SessionId: created.SessionId})
			require.NoError(t, err)
			_, err = store.GetSession(ctx, string(created.SessionId))
			require.NoError(t, err, "ACP close never deletes API-owned persistence")
		})
	}
}

func TestManagedACPProtocolRejectsAuthenticationAndSource(t *testing.T) {
	for _, tc := range []struct{ name, token, source string }{{"authentication", "wrong", "managed-team"}, {"source", "loopback-test-token", "unknown-team"}} {
		t.Run(tc.name, func(t *testing.T) {
			remote, store, _, wd, _ := managedLoopback(t, false, tc.token)
			client := managedProtocol(t, remote, tc.source, wd, &managedTestClient{})
			_, err := client.Initialize(t.Context(), acpsdk.InitializeRequest{ProtocolVersion: acpsdk.ProtocolVersionNumber})
			require.Error(t, err)
			rows, err := store.GetSessionSummaries(t.Context())
			require.NoError(t, err)
			require.Empty(t, rows)
		})
	}
}

func TestManagedACPCloseDrainsOwnedTurnBeforeDetach(t *testing.T) {
	s := &Session{drainTurn: true}
	turnCtx, finish, err := s.startTurn(t.Context())
	require.NoError(t, err)
	finished := false
	defer func() {
		if !finished {
			finish()
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, s.shutdown(ctx), context.DeadlineExceeded)
	require.ErrorIs(t, turnCtx.Err(), context.Canceled)
	finish()
	finished = true
	require.NoError(t, s.shutdown(t.Context()))
}

func managedInitialize(t *testing.T, client *acpsdk.ClientSideConnection) {
	t.Helper()
	_, err := client.Initialize(t.Context(), acpsdk.InitializeRequest{ProtocolVersion: acpsdk.ProtocolVersionNumber})
	require.NoError(t, err)
}

func managedPrompt(t *testing.T, client *acpsdk.ClientSideConnection, sid acpsdk.SessionId, text string) {
	t.Helper()
	response, err := client.Prompt(t.Context(), acpsdk.PromptRequest{SessionId: sid, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock(text)}})
	require.NoError(t, err)
	require.Equal(t, acpsdk.StopReasonEndTurn, response.StopReason)
}

func (c *managedTestClient) transcript() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var text strings.Builder
	for _, u := range c.updates {
		if m := u.Update.UserMessageChunk; m != nil && m.Content.Text != nil {
			fmt.Fprintf(&text, "user:%s\n", m.Content.Text.Text)
		}
		if m := u.Update.AgentMessageChunk; m != nil && m.Content.Text != nil {
			fmt.Fprintf(&text, "agent:%s\n", m.Content.Text.Text)
		}
	}
	return text.String()
}

func TestManagedACPReconnectSharedHistory(t *testing.T) {
	remote, store, probe, wd, source := managedLoopback(t, false, "loopback-test-token")
	first, disconnect := managedProtocolConnection(t, remote, source, wd, &managedTestClient{decision: "allow"})
	managedInitialize(t, first)
	created, err := first.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: wd, McpServers: []acpsdk.McpServer{}})
	require.NoError(t, err)
	managedPrompt(t, first, created.SessionId, "first persisted turn")
	require.EqualValues(t, 1, probe.executed.Load())
	disconnect()
	peer := &managedTestClient{decision: "allow"}
	second := managedProtocol(t, remote, source, wd, peer)
	managedInitialize(t, second)
	listed, err := second.ListSessions(t.Context(), acpsdk.ListSessionsRequest{})
	require.NoError(t, err)
	require.Len(t, listed.Sessions, 1)
	require.Equal(t, created.SessionId, listed.Sessions[0].SessionId)
	_, err = second.LoadSession(t.Context(), acpsdk.LoadSessionRequest{SessionId: created.SessionId, Cwd: wd, McpServers: []acpsdk.McpServer{}})
	require.NoError(t, err)
	require.Contains(t, peer.transcript(), "user:first persisted turn")
	require.Contains(t, peer.transcript(), "agent:finished")
	replay := peer.transcript()
	_, err = second.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: wd, McpServers: []acpsdk.McpServer{}})
	require.NoError(t, err)
	require.Equal(t, replay, peer.transcript(), "resume does not duplicate replay")
	require.EqualValues(t, 1, probe.executed.Load(), "load and resume never execute history")
	managedPrompt(t, second, created.SessionId, "next persisted turn")
	require.EqualValues(t, 2, probe.executed.Load())
	persisted, err := store.GetSession(t.Context(), string(created.SessionId))
	require.NoError(t, err)
	require.Equal(t, string(created.SessionId), persisted.ID)
	var users []string
	for _, m := range persisted.GetAllMessages() {
		if m.Message.Role == chat.MessageRoleUser {
			users = append(users, m.Message.Content)
			require.True(t, m.RequireApproval)
		}
	}
	require.Equal(t, []string{"first persisted turn", "next persisted turn"}, users)
}

func TestManagedACPContinuesAutonomousUIConversationWithoutPolicyMutation(t *testing.T) {
	for _, decision := range []string{"allow", "reject", "allow-always"} {
		t.Run(decision, func(t *testing.T) {
			remote, store, probe, wd, source := managedLoopback(t, false, "loopback-test-token")
			transport, err := runtime.NewSessionTransport(remote, runtime.WithSessionTransportSource(source))
			require.NoError(t, err)
			ui, err := transport.CreateSession(t.Context(), session.New(session.WithWorkingDir(wd), session.WithTitle("Existing TUI title"), session.WithToolsApproved(true)), runtime.SessionBinding{AgentName: "root"})
			require.NoError(t, err)
			_, err = runtimeclient.RunTurn(t.Context(), ui, runtime.TurnInput{Content: "TUI original turn"})
			require.NoError(t, err)
			require.EqualValues(t, 1, probe.executed.Load())
			peer := &managedTestClient{decision: decision}
			client := managedProtocol(t, remote, source, wd, peer)
			managedInitialize(t, client)
			listed, err := client.ListSessions(t.Context(), acpsdk.ListSessionsRequest{})
			require.NoError(t, err)
			require.Len(t, listed.Sessions, 1)
			require.Equal(t, ui.ID(), string(listed.Sessions[0].SessionId))
			require.Equal(t, "Existing TUI title", *listed.Sessions[0].Title)
			require.Equal(t, wd, listed.Sessions[0].Cwd)
			_, err = client.LoadSession(t.Context(), acpsdk.LoadSessionRequest{SessionId: acpsdk.SessionId(ui.ID()), Cwd: wd, McpServers: []acpsdk.McpServer{}})
			require.NoError(t, err)
			require.Contains(t, peer.transcript(), "TUI original turn")
			managedPrompt(t, client, acpsdk.SessionId(ui.ID()), "editor continuation")
			peer.mu.Lock()
			approvals := peer.approvals
			peer.mu.Unlock()
			require.Equal(t, 1, approvals)
			expected := int32(2)
			if decision == "reject" {
				expected = 1
			}
			require.Equal(t, expected, probe.executed.Load())
			stored, err := store.GetSession(t.Context(), ui.ID())
			require.NoError(t, err)
			require.Equal(t, session.SafetyPolicyAutonomous, stored.GetSafetyPolicy())
			require.True(t, stored.IsToolsApproved())
			require.Equal(t, source, stored.AttributesSnapshot()["docker-agent.actor.source"])
			require.Nil(t, stored.Permissions, "ACP allow-always must not persist a grant")
			// The API/TUI can continue under its original YOLO policy, without an editor.
			_, err = runtimeclient.RunTurn(t.Context(), ui, runtime.TurnInput{Content: "subsequent TUI turn"})
			require.NoError(t, err)
			require.Equal(t, expected+1, probe.executed.Load())
		})
	}
}

func TestManagedACPWorkspaceAndProvenanceBoundaries(t *testing.T) {
	remote, store, _, wd, source := managedLoopback(t, false, "loopback-test-token")
	sub := filepath.Join(wd, "project")
	require.NoError(t, os.Mkdir(sub, 0700))
	client := managedProtocol(t, remote, source, wd, &managedTestClient{decision: "allow"})
	managedInitialize(t, client)
	created, err := client.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: sub, McpServers: []acpsdk.McpServer{}})
	require.NoError(t, err)
	stored, err := store.GetSession(t.Context(), string(created.SessionId))
	require.NoError(t, err)
	require.Equal(t, sub, stored.WorkingDir)
	outside, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	escape := filepath.Join(wd, "escape")
	require.NoError(t, os.Symlink(outside, escape))
	for _, tc := range []struct{ id, source, cwd, parent string }{{"other-team", "other-team", wd, ""}, {"outside", source, outside, ""}, {"symlink", source, escape, ""}, {"child", source, wd, string(created.SessionId)}} {
		sess := session.New(session.WithID(tc.id), session.WithWorkingDir(tc.cwd), session.WithAgentName("root"), session.WithParentID(tc.parent), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "root", "docker-agent.actor.source": tc.source}))
		require.NoError(t, store.AddSession(t.Context(), sess))
		_, err = client.LoadSession(t.Context(), acpsdk.LoadSessionRequest{SessionId: acpsdk.SessionId(tc.id), McpServers: []acpsdk.McpServer{}})
		require.Error(t, err, tc.id)
	}
	listed, err := client.ListSessions(t.Context(), acpsdk.ListSessionsRequest{})
	require.NoError(t, err)
	require.Len(t, listed.Sessions, 1)
	require.Equal(t, created.SessionId, listed.Sessions[0].SessionId)
	_, err = client.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: escape, McpServers: []acpsdk.McpServer{}})
	require.Error(t, err)
	_, err = client.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: wd, McpServers: []acpsdk.McpServer{}})
	require.Error(t, err, "never relocate persisted history")
	_, err = client.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: sub, McpServers: []acpsdk.McpServer{}})
	require.NoError(t, err)
	filtered, err := client.ListSessions(t.Context(), acpsdk.ListSessionsRequest{Cwd: &wd})
	require.NoError(t, err)
	require.Empty(t, filtered.Sessions)
}

func TestManagedACPBusyApprovalRemainsOwnedByUI(t *testing.T) {
	remote, _, probe, wd, source := managedLoopback(t, false, "loopback-test-token")
	transport, err := runtime.NewSessionTransport(remote, runtime.WithSessionTransportSource(source))
	require.NoError(t, err)
	ui, err := transport.CreateSession(t.Context(), session.New(session.WithWorkingDir(wd), session.WithSafetyPolicy(session.SafetyPolicyStrict)), runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	observation, err := ui.Observe(t.Context(), runtime.ObserveOptions{})
	require.NoError(t, err)
	defer observation.Cancel()
	submitted, err := ui.Submit(t.Context(), runtime.TurnInput{Content: "UI pending approval"})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var interaction string
	for interaction == "" {
		select {
		case event := <-observation.Events:
			if _, ok := event.Event.(*runtime.ToolCallConfirmationEvent); ok {
				interaction = event.InteractionID
			}
		case <-ctx.Done():
			t.Fatal("UI confirmation missing")
		}
	}
	queued, err := ui.Submit(t.Context(), runtime.TurnInput{Content: "UI queued next turn"})
	require.NoError(t, err)
	before, err := remote.ReadSessionSnapshot(t.Context(), ui.ID())
	require.NoError(t, err)
	peer := &managedTestClient{decision: "allow"}
	client := managedProtocol(t, remote, source, wd, peer)
	managedInitialize(t, client)
	_, err = client.LoadSession(t.Context(), acpsdk.LoadSessionRequest{SessionId: acpsdk.SessionId(ui.ID()), Cwd: wd, McpServers: []acpsdk.McpServer{}})
	require.NoError(t, err, "read-only replay is safe during UI approval")
	_, err = client.Prompt(t.Context(), acpsdk.PromptRequest{SessionId: acpsdk.SessionId(ui.ID()), Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("must not queue")}})
	require.ErrorContains(t, err, "busy")
	require.NoError(t, client.Cancel(t.Context(), acpsdk.CancelNotification{SessionId: acpsdk.SessionId(ui.ID())}))
	_, err = client.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: acpsdk.SessionId(ui.ID())})
	require.NoError(t, err)
	after, err := remote.ReadSessionSnapshot(t.Context(), ui.ID())
	require.NoError(t, err)
	require.Equal(t, before.Status.TurnID, after.Status.TurnID)
	require.Equal(t, before.Status.Pending, after.Status.Pending)
	require.Equal(t, before.Interactions, after.Interactions)
	require.Equal(t, before.Session.GetAllMessages(), after.Session.GetAllMessages())
	require.Zero(t, peer.approvals)
	require.Zero(t, probe.executed.Load())
	removed, err := ui.(runtime.PendingMessageCanceler).CancelPendingMessage(t.Context(), queued.TurnID)
	require.NoError(t, err)
	require.True(t, removed)
	require.NoError(t, ui.Respond(t.Context(), runtime.InteractionResponse{InteractionID: interaction, Kind: runtime.InteractionConfirmation, Resume: runtime.ResumeReject("UI declines")}))
	require.NoError(t, ui.AwaitTurn(t.Context(), submitted.TurnID))
	require.Zero(t, probe.executed.Load())
}

func TestManagedACPRecoveredApprovalConstraintWaitsForExplicitDecision(t *testing.T) {
	remote, store, probe, wd, source := managedLoopback(t, false, "loopback-test-token")
	archived := session.New(session.WithID("recovered-acp-input"), session.WithWorkingDir(wd), session.WithAgentName("root"), session.WithToolsApproved(true), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "root", "docker-agent.actor.source": source}))
	pending := session.UserMessage("accepted before crash")
	pending.RequireApproval, pending.Pending, pending.Accepted, pending.TurnID = true, true, true, "crash-turn"
	archived.AddMessage(pending)
	require.NoError(t, store.AddSession(t.Context(), archived))
	peer := &managedTestClient{decision: "allow"}
	client := managedProtocol(t, remote, source, wd, peer)
	managedInitialize(t, client)
	_, err := client.LoadSession(t.Context(), acpsdk.LoadSessionRequest{SessionId: acpsdk.SessionId(archived.ID), Cwd: wd, McpServers: []acpsdk.McpServer{}})
	require.NoError(t, err)
	require.Zero(t, probe.executed.Load(), "replay never wakes recovered work")
	_, err = client.Prompt(t.Context(), acpsdk.PromptRequest{SessionId: acpsdk.SessionId(archived.ID), Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock("cannot jump pending work")}})
	require.ErrorContains(t, err, "busy")
	transport, err := runtime.NewSessionTransport(remote, runtime.WithSessionTransportSource(source))
	require.NoError(t, err)
	recovered, err := transport.SessionByID(archived.ID)
	require.NoError(t, err)
	observation, err := recovered.Observe(t.Context(), runtime.ObserveOptions{})
	require.NoError(t, err)
	defer observation.Cancel()
	_, err = recovered.Edit(t.Context(), runtime.SessionEdit{Kind: runtime.SessionEditResume})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var interaction string
	for interaction == "" {
		select {
		case event := <-observation.Events:
			if _, ok := event.Event.(*runtime.ToolCallConfirmationEvent); ok {
				interaction = event.InteractionID
			}
		case <-ctx.Done():
			t.Fatal("recovered input bypassed or lost approval")
		}
	}
	require.Zero(t, probe.executed.Load(), "without editor recovery waits rather than auto-executes under YOLO")
	require.NoError(t, recovered.Respond(t.Context(), runtime.InteractionResponse{InteractionID: interaction, Kind: runtime.InteractionConfirmation, Resume: runtime.ResumeReject("recovery denied")}))
	require.NoError(t, recovered.AwaitTurn(t.Context(), "crash-turn"))
	require.Zero(t, probe.executed.Load())
	snapshot, err := recovered.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, session.SafetyPolicyAutonomous, snapshot.GetSafetyPolicy())
	managedPrompt(t, client, acpsdk.SessionId(archived.ID), "fresh editor turn")
	require.EqualValues(t, 1, probe.executed.Load())
}
