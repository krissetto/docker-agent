package runtime

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// gatedStream answers one text reply, but only after the gate is released. It
// signals started once it is being read, so tests can act while a turn is open.
type gatedStream struct {
	started chan struct{}
	gate    chan struct{}
	text    string
	once    sync.Once
	read    bool
}

func (s *gatedStream) Recv() (chat.MessageStreamResponse, error) {
	s.once.Do(func() { close(s.started) })
	<-s.gate
	if s.read {
		return chat.MessageStreamResponse{}, io.EOF
	}
	s.read = true
	return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: s.text}}}}, nil
}

func (s *gatedStream) Close() {}

// scriptedProvider hands out one stream per model call, so one agent can run
// several turns with different replies.
type scriptedProvider struct {
	*mockProvider

	model string
	next  func() chat.MessageStream
}

func (p *scriptedProvider) BaseConfig() base.Config {
	return base.Config{ModelConfig: latest.ModelConfig{Provider: "test", Model: p.model}}
}

func (p *scriptedProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return p.next(), nil
}

// scripted returns first on the first call, when set, and text replies after.
func scripted(first chat.MessageStream, text string) func() chat.MessageStream {
	var calls atomic.Int32
	return func() chat.MessageStream {
		if calls.Add(1) == 1 && first != nil {
			return first
		}
		return &mockStream{responses: []chat.MessageStreamResponse{{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: text}}}}}}
	}
}

// switchFixture is a root and worker team on one persistent store. The root
// records every on_agent_switch dispatch so tests can count them.
type switchFixture struct {
	t       *testing.T
	runtime *LocalRuntime
	store   session.Store
	hooks   *recordingBuiltin
	handle  SessionHandle
}

