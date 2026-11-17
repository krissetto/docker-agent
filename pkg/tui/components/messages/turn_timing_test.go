package messages

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/workblock"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// settledTiming builds the runtime's authoritative timing for one settled turn.
func settledTiming(turnID string, duration time.Duration, outcome string) *session.TurnTiming {
	return &session.TurnTiming{
		Version:    session.TurnTimingVersion,
		Basis:      session.TurnTimingBasisActiveWall,
		TurnID:     turnID,
		DurationMs: duration.Milliseconds(),
		Outcome:    outcome,
	}
}

func TestAuthoritativeTimingAcceptsOnlyCurrentSchema(t *testing.T) {
	valid := session.TurnTiming{Version: session.TurnTimingVersion, Basis: session.TurnTimingBasisActiveWall, TurnID: "turn", DurationMs: 1500, Outcome: session.TurnTimingOutcomeCompleted}
	got, ok := authoritativeTiming("turn", valid)
	require.True(t, ok)
	require.Equal(t, workTiming{measured: true, duration: 1500 * time.Millisecond}, got)

	canceled := valid
	canceled.Outcome = session.TurnTimingOutcomeCanceled
	got, ok = authoritativeTiming("turn", canceled)
	require.True(t, ok)
	require.Equal(t, workblock.OutcomeStopped, got.outcome, "a canceled turn reads as stopped")

	failed := valid
	failed.Outcome = session.TurnTimingOutcomeFailed
	got, ok = authoritativeTiming("turn", failed)
	require.True(t, ok)
	require.Equal(t, workblock.OutcomeFailed, got.outcome)

	for name, bad := range map[string]session.TurnTiming{
		"legacy version":  {Basis: valid.Basis, TurnID: "turn", DurationMs: 1500, Outcome: valid.Outcome},
		"other basis":     {Version: valid.Version, Basis: "wall", TurnID: "turn", DurationMs: 1500, Outcome: valid.Outcome},
		"other turn":      {Version: valid.Version, Basis: valid.Basis, TurnID: "other", DurationMs: 1500, Outcome: valid.Outcome},
		"unknown outcome": {Version: valid.Version, Basis: valid.Basis, TurnID: "turn", DurationMs: 1500, Outcome: "paused"},
		"negative":        {Version: valid.Version, Basis: valid.Basis, TurnID: "turn", DurationMs: -1, Outcome: valid.Outcome},
	} {
		_, ok := authoritativeTiming("turn", bad)
		require.False(t, ok, name)
	}
	_, ok = authoritativeTiming("", valid)
	require.False(t, ok, "a timing without a turn is never attached")
}

func TestSettledLabelRendersOncePerSteeredTurn(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 80, 20, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	m.SetCompactWorkLog(true)
	first := types.User("request")
	first.TurnID = "steered-turn"
	m.addMessage(first)
	m.AppendReasoning("root", "first work")
	steer := types.User("steer")
	steer.TurnID = "steered-turn"
	m.addMessage(steer)
	m.AppendReasoning("root", "second work")
	m.SetWorkStatus(false, 0)
	m.SetTurnTiming("steered-turn", settledTiming("steered-turn", 12400*time.Millisecond, "completed"))
	require.Equal(t, 1, strings.Count(transcriptPlainText(m), "· 12s"), "one timing label per root turn")
}

