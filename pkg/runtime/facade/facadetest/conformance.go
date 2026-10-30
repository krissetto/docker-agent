package facadetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/runtime/facade"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

const waitTimeout = 10 * time.Second

// Config parameterises a fixture for one scenario.
type Config struct {
	// MaxIterations bounds model iterations per turn; zero keeps the runtime default.
	MaxIterations int
	// ReplayEvents bounds the session journal; zero keeps the runtime default.
	ReplayEvents int
}

// Fixture is one host plus the scripted model that drives it.
type Fixture struct {
	Host facade.Host
	// Model drives the root agent; Worker drives the writer subagent and writer agent.
	Model  *Model
	Worker *Model
}

// Factory builds a fresh fixture for each scenario. The factory owns cleanup.
type Factory func(t *testing.T, cfg Config) Fixture

// Run executes every conformance scenario against adapters built by factory.
func Run(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("QueueTicketsAreFIFOAndIdentified", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		queue, ok := sess.Queue()
		if !ok {
			t.Skip("queue capability is not advertised")
		}
		ctx := t.Context()
		obs := observeFrom(t, sess, nil)
		block, entered := make(chan struct{}), make(chan struct{})
		fx.Model.Push(Script{Text: "first answer", Block: block, Entered: entered})
		turn := runTurn(t, sess, facade.Input{Content: "first"})
		awaitStarted(t, obs, turn.ID())
		awaitEntered(t, entered)

		a, err := queue.FollowUp(ctx, facade.Input{Content: "follow A"})
		requireNoError(t, err)
		b, err := queue.FollowUp(ctx, facade.Input{Content: "follow B"})
		requireNoError(t, err)
		requireNotEmptyString(t, a.ID)
		assertNotEqual(t, a.ID, b.ID, "each follow-up has its own ticket")
		awaitEvent(t, obs.Events, queueAccepted(a.ID))
		awaitEvent(t, obs.Events, queueAccepted(b.ID))

		fx.Model.Push(Script{Text: "answer A"}, Script{Text: "answer B"})
		close(block)
		assertEqual(t, facade.OutcomeCompleted, wait(t, turn).Kind)
		drain(t, turn.Events())
		assertEqual(t, []string{a.ID, b.ID}, collectConsumed(t, obs.Events, 2), "follow-ups are consumed FIFO under their own tickets")
		assertEqual(t, []string{"follow A", "follow B"}, userMessagesIn(sessionMessages(t, sess), "follow A", "follow B"), "the transcript keeps FIFO order")
	})

	t.Run("SteerIsConsumedIntoTheActiveTurn", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		queue, ok := sess.Queue()
		if !ok {
			t.Skip("queue capability is not advertised")
		}
		ctx := t.Context()
		obs := observeFrom(t, sess, nil)
		block, entered := make(chan struct{}), make(chan struct{})
		fx.Model.Push(Script{ToolCall: ToolNoop, Block: block, Entered: entered}, Script{Text: "done"})
		turn := runTurn(t, sess, facade.Input{Content: "start"})
		awaitStarted(t, obs, turn.ID())
		awaitEntered(t, entered)

		steer, err := queue.Steer(ctx, facade.Input{Content: "guidance"})
		requireNoError(t, err)
		close(block)
		awaitEvent(t, obs.Events, queueConsumed(steer.ID))
		assertEqual(t, facade.OutcomeCompleted, wait(t, turn).Kind)
		drain(t, turn.Events())
		assertEqual(t, []string{"guidance"}, userMessagesIn(sessionMessages(t, sess), "guidance"), "the steered input reaches the transcript")
	})

	t.Run("QueueRejectsAClosedSession", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		queue, ok := sess.Queue()
		if !ok {
			t.Skip("queue capability is not advertised")
		}
		ctx := t.Context()
		requireNoError(t, sess.Close(ctx))
		_, err := queue.FollowUp(ctx, facade.Input{Content: "late"})
		assertErrorIs(t, err, facade.ErrSessionClosed)
		_, err = queue.Steer(ctx, facade.Input{Content: "late"})
		assertErrorIs(t, err, facade.ErrSessionClosed)
	})

	t.Run("CompactSummarisesTheSessionAndFailsOnceDeleted", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		compact, ok := sess.Compact()
		if !ok {
			t.Skip("compaction capability is not advertised")
		}
		ctx := t.Context()
		obs := observeFrom(t, sess, nil)
		fx.Model.Push(Script{Text: "first answer"})
		turn := runTurn(t, sess, facade.Input{Content: "first"})
		assertEqual(t, facade.OutcomeCompleted, wait(t, turn).Kind)
		drain(t, turn.Events())

		fx.Model.Push(Script{Text: "the summary"})
		requireNoError(t, compact.Compact(ctx, ""))
		awaitEvent(t, obs.Events, summaryEvent("the summary"))
		assertEqual(t, []string{"the summary"}, summariesIn(sessionMessages(t, sess)), "the summary is recorded in the transcript")

		sessionID := sess.Info().SessionID
		requireNoError(t, fx.Host.Delete(ctx, sessionID))
		assertTrue(t, compact.Compact(ctx, "") != nil, "a deleted session cannot be compacted")
	})

	t.Run("CompactWhileBusyIsQueuedUntilTheTurnSettles", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		compact, ok := sess.Compact()
		if !ok {
			t.Skip("compaction capability is not advertised")
		}
		ctx := t.Context()
		obs := observeFrom(t, sess, nil)
		block, entered := make(chan struct{}), make(chan struct{})
		fx.Model.Push(Script{Text: "busy", Block: block, Entered: entered}, Script{Text: "busy summary"})
		turn := runTurn(t, sess, facade.Input{Content: "busy"})
		awaitStarted(t, obs, turn.ID())
		awaitEntered(t, entered)

		// A busy session accepts the compaction as queued rather than failing it.
		requireNoError(t, compact.Compact(ctx, ""))
		close(block)
		assertEqual(t, facade.OutcomeCompleted, wait(t, turn).Kind)
		drain(t, turn.Events())
		awaitEvent(t, obs.Events, summaryEvent("busy summary"))
	})

	t.Run("PauseHoldsTheRunAtItsBoundaryUntilResumed", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		pause, ok := sess.Pause()
		if !ok {
			t.Skip("pause capability is not advertised")
		}
		ctx := t.Context()
		obs := observeFrom(t, sess, nil)
		block, entered := make(chan struct{}), make(chan struct{})
		fx.Model.Push(Script{ToolCall: ToolNoop, Block: block, Entered: entered}, Script{Text: "after resume"})
		turn := runTurn(t, sess, facade.Input{Content: "held"})
		awaitStarted(t, obs, turn.ID())
		awaitEntered(t, entered)

		paused, err := pause.TogglePause(ctx)
		requireNoError(t, err)
		assertTrue(t, paused, "the first toggle pauses")
		status, err := sess.Status(ctx)
		requireNoError(t, err)
		assertTrue(t, status.PauseArmed, "a requested pause is armed")
		assertFalse(t, status.Paused, "the run has not reached its boundary yet")

		close(block)
		awaitEvent(t, obs.Events, pausedEvent)
		assertEqual(t, 1, fx.Model.Calls(), "the second model call waits behind the pause")
		status, err = sess.Status(ctx)
		requireNoError(t, err)
		assertTrue(t, status.Paused, "the run reached its boundary and is paused")

		resumed, err := pause.TogglePause(ctx)
		requireNoError(t, err)
		assertFalse(t, resumed, "the second toggle resumes")
		status, err = sess.Status(ctx)
		requireNoError(t, err)
		assertFalse(t, status.Paused)
		assertFalse(t, status.PauseArmed, "resuming disarms the pause")
		assertEqual(t, facade.OutcomeCompleted, wait(t, turn).Kind)
		drain(t, turn.Events())
		assertEqual(t, 2, fx.Model.Calls(), "the held run continues after resume")

		canceled, cancel := context.WithCancel(ctx)
		cancel()
		_, err = pause.TogglePause(canceled)
		assertTrue(t, err != nil, "a cancelled toggle fails")
		status, err = sess.Status(ctx)
		requireNoError(t, err)
		assertFalse(t, status.PauseArmed, "a failed toggle changes nothing")
	})

	t.Run("SubagentTreeNamesTheSpawnedChild", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		tree, ok := sess.SubagentTree()
		if !ok {
			t.Skip("subagent tree capability is not advertised")
		}
		ctx := t.Context()
		obs := observeFrom(t, sess, nil)
		child := spawnWriter(t, fx, sess, obs)
		requireNotEmptyString(t, child)

		got, err := tree.Tree(ctx)
		requireNoError(t, err)
		requireLen(t, got.Roots, 1, "the tree has one root")
		root := got.Roots[0]
		requireLen(t, root.Children, 1, "exactly the spawned child sits under the root")
		node := root.Children[0]
		assertEqual(t, child, node.SessionID)
		assertEqual(t, WriterAgent, node.Agent)
		assertEqual(t, root.ID, node.Parent, "the child is parented to the root node")
	})

	t.Run("StopSubtreeStopsTheChildAndKeepsTheRoot", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		tree, ok := sess.SubagentTree()
		if !ok {
			t.Skip("subagent tree capability is not advertised")
		}
		ctx := t.Context()
		obs := observeFrom(t, sess, nil)
		child := spawnWriter(t, fx, sess, obs)
		requireNotEmptyString(t, child)

		childSess, err := fx.Host.Open(ctx, child)
		requireNoError(t, err)
		awaitEvent(t, obs.Events, childSettled(child))
		stopper, ok := childSess.StopSubtree()
		requireTrue(t, ok, "a subagent can stop its own subtree")
		requireNoError(t, stopper.StopSubtree(ctx))

		got, err := tree.Tree(ctx)
		requireNoError(t, err)
		node, found := findTreeNode(got.Roots, child)
		requireTrue(t, found, "the stopped child stays in the tree")
		assertEqual(t, "stopped", node.State)

		fx.Model.Push(Script{Text: "root still answers"})
		next := runTurn(t, sess, facade.Input{Content: "still there?"})
		assertEqual(t, facade.OutcomeCompleted, wait(t, next).Kind, "stopping a subagent leaves the root usable")
		drain(t, next.Events())

		_, err = childSess.Run(ctx, facade.Input{Content: "late"})
		assertTrue(t, err != nil, "a stopped subagent refuses new work")
		requireNoError(t, childSess.Close(ctx))
	})

	t.Run("TodoEditorChangesOnlyTheTargetedTodo", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		todos, ok := sess.TodoEditor()
		if !ok {
			t.Skip("todo editing capability is not advertised")
		}
		ctx := t.Context()
		fx.Model.Push(Script{ToolCall: ToolCreateTodos, ToolArgs: `{"descriptions":["alpha","beta","gamma"]}`}, Script{Text: "created"})
		turn := runTurn(t, sess, facade.Input{Content: "plan"})
		events := drain(t, turn.Events())
		assertEqual(t, facade.OutcomeCompleted, wait(t, turn).Kind)
		ids := todoIDs(events)
		requireLen(t, []string{ids["alpha"], ids["beta"], ids["gamma"]}, 3, "the create call returned three todos")
		alpha, beta, gamma := ids["alpha"], ids["beta"], ids["gamma"]

		list, err := todos.SetStatus(ctx, beta, "completed")
		requireNoError(t, err)
		assertEqual(t, map[string]facade.Todo{
			alpha: {ID: alpha, Description: "alpha", Status: "pending"},
			beta:  {ID: beta, Description: "beta", Status: "completed"},
			gamma: {ID: gamma, Description: "gamma", Status: "pending"},
		}, todoMap(list), "only the targeted todo changes status")

		_, err = todos.SetDescription(ctx, alpha, "not alpha", "renamed")
		assertTrue(t, err != nil, "a mismatched expected description fails")
		list, err = todos.SetStatus(ctx, gamma, "in-progress")
		requireNoError(t, err)
		assertEqual(t, "alpha", todoMap(list)[alpha].Description, "the failed rename changed nothing")

		list, err = todos.SetDescription(ctx, alpha, "alpha", "alpha renamed")
		requireNoError(t, err)
		assertEqual(t, map[string]facade.Todo{
			alpha: {ID: alpha, Description: "alpha renamed", Status: "pending"},
			beta:  {ID: beta, Description: "beta", Status: "completed"},
			gamma: {ID: gamma, Description: "gamma", Status: "in-progress"},
		}, todoMap(list), "a matching expectation renames exactly one todo")

		list, err = todos.Remove(ctx, beta)
		requireNoError(t, err)
		assertEqual(t, map[string]facade.Todo{
			alpha: {ID: alpha, Description: "alpha renamed", Status: "pending"},
			gamma: {ID: gamma, Description: "gamma", Status: "in-progress"},
		}, todoMap(list), "remove deletes exactly the targeted todo")

		_, err = todos.SetStatus(ctx, "no-such-todo", "completed")
		assertTrue(t, err != nil, "an unknown todo ID fails")
	})

	t.Run("ObserverResumesExactlyAfterCursor", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		if _, ok := sess.Observer(); !ok {
			t.Skip("observer capability is not advertised")
		}
		full := observeFrom(t, sess, nil)
		fx.Model.Push(Script{ToolCall: ToolNoop}, Script{Text: "two"})
		turn := runTurn(t, sess, facade.Input{Content: "one"})
		recorded := drainObserved(t, full.Events, turn.ID())
		requireTrue(t, len(recorded) >= 4, "the turn produces several events")
		for i := 1; i < len(recorded); i++ {
			assertTrue(t, recorded[i].Seq > recorded[i-1].Seq, "sequences strictly increase")
		}

		cut := recorded[len(recorded)/2]
		want := eventsFrom(recorded, cut.Seq+1)
		resumed := observeFrom(t, sess, &facade.Cursor{Epoch: full.State.Epoch, Sequence: cut.Seq})
		for _, expected := range want {
			got := awaitEvent(t, resumed.Events, anyEvent)
			assertEqual(t, describeEvent(expected), describeEvent(got), "resume replays exactly the events after the cursor")
		}
	})

	t.Run("ObserverReportsGapForEvictedCursor", func(t *testing.T) {
		fx := fixture(t, factory, Config{ReplayEvents: 4})
		sess := newSession(t, fx)
		if _, ok := sess.Observer(); !ok {
			t.Skip("observer capability is not advertised")
		}
		full := observeFrom(t, sess, nil)
		fx.Model.Push(Script{ToolCall: ToolNoop}, Script{Text: "two"})
		turn := runTurn(t, sess, facade.Input{Content: "one"})
		recorded := drainObserved(t, full.Events, turn.ID())
		requireTrue(t, len(recorded) > 4, "the turn produces more events than the journal keeps")
		newest := recorded[len(recorded)-1].Seq

		resumed := observeFrom(t, sess, &facade.Cursor{Epoch: full.State.Epoch, Sequence: 0})
		gap, ok := awaitEvent(t, resumed.Events, anyEvent).Payload.(facade.Gap)
		requireTrue(t, ok, "resuming before the retained window reports a gap, not replay")
		assertTrue(t, gap.FirstAvailable > 1, "the gap names a sequence after the evicted prefix")

		// The gap names the first retained sequence. A retained tail replays exactly, in order.
		// If nothing recorded is retained, replay resumes only after the newest recorded event.
		if tail := eventsFrom(recorded, gap.FirstAvailable); len(tail) > 0 {
			for _, expected := range tail {
				got := awaitEvent(t, resumed.Events, anyEvent)
				assertEqual(t, describeEvent(expected), describeEvent(got), "the retained tail replays exactly")
			}
		} else {
			fx.Model.Push(Script{Text: "newer"})
			newer := runTurn(t, sess, facade.Input{Content: "two"})
			drain(t, newer.Events())
			next := awaitEvent(t, resumed.Events, anyEvent)
			assertTrue(t, next.Seq > newest, "nothing recorded is retained, so replay resumes after the newest event")
		}

		// The gap requires a re-snapshot, which is authoritative about the cursor.
		snapshot := observeFrom(t, sess, nil)
		assertTrue(t, snapshot.State.Cursor >= newest, "a fresh snapshot reports a cursor at or after the newest event")
	})
	t.Run("ObserverCloseDetachesWithoutCancellingWork", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		if _, ok := sess.Observer(); !ok {
			t.Skip("observer capability is not advertised")
		}
		observation := observeFrom(t, sess, nil)
		block, entered := make(chan struct{}), make(chan struct{})
		fx.Model.Push(Script{Text: "finished", Block: block, Entered: entered})
		turn := runTurn(t, sess, facade.Input{Content: "work"})
		awaitEntered(t, entered)
		observation.Close()
		awaitClosed(t, observation.Events)
		close(block)
		outcome := wait(t, turn)
		assertEqual(t, facade.OutcomeCompleted, outcome.Kind, "closing an observation never cancels the turn")
	})

	t.Run("BackgroundReportsExactChildRun", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		bg, ok := sess.Background()
		if !ok {
			t.Skip("background capability is not advertised")
		}
		ctx := t.Context()
		rootID := sess.Info().SessionID
		sub, err := bg.Subscribe(ctx)
		requireNoError(t, err)

		fx.Model.Push(Script{ToolCall: ToolSpawnSubagent, ToolArgs: `{"agent":"writer","task":"investigate"}`}, Script{Text: "parent done"})
		fx.Worker.Push(Script{Text: "child done"})
		turn := runTurn(t, sess, facade.Input{Content: "delegate"})
		assertEqual(t, facade.OutcomeCompleted, wait(t, turn).Kind)
		drain(t, turn.Events())

		var statuses []facade.SubagentStatus
		var usages []*runtime.TokenUsageEvent
		child := ""
		terminal := func(state string) bool {
			return state == "idle" || state == "completed" || state == "failed" || state == "stopped"
		}
		timeout := time.After(waitTimeout)
		for done := false; !done; {
			select {
			case ev, open := <-sub.Events:
				requireTrue(t, open, "the background stream stayed open until cancelled")
				switch payload := ev.Payload.(type) {
				case facade.SubagentStatus:
					statuses = append(statuses, payload)
					if payload.ChildSessionID != "" {
						child = payload.ChildSessionID
					}
					done = child != "" && terminal(payload.State) && len(usages) > 0
				case facade.Passthrough:
					if usage, ok := payload.Event.(*runtime.TokenUsageEvent); ok {
						usages = append(usages, usage)
					}
				}
			case <-timeout:
				t.Fatal("timed out waiting for the child run to settle")
			}
		}
		requireNotEmptyString(t, child)

		// The child's lifecycle as the root observes it: the tree snapshot records the node
		// idle, admission reports starting, the child runs, then returns to idle.
		var trace []string
		for _, status := range statuses {
			if status.ChildSessionID == child {
				assertEqual(t, rootID, status.ParentSessionID, "the child reports the root as its parent")
				trace = append(trace, status.State)
			}
		}
		// Admission and the creation-time idle snapshot race, so only the multiset and the final state are fixed.
		sorted := slices.Clone(trace)
		slices.Sort(sorted)
		assertEqual(t, []string{"idle", "idle", "running", "starting"}, sorted, fmt.Sprintf("the child's lifecycle is exact: %v", trace))
		assertEqual(t, "idle", trace[len(trace)-1], "the child settles idle")
		assertEqual(t, WriterAgent, statuses[len(statuses)-1].Agent, "the terminal status names the child's agent")

		var childUsage []*runtime.TokenUsageEvent
		for _, usage := range usages {
			assertNotEqual(t, rootID, usage.SessionID, "background usage excludes the root session")
			if usage.SessionID == child {
				childUsage = append(childUsage, usage)
			}
		}
		requireLen(t, childUsage, 1, "the child made one model call")
		assertEqual(t, int64(1), childUsage[0].Usage.InputTokens)
		assertEqual(t, int64(1), childUsage[0].Usage.OutputTokens)

		sub.Close()
		awaitClosed(t, sub.Events)

		// The child is a subagent: it cannot switch agents, and the facade says so.
		childSess, err := fx.Host.Open(ctx, child)
		requireNoError(t, err)
		_, switchable := childSess.AgentSwitch()
		assertFalse(t, switchable, "a child session keeps its creation agent")
		assertFalse(t, childSess.Info().Capabilities.AgentSwitch)
		requireNoError(t, childSess.Close(ctx))
	})

	t.Run("ModelsAndThinkingRoundTrip", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		models, ok := sess.Models()
		if !ok {
			t.Skip("models capability is not advertised")
		}
		ctx := t.Context()
		before := sess.Info()
		writerRef, rootRef := modelRef(t, models, "gpt-4o"), modelRef(t, models, "gpt-5")

		requireNoError(t, models.Set(ctx, writerRef))
		after := sess.Info()
		assertNotEqual(t, before.Model, after.Model, "switching the model changes the reported model")
		assertTrue(t, strings.Contains(after.Model, "gpt-4o"), "the reported model follows the switch")
		_, thinking := sess.Thinking()
		assertFalse(t, thinking, "a model without thinking levels hides the thinking capability")
		assertFalse(t, after.Capabilities.Thinking)

		requireNoError(t, models.Set(ctx, rootRef))
		th, ok := sess.Thinking()
		requireTrue(t, ok, "the reasoning model exposes thinking levels")
		levels := th.Levels(ctx)
		requireNotEmpty(t, levels)
		top := levels[len(levels)-1]
		applied, err := th.Set(ctx, top)
		requireNoError(t, err)
		assertEqual(t, top, applied)
		assertEqual(t, applied, th.Current(ctx))
		cycled, err := th.Cycle(ctx)
		requireNoError(t, err)
		assertTrue(t, slices.Contains(levels, cycled), "cycling stays within the supported levels")
	})

	t.Run("AgentSwitchKeepsIdentityAndHistory", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		switcher, ok := sess.AgentSwitch()
		if !ok {
			t.Skip("agent switching is not advertised")
		}
		ctx := t.Context()
		obs := observe(t, sess)
		before := sess.Info()
		assertEqual(t, RootAgent, before.AgentName)
		assertTrue(t, before.Capabilities.Thinking, "the root's reasoning model exposes thinking levels")
		sessionID := before.SessionID

		fx.Model.Push(Script{Text: "first answer"})
		first := runTurn(t, sess, facade.Input{Content: "first question"})
		assertEqual(t, facade.OutcomeCompleted, wait(t, first).Kind)
		drain(t, first.Events())
		historyBefore := sessionMessages(t, sess)
		assertTrue(t, hasUserMessage(historyBefore, "first question"), "the first turn is recorded")

		// A running turn rejects the switch with ErrBusy and changes nothing.
		block, entered := make(chan struct{}), make(chan struct{})
		fx.Model.Push(Script{Text: "held", Block: block, Entered: entered})
		busy := runTurn(t, sess, facade.Input{Content: "hold the turn"})
		awaitStarted(t, obs, busy.ID())
		awaitEntered(t, entered)
		assertErrorIs(t, switcher.SwitchAgent(ctx, WriterAgent), facade.ErrBusy)
		assertEqual(t, before.AgentName, sess.Info().AgentName, "a rejected switch keeps the agent")
		close(block)
		assertEqual(t, facade.OutcomeCompleted, wait(t, busy).Kind)
		drain(t, busy.Events())

		// An unknown agent is rejected with ErrUnknownAgent and changes nothing.
		assertErrorIs(t, switcher.SwitchAgent(ctx, "nobody"), facade.ErrUnknownAgent)
		assertEqual(t, before.AgentName, sess.Info().AgentName)

		// Switching to the active agent is a no-op: no agent-info event follows the busy turn.
		awaitEvent(t, obs.Events, func(ev facade.Event) bool { return ev.TurnID == busy.ID() && isTurnSettled(ev) })
		requireNoError(t, switcher.SwitchAgent(ctx, RootAgent))
		assertNoEvent(t, obs.Events, agentInfoFor(RootAgent))

		requireNoError(t, switcher.SwitchAgent(ctx, WriterAgent))
		after := sess.Info()
		assertEqual(t, sessionID, after.SessionID, "the same session keeps its identity")
		assertEqual(t, WriterAgent, after.AgentName, "the reported agent follows the switch")
		assertFalse(t, after.Capabilities.Thinking, "the writer's model exposes no thinking levels")
		_, thinking := sess.Thinking()
		assertFalse(t, thinking, "the thinking accessor follows the live capability")
		assertTrue(t, after.Capabilities.Models, "model choices are still scoped to the bound agent")

		history := sessionMessages(t, sess)
		assertTrue(t, hasUserMessage(history, "first question"), "history survives the switch")
		assertGreaterOrEqual(t, len(history.GetAllMessages()), len(historyBefore.GetAllMessages()))

		// The switch is persisted: a fresh open of the same session reports the new agent.
		reopened, err := fx.Host.Open(ctx, sessionID)
		requireNoError(t, err)
		assertEqual(t, WriterAgent, reopened.Info().AgentName, "the switch survives a reopen")
		requireNoError(t, reopened.Close(ctx))

		awaitEvent(t, obs.Events, agentInfoFor(WriterAgent))

		fx.Worker.Push(Script{Text: "second answer"})
		second := runTurn(t, sess, facade.Input{Content: "second question"})
		events := drain(t, second.Events())
		assertEqual(t, facade.OutcomeCompleted, wait(t, second).Kind)
		assertTrue(t, anyEventFrom(events, WriterAgent), "the next turn runs as the new agent")
	})

	t.Run("Lifecycle", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		info := sess.Info()
		requireNotEmptyString(t, info.SessionID)
		assertEqual(t, "root", info.AgentName)

		ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
		defer cancel()
		page, err := fx.Host.List(ctx, facade.ListOptions{})
		requireNoError(t, err)
		assertTrue(t, containsSession(page, info.SessionID), "created session should be listed")

		opened, err := fx.Host.Open(ctx, info.SessionID)
		requireNoError(t, err)
		assertEqual(t, info.SessionID, opened.Info().SessionID)
		requireNoError(t, opened.Close(ctx))

		requireNoError(t, fx.Host.Delete(ctx, info.SessionID))
		page, err = fx.Host.List(ctx, facade.ListOptions{})
		requireNoError(t, err)
		assertFalse(t, containsSession(page, info.SessionID), "deleted session should not be listed")
	})
	t.Run("EventsCloseOnceAtSettlement", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		fx.Model.Push(Script{Text: "hello"})
		turn := runTurn(t, sess, facade.Input{Content: "hi"})
		events := drain(t, turn.Events())

		requireNotEmpty(t, events)
		var settled int
		for _, ev := range events {
			if isTurnSettled(ev) {
				settled++
			}
			assertEqual(t, turn.ID(), ev.TurnID)
		}
		assertEqual(t, 1, settled, "exactly one settlement")
		assertTrue(t, isTurnSettled(events[len(events)-1]), "settlement is the last event")
		startedAt := -1
		for i, ev := range events {
			if _, ok := ev.Payload.(facade.TurnStarted); ok {
				startedAt = i
			}
		}
		assertGreaterOrEqual(t, startedAt, 0, "turn reports TurnStarted before settlement")
		outcome := wait(t, turn)
		assertEqual(t, facade.OutcomeCompleted, outcome.Kind)
		// The stream is closed; a second read must not block or yield another event.
		_, open := <-turn.Events()
		assertFalse(t, open)
	})
	t.Run("ConsumerNotReadingDoesNotBlockSettlement", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		hold := make(chan struct{})
		fx.Model.Push(Script{Text: "done", Hold: hold})
		turn := runTurn(t, sess, facade.Input{Content: "hi"})
		close(hold)
		// Settle without reading any event.
		outcome := wait(t, turn)
		assertEqual(t, facade.OutcomeCompleted, outcome.Kind)
		events := drain(t, turn.Events())
		requireNotEmpty(t, events)
		assertTrue(t, isTurnSettled(events[len(events)-1]))
	})
	t.Run("DetachedContextDoesNotCancel", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		hold := make(chan struct{})
		fx.Model.Push(Script{Text: "still running", Hold: hold})
		ctx, cancel := context.WithCancel(t.Context())
		turn, err := sess.Run(ctx, facade.Input{Content: "hi"})
		requireNoError(t, err)
		cancel()
		close(hold)
		outcome := wait(t, turn)
		assertEqual(t, facade.OutcomeCompleted, outcome.Kind, "cancelling the admission context must not cancel the turn")
		drain(t, turn.Events())
	})
	t.Run("CancelOutcomes", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		hold := make(chan struct{})
		defer close(hold)
		fx.Model.Push(Script{Text: "never", Hold: hold})
		turn := runTurn(t, sess, facade.Input{Content: "hi"})
		ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
		defer cancel()
		first, err := turn.Cancel(ctx)
		requireNoError(t, err)
		assertEqual(t, facade.CancelAccepted, first)
		second, err := turn.Cancel(ctx)
		requireNoError(t, err)
		assertOneOf(t, second, facade.CancelAlreadyCancelling, facade.CancelNotActive)
		outcome := wait(t, turn)
		assertEqual(t, facade.OutcomeCanceled, outcome.Kind)
		drain(t, turn.Events())
		after, err := turn.Cancel(ctx)
		requireNoError(t, err)
		assertEqual(t, facade.CancelNotActive, after, "cancel after settlement is not_active")
	})
	t.Run("ConfirmationRoundTrip", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		fx.Model.Push(Script{ToolCall: ToolNeedsApproval}, Script{Text: "done"})
		turn := runTurn(t, sess, facade.Input{Content: "use the tool"})
		ev := awaitEvent(t, turn.Events(), isInteraction(facade.InteractionConfirmation))
		req := ev.Payload.(facade.InteractionRequested)
		requireNotEmptyString(t, req.InteractionID)
		requireNotNil(t, req.Confirmation)
		assertEqual(t, ToolNeedsApproval, req.Confirmation.ToolCall.Function.Name)

		ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
		defer cancel()
		requireNoError(t, sess.Respond(ctx, facade.Response{InteractionID: req.InteractionID, Decision: &facade.Decision{Type: facade.DecisionApprove}}))
		outcome := wait(t, turn)
		assertEqual(t, facade.OutcomeCompleted, outcome.Kind)
		events := drain(t, turn.Events())
		assertEqual(t, 1, countResolved(events, req.InteractionID), "exactly one InteractionResolved")

		// A resolved interaction is stale, not unknown.
		err := sess.Respond(ctx, facade.Response{InteractionID: req.InteractionID, Decision: &facade.Decision{Type: facade.DecisionApprove}})
		assertErrorIs(t, err, facade.ErrStaleInteraction)
	})
	t.Run("InvalidResponsesRejected", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
		defer cancel()
		err := sess.Respond(ctx, facade.Response{InteractionID: "no-such-interaction", Decision: &facade.Decision{Type: facade.DecisionApprove}})
		assertErrorIs(t, err, facade.ErrUnknownInteraction)
		err = sess.Respond(ctx, facade.Response{Decision: &facade.Decision{Type: facade.DecisionApprove}})
		assertErrorIs(t, err, facade.ErrNotActive, "an empty id with nothing outstanding is not active")
		err = sess.Respond(ctx, facade.Response{InteractionID: "x"})
		assertErrorIs(t, err, facade.ErrInvalidResponse, "a response must carry a decision or an elicitation")
	})
	t.Run("MaxIterationsRoundTrip", func(t *testing.T) {
		fx := fixture(t, factory, Config{MaxIterations: 1})
		sess := newSession(t, fx)
		fx.Model.Push(Script{ToolCall: ToolNoop}, Script{Text: "after"})
		turn := runTurn(t, sess, facade.Input{Content: "loop"})
		ev := awaitEvent(t, turn.Events(), isInteraction(facade.InteractionMaxIterations))
		req := ev.Payload.(facade.InteractionRequested)
		assertEqual(t, 1, req.MaxIterations)
		ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
		defer cancel()
		requireNoError(t, sess.Respond(ctx, facade.Response{InteractionID: req.InteractionID, Decision: &facade.Decision{Type: facade.DecisionReject, Reason: "enough"}}))
		outcome := wait(t, turn)
		assertEqual(t, facade.OutcomeCompleted, outcome.Kind)
		events := drain(t, turn.Events())
		assertEqual(t, 1, countResolved(events, req.InteractionID))
	})
	t.Run("ElicitationRoundTrip", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		fx.Model.Push(Script{ToolCall: ToolElicit}, Script{Text: "thanks"})
		turn := runTurn(t, sess, facade.Input{Content: "ask me"})
		ev := awaitEvent(t, turn.Events(), isInteraction(facade.InteractionElicitation))
		req := ev.Payload.(facade.InteractionRequested)
		requireNotNil(t, req.Elicitation)
		requireNotEmptyString(t, req.Elicitation.ElicitationID)
		ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
		defer cancel()
		requireNoError(t, sess.Respond(ctx, facade.Response{InteractionID: req.InteractionID, Elicitation: &facade.ElicitationReply{Action: tools.ElicitationActionAccept, Content: map[string]any{"colour": "blue"}}}))
		outcome := wait(t, turn)
		assertEqual(t, facade.OutcomeCompleted, outcome.Kind)
		events := drain(t, turn.Events())
		assertEqual(t, 1, countResolved(events, req.InteractionID), "exactly one InteractionResolved")
	})
	t.Run("SteerAndWithdraw", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		queue, ok := sess.Queue()
		requireTrue(t, ok)
		withdraw, ok := sess.Withdraw()
		requireTrue(t, ok)
		block, entered := make(chan struct{}), make(chan struct{})
		fx.Model.Push(Script{ToolCall: ToolNoop, Block: block, Entered: entered}, Script{Text: "done"})
		obs := observe(t, sess)
		turn := runTurn(t, sess, facade.Input{Content: "start"})
		awaitStarted(t, obs, turn.ID())
		awaitEntered(t, entered)

		ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
		defer cancel()
		// A follow-up waits behind the active turn, so it stays pending until withdrawn.
		ticket, err := queue.FollowUp(ctx, facade.Input{Content: "withdrawn follow-up"})
		requireNoError(t, err)
		requireNotEmptyString(t, ticket.ID)
		awaitEvent(t, obs.Events, queueAccepted(ticket.ID))
		outcome, err := withdraw.Withdraw(ctx, ticket)
		requireNoError(t, err)
		assertEqual(t, facade.WithdrawWithdrawn, outcome)
		awaitEvent(t, obs.Events, queueWithdrawn(ticket.ID))
		again, err := withdraw.Withdraw(ctx, ticket)
		requireNoError(t, err)
		assertEqual(t, facade.WithdrawNotWithdrawn, again, "a withdrawn ticket cannot be withdrawn twice")

		consumed, err := queue.Steer(ctx, facade.Input{Content: "consumed steer"})
		requireNoError(t, err)
		close(block)
		awaitEvent(t, obs.Events, queueConsumed(consumed.ID))
		after, err := withdraw.Withdraw(ctx, consumed)
		requireNoError(t, err)
		assertEqual(t, facade.WithdrawConsumed, after)
		assertEqual(t, facade.OutcomeCompleted, wait(t, turn).Kind)
	})
	t.Run("FollowUpRunsAsOwnTurn", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		queue, ok := sess.Queue()
		requireTrue(t, ok)
		hold := make(chan struct{})
		fx.Model.Push(Script{Text: "first", Hold: hold}, Script{Text: "second"})
		obs := observe(t, sess)
		turn := runTurn(t, sess, facade.Input{Content: "first"})
		awaitStarted(t, obs, turn.ID())

		ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
		defer cancel()
		ticket, err := queue.FollowUp(ctx, facade.Input{Content: "second"})
		requireNoError(t, err)
		close(hold)
		assertEqual(t, facade.OutcomeCompleted, wait(t, turn).Kind)
		settled := awaitEvent(t, obs.Events, func(ev facade.Event) bool {
			return ev.TurnID == ticket.ID && isTurnSettled(ev)
		})
		assertEqual(t, facade.OutcomeCompleted, settled.Payload.(facade.TurnSettled).Outcome.Kind)
	})
	t.Run("PendingEditIsCompareAndSwap", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		queue, ok := sess.Queue()
		requireTrue(t, ok)
		pending, ok := sess.Pending()
		requireTrue(t, ok)
		block, entered := make(chan struct{}), make(chan struct{})
		defer close(block)
		fx.Model.Push(Script{Text: "busy", Block: block, Entered: entered})
		obs := observe(t, sess)
		turn := runTurn(t, sess, facade.Input{Content: "busy"})
		awaitStarted(t, obs, turn.ID())
		awaitEntered(t, entered)
		ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
		defer cancel()
		// A follow-up waits behind the blocked turn, so it stays pending for the whole scenario.
		ticket, err := queue.FollowUp(ctx, facade.Input{Content: "draft"})
		requireNoError(t, err)
		// The accepted event marks durable acceptance, after which the entry is editable.
		awaitEvent(t, obs.Events, queueAccepted(ticket.ID))

		list, err := pending.List(ctx)
		requireNoError(t, err)
		requireLen(t, list, 1)
		assertEqual(t, ticket.ID, list[0].Ticket.ID)

		outcome, err := pending.Edit(ctx, ticket, "wrong expectation", "edited")
		requireNoError(t, err)
		assertEqual(t, facade.EditStale, outcome)
		outcome, err = pending.Edit(ctx, ticket, "draft", "edited")
		requireNoError(t, err)
		assertEqual(t, facade.EditEdited, outcome)
		_ = turn
	})
	t.Run("SnapshotIsClone", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
		defer cancel()
		first, err := sess.Snapshot(ctx)
		requireNoError(t, err)
		before := len(first.GetAllMessages())
		first.Title = "mutated by consumer"
		first.AddMessage(session.UserMessage("injected"))
		second, err := sess.Snapshot(ctx)
		requireNoError(t, err)
		assertEqual(t, before, len(second.GetAllMessages()), "mutating a snapshot must not change the session")
		assertNotEqual(t, "mutated by consumer", second.Title)
	})
	t.Run("SeqMonotonicPerSession", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		obs := observe(t, sess)
		fx.Model.Push(Script{ToolCall: ToolNoop}, Script{Text: "two"})
		turn := runTurn(t, sess, facade.Input{Content: "one"})
		events := drain(t, turn.Events())
		requireNotEmpty(t, events)
		assertEqual(t, facade.OutcomeCompleted, wait(t, turn).Kind)
		// Observed events for the same session never decrease in sequence, from
		// the observation's start through this turn's settlement.
		var seqs []uint64
		settled := false
		timeout := time.After(waitTimeout)
		for !settled {
			select {
			case ev := <-obs.Events:
				seqs = append(seqs, ev.Seq)
				settled = ev.TurnID == turn.ID() && isTurnSettled(ev)
			case <-timeout:
				t.Fatal("observer did not report the settlement")
			}
		}
		for i := 1; i < len(seqs); i++ {
			assertGreaterOrEqual(t, seqs[i], seqs[i-1], "seq must be non-decreasing")
		}
		requireNotZero(t, seqs[len(seqs)-1])
	})
	t.Run("CapabilityTruthfulness", func(t *testing.T) {
		fx := fixture(t, factory, Config{})
		sess := newSession(t, fx)
		caps := sess.Info().Capabilities
		ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
		defer cancel()

		type check struct {
			name    string
			present bool
			ok      bool
		}
		q, qok := sess.Queue()
		w, wok := sess.Withdraw()
		p, pok := sess.Pending()
		c, cok := sess.Compact()
		pa, paok := sess.Pause()
		o, ook := sess.Observer()
		tr, trok := sess.SubagentTree()
		st, stok := sess.StopSubtree()
		td, tdok := sess.TodoEditor()
		bg, bgok := sess.Background()
		mdl, mok := sess.Models()
		thk, thok := sess.Thinking()
		swt, swok := sess.AgentSwitch()
		checks := []check{
			{"Queue", caps.Queue, qok},
			{"Withdraw", caps.Withdraw, wok},
			{"Pending", caps.Pending, pok},
			{"Compact", caps.Compact, cok},
			{"Pause", caps.Pause, paok},
			{"Observer", caps.Observer, ook},
			{"SubagentTree", caps.SubagentTree, trok},
			{"StopSubtree", caps.StopSubtree, stok},
			{"TodoEditor", caps.TodoEditor, tdok},
			{"Background", caps.Background, bgok},
			{"Models", caps.Models, mok},
			{"Thinking", caps.Thinking, thok},
			{"AgentSwitch", caps.AgentSwitch, swok},
		}
		for _, ch := range checks {
			assertEqual(t, ch.present, ch.ok, fmt.Sprintf("Info().Capabilities.%s must match its accessor", ch.name))
		}

		// Every present capability must be exercised without an unsupported error.
		noUnsupported := func(name string, err error) {
			if err != nil {
				assertFalse(t, errors.Is(err, facade.ErrUnsupported), fmt.Sprintf("%s returned unsupported for an advertised capability: %v", name, err))
			}
		}
		if qok {
			_, err := q.Steer(ctx, facade.Input{Content: "idle steer"})
			noUnsupported("Queue.Steer", err)
		}
		if wok {
			_, err := w.Withdraw(ctx, facade.Ticket{ID: "missing"})
			noUnsupported("Withdraw.Withdraw", err)
		}
		if pok {
			_, err := p.List(ctx)
			noUnsupported("Pending.List", err)
		}
		if cok {
			noUnsupported("Compact.Compact", c.Compact(ctx, ""))
		}
		if paok {
			_, err := pa.TogglePause(ctx)
			noUnsupported("Pause.TogglePause", err)
			_, err = pa.TogglePause(ctx)
			noUnsupported("Pause.TogglePause", err)
		}
		if ook {
			observation, err := o.Observe(ctx, nil)
			noUnsupported("Observer.Observe", err)
			if err == nil {
				observation.Close()
			}
		}
		if trok {
			_, err := tr.Tree(ctx)
			noUnsupported("SubagentTree.Tree", err)
		}
		if stok {
			noUnsupported("StopSubtree.StopSubtree", st.StopSubtree(ctx))
		}
		if tdok {
			_, err := td.SetStatus(ctx, "missing", "completed")
			noUnsupported("TodoEditor.SetStatus", err)
		}
		if bgok {
			sub, err := bg.Subscribe(ctx)
			noUnsupported("Background.Subscribe", err)
			if err == nil {
				sub.Close()
			}
		}
		if mok {
			_ = mdl.Available(ctx)
		}
		if thok {
			levels := thk.Levels(ctx)
			if len(levels) > 0 {
				_, err := thk.Set(ctx, levels[0])
				noUnsupported("Thinking.Set", err)
			}
		}
		if swok {
			noUnsupported("AgentSwitch.SwitchAgent", swt.SwitchAgent(ctx, sess.Info().AgentName))
		}
		status, err := sess.Status(ctx)
		requireNoError(t, err)
		assertFalse(t, status.Busy)
	})
}

