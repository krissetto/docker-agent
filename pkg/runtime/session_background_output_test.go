package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/stretchr/testify/require"
)

func TestBackgroundOutputLiveOnlyDoesNotEvictDurableEvents(t *testing.T) {
	hub := newSessionEventHubWithLimits(2, 4096)
	hub.Publish("s", StreamStarted("s", "agent"))
	_, live, cancel, cursor, _ := hub.SubscribePublic("s", nil, 4, []string{"job"})
	defer cancel()
	require.EqualValues(t, 1, cursor)
	for i := range 20 {
		hub.Publish("s", &BackgroundProcessOutputEvent{Type: "background_process_output", SessionID: "s", ProcessID: "job", ToolCallID: "call", Offset: int64(i * 32), Output: strings.Repeat("x", 32)})
	}
	require.Len(t, hub.replay["s"], 1)
	require.EqualValues(t, 1, hub.nextSeq["s"])
	require.Empty(t, hub.activeTools["s"])
	item := <-live
	require.Zero(t, item.Sequence)
	require.Empty(t, item.TurnID)
	require.IsType(t, &BackgroundProcessOutputEvent{}, item.Event)
	// A saturated ephemeral feed drops output rather than disconnecting observers.
	hub.Publish("s", &BackgroundProcessesEvent{Type: "background_processes", SessionID: "s"})
	for range 2 {
		<-live
	}
	terminal := <-live
	require.IsType(t, &BackgroundProcessesEvent{}, terminal.Event)
	zero := uint64(0)
	seed, _, stop, _ := hub.SubscribeSequenced("s", &zero, 8)
	defer stop()
	require.Len(t, seed, 2)
	for _, item := range seed {
		require.IsNotType(t, &BackgroundProcessOutputEvent{}, item.Event)
	}
	for _, item := range hub.liveSeedLocked("s") {
		require.IsNotType(t, &BackgroundProcessOutputEvent{}, item)
	}
}

type sessionOutputSource struct{ owner *tools.ResourceOwner }

func (s sessionOutputSource) BackgroundProcessOutput(ctx context.Context, id string, _ int) (tools.BackgroundProcessOutput, error) {
	if tools.ResourceOwnerFromContext(ctx) != s.owner {
		return tools.BackgroundProcessOutput{}, tools.ErrBackgroundProcessNotFound
	}
	return tools.BackgroundProcessOutput{ProcessID: id, Output: "tail"}, nil
}

func TestBackgroundOutputSnapshotSessionAndChildIsolation(t *testing.T) {
	r := newDriverTestRuntime(t)
	root := session.New(session.WithID("output-root"))
	child := session.New(session.WithID("output-child"))
	child.ParentID = root.ID
	rootDriver := r.sessionDrivers.Get(root)
	childDriver := r.sessionDrivers.Get(child)
	rootHandle := &sessionHandle{runtime: r, driver: rootDriver, sessionID: root.ID}
	childHandle := &sessionHandle{runtime: r, driver: childDriver, sessionID: child.ID}
	for _, d := range []*sessionDriver{rootDriver, childDriver} {
		require.NoError(t, d.adoptBackgroundProcess(t.Context(), tools.BackgroundProcess{ID: "job-" + d.sessionID(), SessionID: d.sessionID(), State: tools.BackgroundProcessRunning, StartedAt: time.Now()}, sessionOutputSource{owner: d.resourceOwner}))
	}
	output, err := rootHandle.BackgroundProcessOutput(t.Context(), "job-"+root.ID, 10)
	require.NoError(t, err)
	require.Equal(t, "tail", output.Output)
	_, err = rootHandle.BackgroundProcessOutput(t.Context(), "job-"+child.ID, 10)
	require.ErrorIs(t, err, tools.ErrBackgroundProcessNotFound)
	_, err = childHandle.BackgroundProcessOutput(t.Context(), "job-"+root.ID, 10)
	require.ErrorIs(t, err, tools.ErrBackgroundProcessNotFound)
	rootDriver.finishBackgroundProcessResult(t.Context(), "job-"+root.ID, tools.BackgroundProcessFailed, time.Now().Add(-5*time.Second), 7, false)
	_, err = rootHandle.BackgroundProcessOutput(t.Context(), "job-"+root.ID, 10)
	require.ErrorIs(t, err, tools.ErrBackgroundProcessNotFound)
}

