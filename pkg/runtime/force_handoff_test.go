package runtime

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// handoffRecordingProvider wraps mockProvider to count model invocations and
// capture the messages of the most recent call, so tests can assert
// whether and with what context a forced-handoff target was invoked.
type handoffRecordingProvider struct {
	mockProvider

	mu       sync.Mutex
	calls    int
	lastMsgs []chat.Message
}

func (p *handoffRecordingProvider) CreateChatCompletionStream(ctx context.Context, msgs []chat.Message, t []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	p.calls++
	p.lastMsgs = append([]chat.Message(nil), msgs...)
	p.mu.Unlock()
	return p.mockProvider.CreateChatCompletionStream(ctx, msgs, t)
}

func (p *handoffRecordingProvider) handoffCallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *handoffRecordingProvider) lastMessages() []chat.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]chat.Message(nil), p.lastMsgs...)
}

// forceHandoffTeam builds a two-agent team where root force-hands off to
// summarizer, plus a runtime wired with the usual test doubles.
func forceHandoffTeam(t *testing.T, rootStream, summarizerStream *mockStream) (*LocalRuntime, *handoffRecordingProvider) {
	t.Helper()

	rootProv := &mockProvider{id: "test/mock-model", stream: rootStream}
	sumProv := &handoffRecordingProvider{mockProvider: mockProvider{id: "test/mock-model", stream: summarizerStream}}

	summarizer := agent.New("summarizer", "You summarize", agent.WithModel(sumProv))
	root := agent.New("root", "You extract", agent.WithModel(rootProv), agent.WithForceHandoff(summarizer))
	tm := team.New(team.WithAgents(root, summarizer))

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	return rt, sumProv
}

// TestForceHandoff_RoutesToTargetOnNaturalStop pins the core contract of
// force_handoff: when the first agent produces a final response, the
// runtime deterministically routes the conversation to the configured
// target — no LLM tool call involved — and the target runs in the same
// session with the carried-over context.
func TestForceHandoff_RoutesToTargetOnNaturalStop(t *testing.T) {
	t.Parallel()

	rt, sumProv := forceHandoffTeam(t,
		newStreamBuilder().AddContent("extracted facts").AddStopWithUsage(10, 5).Build(),
		newStreamBuilder().AddContent("final summary").AddStopWithUsage(10, 5).Build(),
	)

	sess := session.New(session.WithUserMessage("Summarize this article"))
	sess.Title = "Unit Test"

	var events []Event
	for ev := range rt.runExecution(t.Context(), sess) {
		events = append(events, ev)
	}

	for _, ev := range events {
		errEv, isErr := ev.(*ErrorEvent)
		require.False(t, isErr, "unexpected error event: %+v", errEv)
	}

	assert.Equal(t, "summarizer", sess.AgentName, "session must be pinned to the force_handoff target after the run")
	assert.Equal(t, 1, sumProv.handoffCallCount(), "target agent's model must be invoked exactly once")
	assert.Equal(t, "final summary", sess.GetLastAssistantMessageContent())

	// The runtime injects an implicit user message so the target's model
	// call doesn't start on a dangling assistant message.
	var implicitHandoffMsg bool
	for _, m := range sess.GetAllMessages() {
		if m.Implicit && m.Message.Role == chat.MessageRoleUser && strings.Contains(m.Message.Content, "automatically handed off") {
			implicitHandoffMsg = true
		}
	}
	assert.True(t, implicitHandoffMsg, "implicit handoff user message must be recorded in the session")

	// The target agent must see the previous agent's output (carried-over
	// context) and the handoff notice in its prompt.
	prompt := sumProv.lastMessages()
	var sawExtracted, sawNotice bool
	for _, m := range prompt {
		if m.Role == chat.MessageRoleAssistant && strings.Contains(m.Content, "extracted facts") {
			sawExtracted = true
		}
		if m.Role == chat.MessageRoleUser && strings.Contains(m.Content, "automatically handed off") {
			sawNotice = true
		}
	}
	assert.True(t, sawExtracted, "target agent must see the previous agent's final response")
	assert.True(t, sawNotice, "target agent must see the handoff notice")
}

// TestForceHandoff_UpdatesPinnedSession documents session-v2 force handoff:
// session identity moves to the configured target and cannot loop on stale
// process-global state.
func TestForceHandoff_UpdatesPinnedSession(t *testing.T) {
	t.Parallel()

	rt, sumProv := forceHandoffTeam(t,
		newStreamBuilder().AddContent("done").AddStopWithUsage(10, 5).Build(),
		newStreamBuilder().AddContent("should never run").AddStopWithUsage(10, 5).Build(),
	)

	sess := session.New(session.WithUserMessage("hi"), session.WithAgentName("root"))
	sess.Title = "Unit Test"

	for range rt.runExecution(t.Context(), sess) {
	}

	assert.Equal(t, 1, sumProv.handoffCallCount(), "force_handoff target runs once")
	assert.Equal(t, "summarizer", sess.AgentName)
	assert.Equal(t, "should never run", sess.GetLastAssistantMessageContent())
}
