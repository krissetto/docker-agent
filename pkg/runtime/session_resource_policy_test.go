package runtime

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func newPolicyTestTeam() *team.Team {
	provider := &mockProvider{id: "test/policy", stream: newStreamBuilder().AddStopWithUsage(1, 1).Build()}
	return team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(provider))))
}

func TestSessionResourcePolicyDefaultsAndConstruction(t *testing.T) {
	var sawConstructed bool
	rt, err := NewLocalRuntime(t.Context(), newPolicyTestTeam(), func(r *LocalRuntime) {
		sawConstructed = r.sessionEvents != nil || r.sessionDrivers != nil
	})
	require.NoError(t, err)
	assert.False(t, sawConstructed, "session infrastructure is constructed only after options")
	assert.NotNil(t, rt.sessionEvents)
	assert.NotNil(t, rt.sessionDrivers)

	want := DefaultSessionResourcePolicy()
	assert.Equal(t, want, rt.policy)
	assert.Equal(t, want.MaxSessions, rt.maxSessions)
	assert.Equal(t, want.MaxActiveDescendants, rt.maxActiveDescendants)
	assert.Equal(t, want.MaxActivePerRoot, rt.maxActiveDescendantsRoot)
	assert.Equal(t, want.MaxDepth, rt.maxSubagentDepth)
	assert.Equal(t, want.MailboxMessages, rt.maxPendingMailbox)
	assert.Equal(t, want.OrphanMessages, rt.maxOrphanMailbox)
	assert.Equal(t, want.ReplayEvents, rt.maxReplayEvents)
	assert.Equal(t, want.ReplayBytes, rt.maxReplayBytes)
	assert.Equal(t, want.IdleRetention, rt.idleRetention)
}

func TestSessionResourcePolicyOptionsComposeAndReplace(t *testing.T) {
	replacement := SessionResourcePolicy{MaxSessions: 7, MaxActiveDescendants: 8, MaxActivePerRoot: 9, MaxDepth: 10, MailboxMessages: 11, OrphanMessages: 12, ReplayEvents: 13, ReplayBytes: 14}
	rt, err := NewLocalRuntime(t.Context(), newPolicyTestTeam(),
		WithSessionResourcePolicy(replacement),
		WithMaxActiveDescendants(18),
		WithMaxActiveDescendantsPerRoot(19),
		WithMaxSubagentDepth(20),
		WithMaxSubagentMailbox(21),
	)
	require.NoError(t, err)
	replacement.MaxActiveDescendants = 18
	replacement.MaxActivePerRoot = 19
	replacement.MaxDepth = 20
	replacement.MailboxMessages = 21
	replacement.OrphanMessages = 21
	assert.Equal(t, replacement, rt.policy)
	assert.Equal(t, 7, rt.maxSessions)
	assert.Equal(t, 21, rt.maxPendingMailbox)
	assert.Equal(t, 21, rt.maxOrphanMailbox)

	rt, err = NewLocalRuntime(t.Context(), newPolicyTestTeam(),
		WithMaxSubagentMailbox(21),
		WithSessionResourcePolicy(SessionResourcePolicy{}),
	)
	require.NoError(t, err)
	assert.Equal(t, SessionResourcePolicy{}, rt.policy, "full policy option replaces earlier individual mutations")
	assert.Zero(t, rt.maxSessions)
	assert.Zero(t, rt.maxPendingMailbox)
	assert.Zero(t, rt.maxOrphanMailbox)
	assert.False(t, rt.sessionDrivers.PostOrBuffer(t.Context(), "disabled-orphan", QueuedMessage{Content: "no"}, true))

	disabledDriver := newSessionDriver(rt, session.New(session.WithID("disabled-mailbox")))
	disabledDriver.mu.Lock()
	disabledDriver.phase = sessionRunning
	disabledDriver.mu.Unlock()
	assert.False(t, disabledDriver.Post(t.Context(), QueuedMessage{Content: "no"}, true))
}

func TestUnlimitedSessionResourcesForSessionsAndMailboxes(t *testing.T) {
	policy := DefaultSessionResourcePolicy()
	policy.MaxSessions = UnlimitedSessionResources
	policy.MailboxMessages = UnlimitedSessionResources
	policy.OrphanMessages = UnlimitedSessionResources
	rt, err := NewLocalRuntime(t.Context(), newPolicyTestTeam(), WithSessionResourcePolicy(policy))
	require.NoError(t, err)

	for i := range 3 {
		_, err := rt.CreateSession(t.Context(), session.New(session.WithID(fmt.Sprintf("session-%d", i))), SessionBinding{})
		require.NoError(t, err)
	}

	driver := rt.sessionDrivers.Get(session.New(session.WithID("mailbox")))
	require.NotNil(t, driver)
	driver.mu.Lock()
	driver.phase = sessionRunning
	driver.mu.Unlock()
	for i := range defaultMaxSubagentMailbox + 1 {
		assert.True(t, driver.Post(t.Context(), QueuedMessage{Content: fmt.Sprintf("pending-%d", i)}, true))
	}

	for i := range defaultMaxOrphanMailbox + 1 {
		assert.True(t, rt.sessionDrivers.PostOrBuffer(t.Context(), "orphan", QueuedMessage{Content: fmt.Sprintf("orphan-%d", i)}, true))
	}
}

func TestSessionDriverRegistryGetPanicsOnDeniedAdmission(t *testing.T) {
	registry := newSessionDriverRegistry(&LocalRuntime{})
	assert.PanicsWithError(t, "session create_session: capacity limit reached (limit 0)", func() {
		registry.Get(session.New(session.WithID("disabled")))
	})
}

func TestWithMaxSubagentMailboxAcceptsUnlimitedSentinel(t *testing.T) {
	rt, err := NewLocalRuntime(t.Context(), newPolicyTestTeam(), WithMaxSubagentMailbox(UnlimitedSessionResources))
	require.NoError(t, err)
	assert.Equal(t, UnlimitedSessionResources, rt.policy.MailboxMessages)
	assert.Equal(t, UnlimitedSessionResources, rt.policy.OrphanMessages)
	assert.Equal(t, UnlimitedSessionResources, rt.maxPendingMailbox)
	assert.Equal(t, UnlimitedSessionResources, rt.maxOrphanMailbox)
}