func TestBackgroundWorkOutputFlushPrecedesTerminalAndStaysOutOfTranscript(t *testing.T) {
	r := newDriverTestRuntime(t)
	sess := session.New(session.WithID("output-owner"))
	d := r.sessionDrivers.Get(sess)
	_, live, cancel, _, _ := d.events.SubscribePublic(sess.ID, nil, 16, []string{"job"})
	defer cancel()
	_, foreign, foreignCancel, _ := d.events.SubscribeSequenced("foreign", nil, 16)
	defer foreignCancel()
	work := &sessionBackgroundWork{runtime: r, driver: d, sessionID: sess.ID, toolCallID: "call", agentName: "agent"}
	work.SetBackgroundProcessOutputSource(sessionOutputSource{owner: d.resourceOwner})
	require.NoError(t, work.AdoptBackgroundProcess(tools.BackgroundProcess{ID: "job", SessionID: sess.ID, ToolCallID: "call", State: tools.BackgroundProcessRunning, StartedAt: time.Now()}))
	<-live
	work.EmitBackgroundProcessOutput(17, "tail")
	work.FinishBackgroundProcessResult(tools.BackgroundProcessFailed, time.Now(), 7, false)
	output := <-live
	require.Zero(t, output.Sequence)
	require.True(t, output.Event.(*BackgroundProcessOutputEvent).Offset == 17)
	require.Equal(t, "call", output.Event.(*BackgroundProcessOutputEvent).ToolCallID)
	terminal := <-live
	process := terminal.Event.(*BackgroundProcessesEvent).Processes[0]
	require.Equal(t, 7, process.ExitCode)
	require.Equal(t, tools.BackgroundProcessFailed, process.State)
	require.Empty(t, sess.Messages)
	require.Empty(t, d.events.activeTools[sess.ID])
	require.Empty(t, foreign)
	for _, event := range d.events.replay[sess.ID] {
		require.IsNotType(t, &BackgroundProcessOutputEvent{}, event.event)
	}
}

func TestBackgroundOutputObservationInterest(t *testing.T) {
	r := newDriverTestRuntime(t)
	sess := session.New(session.WithID("interest"))
	d := r.sessionDrivers.Get(sess)
	handle := &sessionHandle{runtime: r, driver: d, sessionID: sess.ID}
	first, err := handle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer first.Cancel()
	second, err := handle.Observe(t.Context(), ObserveOptions{BackgroundOutputInterest: map[string][]string{sess.ID: {"other"}}})
	require.NoError(t, err)
	defer second.Cancel()
	output := func(id string) {
		d.events.Publish(sess.ID, &BackgroundProcessOutputEvent{Type: "background_process_output", SessionID: sess.ID, ProcessID: id, Output: "bytes"})
	}
	output("job")
	require.Empty(t, first.Events)
	require.Empty(t, second.Events)
	require.False(t, d.events.HasBackgroundOutputInterest(sess.ID, "job"))
	require.NoError(t, first.SetBackgroundOutputInterest(sess.ID, []string{"job"}))
	output("job")
	require.IsType(t, &BackgroundProcessOutputEvent{}, (<-first.Events).Event)
	require.Empty(t, second.Events)
	output("other")
	require.IsType(t, &BackgroundProcessOutputEvent{}, (<-second.Events).Event)
	require.Empty(t, first.Events)
	require.NoError(t, first.SetBackgroundOutputInterest(sess.ID, nil))
	output("job")
	require.Empty(t, first.Events)
	require.NoError(t, first.SetBackgroundOutputInterest(sess.ID, []string{"job"}))
	first.Cancel()
	require.False(t, d.events.HasBackgroundOutputInterest(sess.ID, "job"))
	require.ErrorIs(t, first.SetBackgroundOutputInterest(sess.ID, []string{"job"}), ErrSessionClosed)
	replacement, err := handle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer replacement.Cancel()
	require.NoError(t, replacement.SetBackgroundOutputInterest(sess.ID, []string{"job"}))
	output("job")
	require.IsType(t, &BackgroundProcessOutputEvent{}, (<-replacement.Events).Event)
	d.events.Publish(sess.ID, &BackgroundProcessesEvent{Type: "background_processes", SessionID: sess.ID})
	require.IsType(t, &BackgroundProcessesEvent{}, (<-replacement.Events).Event)
	require.IsType(t, &BackgroundProcessesEvent{}, (<-second.Events).Event)
	d.events.Publish(sess.ID, StreamStopped(sess.ID, "agent", "completed"))
	require.IsType(t, &StreamStoppedEvent{}, (<-replacement.Events).Event)
	require.IsType(t, &StreamStoppedEvent{}, (<-second.Events).Event)
}

