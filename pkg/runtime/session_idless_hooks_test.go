package runtime

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// idlessHookProbe records each user_prompt_submit invocation and what the
// durable store and the provider had seen when it fired.
type idlessHookProbe struct {
	mu       sync.Mutex
	prompts  []string
	storedAt []int
	modelAt  []int
	deny     bool
}

const idlessHookName = "test-idless-user-prompt-submit"

// idlessHookRuntimeOn builds a runtime over store whose user_prompt_submit hook
// feeds probe. A restart reuses the store, probe, and provider.
func idlessHookRuntimeOn(t *testing.T, store session.Store, prov *idlessProvider, probe *idlessHookProbe) *LocalRuntime {
	t.Helper()
	tm := team.New(team.WithAgents(agent.New("root", "prompt",
		agent.WithModel(prov),
		agent.WithHooks(&latest.HooksConfig{UserPromptSubmit: []latest.HookDefinition{{Type: "builtin", Command: idlessHookName}}}),
	)))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })
	require.NoError(t, rt.hooksRegistry.RegisterBuiltin(idlessHookName, func(ctx context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
		stored := idlessStoredUsersCount(ctx, store, in.SessionID)
		calls, _ := prov.modelInputs()
		probe.mu.Lock()
		defer probe.mu.Unlock()
		probe.prompts = append(probe.prompts, in.Prompt)
		probe.storedAt = append(probe.storedAt, stored)
		probe.modelAt = append(probe.modelAt, len(calls))
		if probe.deny {
			return &hooks.Output{Decision: hooks.DecisionBlockValue, Reason: "denied by test"}, nil
		}
		return nil, nil
	}))
	return rt
}

