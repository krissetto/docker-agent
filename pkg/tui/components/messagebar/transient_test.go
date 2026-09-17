package messagebar

import (
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
)

func canonical(owner, id string, kind EventKind, sequence uint64) Event {
	return Event{Owner: owner, ID: id, Kind: kind, Sequence: sequence}
}

func TestAggregateCanonicalFactsCoalesceAndRetargetDeadline(t *testing.T) {
	var a Aggregator
	now := time.Unix(10, 0)
	turn := canonical("session-a", "turn-1", CompletedTurn, 1)
	first, ok := a.Add(turn, now)
	require.True(t, ok)
	assert.Equal(t, "1 turn completed", first.Text)
	assert.Equal(t, now.Add(3*time.Second), first.Deadline)
	_, ok = a.Add(turn, now.Add(time.Second))
	assert.False(t, ok)
	duplicate := turn
	duplicate.Sequence = 2
	_, ok = a.Add(duplicate, now.Add(time.Second))
	assert.False(t, ok, "same identity cannot count twice within a burst")
	current, ok := a.Current("session-a", now.Add(time.Second))
	require.True(t, ok)
	assert.Equal(t, first, current, "duplicates cannot postpone expiry")
	second, ok := a.Add(canonical("session-a", "child-1", SpawnedSubagent, 1), now.Add(time.Second))
	require.True(t, ok)
	assert.Equal(t, "1 turn completed · 1 subagent spawned", second.Text)
	assert.Equal(t, now.Add(4*time.Second), second.Deadline)
	assert.Greater(t, second.Generation, first.Generation)
	third, ok := a.Add(canonical("session-a", "turn-2", CompletedTurn, 3), now.Add(2*time.Second))
	require.True(t, ok)
	assert.Equal(t, "2 turns completed · 1 subagent spawned", third.Text)
	fourth, ok := a.Add(canonical("session-a", "child-2", SpawnedSubagent, 2), now.Add(2*time.Second))
	require.True(t, ok)
	assert.Equal(t, "2 turns completed · 2 subagents spawned", fourth.Text)
	_, ok = a.Current("session-a", fourth.Deadline)
	assert.False(t, ok, "deadline is exclusive")
	_, ok = a.Add(turn, fourth.Deadline)
	assert.False(t, ok, "high-water survives burst expiry")
	fresh, ok := a.Add(canonical("session-a", "turn-3", CompletedTurn, 4), fourth.Deadline)
	require.True(t, ok)
	assert.Equal(t, "1 turn completed", fresh.Text)
	assert.Greater(t, fresh.Generation, fourth.Generation)
	assert.Len(t, a.owners["session-a"].seen, 1)
}

func TestAggregateOwnerFocusAndReplayIsolation(t *testing.T) {
	var a Aggregator
	now := time.Unix(10, 0)
	first, ok := a.Add(canonical("a", "same-id", CompletedTurn, 1), now)
	require.True(t, ok)
	second, ok := a.Add(canonical("b", "same-id", CompletedTurn, 1), now)
	require.True(t, ok)
	assert.Equal(t, "a", first.Owner)
	assert.Equal(t, "b", second.Owner)
	assert.NotEqual(t, first.Generation, second.Generation)
	for _, owner := range []string{"a", "b", "missing", "a"} {
		a.Current(owner, now.Add(time.Second))
	}
	current, ok := a.Current("a", now.Add(time.Second))
	require.True(t, ok)
	assert.Equal(t, first, current, "focus does not reset identity or deadline")
	for _, event := range []Event{
		{Owner: "a", ID: "hydrated", Kind: CompletedTurn, Sequence: 100, Replay: true},
		{Owner: "a", ID: "reattached", Kind: SpawnedSubagent, Sequence: 100, Replay: true},
		{Owner: "a", ID: "missing-sequence", Kind: CompletedTurn},
		{Owner: "a", Kind: CompletedTurn, Sequence: 2},
		{ID: "missing-owner", Kind: CompletedTurn, Sequence: 2},
		{Owner: "a", ID: "stream-stop", Kind: EventKind(99), Sequence: 2},
	} {
		_, accepted := a.Add(event, now.Add(time.Second))
		assert.False(t, accepted, "%+v", event)
	}
	current, ok = a.Current("a", now.Add(time.Second))
	require.True(t, ok)
	assert.Equal(t, first, current)
	_, ok = a.Add(canonical("a", "new", CompletedTurn, 2), now.Add(time.Second))
	assert.True(t, ok, "replay must not advance the live cursor")
}

func TestAggregateStateBoundsAndEviction(t *testing.T) {
	var a Aggregator
	now := time.Unix(10, 0)
	for i := 1; i <= MaxBurstIDs; i++ {
		_, ok := a.Add(canonical("a", strconv.Itoa(i), CompletedTurn, uint64(i)), now)
		require.True(t, ok)
	}
	_, ok := a.Add(canonical("a", "overflow", CompletedTurn, MaxBurstIDs+1), now.Add(time.Second))
	assert.True(t, ok, "canonical facts keep counting beyond the optional ID cache")
	assert.Len(t, a.owners["a"].seen, MaxBurstIDs)
	current, ok := a.Current("a", now.Add(time.Second))
	require.True(t, ok)
	assert.Equal(t, MaxBurstIDs+1, current.CompletedTurns)
	assert.Equal(t, now.Add(time.Second+DefaultLifetime), current.Deadline)
	_, ok = a.Add(canonical("a", "overflow", CompletedTurn, MaxBurstIDs+1), current.Deadline)
	assert.False(t, ok, "canonical high-water also protects facts beyond the ID cache")
	fresh, ok := a.Add(canonical("a", "new-burst", CompletedTurn, MaxBurstIDs+2), current.Deadline)
	require.True(t, ok)
	assert.Equal(t, 1, fresh.CompletedTurns)
	for i := range MaxAggregateOwners {
		_, ok := a.Add(canonical(strconv.Itoa(i), "first", CompletedTurn, 1), now)
		require.True(t, ok)
	}
	assert.Len(t, a.owners, MaxAggregateOwners)
	_, ok = a.Current("a", now)
	assert.False(t, ok)
	replay := canonical("a", "new-burst", CompletedTurn, MaxBurstIDs+2)
	replay.Replay = true // Existing observer cursor must mark replay after eviction.
	_, ok = a.Add(replay, now)
	assert.False(t, ok)
	assert.Len(t, a.owners, MaxAggregateOwners)
}

