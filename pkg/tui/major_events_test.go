package tui

import (
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/components/messagebar"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func completedFact(owner, turn string, sequence uint64) messages.SessionRuntimeEventMsg {
	return messages.SessionRuntimeEventMsg{
		OriginSessionID: owner, TurnID: turn, Epoch: 1, Sequence: sequence,
		Event: &runtime.TurnSettledEvent{SessionID: owner, TurnID: turn, Outcome: runtime.TurnCompleted},
	}
}

func TestMajorEventsOnlyCanonicalCompletedAndDurableSpawn(t *testing.T) {
	root := splitTestRoot(t)
	owner := root.application.Session().ID
	root.observeMajorEvent(messages.SessionRuntimeEventMsg{OriginSessionID: owner, Epoch: 1, Sequence: 1, Event: runtime.StreamStopped(owner, "root", "normal")})
	_, ok := root.majorEvents.Current(owner, time.Now())
	require.False(t, ok)
	seed := completedFact(owner, "old", 2)
	seed.Seed = true
	root.observeMajorEvent(seed)
	root.observeMajorEvent(completedFact(owner, "old", 2))
	root.observeMajorEvent(completedFact(owner, "first", 3))
	root.observeMajorEvent(completedFact(owner, "first", 4))
	failed := completedFact(owner, "failed", 5)
	failed.Event.(*runtime.TurnSettledEvent).Outcome = runtime.TurnFailed
	root.observeMajorEvent(failed)
	root.observeMajorEvent(messages.SessionRuntimeEventMsg{
		OriginSessionID: owner, Epoch: 1, Sequence: 6,
		Event: &runtime.SubagentCreatedEvent{SessionID: owner, ParentSessionID: owner, ChildSessionID: "child-session-full", NodeID: "a1b2c", CreatedAt: time.Unix(100, 0)},
	})
	snapshot, ok := root.majorEvents.Current(owner, time.Now())
	require.True(t, ok)
	require.Equal(t, 1, snapshot.CompletedTurns)
	require.Equal(t, 1, snapshot.SpawnedSubagents)
	require.Equal(t, runtime.TurnFailed, root.paneOutcomes[owner])
}

func TestMajorNoticeCannotReplaceWarningAndStaleExpiryCannotClear(t *testing.T) {
	root := splitTestRoot(t)
	owner := root.application.Session().ID
	root.observeMajorEvent(completedFact(owner, "first", 1))
	old := root.majorNoticeToken
	root.messageBar.SetNotice(messagebar.Message{Owner: owner, Text: "protected warning", Severity: messagebar.Warning}, time.Time{})
	root.observeMajorEvent(completedFact(owner, "second", 2))
	require.Equal(t, old, root.majorNoticeToken)
	root.messageBar.Expire(old, time.Now().Add(time.Hour))
	require.Contains(t, root.messageBar.View(), "protected warning")
	root.finishInteractionHint(interactionHintReadyMsg{owner: owner, generation: root.interactionHintGeneration, sessionID: owner, text: "hint"})
	require.Contains(t, root.messageBar.View(), "protected warning")
}

func TestMajorEventEnvelopeIdentityAndEpochFences(t *testing.T) {
	root := splitTestRoot(t)
	owner := root.application.Session().ID
	wrong := completedFact(owner, "turn", 1)
	wrong.OriginSessionID = "unrelated"
	root.observeMajorEvent(wrong)
	_, ok := root.majorEvents.Current("unrelated", time.Now())
	require.False(t, ok)
	root.observeMajorEvent(completedFact(owner, "first", 2))
	older := completedFact(owner, "older", 3)
	older.Epoch = 0
	root.observeMajorEvent(older)
	snapshot, ok := root.majorEvents.Current(owner, time.Now())
	require.True(t, ok)
	require.Equal(t, 1, snapshot.CompletedTurns)
	reattached := completedFact(owner, "first", 2)
	reattached.Epoch = 2
	root.observeMajorEvent(reattached)
	snapshot, _ = root.majorEvents.Current(owner, time.Now())
	require.Equal(t, 1, snapshot.CompletedTurns, "new observer epoch cannot replay an old journal sequence")
}

