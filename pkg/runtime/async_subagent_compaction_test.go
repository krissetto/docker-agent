package runtime

import (
	"context"
	"path/filepath"
	"slices"
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
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type asyncCompactionRecordingProvider struct {
	mu      sync.Mutex
	streams []chat.MessageStream
	calls   [][]chat.Message
}

func (p *asyncCompactionRecordingProvider) ID() modelsdev.ID {
	return modelsdev.ParseIDOrZero("test/async-compaction")
}

func (p *asyncCompactionRecordingProvider) CreateChatCompletionStream(_ context.Context, messages []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, slices.Clone(messages))
	if len(p.streams) == 0 {
		return newStreamBuilder().AddContent("restored reply").AddStopWithUsage(5, 2).Build(), nil
	}
	stream := p.streams[0]
	p.streams = p.streams[1:]
	return stream, nil
}

func (p *asyncCompactionRecordingProvider) BaseConfig() base.Config { return base.Config{} }
func (p *asyncCompactionRecordingProvider) MaxTokens() int          { return 0 }

func (p *asyncCompactionRecordingProvider) snapshotCalls() [][]chat.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

func asyncCompactionTeam(p *asyncCompactionRecordingProvider) *team.Team {
	root := agent.New("root", "root prompt", agent.WithModel(&mockProvider{
		id:     "test/root",
		stream: newStreamBuilder().AddContent("root reply").AddStopWithUsage(5, 2).Build(),
	}), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"}))
	worker := agent.New("worker", "worker prompt",
		agent.WithModel(p),
		agent.WithCompactionThreshold(0.10),
	)
	return team.New(team.WithAgents(root, worker))
}

func waitForAsyncChildIdle(t *testing.T, rt *LocalRuntime, id subagent.NodeID) {
	t.Helper()
	require.Eventually(t, func() bool {
		rec, ok := rt.subagents.Read(id)
		if !ok || rec.state != subagent.NodeIdle {
			return false
		}
		handle, err := rt.SessionByID(rec.sessionID)
		if err != nil {
			return false
		}
		status, err := handle.Status(t.Context())
		return err == nil && status.State == SessionStateSettled && status.Pending == 0
	}, 10*time.Second, 10*time.Millisecond)
}

func containsSessionSummary(messages []chat.Message) bool {
	for _, message := range messages {
		if message.Role == chat.MessageRoleUser && len(message.Content) >= len("Session Summary: ") && message.Content[:len("Session Summary: ")] == "Session Summary: " {
			return true
		}
	}
	return false
}

// This exercises automatic threshold compaction on the runtime-owned driver
// used by an async subagent, then proves both an idle wake and a restored wake
// receive the generated summary in their actual model input.
func TestAsyncSubagentAutomaticCompactionSurvivesWakeAndReload(t *testing.T) {
	// The fixed async harness is itself part of the compaction prompt. Keep the
	// synthetic window comfortably above that prompt while the low threshold and
	// recorded usage still force the behavior under test.
	const contextLimit = 4_000

	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	storeA, err := sqlitestore.New(t.Context(), dbPath)
	require.NoError(t, err)

	providerA := &asyncCompactionRecordingProvider{streams: []chat.MessageStream{
		newStreamBuilder().AddContent("initial child reply").AddStopWithUsage(600, 5).Build(),
		newStreamBuilder().AddContent("durable compacted context").AddStopWithUsage(20, 5).Build(),
		newStreamBuilder().AddContent("follow-up reply").AddStopWithUsage(10, 3).Build(),
	}}
	rtA, err := NewLocalRuntime(t.Context(), asyncCompactionTeam(providerA),
		WithSessionCompaction(true),
		WithModelStore(mockModelStoreWithLimit{limit: contextLimit}),
		WithSessionStore(storeA),
	)
	require.NoError(t, err)

	rootSession := session.New(session.WithID("async-compaction-root"))
	require.NoError(t, storeA.AddSession(t.Context(), rootSession))
	longTask := "retain this task context: " + strings.Repeat("x", 1_200)
	id, err := rtA.subagents.Spawn(rootSession, "root", subagent.AllowedSubagent{Agent: "worker"}, longTask)
	require.NoError(t, err)
	waitForAsyncChildIdle(t, rtA, id)

	info, ok := rtA.SubagentAttachInfo(id)
	require.True(t, ok)
	childSession, err := rtA.SessionByID(info.Session.ID)
	require.NoError(t, err)
	childObservation, err := childSession.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer childObservation.Cancel()

	_, err = childSession.Submit(t.Context(), TurnInput{Content: "use the retained context"})
	require.NoError(t, err)
	waitForAsyncChildIdle(t, rtA, id)

	var sawStarted, sawSummary, sawApplied bool
	drainDeadline := time.After(2 * time.Second)
	for !sawStarted || !sawSummary || !sawApplied {
		select {
		case envelope := <-childObservation.Events:
			event := envelope.Event
			switch e := event.(type) {
			case *SessionCompactionEvent:
				sawStarted = sawStarted || e.Status == "started"
				sawApplied = sawApplied || (e.Status == "completed" && e.Outcome == CompactionOutcomeApplied)
			case *SessionSummaryEvent:
				sawSummary = e.Summary == "durable compacted context"
			}
		case <-drainDeadline:
			t.Fatalf("missing child compaction events (started=%v summary=%v applied=%v)", sawStarted, sawSummary, sawApplied)
		}
	}

	callsA := providerA.snapshotCalls()
	require.Len(t, callsA, 3, "initial child turn, summary call, and idle wake")
	assert.False(t, containsSessionSummary(callsA[0]))
	assert.True(t, containsSessionSummary(callsA[2]), "the idle wake must consume the generated summary")
	snapshot, err := childSession.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "durable compacted context", snapshot.LastSummary())

	// A root driver follows the same pre-turn threshold path; this guards the
	// comparison against accidentally validating a child-only test double.
	rootProbe := session.New(session.WithID("root-probe"))
	rootProbe.InputTokens = contextLimit - 50
	rootHandle, err := rtA.CreateSession(t.Context(), rootProbe, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	rootObservation, err := rootHandle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer rootObservation.Cancel()
	rootSubmission, err := rootHandle.Submit(t.Context(), TurnInput{Content: "probe"})
	require.NoError(t, err)
	var rootStarted bool
	for envelope := range rootObservation.Events {
		if envelope.TurnID != rootSubmission.TurnID {
			continue
		}
		if e, ok := envelope.Event.(*SessionCompactionEvent); ok && e.Status == "started" {
			rootStarted = true
		}
		if _, stopped := envelope.Event.(*StreamStoppedEvent); stopped {
			break
		}
	}
	assert.True(t, rootStarted)

	require.NoError(t, rtA.shutdownSessions(t.Context()))
	require.NoError(t, storeA.(*session.SQLiteSessionStore).Close())

	storeB, err := sqlitestore.New(t.Context(), dbPath)
	require.NoError(t, err)
	defer storeB.(*session.SQLiteSessionStore).Close()
	providerB := &asyncCompactionRecordingProvider{}
	rtB, err := NewLocalRuntime(t.Context(), asyncCompactionTeam(providerB),
		WithSessionCompaction(true),
		WithModelStore(mockModelStoreWithLimit{limit: 1_000}),
		WithSessionStore(storeB),
	)
	require.NoError(t, err)
	defer rtB.Close()

	loadedRoot, err := storeB.GetSession(t.Context(), rootSession.ID)
	require.NoError(t, err)
	_, err = rtB.RestoreSubagentTree(t.Context(), loadedRoot)
	require.NoError(t, err)
	restored, ok := rtB.SubagentAttachInfo(id)
	require.True(t, ok)
	assert.Equal(t, "durable compacted context", restored.Session.LastSummary())

	restoredSession, err := rtB.SessionByID(restored.Session.ID)
	require.NoError(t, err)
	_, err = restoredSession.Submit(t.Context(), TurnInput{Content: "continue after reload"})
	require.NoError(t, err)
	waitForAsyncChildIdle(t, rtB, id)
	callsB := providerB.snapshotCalls()
	require.Len(t, callsB, 1)
	assert.True(t, containsSessionSummary(callsB[0]), "the restored child must consume the persisted summary")
}