func TestBackgroundOutputSkipsSnapshotWithoutInterest(t *testing.T) {
	hub := newSessionEventHubWithLimits(2, 4096)
	_, compat, cancel := hub.Subscribe("s", 4)
	defer cancel()
	_, sequenced, stop, _ := hub.SubscribeSequenced("s", nil, 4)
	defer stop()
	_, public, end, _, _ := hub.SubscribePublic("s", nil, 4)
	defer end()
	event := &BackgroundProcessOutputEvent{ProcessID: "job", Output: "bytes"}
	require.Zero(t, testing.AllocsPerRun(100, func() { hub.Publish("s", event) }))
	for range 10000 {
		hub.Publish("s", &BackgroundProcessOutputEvent{ProcessID: "job", Output: strings.Repeat("x", 1024)})
	}
	require.Empty(t, compat)
	require.Empty(t, sequenced)
	require.Empty(t, public)
	require.Empty(t, hub.nextSeq)
	require.Empty(t, hub.replay)
	for sub := range hub.publicSubs["s"] {
		require.Empty(t, sub.queuedSizes)
		require.Zero(t, sub.queuedBytes)
	}
	require.NoError(t, hub.backgroundOutputInterestSetter("s", public)("s", []string{"job"}))
	for range 100 {
		hub.Publish("s", &BackgroundProcessOutputEvent{ProcessID: "job", Output: strings.Repeat("x", 1024)})
	}
	require.Empty(t, compat)
	require.Empty(t, sequenced)
	require.Len(t, public, 3)
	for sub := range hub.publicSubs["s"] {
		require.LessOrEqual(t, sub.queuedBytes, maxSessionEventSubscriberBytes)
	}
}

func TestBackgroundOutputTreeInterest(t *testing.T) {
	for _, ordered := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "ordered"}[ordered], func(t *testing.T) {
			m := newTestSubagentManager(t)
			r := m.r
			root := session.New(session.WithID("tree-output-root"), session.WithAgentName("root"))
			d := r.sessionDrivers.Get(root)
			m.ensureRoot(root, "root")
			h := &sessionHandle{runtime: r, driver: d, sessionID: root.ID, agentName: "root"}
			observation, err := h.Observe(t.Context(), ObserveOptions{Tree: true, OrderedTree: ordered, BackgroundOutputInterest: map[string][]string{root.ID: {"job"}}})
			require.NoError(t, err)
			defer observation.Cancel()
			require.True(t, d.events.HasBackgroundOutputInterest(root.ID, "job"))
			require.NoError(t, observation.SetBackgroundOutputInterest(root.ID, []string{"replacement"}))
			require.False(t, d.events.HasBackgroundOutputInterest(root.ID, "job"))
			d.events.Publish(root.ID, &BackgroundProcessOutputEvent{Type: "background_process_output", SessionID: root.ID, ProcessID: "replacement", Output: "bytes"})
			if ordered {
				update := <-observation.TreeUpdates
				require.IsType(t, &BackgroundProcessOutputEvent{}, update.Event.Event)
			} else {
				require.IsType(t, &BackgroundProcessOutputEvent{}, (<-observation.Events).Event)
			}
			observation.Cancel()
			require.False(t, d.events.HasBackgroundOutputInterest(root.ID, "replacement"))
			require.ErrorIs(t, observation.SetBackgroundOutputInterest(root.ID, []string{"job"}), ErrSessionClosed)
		})
	}
}

func TestBackgroundOutputHistoryTailInterest(t *testing.T) {
	r := newDriverTestRuntime(t)
	sess := session.New(session.WithID("tail-output"))
	d := r.sessionDrivers.Get(sess)
	h := &sessionHandle{runtime: r, driver: d, sessionID: sess.ID}
	observation, err := h.Observe(t.Context(), ObserveOptions{HistoryTail: &SessionHistoryLimits{}, BackgroundOutputInterest: map[string][]string{sess.ID: {"job"}}})
	require.NoError(t, err)
	defer observation.Cancel()
	require.True(t, d.events.HasBackgroundOutputInterest(sess.ID, "job"))
	require.NoError(t, observation.SetBackgroundOutputInterest(sess.ID, nil))
	require.False(t, d.events.HasBackgroundOutputInterest(sess.ID, "job"))
}
