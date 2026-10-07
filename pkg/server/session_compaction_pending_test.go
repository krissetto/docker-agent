package server

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
)

type pendingCompactionHTTPProvider struct {
	sessionHTTPProvider
	mu    sync.Mutex
	calls [][]chat.Message
}

func (*pendingCompactionHTTPProvider) BaseConfig() base.Config {
	return base.Config{ModelConfig: latest.ModelConfig{ProviderOpts: map[string]any{"context_size": 100_000}}}
}

func (p *pendingCompactionHTTPProvider) CreateChatCompletionStream(_ context.Context, messages []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	p.calls = append(p.calls, append([]chat.Message(nil), messages...))
	p.mu.Unlock()
	return &sessionHTTPStream{}, nil
}

func (p *pendingCompactionHTTPProvider) contents() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.calls))
	for i, call := range p.calls {
		for _, message := range call {
			out[i] += message.Content + "\n"
		}
	}
	return out
}

func TestRemoteCompactionPreservesDormantPendingInputs(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "owned.db")
	store, err := sqlitestore.New(ctx, path)
	require.NoError(t, err)
	root := session.New(session.WithID("compact-root"), session.WithAgentName("root"), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "root"}))
	root.AddMessage(session.UserMessage("settled history"))
	root.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "settled answer"}))
	pendingContents := []string{"queued user unique", "child guidance unique", "child report unique"}
	for i, origin := range []session.InputOrigin{session.InputOriginUser, session.InputOriginAgent, session.InputOriginRuntime} {
		message := session.UserMessage(pendingContents[i])
		message.Accepted, message.Pending, message.TurnID = true, true, pendingContents[i]
		message.InputOrigin, message.InputMode = origin, "submit"
		if origin != session.InputOriginUser {
			message.SenderID, message.SenderName, message.InputMode = "child-node", "worker", "steer"
		}
		if origin == session.InputOriginRuntime {
			message.ReportOutcome = session.ReportOutcomeFinished
		}
		root.AddMessage(message)
	}
	require.NoError(t, store.AddSession(ctx, root))
	child := session.New(session.WithID("compact-child"), session.WithParentID(root.ID), session.WithAgentName("worker"), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "worker"}))
	message := session.UserMessage("dormant child unique")
	message.Accepted, message.Pending, message.TurnID = true, true, "child-turn"
	child.AddMessage(message)
	node := subagent.Node{ID: "child-node", Parent: subagent.SessionRootID(root.ID), SessionID: child.ID, Agent: "worker", State: subagent.NodeIdle}
	require.NoError(t, store.(session.CoordinationStore).AdmitChild(ctx, session.ChildAdmission{Child: child, Record: session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: node}}))
	require.NoError(t, store.Close())
	store, err = sqlitestore.New(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	rootProvider, childProvider := &pendingCompactionHTTPProvider{}, &pendingCompactionHTTPProvider{}
	srv, owner := newCanonicalLocalServer(t, store,
		agent.New("root", "prompt", agent.WithModel(rootProvider), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
		agent.New("worker", "prompt", agent.WithModel(childProvider)),
	)
	httpServer := httptest.NewServer(srv.e)
	defer httpServer.Close()
	client, err := runtime.NewClient(httpServer.URL, runtime.WithHTTPClient(httpServer.Client()))
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	remote, err := transport.SessionByID(root.ID)
	require.NoError(t, err)
	require.NoError(t, remote.(runtime.Hydrator).Hydrate(ctx))
	observation, err := remote.Observe(ctx, runtime.ObserveOptions{})
	require.NoError(t, err)
	defer observation.Cancel()
	require.True(t, observation.Primary().Status.Dormant)
	require.Len(t, observation.Primary().PendingInputs, 3)
	require.NoError(t, remote.Compact(ctx, "focus on settled history", nil))
	completed := false
	for !completed {
		select {
		case envelope, open := <-observation.Events:
			require.True(t, open, "observation closed before compaction completion")
			if event, ok := envelope.Event.(*runtime.SessionCompactionEvent); ok && event.Status == "completed" {
				require.Equal(t, runtime.CompactionOutcomeApplied, event.Outcome)
				completed = true
			}
		case <-ctx.Done():
			t.Fatal("compaction terminal event missing")
		}
	}
	calls := rootProvider.contents()
	require.Len(t, calls, 1)
	for _, content := range pendingContents {
		assert.NotContains(t, calls[0], content, "pending input must not enter the summary request")
	}
	local, err := owner.Runtime().SessionByID(root.ID)
	require.NoError(t, err)
	for _, handle := range []runtime.SessionHandle{local, remote} {
		status, err := handle.Status(ctx)
		require.NoError(t, err)
		assert.True(t, status.Dormant)
		assert.Equal(t, 3, status.Pending)
		assert.Empty(t, status.TurnID)
		snapshot, err := handle.Snapshot(ctx)
		require.NoError(t, err)
		assert.Equal(t, "HTTP integration reply", snapshot.LastSummary())
	}
	stored, err := store.GetSession(ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, "HTTP integration reply", stored.LastSummary())
	var retained []string
	for _, item := range stored.MessagesSnapshot() {
		if item.Message != nil && item.Message.Pending {
			retained = append(retained, item.Message.Message.Content)
		}
	}
	assert.Equal(t, pendingContents, retained)
	childHandle, err := owner.Runtime().SessionByID(child.ID)
	require.NoError(t, err)
	childStatus, err := childHandle.Status(ctx)
	require.NoError(t, err)
	assert.True(t, childStatus.Dormant)
	assert.Equal(t, 1, childStatus.Pending)
	assert.Empty(t, childProvider.contents(), "compaction cannot authorize dormant descendants")

	resumed, err := remote.Submit(ctx, runtime.TurnInput{Content: "explicit root resume", RequestID: "resume"})
	require.NoError(t, err)
	require.NoError(t, remote.AwaitTurn(ctx, resumed.TurnID))
	calls = rootProvider.contents()
	require.Greater(t, len(calls), 1)
	last := calls[len(calls)-1]
	previous := -1
	for _, content := range append(pendingContents, "explicit root resume") {
		index := strings.Index(last, content)
		require.Greater(t, index, previous, "pending input must enter the resumed prompt once in FIFO order: %q", content)
		assert.Equal(t, 1, strings.Count(last, content))
		previous = index
	}
	assert.Empty(t, childProvider.contents(), "root admission cannot wake dormant descendants")
}