// fixture builds one scenario's fixture. It is a helper so scenario bodies start with their own assertions.
func fixture(t *testing.T, factory Factory, cfg Config) Fixture {
	t.Helper()
	return factory(t, cfg)
}

func newSession(t *testing.T, fx Fixture) facade.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
	defer cancel()
	sess, err := fx.Host.Create(ctx, facade.CreateOptions{AgentName: "root"})
	requireNoError(t, err)
	t.Cleanup(func() { _ = sess.Close(context.WithoutCancel(t.Context())) })
	return sess
}

func runTurn(t *testing.T, sess facade.Session, input facade.Input) facade.Turn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
	defer cancel()
	turn, err := sess.Run(ctx, input)
	requireNoError(t, err)
	return turn
}

// awaitEvent returns the first event from ch matching pred, or fails the test.
func awaitEvent(t *testing.T, ch <-chan facade.Event, pred func(facade.Event) bool) facade.Event {
	t.Helper()
	timeout := time.After(waitTimeout)
	for {
		select {
		case ev, ok := <-ch:
			requireTrue(t, ok, "event stream closed before the expected event")
			if pred(ev) {
				return ev
			}
		case <-timeout:
			t.Fatal("timed out waiting for event")
		}
	}
}

// drain reads ch until it closes and returns everything received.
func drain(t *testing.T, ch <-chan facade.Event) []facade.Event {
	t.Helper()
	var out []facade.Event
	timeout := time.After(waitTimeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-timeout:
			t.Fatal("event stream did not close after settlement")
		}
	}
}

