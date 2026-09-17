package e2e_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/server"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// These deliberately frozen requests and raw-event decoding follow
// upstream/main 7ad21a750676130093b5c1596c476ce1456cb908,
// pkg/runtime/client.go: CreateSession, GetSessions, RunAgentWithAgentName,
// ResumeSession, SteerSession, FollowUpSession and StreamSessionEvents.
// Do not replace them with current DTOs: that would allow both ends to drift
// together and stop testing old HTTP clients.
type upstreamHTTPClient struct {
	url    string
	client *http.Client
}

func (c upstreamHTTPClient) request(t *testing.T, ctx context.Context, method, path string, body any) *http.Response {
	t.Helper()
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url+path, bytes.NewReader(data))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.client.Do(req)
	require.NoError(t, err)
	return resp
}

func (c upstreamHTTPClient) json(t *testing.T, ctx context.Context, method, path string, body, result any) {
	t.Helper()
	resp := c.request(t, ctx, method, path, body)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Less(t, resp.StatusCode, 300, "%s %s: %s", method, path, data)
	if result != nil {
		require.NoError(t, json.Unmarshal(data, result))
	}
}

func (c upstreamHTTPClient) create(t *testing.T, ctx context.Context) string {
	t.Helper()
	var created struct {
		ID string `json:"id"`
	}
	c.json(t, ctx, http.MethodPost, "/api/sessions", map[string]any{
		"title": "HTTP compatibility", "agent_name": "root", "tools_approved": false,
	}, &created)
	require.NotEmpty(t, created.ID, "upstream clients read id, not session_id")
	return created.ID
}

func upstreamMessages(content string) map[string]any {
	return map[string]any{"messages": []map[string]string{{"role": "user", "content": content}}}
}

// Like the upstream SSE decoder, this reads data: objects by their top-level
// type. Canonical snapshot/event wrappers cannot stand in for these events.
func upstreamEvent(t *testing.T, scanner *bufio.Scanner, wanted string) {
	t.Helper()
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		var event map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(data), &event))
		var kind string
		require.NoError(t, json.Unmarshal(event["type"], &kind))
		require.NotContains(t, []string{"snapshot", "ready", "event"}, kind, "legacy stream must contain raw runtime events")
		require.NotEqual(t, "error", kind, "%s", data)
		if kind == wanted {
			return
		}
	}
	require.NoError(t, scanner.Err())
	t.Fatalf("legacy stream ended before %s", wanted)
}

type compatibilityProvider struct {
	calls   atomic.Int32
	blocked chan struct{}
}

func (*compatibilityProvider) ID() modelsdev.ID {
	return modelsdev.ParseIDOrZero("test/http-compatibility")
}
func (*compatibilityProvider) BaseConfig() base.Config { return base.Config{} }
func (p *compatibilityProvider) CreateChatCompletionStream(ctx context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	n := p.calls.Add(1)
	if p.blocked != nil {
		if n == 1 {
			close(p.blocked)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if n == 1 {
		return &compatibilityStream{responses: []chat.MessageStreamResponse{
			{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{ToolCalls: []tools.ToolCall{{ID: "approval", Type: "function", Function: tools.FunctionCall{Name: "danger", Arguments: "{}"}}}}}}},
			{Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonToolCalls}}},
		}}, nil
	}
	return &compatibilityStream{responses: []chat.MessageStreamResponse{
		{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "done"}}}},
		{Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}}},
	}}, nil
}

type compatibilityStream struct{ responses []chat.MessageStreamResponse }

func (s *compatibilityStream) Recv() (chat.MessageStreamResponse, error) {
	if len(s.responses) == 0 {
		return chat.MessageStreamResponse{}, io.EOF
	}
	r := s.responses[0]
	s.responses = s.responses[1:]
	return r, nil
}
func (*compatibilityStream) Close() {}

type compatibilityTools struct{ tool tools.Tool }

func (s compatibilityTools) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{s.tool}, nil
}