func TestReloadRestoresTurnTimingAndKeepsLearnedTimings(t *testing.T) {
	sess := session.New()
	persisted := session.UserMessage("persisted request")
	persisted.TurnID = "persisted-turn"
	sess.AddMessage(persisted)
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Presentation: []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "persisted work"}}}))
	sess.SetTurnTiming("persisted-turn", session.TurnTiming{Version: session.TurnTimingVersion, Basis: session.TurnTimingBasisActiveWall, TurnID: "persisted-turn", DurationMs: 65000, Outcome: session.TurnTimingOutcomeCompleted})
	legacy := session.UserMessage("legacy request")
	legacy.TurnID = "legacy-turn"
	sess.AddMessage(legacy)
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Presentation: []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "legacy work"}}}))

	m := NewScrollableView(animation.NewRuntime(), 80, 20, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	m.SetCompactWorkLog(true)
	// Learned live earlier in this process, for a turn the reloaded session does not time.
	m.SetTurnTiming("legacy-turn", settledTiming("legacy-turn", 5*time.Second, "completed"))
	m.LoadFromSession(sess, nil)
	m.ensureAllItemsRendered()

	plain := transcriptPlainText(m)
	require.Equal(t, 1, strings.Count(plain, "· 1m5s"), "persisted timing renders after reload")
	require.Equal(t, 1, strings.Count(plain, "· 5s"), "a timing learned before the reload survives it")

	// Reloading again keeps the restored labels too.
	m.LoadFromSession(sess, nil)
	m.ensureAllItemsRendered()
	require.Equal(t, 1, strings.Count(transcriptPlainText(m), "· 1m5s"))
}

func TestStoppedAndFailedKeepOutcomeWithoutDuration(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 80, 20, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	m.SetCompactWorkLog(true)
	for _, id := range []string{"no-timing", "quick", "done"} {
		request := types.User("request " + id)
		request.TurnID = id
		m.addMessage(request)
		m.AppendReasoning("root", "work "+id)
	}
	m.SetWorkStatus(false, 0)
	m.SetTurnOutcome("no-timing", "canceled")
	m.SetTurnTiming("quick", settledTiming("quick", 400*time.Millisecond, "failed"))
	m.SetTurnOutcome("done", "completed")
	plain := transcriptPlainText(m)
	require.Equal(t, 1, strings.Count(plain, "· Stopped"), "a canceled turn without accepted duration stays explicit")
	require.Equal(t, 1, strings.Count(plain, "· Failed"), "a sub-second failure stays explicit without seconds")
	require.NotContains(t, plain, "after", "no duration is invented for these turns")
	require.Equal(t, 1, unlabelledWorkHeaders(plain), "the completed turn with no usable duration is unlabelled")
}

func TestMeasuredTimingWinsOverOutcomeOnlyRecord(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 80, 20, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	m.SetCompactWorkLog(true)
	request := types.User("request")
	request.TurnID = "measured"
	m.addMessage(request)
	m.AppendReasoning("root", "work")
	m.SetWorkStatus(false, 0)
	m.SetTurnTiming("measured", settledTiming("measured", 12400*time.Millisecond, "canceled"))
	m.SetTurnOutcome("measured", "failed")
	plain := transcriptPlainText(m)
	require.Contains(t, plain, "Stopped after 12s")
	require.NotContains(t, plain, "Failed")
}

func TestReloadKeepsOutcomeOnlyTurnsWithoutInventingDuration(t *testing.T) {
	sess := session.New()
	for _, id := range []string{"canceled-turn", "legacy-turn"} {
		request := session.UserMessage("request " + id)
		request.TurnID = id
		sess.AddMessage(request)
		sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Presentation: []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "work " + id}}}))
	}
	sess.SetTurnOutcome("canceled-turn", string(session.TurnTimingOutcomeCanceled))

	m := NewScrollableView(animation.NewRuntime(), 80, 20, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	m.SetCompactWorkLog(true)
	m.LoadFromSession(sess, nil)
	m.ensureAllItemsRendered()
	plain := transcriptPlainText(m)
	require.Equal(t, 1, strings.Count(plain, "· Stopped"), "a persisted stopped outcome is shown on reload")
	require.NotContains(t, plain, "after", "no duration without accepted timing")
	require.Equal(t, 1, unlabelledWorkHeaders(plain), "a legacy turn with no outcome or timing stays unlabelled")
}

// unlabelledWorkHeaders counts Work header rows that carry no settled label.
func unlabelledWorkHeaders(plain string) int {
	count := 0
	for line := range strings.SplitSeq(plain, "\n") {
		if strings.TrimSpace(line) == "Work [+]" || strings.HasPrefix(strings.TrimSpace(line), "Work [+] (") && !strings.Contains(line, "·") {
			count++
		}
	}
	return count
}