// awaitEntered blocks until the scripted model call has begun, so queue operations
// are ordered after the call's admission rather than racing it.
func awaitEntered(tb testing.TB, entered <-chan struct{}) {
	tb.Helper()
	select {
	case <-entered:
	case <-time.After(waitTimeout):
		tb.Fatal("the scripted model call never started")
	}
}

// awaitStarted blocks until the observation reports the turn as started, so
// queue operations are issued against a running turn rather than racing its start.
func awaitStarted(t *testing.T, obs facade.Observation, turnID string) {
	t.Helper()
	awaitEvent(t, obs.Events, func(ev facade.Event) bool {
		_, started := ev.Payload.(facade.TurnStarted)
		return started && ev.TurnID == turnID
	})
}

func wait(t *testing.T, turn facade.Turn) facade.Outcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
	defer cancel()
	outcome, err := turn.Wait(ctx)
	requireNoError(t, err)
	return outcome
}

func isTurnSettled(ev facade.Event) bool {
	_, ok := ev.Payload.(facade.TurnSettled)
	return ok
}

func isInteraction(kind facade.InteractionKind) func(facade.Event) bool {
	return func(ev facade.Event) bool {
		req, ok := ev.Payload.(facade.InteractionRequested)
		return ok && req.Kind == kind
	}
}

