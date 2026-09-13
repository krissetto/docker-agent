package app

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func runSessionHandle(ctx context.Context, rt runtime.SessionRuntime, sess *session.Session) <-chan runtime.SessionEvent {
	events := make(chan runtime.SessionEvent, 64)
	go func() {
		defer close(events)
		handle, err := rt.CreateSession(ctx, sess, runtime.SessionBinding{AgentName: sess.AgentName})
		if err != nil {
			return
		}
		observation, err := handle.Observe(ctx, runtime.ObserveOptions{})
		if err != nil {
			return
		}
		defer observation.Cancel()
		go func() {
			<-ctx.Done()
			_ = handle.Release(context.WithoutCancel(ctx))
		}()
		if _, err := handle.Submit(ctx, runtime.TurnInput{}); err != nil {
			return
		}
		for event := range observation.Events {
			events <- event
			if _, stopped := event.Event.(*runtime.StreamStoppedEvent); stopped {
				return
			}
		}
	}()
	return events
}

type replyStream struct{ idx int }

func (s *replyStream) Recv() (chat.MessageStreamResponse, error) {
	defer func() { s.idx++ }()
	switch s.idx {
	case 0:
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{
			Delta: chat.MessageDelta{Content: "ok"},
		}}}, nil
	case 1:
		return chat.MessageStreamResponse{
			Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}},
			Usage:   &chat.Usage{InputTokens: 1, OutputTokens: 1},
		}, nil
	default:
		return chat.MessageStreamResponse{}, io.EOF
	}
}

func (s *replyStream) Close() {}

type replyProvider struct{ stubProvider }

func (replyProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return &replyStream{}, nil
}

func TestAttachedAppSteerTargetsChildSession(t *testing.T) {
	t.Parallel()

	tm := team.New(team.WithAgents(
		agent.New("root", "prompt", agent.WithModel(replyProvider{}), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "planner"})),
		agent.New("planner", "prompt", agent.WithModel(replyProvider{})),
	))
	rt, err := runtime.NewLocalRuntime(t.Context(), tm)
	require.NoError(t, err)
	child := session.New(
		session.WithID("child"),
		session.WithParentID("parent"),
		session.WithAgentName("planner"),
		session.WithNonInteractive(true),
	)
	// Run once to register the child with the session driver.
	for range runSessionHandle(t.Context(), rt, child) {
	}

	info := runtime.SubagentAttachInfo{NodeID: "abcde", Session: child, ParentSessionID: "parent"}
	a := New(t.Context(), rt, child, runtime.SessionBinding{}, WithRuntimeServices(rt), WithSubagentAttach(info))
	_, err = a.FollowUpMessage(t.Context(), "target child", []messages.Attachment{{Name: "note.txt", Content: "attached context"}})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		snapshot, snapshotErr := a.SessionHandle().Snapshot(t.Context())
		if snapshotErr != nil || snapshot == nil {
			return false
		}
		if snapshot.ID != child.ID {
			return false
		}
		for _, item := range snapshot.ItemsSnapshot() {
			if item.Message != nil && item.Message.Message.Content == "target child" && len(item.Message.Message.MultiContent) > 0 && strings.Contains(item.Message.Message.MultiContent[0].Text, "attached context") {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, 0, rt.QueueStatus().SteerDepth)
}

func TestIdleAttachedAppRunPreservesAttachments(t *testing.T) {
	t.Parallel()

	tm := team.New(team.WithAgents(
		agent.New("root", "prompt", agent.WithModel(replyProvider{}), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "planner"})),
		agent.New("planner", "prompt", agent.WithModel(replyProvider{})),
	))
	rt, err := runtime.NewLocalRuntime(t.Context(), tm)
	require.NoError(t, err)
	child := session.New(
		session.WithID("idle-child"),
		session.WithParentID("parent"),
		session.WithAgentName("planner"),
		session.WithNonInteractive(true),
	)
	for range runSessionHandle(t.Context(), rt, child) {
	}

	a := New(t.Context(), rt, child, runtime.SessionBinding{}, WithRuntimeServices(rt), WithSubagentAttach(runtime.SubagentAttachInfo{NodeID: "abcde", Session: child}))
	ctx, cancel := context.WithCancel(t.Context())
	a.Run(ctx, cancel, "idle message", []messages.Attachment{{Name: "note.txt", Content: "attached context"}})
	require.Eventually(t, func() bool {
		snapshot, snapshotErr := a.SessionHandle().Snapshot(t.Context())
		if snapshotErr != nil || snapshot == nil {
			return false
		}
		if snapshot.ID != child.ID {
			return false
		}
		for _, item := range snapshot.ItemsSnapshot() {
			if item.Message != nil && item.Message.Message.Content == "idle message" {
				return len(item.Message.Message.MultiContent) > 0 && strings.Contains(item.Message.Message.MultiContent[0].Text, "attached context")
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond)
}

// Every tab is a viewer of its session's event stream: a runtime-owned wake
// run (the session answering a subagent note while the tab is idle)
// renders on the App bus exactly like a turn the user started. This is the
// consolidation that removed the receiver-managed/session-managed split.
func TestWakeRunRendersOnAppBus(t *testing.T) {
	t.Parallel()

	tm := team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(replyProvider{}))))
	rt, err := runtime.NewLocalRuntime(t.Context(), tm)
	require.NoError(t, err)

	sess := session.New(session.WithID("viewer-sess"))
	a := New(t.Context(), rt, sess, runtime.SessionBinding{}, WithRuntimeServices(rt))
	a.Start(t.Context())

	events := make(chan any, 256)
	go a.SubscribeWith(t.Context(), func(msg tea.Msg) { events <- msg })
	waitForStartup(t, events)

	// One user-driven turn first (proves the bridge carries the App's own
	// runs — the classic path).
	runCtx, cancel := context.WithCancel(t.Context())
	a.Run(runCtx, cancel, "hello", nil)
	waitForStop(t, events, "the App's own turn must render on the bus")

	// A run this App did not start (stand-in for the session waking
	// the session on a subagent note — the wake mechanics themselves are
	// covered in pkg/runtime): the tab renders it live, no receiver wiring
	// anywhere. the session is the execution door, so this cannot race the
	// App's own run winding down.
	sess.AddMessage(session.UserMessage("<system_info>subagent report</system_info>"))
	go func() {
		for range runSessionHandle(context.WithoutCancel(t.Context()), rt, sess) {
		}
	}()
	waitForStop(t, events, "a runtime-owned run must render on the bus")
}