func compatibilityServer(t *testing.T, provider *compatibilityProvider) (upstreamHTTPClient, *runtime.SessionTransport, *atomic.Int32) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	store := session.NewInMemorySessionStore()
	toolCalls := &atomic.Int32{}
	tool := tools.Tool{Name: "danger", Parameters: map[string]any{}, Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
		toolCalls.Add(1)
		return tools.ResultSuccess("ok"), nil
	}}
	agt := agent.New("root", "Answer briefly", agent.WithModel(provider), agent.WithToolSets(compatibilityTools{tool}), agent.WithCommands(types.Commands{"work": {Agent: "worker"}}))
	worker := agent.New("worker", "Answer briefly as the worker", agent.WithModel(provider))
	rt, err := runtime.NewLocalRuntime(ctx, team.New(team.WithAgents(agt, worker)), runtime.WithSessionStore(store))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	sources := config.Sources{"default.yaml": config.NewBytesSource("default.yaml", []byte("agents:\n  root:\n    model: openai/test\n    commands:\n      work:\n        agent: worker\n  worker:\n    model: openai/test\n"))}
	srv, err := server.New(ctx, store, &config.RuntimeConfig{}, 0, sources, "", 0,
		server.WithSessionRuntimes(map[string]runtime.SessionRuntime{"default.yaml": owner.Runtime()}))
	require.NoError(t, err)
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	transport := &http.Transport{}
	httpClient := &http.Client{Transport: transport}
	t.Cleanup(func() {
		cancel()
		transport.CloseIdleConnections()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Error("compatibility server did not stop")
		}
		shutdownCtx, stop := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer stop()
		require.NoError(t, owner.Shutdown(shutdownCtx))
	})
	old := upstreamHTTPClient{url: "http://" + ln.Addr().String(), client: httpClient}
	client, err := runtime.NewClient(old.url, runtime.WithHTTPClient(httpClient))
	require.NoError(t, err)
	v2, err := runtime.NewSessionTransport(client, runtime.WithSessionTransportSource("default.yaml"))
	require.NoError(t, err)
	return old, v2, toolCalls
}

func TestHTTPCompatibility_OldRunAndV2ShareSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	old, v2, toolCalls := compatibilityServer(t, &compatibilityProvider{})
	id := old.create(t, ctx)
	path := "/api/sessions/" + id

	var listed []struct {
		ID string `json:"id"`
	}
	old.json(t, ctx, http.MethodGet, "/api/sessions", nil, &listed)
	require.Contains(t, listed, struct {
		ID string `json:"id"`
	}{id})
	resp := old.request(t, ctx, http.MethodPost, path+"/agent/default.yaml/root", upstreamMessages("old run"))
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	scanner := bufio.NewScanner(resp.Body)
	upstreamEvent(t, scanner, "tool_call_confirmation")
	// Old templates bind on first run. Attach v2 only after that admission.
	handle, err := v2.SessionByID(id)
	require.NoError(t, err)
	observation, err := handle.Observe(ctx, runtime.ObserveOptions{})
	require.NoError(t, err)
	defer observation.Cancel()
	require.Equal(t, id, observation.Initial[0].Session.ID)
	require.Len(t, observation.Initial[0].Interactions, 1)
	interaction := observation.Initial[0].Interactions[0]
	require.NotEmpty(t, interaction.InteractionID)
	require.Equal(t, id, interaction.SessionID)

	// Both client generations append to the same mailbox while approval holds
	// the active turn at a deterministic boundary.
	old.json(t, ctx, http.MethodPost, path+"/followup", upstreamMessages("old followup"), nil)
	submission, err := handle.Submit(ctx, runtime.TurnInput{Content: "v2 followup", RequestID: "v2-once"})
	require.NoError(t, err)
	duplicate, err := handle.Submit(ctx, runtime.TurnInput{Content: "v2 followup", RequestID: "v2-once"})
	require.NoError(t, err)
	require.Equal(t, submission.TurnID, duplicate.TurnID)
	old.json(t, ctx, http.MethodPost, path+"/steer", upstreamMessages("old steer"), nil)
	var queue struct {
		Steer struct {
			Depth int `json:"depth"`
		} `json:"steer"`
		Followup struct {
			Depth int `json:"depth"`
		} `json:"followup"`
	}
	old.json(t, ctx, http.MethodGet, path+"/queue", nil, &queue)
	require.Equal(t, 1, queue.Steer.Depth)
	require.Equal(t, 2, queue.Followup.Depth)

	old.json(t, ctx, http.MethodPost, path+"/resume", map[string]string{"confirmation": "approve"}, nil)
	upstreamEvent(t, scanner, "stream_stopped")
	require.NoError(t, handle.AwaitTurn(ctx, submission.TurnID))

	// A stale uncorrelated answer must fail, never hang or approve other work.
	stale := old.request(t, ctx, http.MethodPost, path+"/resume", map[string]string{"confirmation": "approve"})
	defer stale.Body.Close()
	require.Equal(t, http.StatusConflict, stale.StatusCode)
	var snapshot struct {
		ID           string            `json:"id"`
		Messages     []session.Message `json:"messages"`
		LastEventSeq uint64            `json:"last_event_seq"`
	}
	old.json(t, ctx, http.MethodGet, path+"/snapshot", nil, &snapshot)
	require.Equal(t, id, snapshot.ID)
	require.Positive(t, snapshot.LastEventSeq)
	canonical, err := handle.Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, canonical.GetAllMessages(), snapshot.Messages)
	for _, text := range []string{"old run", "old followup", "v2 followup", "old steer"} {
		count := 0
		for _, message := range snapshot.Messages {
			if message.Message.Role == chat.MessageRoleUser && message.Message.Content == text {
				count++
			}
		}
		require.Equal(t, 1, count, "one transcript entry per accepted input: %s", text)
	}
	require.EqualValues(t, 1, toolCalls.Load(), "no second executor for the compatibility stream")

	// The canonical observer saw the same approval with correlation identity.
	for {
		select {
		case event, ok := <-observation.Events:
			require.True(t, ok)
			if resolved, ok := event.Event.(*runtime.InteractionResolvedEvent); ok {
				require.Equal(t, interaction.InteractionID, resolved.InteractionID)
				require.NotEmpty(t, event.InteractionID)
				require.Equal(t, id, event.SessionID)
				return
			}
		case <-ctx.Done():
			t.Fatal("canonical observer missed legacy interaction resolution")
		}
	}
}