func observe(t *testing.T, sess facade.Session) facade.Observation {
	t.Helper()
	obs, ok := sess.Observer()
	requireTrue(t, ok, "observer capability is required by the suite")
	observation, err := obs.Observe(t.Context(), nil)
	requireNoError(t, err)
	t.Cleanup(observation.Close)
	return observation
}

func containsSession(page facade.Page, id string) bool {
	for _, s := range page.Sessions {
		if s.SessionID == id {
			return true
		}
	}
	return false
}

func countResolved(events []facade.Event, id string) int {
	n := 0
	for _, ev := range events {
		if r, ok := ev.Payload.(facade.InteractionResolved); ok && r.InteractionID == id {
			n++
		}
	}
	return n
}

func queueAccepted(id string) func(facade.Event) bool {
	return func(ev facade.Event) bool {
		p, ok := ev.Payload.(facade.QueueAccepted)
		return ok && p.Ticket.ID == id
	}
}

func queueWithdrawn(id string) func(facade.Event) bool {
	return func(ev facade.Event) bool {
		p, ok := ev.Payload.(facade.QueueWithdrawn)
		return ok && p.Ticket.ID == id
	}
}

func queueConsumed(id string) func(facade.Event) bool {
	return func(ev facade.Event) bool {
		p, ok := ev.Payload.(facade.QueueConsumed)
		return ok && p.Ticket.ID == id
	}
}