func TestSettledPresentationObservesVisibleWorkAndSynchronousNoticeEntry(t *testing.T) {
	root := splitTestRoot(t)
	require.True(t, root.SettledPresentation())
	root.messageBar.SetNotice(messagebar.Message{Text: "entry", Owner: "profile", Category: messagebar.Background}, time.Now().Add(3*time.Second))
	require.False(t, root.SettledPresentation(), "SetNotice acquires entry lease before its returned command is executed")
	root = splitTestRoot(t)
	root.splitPane("second", "profile", splitRight)
	root.Update(messages.RoutedMsg{SessionID: "profile", Inner: runtime.StreamStarted("profile", "root")})
	require.False(t, root.SettledPresentation(), "unfocused visible work is part of current presentation")
}

func TestMajorEventNodeIDUsesCanonicalSourcesNotSessionShape(t *testing.T) {
	sess := session.New(session.WithID("abcde"))
	application := app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	require.Empty(t, majorEventNodeID(nil))
	require.Empty(t, majorEventNodeID(application), "a five-hex session identity is not a node identity")
	sess.SetSubagentTree(&subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{
		ID: subagent.SessionRootID(sess.ID), SessionID: sess.ID,
	}}}})
	require.Empty(t, majorEventNodeID(application), "synthetic roots are not human short IDs")
	sess.SetSubagentTree(&subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{
		ID: "parent-node", SessionID: "different-session",
	}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "node-X", SessionID: sess.ID}}}}}})
	require.Equal(t, "node-X", majorEventNodeID(application), "canonical node IDs must not be filtered by shape or truncated")
	info := runtime.SubagentAttachInfo{NodeID: "a1b2c", Session: sess, Agent: "worker"}
	attached := app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}), app.WithSubagentAttach(info))
	require.Equal(t, "a1b2c", majorEventNodeID(attached), "attachment identity wins over restored topology")
	services := &openSubagentRuntime{closeTabRuntime: newCloseTabRuntime("worker", false), info: info}
	live := app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(services))
	require.Equal(t, "a1b2c", majorEventNodeID(live), "runtime session-to-node lookup wins over restored topology")
}

func TestMajorNoticeRendersOwnerNodeNotFocusedOrSpawnedChild(t *testing.T) {
	root := splitTestRoot(t)
	owner := "second"
	sess := root.supervisor.GetRunner(owner).App.Session()
	sess.SetSubagentTree(&subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{
		ID: "b2c3d", SessionID: owner, Agent: "worker",
	}}}})
	root.messageBar = messagebar.New()
	root.messageBar.SetSize(100, 1)
	root.observeMajorEvent(completedFact(owner, "completed", 1))
	require.Contains(t, ansi.Strip(root.messageBar.View()), "(b2c3d) · 1 turn completed")
	deadline := root.majorNoticeDeadline
	root.observeMajorEvent(completedFact(owner, "completed", 1))
	require.Equal(t, deadline, root.majorNoticeDeadline, "duplicate delivery cannot extend the hold")
	root.observeMajorEvent(messages.SessionRuntimeEventMsg{
		OriginSessionID: owner, Epoch: 1, Sequence: 2,
		Event: &runtime.SubagentCreatedEvent{SessionID: owner, ParentSessionID: owner, ChildSessionID: "spawned-session", NodeID: "c3d4e", CreatedAt: time.Unix(100, 0)},
	})
	view := ansi.Strip(root.messageBar.View())
	require.Contains(t, view, "(b2c3d) · 1 turn completed · 1 subagent spawned")
	require.NotContains(t, view, "c3d4e", "the count belongs to the parent owner, not its newest child")
	require.NotEqual(t, owner, root.paneFocus(), "the event owner is deliberately unfocused")
}
