package runtime

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionEventHubDisconnectsSlowCompatibilitySubscriber(t *testing.T) {
	h := newSessionEventHubWithLimits(32, 8<<20)
	_, events, cancel := h.Subscribe("s", 4)
	defer cancel()
	for i := range 100 {
		h.Publish("s", AgentChoice("root", "s", strconv.Itoa(i)))
	}
	var got int
	for range events {
		got++
	}
	assert.LessOrEqual(t, got, 4)
}

func TestSessionEventHubReportsReplayGap(t *testing.T) {
	h := newSessionEventHubWithLimits(32, 8<<20)
	for i := range 100 {
		h.Publish("s", AgentChoice("root", "s", strconv.Itoa(i)))
	}
	since := uint64(0)
	seed, _, cancel, cursor := h.SubscribeSequenced("s", &since, 4)
	defer cancel()
	require.NotEmpty(t, seed)
	assert.True(t, seed[0].Gap)
	assert.Equal(t, uint64(69), seed[0].FirstAvailable)
	assert.Len(t, seed, 33)
	assert.Equal(t, uint64(100), cursor)
}

func TestSessionEventHubReportsGapWhenReplayIsEmpty(t *testing.T) {
	h := newSessionEventHubWithLimits(10, 1)
	h.Publish("s", AgentChoice("root", "s", "too large"))
	since := uint64(0)
	seed, _, cancel, cursor := h.SubscribeSequenced("s", &since, 1)
	defer cancel()
	require.Len(t, seed, 1)
	assert.True(t, seed[0].Gap)
	assert.Equal(t, uint64(2), seed[0].FirstAvailable)
	assert.Equal(t, uint64(1), cursor)
}

func TestSessionEventHubReplaysAfterDisconnect(t *testing.T) {
	h := newSessionEventHubWithLimits(128, 8<<20)
	_, events, cancel, cursor := h.SubscribeSequenced("s", nil, 2)
	for i := range 10 {
		h.Publish("s", AgentChoice("root", "s", strconv.Itoa(i)))
	}
	for range events {
	}
	cancel()
	seed, _, cancel2, _ := h.SubscribeSequenced("s", &cursor, 2)
	defer cancel2()
	require.Len(t, seed, 10)
	for i, item := range seed {
		assert.False(t, item.Gap)
		assert.Equal(t, uint64(i+1), item.Sequence)
		assert.Equal(t, strconv.Itoa(i), item.Event.(*AgentChoiceEvent).Content)
	}
}

func TestSessionEventHubSeedsInflightAssistant(t *testing.T) {
	h := newSessionEventHubWithLimits(defaultSessionEventReplayCapacity, 8<<20)
	h.Publish("s1", AgentChoiceReasoning("planner", "s1", "thinking… "))
	h.Publish("s1", AgentChoice("planner", "s1", "Hello, "))
	h.Publish("s1", AgentChoice("planner", "s1", "wor"))
	seed, ch, cancel := h.Subscribe("s1", 16)
	defer cancel()
	require.Len(t, seed, 2)
	assert.Equal(t, "thinking… ", seed[0].(*AgentChoiceReasoningEvent).Content)
	assert.Equal(t, "Hello, wor", seed[1].(*AgentChoiceEvent).Content)
	h.Publish("s1", AgentChoice("planner", "s1", "ld!"))
	assert.Equal(t, "ld!", (<-ch).(*AgentChoiceEvent).Content)
	h.Publish("s1", &MessageAddedEvent{SessionID: "s1"})
	seed2, _, cancel2 := h.Subscribe("s1", 16)
	defer cancel2()
	assert.Empty(t, seed2)
}

func TestSessionEventHubResetsInflightOnBoundaries(t *testing.T) {
	for _, boundary := range []Event{&StreamStoppedEvent{}, &ErrorEvent{}, UserMessage("hi", "s1", nil)} {
		h := newSessionEventHubWithLimits(defaultSessionEventReplayCapacity, 8<<20)
		h.Publish("s1", AgentChoice("planner", "s1", "partial"))
		h.Publish("s1", boundary)
		seed, _, cancel := h.Subscribe("s1", 1)
		assert.Empty(t, seed)
		cancel()
	}
}

func TestSessionEventHubSeedsLiveRun(t *testing.T) {
	h := newSessionEventHubWithLimits(defaultSessionEventReplayCapacity, 8<<20)
	h.Publish("s1", StreamStarted("s1", "coder"))
	h.Publish("s1", AgentChoice("coder", "s1", "Working…"))
	seed, _, cancel := h.Subscribe("s1", 16)
	cancel()
	require.Len(t, seed, 2)
	assert.IsType(t, &StreamStartedEvent{}, seed[0])
	h.Publish("s1", StreamStopped("s1", "coder", ""))
	seed, _, cancel = h.Subscribe("s1", 16)
	cancel()
	assert.Empty(t, seed)
}