// modelRef returns the switch reference of the configured model called name.
func modelRef(t *testing.T, models facade.Models, name string) string {
	t.Helper()
	for _, choice := range models.Available(t.Context()) {
		if choice.Name == name {
			return choice.Ref
		}
	}
	t.Fatalf("model %q is not available", name)
	return ""
}

// sessionMessages returns a clone of the session's transcript.
func sessionMessages(t *testing.T, sess facade.Session) *session.Session {
	t.Helper()
	snapshot, err := sess.Snapshot(t.Context())
	requireNoError(t, err)
	return snapshot
}

func hasUserMessage(sess *session.Session, text string) bool {
	for _, msg := range sess.GetAllMessages() {
		if msg.Message.Role == "user" && msg.Message.Content == text {
			return true
		}
	}
	return false
}

// agentInfoFor matches the agent-info event that announces an active agent.
func agentInfoFor(name string) func(facade.Event) bool {
	return func(ev facade.Event) bool {
		p, ok := ev.Payload.(facade.Passthrough)
		if !ok {
			return false
		}
		_, info := p.Event.(*runtime.AgentInfoEvent)
		return info && p.Event.GetAgentName() == name
	}
}

// anyEventFrom reports whether any passthrough event was produced by the named agent.
func anyEventFrom(events []facade.Event, name string) bool {
	for _, ev := range events {
		if p, ok := ev.Payload.(facade.Passthrough); ok && p.Event.GetAgentName() == name {
			return true
		}
	}
	return false
}