func idlessHookRuntime(t *testing.T, deny bool) (*sessionDriver, *idlessProvider, *idlessHookProbe, session.Store) {
	t.Helper()
	probe := &idlessHookProbe{deny: deny}
	store := session.NewInMemorySessionStore()
	prov := &idlessProvider{mockProvider: &mockProvider{id: "test/mock-model"}}
	rt := idlessHookRuntimeOn(t, store, prov, probe)
	sess := session.New(session.WithID(t.Name() + "/session"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	return rt.sessionDrivers.Get(sess), prov, probe, store
}

func idlessStoredUsersCount(ctx context.Context, store session.Store, id string) int {
	stored, err := store.GetSession(ctx, id)
	if err != nil {
		return -1
	}
	n := 0
	for _, item := range stored.GetAllMessages() {
		if item.Message.Role == chat.MessageRoleUser {
			n++
		}
	}
	return n
}

// idlessStoredUserFlags describes each durable user row's state flags, so the
// ID and ID-less rows can be compared without comparing their identities.
func idlessStoredUserFlags(t *testing.T, store session.Store, id string) []string {
	t.Helper()
	stored, err := store.GetSession(t.Context(), id)
	require.NoError(t, err)
	var flags []string
	for _, item := range stored.GetAllMessages() {
		if item.Message.Role == chat.MessageRoleUser {
			flags = append(flags, fmt.Sprintf("pending=%t accepted=%t implicit=%t", item.Pending, item.Accepted, item.Implicit))
		}
	}
	return flags
}

func (p *idlessHookProbe) snapshot() (prompts []string, storedAt, modelAt []int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.prompts...), append([]int(nil), p.storedAt...), append([]int(nil), p.modelAt...)
}

func TestIDlessUserPromptSubmitFiresOncePerHeadInFIFOOrder(t *testing.T) {
	d, prov, probe, store := idlessHookRuntime(t, false)
	require.True(t, d.Post(t.Context(), QueuedMessage{Content: "first"}, true))
	require.True(t, d.Post(t.Context(), QueuedMessage{Content: "second"}, true))
	d.Wait()

	prompts, _, _ := probe.snapshot()
	assert.Equal(t, []string{"first", "second"}, prompts, "the hook fires exactly once per head, in FIFO order")
	inputs, overrun := prov.modelInputs()
	assert.False(t, overrun)
	assert.Equal(t, []string{"first", "second"}, inputs)
	assert.Equal(t, 2, idlessStoredUsersCount(t.Context(), store, d.sessionID()))
}

// A denied head has already been appended when the hook fires, on both the ID
// path and the ID-less path. The deny stops the run before any model call.
func TestIDlessUserPromptSubmitDenyBlocksModelCall(t *testing.T) {
	d, prov, probe, store := idlessHookRuntime(t, true)
	require.True(t, d.Post(t.Context(), QueuedMessage{Content: "first"}, true))
	d.Wait()
	idlessFlags := idlessStoredUserFlags(t, store, d.sessionID())

	prompts, storedAt, modelAt := probe.snapshot()
	assert.Equal(t, []string{"first"}, prompts)
	assert.Equal(t, []int{1}, storedAt, "the head is appended before the hook, as on the ID path")
	assert.Equal(t, []int{0}, modelAt)
	inputs, overrun := prov.modelInputs()
	assert.False(t, overrun)
	assert.Empty(t, inputs, "a denied prompt never reaches the model")
	assert.Equal(t, 1, idlessStoredUsersCount(t.Context(), store, d.sessionID()))
	t.Logf("IDLESS-ROW-FLAGS %v", idlessFlags)
}

// A denied head that was persisted must restart as a settled, non-pending row:
// no hook re-run, no model call, and no second durable copy.
func TestIDlessDeniedHeadRestartDoesNotRerunOrDuplicate(t *testing.T) {
	probe := &idlessHookProbe{deny: true}
	store := session.NewInMemorySessionStore()
	prov := &idlessProvider{mockProvider: &mockProvider{id: "test/mock-model"}}
	rt := idlessHookRuntimeOn(t, store, prov, probe)
	sess := session.New(session.WithID(t.Name() + "/session"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	d := rt.sessionDrivers.Get(sess)
	require.True(t, d.Post(t.Context(), QueuedMessage{Content: "first"}, true))
	d.Wait()
	require.NoError(t, rt.Close())

	restarted := idlessHookRuntimeOn(t, store, prov, probe)
	reloaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	rd := restarted.sessionDrivers.Get(reloaded)
	assert.False(t, rd.WakePending(), "a settled denied head is not wakeable after restart")
	rd.Wait()

	prompts, _, _ := probe.snapshot()
	assert.Equal(t, []string{"first"}, prompts, "the hook does not re-run after restart")
	inputs, overrun := prov.modelInputs()
	assert.False(t, overrun)
	assert.Empty(t, inputs, "no model call after restart")
	assert.Equal(t, []string{"pending=false accepted=false implicit=false"}, idlessStoredUserFlags(t, store, sess.ID))
	assert.Equal(t, 1, idlessStoredUsersCount(t.Context(), store, sess.ID), "no duplicate durable row")
}

func TestIDUserPromptSubmitDenyOrderMatchesIDLess(t *testing.T) {
	d, prov, probe, store := idlessHookRuntime(t, true)
	require.True(t, d.Post(t.Context(), QueuedMessage{Content: "first", RequestID: "turn-first"}, true))
	d.Wait()

	prompts, storedAt, modelAt := probe.snapshot()
	assert.Equal(t, []string{"first"}, prompts)
	assert.Equal(t, []int{1}, storedAt)
	assert.Equal(t, []int{0}, modelAt)
	inputs, _ := prov.modelInputs()
	assert.Empty(t, inputs)
	assert.Equal(t, 1, idlessStoredUsersCount(t.Context(), store, d.sessionID()))
	t.Logf("ID-ROW-FLAGS %v", idlessStoredUserFlags(t, store, d.sessionID()))
}

func TestMixedIDAndIDlessHooksKeepFIFOOrder(t *testing.T) {
	d, prov, probe, _ := idlessHookRuntime(t, false)
	require.True(t, d.Post(t.Context(), QueuedMessage{Content: "A", RequestID: "turn-a"}, true))
	require.True(t, d.Post(t.Context(), QueuedMessage{Content: "B"}, true))
	require.True(t, d.Post(t.Context(), QueuedMessage{Content: "C", RequestID: "turn-c"}, true))
	d.Wait()

	prompts, _, _ := probe.snapshot()
	assert.Equal(t, []string{"A", "B", "C"}, prompts)
	inputs, _ := prov.modelInputs()
	assert.Equal(t, []string{"A", "B", "C"}, inputs)
}

// TestIDlessHeldStartKeepsLaterPostsBehindItsHead holds A's run after its start
// is published but before its drive goroutine runs the ID-less promotion. The
// OnStarted callbacks run in exactly that window: Post calls them after
// prepareStart returns and before startWake launches the drive, so no new test
// hook is needed. B (ID-less) and C (ID'd) are posted and return inside the
// hold. A's run must not absorb B; B and C run after A, in FIFO order.
func TestIDlessHeldStartKeepsLaterPostsBehindItsHead(t *testing.T) {
	d, prov, probe, _ := idlessHookRuntime(t, false)
	entered, release := make(chan struct{}), make(chan struct{})
	var holdOnce sync.Once
	unwatch := d.OnStarted(func() {
		holdOnce.Do(func() {
			close(entered)
			<-release
		})
	})
	t.Cleanup(unwatch)
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)

	posted := make(chan bool, 1)
	go func() { posted <- d.Post(t.Context(), QueuedMessage{Content: "A", RequestID: "turn-a"}, true) }()
	<-entered
	require.True(t, d.Post(t.Context(), QueuedMessage{Content: "B"}, true))
	require.True(t, d.Post(t.Context(), QueuedMessage{Content: "C", RequestID: "turn-c"}, true))
	releaseOnce()
	require.True(t, <-posted)
	d.Wait()

	prompts, _, _ := probe.snapshot()
	assert.Equal(t, []string{"A", "B", "C"}, prompts, "A's run must not promote B")
	inputs, overrun := prov.modelInputs()
	assert.False(t, overrun)
	assert.Equal(t, []string{"A", "B", "C"}, inputs)
	users, _ := idlessCanonical(t, d)
	assert.Equal(t, []string{"A", "B", "C"}, users)
}