func TestNoticeExplicitPriorityAndProtectedOwners(t *testing.T) {
	ordered := []Message{
		{Text: "background", Category: Background, Severity: Success},
		{Text: "hint", Category: Hint},
		{Text: "notice", Severity: Success},
		{Text: "warning", Severity: Warning},
		{Text: "error", Severity: Error},
		{Text: "cancel", Category: Cancellation, Severity: Warning},
	}
	for i, current := range ordered {
		for j, incoming := range ordered {
			t.Run(current.Text+"/"+incoming.Text, func(t *testing.T) {
				current.Owner, incoming.Owner = "same", "same"
				assert.Equal(t, j >= i, CanReplace(current, incoming))
				m := New()
				m.SetSize(40, 1)
				old, _, accepted := m.SetNotice(current, time.Time{})
				require.True(t, accepted)
				m.TakeVisualDirty()
				token, _, accepted := m.SetNotice(incoming, time.Time{})
				assert.Equal(t, j >= i, accepted)
				if !accepted {
					assert.Zero(t, token)
					assert.Equal(t, current.Text, m.message.Text)
					assert.False(t, m.TakeVisualDirty())
					m.ClearOwned(old)
					assert.Empty(t, m.message.Text, "explicit owner dismissal bypasses priority")
				}
			})
		}
	}
}

func TestNoticeOwnedExpiryRetargetAndIdleHold(t *testing.T) {
	scheduler := &noticeScheduler{now: time.Unix(10, 0)}
	ar := animation.NewRuntimeWithScheduler(scheduler)
	m := NewWithRuntime(ar)
	m.SetSize(80, 1)
	now := scheduler.now
	first, cmd, ok := m.SetNotice(Message{Text: "1 turn completed", Category: Background, Owner: "a"}, now.Add(DefaultLifetime))
	require.True(t, ok)
	require.Nil(t, cmd)
	cmd = ar.Continue()
	tick := acceptNoticeTick(t, ar, scheduler, cmd, fadeDuration/2)
	m.Update(tick)
	assert.InDelta(t, 0.5, m.alpha, 0)
	second, cmd, ok := m.SetNotice(Message{Text: "2 turns completed", Category: Background, Owner: "a"}, now.Add(4*time.Second))
	require.True(t, ok)
	assert.Nil(t, cmd)
	assert.InDelta(t, 0.5, m.alpha, 0, "mid-entry retarget preserves visible alpha")
	assert.NotEqual(t, first, second)
	tick = acceptNoticeTick(t, ar, scheduler, ar.Continue(), fadeDuration)
	m.Update(tick)
	require.Equal(t, math.Float64bits(1.0), math.Float64bits(m.alpha), "entry must settle at exactly full opacity")
	assert.Zero(t, ar.ActiveCount(), "three-second hold owns no frame lease")
	assert.Nil(t, m.Expire(first, now.Add(5*time.Second)))
	assert.Nil(t, m.Expire(second, now.Add(3*time.Second)))
	assert.Nil(t, m.ClearOwned(Token{Owner: "b", Generation: second.Generation}))
	assert.Equal(t, "2 turns completed", m.message.Text)
	cmd = m.Expire(second, now.Add(4*time.Second))
	require.Nil(t, cmd)
	cmd = ar.Continue()
	require.NotNil(t, cmd)
	tick = acceptNoticeTick(t, ar, scheduler, cmd, fadeDuration/2)
	m.Update(tick)
	assert.InDelta(t, 0.5, m.alpha, 0)
	third, cmd, ok := m.SetNotice(Message{Text: "Warning", Severity: Warning, Owner: "warning"}, time.Time{})
	require.True(t, ok)
	assert.Nil(t, cmd)
	assert.InDelta(t, 0.5, m.alpha, 0, "exit retarget preserves visible alpha")
	assert.Nil(t, m.Expire(second, now.Add(10*time.Second)))
	assert.Nil(t, m.Expire(third, now.Add(10*time.Second)), "persistent notices never expire")
	tick = acceptNoticeTick(t, ar, scheduler, ar.Continue(), fadeDuration)
	m.Update(tick)
	assert.Equal(t, "Warning", m.message.Text)
	assert.Zero(t, ar.ActiveCount())
	cmd = m.ClearOwned(third)
	require.Nil(t, cmd)
	cmd = ar.Continue()
	require.NotNil(t, cmd)
	tick = acceptNoticeTick(t, ar, scheduler, cmd, fadeDuration)
	m.Update(tick)
	assert.Empty(t, m.message.Text)
	assert.Zero(t, ar.ActiveCount())
}
