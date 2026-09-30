package tui

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/components/messagebar"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func completedFact(owner, turn string, sequence uint64) messages.SessionRuntimeEventMsg {
	return messages.SessionRuntimeEventMsg{OriginSessionID: owner, TurnID: turn, Epoch: 1, Sequence: sequence,
		Event: &runtime.TurnSettledEvent{SessionID: owner, TurnID: turn, Outcome: runtime.TurnCompleted}}
}

func TestMajorEventsKeepOutcomesWithoutRoutineNotices(t *testing.T) {
	root := splitTestRoot(t)
	for _, owner := range []string{"profile", "second"} {
		for _, severity := range []messagebar.Severity{messagebar.Info, messagebar.Warning} {
			root.messageBar.SetNotice(messagebar.Message{Text: "useful instruction", Severity: severity}, time.Time{})
			before := root.messageBar.View()
			for i, outcome := range []runtime.TurnOutcome{runtime.TurnCompleted, runtime.TurnFailed, runtime.TurnCanceled} {
				fact := completedFact(owner, "turn", uint64(i+1))
				fact.Event.(*runtime.TurnSettledEvent).Outcome = outcome
				require.Nil(t, root.observeMajorEvent(fact))
				require.Equal(t, outcome, root.paneOutcomes[owner])
				require.Equal(t, before, root.messageBar.View(), "foreground/background completion never replaces useful notices")
			}
			require.Nil(t, root.observeMajorEvent(messages.SessionRuntimeEventMsg{OriginSessionID: owner, Epoch: 1, Sequence: 4,
				Event: &runtime.SubagentCreatedEvent{SessionID: owner, ParentSessionID: owner, ChildSessionID: "child", NodeID: "node", CreatedAt: time.Now()}}))
			require.Equal(t, before, root.messageBar.View(), "spawn summaries are not notices")
			delete(root.majorEventWater, owner)
		}
	}
}

func TestMajorEventEnvelopeIdentityAndEpochFences(t *testing.T) {
	root := splitTestRoot(t)
	owner := root.application.Session().ID
	seed := completedFact(owner, "old", 1)
	seed.Seed = true
	root.observeMajorEvent(seed)
	require.Empty(t, root.paneOutcomes)
	root.observeMajorEvent(completedFact(owner, "old", 1))
	require.Empty(t, root.paneOutcomes)
	root.observeMajorEvent(completedFact(owner, "fresh", 2))
	require.Equal(t, runtime.TurnCompleted, root.paneOutcomes[owner])
	for _, fact := range []messages.SessionRuntimeEventMsg{
		{OriginSessionID: "unrelated", Epoch: 1, Sequence: 3, TurnID: "wrong", Event: &runtime.TurnSettledEvent{SessionID: owner, TurnID: "wrong", Outcome: runtime.TurnFailed}},
		{OriginSessionID: owner, Epoch: 0, Sequence: 3, TurnID: "old", Event: &runtime.TurnSettledEvent{SessionID: owner, TurnID: "old", Outcome: runtime.TurnFailed}},
		{OriginSessionID: owner, Epoch: 2, Sequence: 2, TurnID: "repeat", Event: &runtime.TurnSettledEvent{SessionID: owner, TurnID: "repeat", Outcome: runtime.TurnFailed}},
	} {
		root.observeMajorEvent(fact)
		require.Equal(t, runtime.TurnCompleted, root.paneOutcomes[owner])
	}
}

func TestSettledPresentationObservesVisibleWorkAndSynchronousNoticeEntry(t *testing.T) {
	root := splitTestRoot(t)
	require.True(t, root.SettledPresentation())
	root.messageBar.SetNotice(messagebar.Message{Text: "entry", Owner: "profile"}, time.Now().Add(5*time.Second))
	require.False(t, root.SettledPresentation())
	root = splitTestRoot(t)
	root.splitPane("second", "profile", splitRight)
	root.Update(messages.RoutedMsg{SessionID: "profile", Inner: runtime.StreamStarted("profile", "root")})
	require.False(t, root.SettledPresentation())
}
