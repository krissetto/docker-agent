package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// idlessMaxModelCalls bounds the fake provider: a runaway loop fails the test
// after this many model calls instead of spinning until the test times out.
const idlessMaxModelCalls = 4

// idlessProvider records the newest user message each model call receives.
type idlessProvider struct {
	*mockProvider

	mu       sync.Mutex
	lastUser []string
	overrun  bool
}

func (p *idlessProvider) CreateChatCompletionStream(_ context.Context, msgs []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.lastUser) >= idlessMaxModelCalls {
		p.overrun = true
		return nil, errors.New("idless test: model call bound exceeded")
	}
	last := ""
	for _, msg := range msgs {
		if msg.Role == chat.MessageRoleUser {
			last = msg.Content
		}
	}
	p.lastUser = append(p.lastUser, last)
	return newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build(), nil
}

// modelInputs returns the newest user content of each model call and whether
// the bound was exceeded.
func (p *idlessProvider) modelInputs() ([]string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.lastUser...), p.overrun
}

func idlessRuntime(t *testing.T, store session.Store) (*sessionDriver, *idlessProvider, *session.Session) {
	t.Helper()
	prov := &idlessProvider{mockProvider: &mockProvider{id: "test/mock-model"}}
	tm := team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(prov))))
	if store == nil {
		store = session.NewInMemorySessionStore()
	}
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionStore(store))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })
	sess := session.New(session.WithID(t.Name() + "/session"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	return rt.sessionDrivers.Get(sess), prov, sess
}

// idlessCanonical returns the user and assistant rows of the owner transcript.
func idlessCanonical(t *testing.T, d *sessionDriver) (users, assistants []string) {
	t.Helper()
	snap, err := d.ownerSnapshot(t.Context())
	require.NoError(t, err)
	for _, item := range snap.GetAllMessages() {
		switch item.Message.Role {
		case chat.MessageRoleUser:
			users = append(users, item.Message.Content)
		case chat.MessageRoleAssistant:
			assistants = append(assistants, item.Message.Content)
		}
	}
	return users, assistants
}

// idlessStoredUsers returns the durable user rows, so duplicates are caught at the store.
func idlessStoredUsers(t *testing.T, store session.Store, id string) []string {
	t.Helper()
	stored, err := store.GetSession(t.Context(), id)
	require.NoError(t, err)
	var users []string
	for _, item := range stored.GetAllMessages() {
		if item.Message.Role == chat.MessageRoleUser {
			users = append(users, item.Message.Content)
		}
	}
	return users
}

func TestIDlessPostsReachModelInFIFOOrder(t *testing.T) {
	store := session.NewInMemorySessionStore()
	d, prov, _ := idlessRuntime(t, store)
	require.True(t, d.Post(t.Context(), QueuedMessage{Content: "first"}, true))
	require.True(t, d.Post(t.Context(), QueuedMessage{Content: "second"}, true))
	d.Wait()

	inputs, overrun := prov.modelInputs()
	assert.False(t, overrun, "model calls must stop at the bound")
	assert.Equal(t, []string{"first", "second"}, inputs, "each model call must see its own FIFO head")
	users, assistants := idlessCanonical(t, d)
	assert.Equal(t, []string{"first", "second"}, users)
	assert.LessOrEqual(t, len(assistants), 2, "assistant turns never exceed posts")
	assert.Equal(t, []string{"first", "second"}, idlessStoredUsers(t, store, d.sessionID()), "durable rows match the FIFO once each")
	assert.False(t, d.HasPending())
}

func TestIDlessRestartAfterCommitDoesNotReplay(t *testing.T) {
	store := session.NewInMemorySessionStore()
	d, prov, sess := idlessRuntime(t, store)
	require.True(t, d.Post(t.Context(), QueuedMessage{Content: "first"}, true))
	d.Wait()
	inputs, _ := prov.modelInputs()
	require.Equal(t, []string{"first"}, inputs)

	// The first runtime must release its process ownership before a restart.
	require.NoError(t, d.r.Close())
	restarted, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(prov)))), WithSessionStore(store))
	require.NoError(t, err)
	t.Cleanup(func() { _ = restarted.Close() })
	reloaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	rd := restarted.sessionDrivers.Get(reloaded)
	assert.False(t, rd.WakePending(), "a committed head is never replayed after restart")
	rd.Wait()
	inputs, overrun := prov.modelInputs()
	assert.False(t, overrun)
	assert.Equal(t, []string{"first"}, inputs, "no extra model call after restart")
	assert.Equal(t, []string{"first"}, idlessStoredUsers(t, store, sess.ID))
}

func TestIDlessEmptyWakeMakesNoModelCall(t *testing.T) {
	d, prov, _ := idlessRuntime(t, nil)
	assert.False(t, d.WakePending())
	d.Wait()
	inputs, _ := prov.modelInputs()
	assert.Empty(t, inputs)
	users, assistants := idlessCanonical(t, d)
	assert.Empty(t, users)
	assert.Empty(t, assistants)
}

func TestIDlessMixedFIFOKeepsAcceptedOrder(t *testing.T) {
	d, prov, _ := idlessRuntime(t, nil)
	require.True(t, d.Post(t.Context(), QueuedMessage{Content: "A", RequestID: "turn-a"}, true))
	require.True(t, d.Post(t.Context(), QueuedMessage{Content: "B"}, true))
	require.True(t, d.Post(t.Context(), QueuedMessage{Content: "C", RequestID: "turn-c"}, true))
	d.Wait()

	inputs, overrun := prov.modelInputs()
	assert.False(t, overrun)
	assert.Equal(t, []string{"A", "B", "C"}, inputs)
	users, _ := idlessCanonical(t, d)
	assert.Equal(t, []string{"A", "B", "C"}, users)
}