func TestHTTPCompatibility_V2CancelSettlesOldRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	provider := &compatibilityProvider{blocked: make(chan struct{})}
	old, v2, _ := compatibilityServer(t, provider)
	id := old.create(t, ctx)
	resp := old.request(t, ctx, http.MethodPost, "/api/sessions/"+id+"/agent/default.yaml/root", upstreamMessages("hold"))
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	select {
	case <-provider.blocked:
	case <-ctx.Done():
		t.Fatal("old run never reached provider")
	}
	handle, err := v2.SessionByID(id)
	require.NoError(t, err)
	status, err := handle.Status(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, status.TurnID)
	_, err = handle.Cancel(ctx, status.TurnID)
	require.NoError(t, err)
	require.NoError(t, handle.AwaitTurn(ctx, status.TurnID))
	upstreamEvent(t, bufio.NewScanner(resp.Body), "stream_stopped")
	require.EqualValues(t, 1, provider.calls.Load())
}

func TestHTTPCompatibility_V2CatalogIsNotLegacyArray(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	old, v2, _ := compatibilityServer(t, &compatibilityProvider{})
	first := old.create(t, ctx)
	second, err := v2.CreateSession(ctx, session.New(), runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	var catalog api.SessionCatalog[runtime.SessionState]
	old.json(t, ctx, http.MethodGet, "/api/v2/sessions?limit=1", nil, &catalog)
	require.Equal(t, 2, catalog.Version)
	require.Len(t, catalog.Sessions, 1)
	require.NotEmpty(t, catalog.NextCursor)
	seen := []string{catalog.Sessions[0].SessionID}
	old.json(t, ctx, http.MethodGet, "/api/v2/sessions?limit=1&cursor="+catalog.NextCursor, nil, &catalog)
	require.Len(t, catalog.Sessions, 1)
	seen = append(seen, catalog.Sessions[0].SessionID)
	require.ElementsMatch(t, []string{first, second.ID()}, seen)
	resp := old.request(t, ctx, http.MethodGet, "/api/v1/sessions", nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotAcceptable, resp.StatusCode, "paused v1 experiment is not an alias")
}

func TestHTTPCompatibility_DisconnectOwnership(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	provider := &compatibilityProvider{blocked: make(chan struct{})}
	old, v2, _ := compatibilityServer(t, provider)
	id := old.create(t, ctx)
	runCtx, disconnectRun := context.WithCancel(ctx)
	defer disconnectRun()
	resp := old.request(t, runCtx, http.MethodPost, "/api/sessions/"+id+"/agent/default.yaml/root", upstreamMessages("owned run"))
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	select {
	case <-provider.blocked:
	case <-ctx.Done():
		t.Fatal("old run never reached provider")
	}
	handle, err := v2.SessionByID(id)
	require.NoError(t, err)
	status, err := handle.Status(ctx)
	require.NoError(t, err)
	turnID := status.TurnID
	require.NotEmpty(t, turnID)

	// GET with no cursor replays the canonical journal as raw old events.
	// Closing that independent observer must not cancel the POST-owned turn.
	observeCtx, disconnectObserver := context.WithCancel(ctx)
	observer := old.request(t, observeCtx, http.MethodGet, "/api/sessions/"+id+"/events", nil)
	require.Equal(t, http.StatusOK, observer.StatusCode)
	upstreamEvent(t, bufio.NewScanner(observer.Body), "stream_started")
	disconnectObserver()
	require.NoError(t, observer.Body.Close())
	// AwaitTurn is a deterministic check that disconnecting the observer has
	// not settled the blocked provider's execution.
	waitCtx, stopWait := context.WithTimeout(ctx, 100*time.Millisecond)
	err = handle.AwaitTurn(waitCtx, turnID)
	stopWait()
	require.Error(t, err)
	require.ErrorIs(t, waitCtx.Err(), context.DeadlineExceeded)
	status, err = handle.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, turnID, status.TurnID)

	disconnectRun()
	require.NoError(t, resp.Body.Close())
	require.NoError(t, handle.AwaitTurn(ctx, turnID), "disconnecting the admitting POST cancels and drains its exact turn")
	require.EqualValues(t, 1, provider.calls.Load())
}