// describeEvent renders an event's identity and payload kind for exact comparison.
func describeEvent(ev facade.Event) string {
	switch p := ev.Payload.(type) {
	case facade.Passthrough:
		return fmt.Sprintf("%d|%s|passthrough:%T", ev.Seq, ev.TurnID, p.Event)
	case facade.TurnSettled:
		return fmt.Sprintf("%d|%s|settled:%s", ev.Seq, ev.TurnID, p.Outcome.Kind)
	case facade.Gap:
		return fmt.Sprintf("%d|%s|gap:%d", ev.Seq, ev.TurnID, p.FirstAvailable)
	default:
		return fmt.Sprintf("%d|%s|%T", ev.Seq, ev.TurnID, ev.Payload)
	}
}

func anyEvent(facade.Event) bool { return true }

// drainObserved reads an observation until turnID settles and returns every event read.
func drainObserved(t *testing.T, ch <-chan facade.Event, turnID string) []facade.Event {
	t.Helper()
	var out []facade.Event
	timeout := time.After(waitTimeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("the observation closed before the turn settled")
			}
			out = append(out, ev)
			if ev.TurnID == turnID && isTurnSettled(ev) {
				return out
			}
		case <-timeout:
			t.Fatal("timed out waiting for the turn to settle")
		}
	}
}