// idlessFailingStore fails the first failWrites AddMessage calls. With
// commitFirst the row is persisted before the error, which is the ambiguous case.
type idlessFailingStore struct {
	session.Store

	mu          sync.Mutex
	writes      int
	failWrites  int
	commitFirst bool
}

func (s *idlessFailingStore) AddMessage(ctx context.Context, sessionID string, msg *session.Message) (int64, error) {
	if msg.Message.Role != chat.MessageRoleUser {
		return s.Store.AddMessage(ctx, sessionID, msg)
	}
	s.mu.Lock()
	s.writes++
	fail := s.writes <= s.failWrites
	commit := s.commitFirst
	s.mu.Unlock()
	if !fail {
		return s.Store.AddMessage(ctx, sessionID, msg)
	}
	if commit {
		_, _ = s.Store.AddMessage(ctx, sessionID, msg)
	}
	return 0, errors.New("ambiguous storage failure")
}

// writeCount counts user-row writes; assistant rows share the same store.
func (s *idlessFailingStore) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

// idlessAmbiguousFailure drives a failed head write, then an explicit resume.
// The write error is opaque, so it is always treated as possibly committed.
func idlessAmbiguousFailure(t *testing.T, commitFirst bool) {
	t.Helper()
	store := &idlessFailingStore{Store: session.NewInMemorySessionStore(), failWrites: 1, commitFirst: commitFirst}
	d, prov, sess := idlessRuntime(t, store)
	_, events, cancel := d.Subscribe(64)
	defer cancel()

	require.True(t, d.Post(t.Context(), QueuedMessage{Content: "first"}, true))
	d.Wait()

	inputs, _ := prov.modelInputs()
	assert.Empty(t, inputs, "an uncertain head must not reach the model")
	assert.Equal(t, 1, store.writeCount(), "no automatic re-write after an ambiguous failure")
	assert.True(t, d.HasPending(), "the head is retained for an explicit retry")
	d.mu.Lock()
	lastError, uncertain := d.lastError, d.unidentifiedUncertain
	d.mu.Unlock()
	assert.Contains(t, lastError, "ambiguous storage failure")
	assert.True(t, uncertain)
	require.Eventually(t, func() bool {
		for {
			select {
			case event := <-events:
				if e, ok := event.(*ErrorEvent); ok && e.Code == "input_promotion_failed" {
					return true
				}
			default:
				return false
			}
		}
	}, 2*time.Second, time.Millisecond, "the failure is visible as a session-scoped error")

	// Manual resume admits the head once more. Residual duplicate risk is
	// documented: a write that committed before failing is written again.
	_, err := d.post(t.Context(), QueuedMessage{Retry: true}, true)
	require.NoError(t, err)
	d.Wait()
	inputs, overrun := prov.modelInputs()
	assert.False(t, overrun)
	assert.Equal(t, []string{"first"}, inputs, "exactly one model call after resume")
	users, _ := idlessCanonical(t, d)
	assert.Equal(t, []string{"first"}, users)
	stored := idlessStoredUsers(t, store, sess.ID)
	if commitFirst {
		assert.Equal(t, []string{"first", "first"}, stored, "ambiguous commit plus resume is the documented duplicate")
	} else {
		assert.Equal(t, []string{"first"}, stored)
	}
	assert.Equal(t, 2, store.writeCount())
}

func TestIDlessAmbiguousCommittedWriteFailsClosed(t *testing.T) {
	idlessAmbiguousFailure(t, true)
}

func TestIDlessOpaqueWriteErrorFailsClosed(t *testing.T) {
	idlessAmbiguousFailure(t, false)
}

// idlessGatedStore holds the first AddMessage until gate is closed.
type idlessGatedStore struct {
	session.Store

	gate    chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (s *idlessGatedStore) AddMessage(ctx context.Context, sessionID string, msg *session.Message) (int64, error) {
	s.once.Do(func() {
		s.entered <- struct{}{}
		<-s.gate
	})
	return s.Store.AddMessage(ctx, sessionID, msg)
}

func TestIDlessLateAckAfterStopDoesNotConsumeOrCallModel(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	store := &idlessGatedStore{Store: session.NewInMemorySessionStore(), gate: gate, entered: entered}
	d, prov, sess := idlessRuntime(t, store)

	require.True(t, d.Post(t.Context(), QueuedMessage{Content: "first"}, true))
	<-entered
	stopped := make(chan bool, 1)
	go func() { stopped <- d.StopAll() }()
	require.Eventually(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.stopped
	}, 2*time.Second, time.Millisecond)
	close(gate)
	<-stopped
	d.Wait()

	inputs, overrun := prov.modelInputs()
	assert.False(t, overrun)
	assert.Empty(t, inputs, "a stopped head never reaches the model")
	users, _ := idlessCanonical(t, d)
	assert.Equal(t, []string{"first"}, users, "the acknowledged row stays visible")
	assert.Equal(t, []string{"first"}, idlessStoredUsers(t, store, sess.ID), "the acknowledged row stays durable once")
	d.mu.Lock()
	uncertain, pending := d.unidentifiedUncertain, len(d.pending)
	d.mu.Unlock()
	assert.True(t, uncertain)
	assert.Zero(t, pending)
}