func newSwitchFixture(t *testing.T, rootFirst chat.MessageStream) *switchFixture {
	t.Helper()
	store := session.NewInMemorySessionStore()
	rb := &recordingBuiltin{}
	root := agent.New("root", "instructions",
		agent.WithModel(&scriptedProvider{mockProvider: &mockProvider{id: "test/root"}, model: "root", next: scripted(rootFirst, "root reply")}),
		agent.WithHooks(&hooks.Config{OnAgentSwitch: []hooks.Hook{{Type: hooks.HookTypeBuiltin, Command: "test_user_switch"}}}),
	)
	worker := agent.New("worker", "instructions",
		agent.WithModel(&scriptedProvider{mockProvider: &mockProvider{id: "test/worker"}, model: "worker", next: scripted(nil, "worker reply")}),
	)
	r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, worker)), WithModelStore(mockModelStore{}), WithSessionStore(store))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	require.NoError(t, r.hooksRegistry.RegisterBuiltin("test_user_switch", rb.hook))
	r.buildHooksExecutors()
	handle, err := r.CreateSession(t.Context(), session.New(session.WithAgentName("root")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	return &switchFixture{t: t, runtime: r, store: store, hooks: rb, handle: handle}
}

func (f *switchFixture) switcher() ActiveAgentSwitcher {
	f.t.Helper()
	switcher, ok := f.handle.(ActiveAgentSwitcher)
	require.True(f.t, ok, "local root handles implement ActiveAgentSwitcher")
	return switcher
}

// persisted reloads the session from the store, the durable source of truth.
func (f *switchFixture) persisted() *session.Session {
	f.t.Helper()
	stored, err := f.store.GetSession(f.t.Context(), f.handle.ID())
	require.NoError(f.t, err)
	return stored
}

// assertRooted checks that nothing observable changed: the live and persisted
// active agent are still root, and no on_agent_switch hook was dispatched.
func (f *switchFixture) assertRooted() {
	f.t.Helper()
	assert.Equal(f.t, "root", f.handle.AgentName(), "live active agent unchanged")
	assert.Equal(f.t, "root", f.persisted().AgentName, "persisted active agent unchanged")
	assert.Empty(f.t, f.hooks.snapshot(), "no on_agent_switch hook for a rejected or no-op call")
}

// switchEventsThrough reads events until the next switch has published both
// AgentInfo and TeamInfo, returning everything read in order.
func switchEventsThrough(t *testing.T, observation Observation) []Event {
	t.Helper()
	var events []Event
	var agentSeen, teamSeen bool
	timeout := time.After(10 * time.Second)
	for !agentSeen || !teamSeen {
		select {
		case envelope := <-observation.Events:
			events = append(events, envelope.Event)
			switch envelope.Event.(type) {
			case *AgentInfoEvent:
				agentSeen = true
			case *TeamInfoEvent:
				teamSeen = true
			}
		case <-timeout:
			require.FailNow(t, "switch did not publish AgentInfo and TeamInfo")
		}
	}
	return events
}

// assertOnlySwitchInfo checks that the collected events hold exactly one
// AgentInfo and one TeamInfo, and that the AgentInfo names the switched-to agent (worker). A rejected
// call that had published anything would show up here as an extra or stale event.
func assertOnlySwitchInfo(t *testing.T, events []Event, agentName string) {
	t.Helper()
	var agents, teams []Event
	for _, event := range events {
		switch event := event.(type) {
		case *AgentInfoEvent:
			agents = append(agents, event)
			assert.Equal(t, agentName, event.AgentName)
		case *TeamInfoEvent:
			teams = append(teams, event)
		}
	}
	assert.Len(t, agents, 1, "exactly one AgentInfo per committed switch")
	assert.Len(t, teams, 1, "exactly one TeamInfo per committed switch")
}

// runTurn submits one message and waits for the turn to settle.
func runTurn(t *testing.T, handle SessionHandle, requestID, content string) {
	t.Helper()
	submission, err := handle.Submit(t.Context(), TurnInput{Content: content, RequestID: requestID})
	require.NoError(t, err)
	require.NoError(t, handle.AwaitTurn(t.Context(), submission.TurnID))
}

func lastAssistantText(t *testing.T, handle SessionHandle) string {
	t.Helper()
	snapshot, err := handle.Snapshot(t.Context())
	require.NoError(t, err)
	return snapshot.GetLastAssistantMessageContent()
}

func userTexts(t *testing.T, handle SessionHandle) []string {
	t.Helper()
	snapshot, err := handle.Snapshot(t.Context())
	require.NoError(t, err)
	var users []string
	for _, item := range snapshot.MessagesSnapshot() {
		if item.Message != nil && item.Message.Message.Role == chat.MessageRoleUser {
			users = append(users, item.Message.Message.Content)
		}
	}
	return users
}

// liveModel reports the model of the session's live provider binding, which
// the switch replaces together with the active agent.
func liveModel(t *testing.T, handle SessionHandle) string {
	t.Helper()
	_, providers := handle.(*sessionHandle).driver.ModelSnapshot()
	require.Len(t, providers, 1)
	return providers[0].BaseConfig().ModelConfig.Model
}

func messageCount(t *testing.T, handle SessionHandle) int {
	t.Helper()
	snapshot, err := handle.Snapshot(t.Context())
	require.NoError(t, err)
	return len(snapshot.MessagesSnapshot())
}

func TestActiveAgentSwitchKeepsSessionAndRoutesNextTurn(t *testing.T) {
	t.Parallel()
	f := newSwitchFixture(t, nil)
	runTurn(t, f.handle, "first", "first")
	require.Equal(t, "root reply", lastAssistantText(t, f.handle))
	id, before := f.handle.ID(), messageCount(t, f.handle)
	require.Equal(t, "root", liveModel(t, f.handle))
	observation, err := f.handle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer observation.Cancel()

	require.NoError(t, f.switcher().SwitchActiveAgent(t.Context(), "worker"))

	assert.Equal(t, id, f.handle.ID(), "the same session continues")
	assert.Equal(t, before, messageCount(t, f.handle), "history is neither cloned nor truncated")
	assert.Equal(t, "worker", f.handle.AgentName(), "live active agent")
	assert.Equal(t, "worker", f.handle.Metadata().AgentName)
	assert.Equal(t, "worker", liveModel(t, f.handle), "the live model binding follows the new agent")
	assert.True(t, f.handle.Metadata().Capabilities.ActiveAgentSwitching)
	stored := f.persisted()
	assert.Equal(t, "worker", stored.AgentName, "active identity is persisted")
	assert.Equal(t, "root", stored.AttributesSnapshot()[SessionAgentAttribute], "creation binding is immutable provenance")
	assertOnlySwitchInfo(t, switchEventsThrough(t, observation), "worker")
	require.Len(t, f.hooks.snapshot(), 1, "the hook fires exactly once")
	hook := f.hooks.snapshot()[0]
	assert.Equal(t, id, hook.SessionID)
	assert.Equal(t, "root", hook.FromAgent)
	assert.Equal(t, "worker", hook.ToAgent)
	assert.Equal(t, agentSwitchKindUserSwitch, hook.AgentSwitchKind)

	runTurn(t, f.handle, "second", "second")
	assert.Equal(t, "worker reply", lastAssistantText(t, f.handle), "the next turn runs as the switched agent")
	assert.Equal(t, []string{"first", "second"}, userTexts(t, f.handle), "history continues in the same session")
}

func TestActiveAgentSwitchRejectsUnknownAgentWithoutChange(t *testing.T) {
	t.Parallel()
	f := newSwitchFixture(t, nil)
	observation, err := f.handle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer observation.Cancel()

	err = f.switcher().SwitchActiveAgent(t.Context(), "missing")
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	assert.Equal(t, SessionErrorNotFound, sessionErr.Kind)
	assert.Equal(t, sessionOperationSwitchActiveAgent, sessionErr.Operation)
	f.assertRooted()

	// Control: a later committed switch is the first AgentInfo, so the rejected call published none.
	require.NoError(t, f.switcher().SwitchActiveAgent(t.Context(), "worker"))
	assertOnlySwitchInfo(t, switchEventsThrough(t, observation), "worker")
}

// assertBusy checks the typed busy error and that the rejected call left the
// session untouched. The caller releases the busy cause itself.
func assertBusy(t *testing.T, f *switchFixture, err error) {
	t.Helper()
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	assert.Equal(t, SessionErrorCapacity, sessionErr.Kind)
	assert.Equal(t, SessionErrorReasonBusy, sessionErr.Reason)
	assert.Equal(t, sessionOperationSwitchActiveAgent, sessionErr.Operation)
	f.assertRooted()
}

func TestActiveAgentSwitchIsBusyWhileTurnRuns(t *testing.T) {
	t.Parallel()
	gate := &gatedStream{started: make(chan struct{}), gate: make(chan struct{}), text: "gated"}
	f := newSwitchFixture(t, gate)
	observation, err := f.handle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer observation.Cancel()
	submission, err := f.handle.Submit(t.Context(), TurnInput{Content: "busy", RequestID: "active"})
	require.NoError(t, err)
	<-gate.started
	// The running turn publishes its own root AgentInfo first; consume it so the
	// busy call and the control switch are judged on their own events.
	assertOnlySwitchInfo(t, switchEventsThrough(t, observation), "root")

	assertBusy(t, f, f.switcher().SwitchActiveAgent(t.Context(), "worker"))

	close(gate.gate)
	require.NoError(t, f.handle.AwaitTurn(t.Context(), submission.TurnID))
	require.NoError(t, f.switcher().SwitchActiveAgent(t.Context(), "worker"))
	assertOnlySwitchInfo(t, switchEventsThrough(t, observation), "worker")
}

func TestActiveAgentSwitchIsBusyWithQueuedInput(t *testing.T) {
	t.Parallel()
	gate := &gatedStream{started: make(chan struct{}), gate: make(chan struct{}), text: "gated"}
	f := newSwitchFixture(t, gate)
	active, err := f.handle.Submit(t.Context(), TurnInput{Content: "busy", RequestID: "active"})
	require.NoError(t, err)
	<-gate.started
	queued, err := f.handle.Submit(t.Context(), TurnInput{Content: "queued", RequestID: "queued"})
	require.NoError(t, err)

	assertBusy(t, f, f.switcher().SwitchActiveAgent(t.Context(), "worker"))

	close(gate.gate)
	require.NoError(t, f.handle.AwaitTurn(t.Context(), active.TurnID))
	require.NoError(t, f.handle.AwaitTurn(t.Context(), queued.TurnID))
	// Opened after the queued turn settles: its run may publish its own root
	// AgentInfo, which must not be mistaken for the switch's events.
	observation, err := f.handle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer observation.Cancel()
	require.NoError(t, f.switcher().SwitchActiveAgent(t.Context(), "worker"))
	assertOnlySwitchInfo(t, switchEventsThrough(t, observation), "worker")
}

func TestActiveAgentSwitchIsBusyWithPendingInteraction(t *testing.T) {
	t.Parallel()
	f := newSwitchFixture(t, nil)
	driver := f.handle.(*sessionHandle).driver
	// Driver-state injection, as in the admission table: a parked interaction
	// keeps the session busy without a tool approval round trip.
	driver.mu.Lock()
	driver.interactions["probe"] = sessionInteraction{}
	driver.mu.Unlock()
	observation, err := f.handle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer observation.Cancel()

	assertBusy(t, f, f.switcher().SwitchActiveAgent(t.Context(), "worker"))

	driver.mu.Lock()
	delete(driver.interactions, "probe")
	driver.mu.Unlock()
	require.NoError(t, f.switcher().SwitchActiveAgent(t.Context(), "worker"))
	assertOnlySwitchInfo(t, switchEventsThrough(t, observation), "worker")
}

func TestActiveAgentSwitchRejectsChildSessions(t *testing.T) {
	t.Parallel()
	f := newSwitchFixture(t, nil)
	child, err := f.runtime.CreateSession(t.Context(), session.New(session.WithAgentName("root"), session.WithParentID(f.handle.ID())), SessionBinding{AgentName: "root"})
	require.NoError(t, err)

	err = child.(ActiveAgentSwitcher).SwitchActiveAgent(t.Context(), "worker")
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	assert.Equal(t, SessionErrorUnsupported, sessionErr.Kind)
	assert.Equal(t, sessionOperationSwitchActiveAgent, sessionErr.Operation)
	assert.False(t, child.Metadata().Capabilities.ActiveAgentSwitching, "child handles do not advertise the capability")
	assert.Equal(t, "root", child.AgentName(), "child keeps its creation agent")
	f.assertRooted()
	// A parent switch does not retarget its child, which keeps the creation agent.
	require.NoError(t, f.switcher().SwitchActiveAgent(t.Context(), "worker"))
	assert.Equal(t, "worker", f.handle.AgentName())
	assert.Equal(t, "root", child.AgentName())
}

func TestActiveAgentSwitchToCurrentAgentIsNoop(t *testing.T) {
	t.Parallel()
	f := newSwitchFixture(t, nil)
	observation, err := f.handle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer observation.Cancel()
	before := f.handle.Metadata()
	beforeStored := f.persisted()

	require.NoError(t, f.switcher().SwitchActiveAgent(t.Context(), "root"))

	f.assertRooted()
	assert.Equal(t, before, f.handle.Metadata(), "metadata is unchanged")
	assert.Equal(t, beforeStored.AgentName, f.persisted().AgentName)

	// Control: a later committed switch is the first AgentInfo, so the no-op published none.
	require.NoError(t, f.switcher().SwitchActiveAgent(t.Context(), "worker"))
	assertOnlySwitchInfo(t, switchEventsThrough(t, observation), "worker")
}

func TestActiveAgentSwitchCapabilityIsLocalOnly(t *testing.T) {
	t.Parallel()
	var remote SessionHandle = &remoteSession{}
	_, ok := remote.(ActiveAgentSwitcher)
	assert.False(t, ok, "remote handles must not expose in-place switching")
	assert.False(t, remoteSessionMetadata(api.SessionMetadata{}).runtime().Capabilities.ActiveAgentSwitching)
}

// gatedUpdateStore holds session updates once its gate is armed, so an
// in-place switch's durable write stays in flight while a test probes reclaim.
type gatedUpdateStore struct {
	session.Store

	gate    chan struct{}
	entered chan struct{}
}

func (s *gatedUpdateStore) UpdateSession(ctx context.Context, sess *session.Session) error {
	if s.gate != nil {
		s.entered <- struct{}{}
		<-s.gate
	}
	return s.Store.UpdateSession(ctx, sess)
}

// The durable write of an in-place switch keeps its driver out of capacity
// reclaim: while the write is in flight reclaim reports busy, and the switch
// still completes once the write settles.
func TestActiveAgentSwitchDurableWriteHoldsOffCapacityReclaim(t *testing.T) {
	store := &gatedUpdateStore{Store: session.NewInMemorySessionStore(), entered: make(chan struct{}, 1)}
	root := agent.New("root", "instructions", agent.WithModel(&scriptedProvider{mockProvider: &mockProvider{id: "test/root"}, model: "root", next: scripted(nil, "root reply")}))
	worker := agent.New("worker", "instructions", agent.WithModel(&scriptedProvider{mockProvider: &mockProvider{id: "test/worker"}, model: "worker", next: scripted(nil, "worker reply")}))
	r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, worker)), WithModelStore(mockModelStore{}), WithSessionStore(store))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	handle, err := r.CreateSession(t.Context(), session.New(session.WithAgentName("root")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	driver := handle.(*sessionHandle).driver

	store.gate = make(chan struct{})
	var release sync.Once
	releaseWrite := func() { release.Do(func() { close(store.gate) }) }
	t.Cleanup(releaseWrite)
	switched := make(chan error, 1)
	go func() { switched <- handle.(ActiveAgentSwitcher).SwitchActiveAgent(t.Context(), "worker") }()
	select {
	case <-store.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("switch did not reach its durable write")
	}

	var outcome driverReclaimOutcome
	require.NoError(t, driver.ownerCall(t.Context(), func() error {
		outcome = driver.beginReclaimLocked()
		return nil
	}))
	assert.Equal(t, driverReclaimBusy, outcome, "reclaim must wait for the switch's durable write")

	releaseWrite()
	select {
	case err := <-switched:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("switch did not complete after its write settled")
	}
	assert.Equal(t, "worker", handle.AgentName())
}