func TestHTTPCompatibility_CommandChangesActiveAgentNotSessionBinding(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	provider := &compatibilityProvider{}
	// This scenario needs an ordinary completion, not the approval fixture's
	// first tool-call response. Subsequent calls remain measurable.
	provider.calls.Store(1)
	old, v2, _ := compatibilityServer(t, provider)
	id := old.create(t, ctx)
	resp := old.request(t, ctx, http.MethodPost, "/api/sessions/"+id+"/agent/default.yaml/root", upstreamMessages("/work inspect this"))
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	upstreamEvent(t, bufio.NewScanner(resp.Body), "stream_stopped")
	handle, err := v2.SessionByID(id)
	require.NoError(t, err)
	snapshot, err := handle.Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, id, snapshot.ID)
	// Session.AgentName is intentionally json:"-"; the canonical snapshot
	// transports the active agent in status, not the persisted session object.
	var wireSnapshot struct {
		Status struct {
			AgentName string `json:"agent_name"`
		} `json:"status"`
	}
	old.json(t, ctx, http.MethodGet, "/api/v2/sessions/"+id+"/snapshot", nil, &wireSnapshot)
	require.Equal(t, "worker", wireSnapshot.Status.AgentName, "command changes the active agent in this session")
	require.Equal(t, "root", snapshot.AttributesSnapshot()[runtime.SessionAgentAttribute], "initial binding remains immutable")
	status, err := handle.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, "worker", status.AgentName)
	messages := snapshot.GetAllMessages()
	count := 0
	for _, message := range messages {
		require.NotEqual(t, "/work inspect this", message.Message.Content, "unresolved command must not be appended alongside its expansion")
		if message.Message.Role == chat.MessageRoleUser && message.Message.Content == "inspect this" {
			count++
		}
	}
	require.Equal(t, 1, count, "resolved command input is appended exactly once (upstream forwards trailing arguments)")
	var listed []struct {
		ID string `json:"id"`
	}
	old.json(t, ctx, http.MethodGet, "/api/sessions", nil, &listed)
	require.Len(t, listed, 1, "in-session active-agent switch must not create a fork")
	require.Equal(t, id, listed[0].ID)
	require.EqualValues(t, 2, provider.calls.Load(), "one provider call for the command run")
}