// eventsFrom returns the events at or after seq, in order.
func eventsFrom(events []facade.Event, seq uint64) []facade.Event {
	var out []facade.Event
	for _, ev := range events {
		if ev.Seq >= seq {
			out = append(out, ev)
		}
	}
	return out
}

// observeFrom opens an observation from the cursor (nil for a snapshot plus tail).
func observeFrom(t *testing.T, sess facade.Session, since *facade.Cursor) facade.Observation {
	t.Helper()
	obs, ok := sess.Observer()
	requireTrue(t, ok, "the observer capability is required by this scenario")
	observation, err := obs.Observe(t.Context(), since)
	requireNoError(t, err)
	t.Cleanup(observation.Close)
	return observation
}

// awaitClosed waits until ch is closed, discarding anything delivered first.
func awaitClosed(t *testing.T, ch <-chan facade.Event) {
	t.Helper()
	timeout := time.After(waitTimeout)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("the stream did not close")
		}
	}
}

// assertNoEvent fails if an event matching pred arrives within a short window.
func assertNoEvent(t *testing.T, ch <-chan facade.Event, pred func(facade.Event) bool) {
	t.Helper()
	window := time.After(200 * time.Millisecond)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if pred(ev) {
				t.Fatalf("unexpected event %s", describeEvent(ev))
			}
		case <-window:
			return
		}
	}
}

