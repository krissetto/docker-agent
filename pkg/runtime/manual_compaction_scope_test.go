package runtime

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/telemetry/genai"
	"github.com/docker/docker-agent/pkg/tools"
)

// callbackScope records what a manual compaction's hook or model call can reach.
type callbackScope struct {
	scoped         bool
	elicitation    bool
	sampling       bool
	conversationID string
}

func scopeOf(ctx context.Context) callbackScope {
	scope, ok := tools.HandlerScopeFrom(ctx)
	return callbackScope{
		scoped:         ok,
		elicitation:    scope.Elicitation != nil,
		sampling:       scope.Sampling != nil && scope.SamplingWithTools != nil,
		conversationID: genai.ConversationIDFromContext(ctx),
	}
}

type scopeRecorder struct {
	mu     sync.Mutex
	scopes map[string]callbackScope
}

func (r *scopeRecorder) record(name string, ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.scopes == nil {
		r.scopes = map[string]callbackScope{}
	}
	r.scopes[name] = scopeOf(ctx)
}

func (r *scopeRecorder) get(name string) (callbackScope, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	scope, ok := r.scopes[name]
	return scope, ok
}

// scopedCompactor is a native compactor that records its call's scope.
type scopedCompactor struct {
	*mockCompactor

	rec *scopeRecorder
}

func (c *scopedCompactor) CompactConversation(ctx context.Context, messages []chat.Message, requestTools []tools.Tool, instructions string) (*chat.CompactionResult, error) {
	c.rec.record("native", ctx)
	return c.mockCompactor.CompactConversation(ctx, messages, requestTools, instructions)
}

const scopeHook = "test-manual-compaction-scope"

// nativeScopeRuntime runs native compaction with a before_compaction hook, both
// recording the callback scope they see.
func nativeScopeRuntime(t *testing.T) (*LocalRuntime, *scopeRecorder) {
	t.Helper()
	rec := &scopeRecorder{}
	comp := &scopedCompactor{mockCompactor: &mockCompactor{id: "anthropic/claude-test", opts: nativeOpts, result: nativeResult()}, rec: rec}
	root := agent.New("root", "system instruction", agent.WithModel(comp), agent.WithHooks(&latest.HooksConfig{
		BeforeCompaction: []latest.HookDefinition{{Type: "builtin", Command: scopeHook}},
	}))
	rt := newNativeRuntime(t, root)
	require.NoError(t, rt.hooksRegistry.RegisterBuiltin(scopeHook, func(ctx context.Context, _ *hooks.Input, _ []string) (*hooks.Output, error) {
		rec.record("hook", ctx)
		return nil, nil
	}))
	return rt, rec
}

// scopeTestSession is nativeTestSession's transcript under a fresh ID, so
// repeated and parallel runs never share a session owner.
func scopeTestSession() *session.Session {
	return session.New(session.WithMessages(nativeTestSession().Messages))
}

func assertTurnScope(t *testing.T, rec *scopeRecorder, name, sessionID string) {
	t.Helper()
	got, ok := rec.get(name)
	require.True(t, ok, "%s was not called", name)
	assert.Equal(t, callbackScope{scoped: true, elicitation: true, sampling: true, conversationID: sessionID}, got, "%s runs with the runtime's callback scope", name)
}

// Summarize is not a turn, but its hooks and native compaction may start tools,
// so they get the callback scope a turn gives them.
func TestSummarizeRunsHooksAndNativeCompactionInHandlerScope(t *testing.T) {
	t.Parallel()
	rt, rec := nativeScopeRuntime(t)
	sess := scopeTestSession()

	events := make(chan Event, 64)
	rt.Summarize(t.Context(), sess, "", NewChannelSink(events))

	assertTurnScope(t, rec, "hook", sess.ID)
	assertTurnScope(t, rec, "native", sess.ID)
	assert.Contains(t, sess.Messages[len(sess.Messages)-1].Summary, "native summary")
}

// The live-session path (/compact on a settled session) runs the compaction on
// a worker outside any turn; it gets the same scope.
func TestLiveCompactionRunsHooksAndNativeCompactionInHandlerScope(t *testing.T) {
	t.Parallel()
	rt, rec := nativeScopeRuntime(t)
	sess := scopeTestSession()
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "root"})
	require.NoError(t, err)

	events := make(chan Event, 64)
	require.NoError(t, handle.Compact(t.Context(), "", NewChannelSink(events)))
	handle.(*sessionHandle).driver.Wait()

	assertTurnScope(t, rec, "hook", sess.ID)
	assertTurnScope(t, rec, "native", sess.ID)
}

// A compaction that already runs inside a turn keeps that turn's scope.
func TestManualCompactionKeepsAnExistingScope(t *testing.T) {
	t.Parallel()
	rt, rec := nativeScopeRuntime(t)
	sess := scopeTestSession()
	// The caller's (here empty) scope stands in for a running turn's.
	ctx := tools.WithHandlerScope(t.Context(), tools.HandlerScope{})

	events := make(chan Event, 64)
	rt.Summarize(ctx, sess, "", NewChannelSink(events))

	got, ok := rec.get("hook")
	require.True(t, ok)
	assert.True(t, got.scoped)
	assert.False(t, got.elicitation, "the caller's scope is kept, not replaced")
}