func waitForStop(t *testing.T, events <-chan any, msg string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case e := <-events:
			if _, ok := e.(*runtime.StreamStoppedEvent); ok {
				return
			}
		case <-deadline:
			t.Fatal(msg)
		}
	}
}

// waitForStartup drains events until the async startup-info emit finishes
// (it reads the session's usage, which a run writes — running earlier races
// EmitStartupInfo; same wait as the attach tests).
func waitForStartup(t *testing.T, events <-chan any) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case msg := <-events:
			if e, ok := msg.(*runtime.ToolsetInfoEvent); ok && !e.Loading {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for startup info")
		}
	}
}

// Retry and RunWithMessage must not feed the bus themselves when the bridge
// is active — the run's events arrive through the hub, and a second direct
// forward doubles every delta (the "mixed messages" regression).
func TestBridgedRetryDoesNotDuplicateEvents(t *testing.T) {
	t.Parallel()

	tm := team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(replyProvider{}))))
	rt, err := runtime.NewLocalRuntime(t.Context(), tm)
	require.NoError(t, err)

	sess := session.New(session.WithID("retry-sess"))
	sess.AddMessage(session.UserMessage("hello"))
	a := New(t.Context(), rt, sess, runtime.SessionBinding{}, WithRuntimeServices(rt))
	a.Start(t.Context())

	events := make(chan any, 256)
	go a.SubscribeWith(t.Context(), func(msg tea.Msg) { events <- msg })
	waitForStartup(t, events)

	a.Retry(t.Context(), func() {})

	var content strings.Builder
	deadline := time.After(10 * time.Second)
	for {
		select {
		case msg := <-events:
			switch e := msg.(type) {
			case *runtime.AgentChoiceEvent:
				content.WriteString(e.Content)
			case *runtime.StreamStoppedEvent:
				assert.Equal(t, "ok", content.String(), "each delta must reach the bus exactly once")
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for the retried turn")
		}
	}
}

// The cancel gate mutes exactly the cancelled run's tail: the run's
// stream-stop releases it, so a later run (a wake run, a retry) renders
// normally instead of vanishing behind a stale gate.
func TestRetrySuppressesReplayBeforeStartButPreservesSteerAfterStart(t *testing.T) {
	t.Parallel()
	a := &App{cancelledRequests: map[string]struct{}{}}
	a.suppressUserEcho.Store(true)
	assert.Nil(t, a.filterBridgedEvent("retry", runtime.UserMessage("original", "session", nil)), "retry replay before stream start is suppressed")
	assert.NotNil(t, a.filterBridgedEvent("retry", runtime.StreamStarted("session", "root")))
	steered := runtime.UserMessage("steered", "session", nil)
	assert.Same(t, steered, a.filterBridgedEvent("steer", steered), "post-start steered input remains visible")
}

func TestCancelGateReleasesOnOwnStreamStop(t *testing.T) {
	t.Parallel()
	a := &App{cancelledRequests: map[string]struct{}{}, latestRequestID: "old", projectedRequestID: "old"}
	a.MarkRunCancelled()

	assert.Nil(t, a.filterBridgedEvent("old", &runtime.AgentChoiceEvent{}), "tail events of the cancelled run are muted")
	assert.NotNil(t, a.filterBridgedEvent("old", &runtime.StreamStoppedEvent{}), "the stop must pass to clear its spinner")
	assert.NotNil(t, a.filterBridgedEvent("next", &runtime.AgentChoiceEvent{}), "the next run's events must render")
}

func TestStaleCancelledStopCannotClearNewRequestProjection(t *testing.T) {
	t.Parallel()

	for _, order := range []string{"prompt-before-start", "start-before-prompt"} {
		t.Run(order, func(t *testing.T) {
			t.Parallel()
			a := &App{cancelledRequests: map[string]struct{}{}, latestRequestID: "old", projectedRequestID: "old"}
			a.MarkRunCancelled()
			a.recordSubmission("new")

			projectNew := func() {
				assert.NotNil(t, a.filterBridgedEvent("new", &runtime.UserMessageEvent{Message: "next"}))
				assert.NotNil(t, a.filterBridgedEvent("new", &runtime.StreamStartedEvent{}))
			}
			if order == "prompt-before-start" {
				projectNew()
			} else {
				assert.NotNil(t, a.filterBridgedEvent("new", &runtime.StreamStartedEvent{}))
				assert.NotNil(t, a.filterBridgedEvent("new", &runtime.UserMessageEvent{Message: "next"}))
			}

			assert.Nil(t, a.filterBridgedEvent("old", &runtime.StreamStoppedEvent{}), "stale old stop must not stop new spinner")
			assert.NotNil(t, a.filterBridgedEvent("new", &runtime.AgentChoiceEvent{Content: "answer"}))
			assert.NotNil(t, a.filterBridgedEvent("new", &runtime.StreamStoppedEvent{}))
			assert.Empty(t, a.cancelledRequests)
		})
	}
}