// collectConsumed returns the tickets of the next n consumption events, in order.
func collectConsumed(t *testing.T, ch <-chan facade.Event, n int) []string {
	t.Helper()
	var ids []string
	timeout := time.After(waitTimeout)
	for len(ids) < n {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("the observation closed before every ticket was consumed")
			}
			if consumed, ok := ev.Payload.(facade.QueueConsumed); ok {
				ids = append(ids, consumed.Ticket.ID)
			}
		case <-timeout:
			t.Fatal("timed out waiting for queue consumption")
		}
	}
	return ids
}

// userMessagesIn returns, in transcript order, the user messages whose text is in texts.
func userMessagesIn(sess *session.Session, texts ...string) []string {
	var out []string
	for _, msg := range sess.GetAllMessages() {
		if msg.Message.Role == "user" && slices.Contains(texts, msg.Message.Content) {
			out = append(out, msg.Message.Content)
		}
	}
	return out
}

// summariesIn returns the compaction summaries recorded in the transcript, in order.
func summariesIn(sess *session.Session) []string {
	var out []string
	for _, item := range sess.ItemsSnapshot() {
		if item.Summary != "" {
			out = append(out, item.Summary)
		}
	}
	return out
}

func summaryEvent(text string) func(facade.Event) bool {
	return func(ev facade.Event) bool {
		p, ok := ev.Payload.(facade.Passthrough)
		if !ok {
			return false
		}
		summary, ok := p.Event.(*runtime.SessionSummaryEvent)
		return ok && summary.Summary == text
	}
}

func pausedEvent(ev facade.Event) bool {
	p, ok := ev.Payload.(facade.Passthrough)
	if !ok {
		return false
	}
	_, paused := p.Event.(*runtime.PausedEvent)
	return paused
}

func subagentCreated(ev facade.Event) bool {
	status, ok := ev.Payload.(facade.SubagentStatus)
	return ok && status.ChildSessionID != "" && status.State == "starting"
}

// spawnWriter has the root spawn the writer as an async subagent and returns the
// child's session ID as reported by the admission event.
func spawnWriter(t *testing.T, fx Fixture, sess facade.Session, obs facade.Observation) string {
	t.Helper()
	fx.Model.Push(Script{ToolCall: ToolSpawnSubagent, ToolArgs: `{"agent":"writer","task":"investigate"}`}, Script{Text: "parent done"})
	fx.Worker.Push(Script{Text: "child done"})
	turn := runTurn(t, sess, facade.Input{Content: "delegate"})
	assertEqual(t, facade.OutcomeCompleted, wait(t, turn).Kind)
	drain(t, turn.Events())
	created := awaitEvent(t, obs.Events, subagentCreated)
	return created.Payload.(facade.SubagentStatus).ChildSessionID
}

// findTreeNode searches the tree for the node that runs sessionID.
func findTreeNode(nodes []facade.TreeNode, sessionID string) (facade.TreeNode, bool) {
	for _, node := range nodes {
		if node.SessionID == sessionID {
			return node, true
		}
		if found, ok := findTreeNode(node.Children, sessionID); ok {
			return found, true
		}
	}
	return facade.TreeNode{}, false
}

// todoIDs maps each description to the ID the create call reported for it.
func todoIDs(events []facade.Event) map[string]string {
	ids := map[string]string{}
	for _, ev := range events {
		p, ok := ev.Payload.(facade.Passthrough)
		if !ok {
			continue
		}
		response, ok := p.Event.(*runtime.ToolCallResponseEvent)
		if !ok {
			continue
		}
		var created struct {
			Created []struct {
				ID          string `json:"id"`
				Description string `json:"description"`
			} `json:"created"`
		}
		if json.Unmarshal([]byte(response.Response), &created) != nil {
			continue
		}
		for _, todo := range created.Created {
			ids[todo.Description] = todo.ID
		}
	}
	return ids
}

func todoMap(list []facade.Todo) map[string]facade.Todo {
	out := make(map[string]facade.Todo, len(list))
	for _, todo := range list {
		out[todo.ID] = todo
	}
	return out
}

// childSettled matches the root observer's status for child once it is idle or completed.
func childSettled(child string) func(facade.Event) bool {
	return func(ev facade.Event) bool {
		status, ok := ev.Payload.(facade.SubagentStatus)
		return ok && status.ChildSessionID == child && (status.State == "idle" || status.State == "completed")
	}
}
