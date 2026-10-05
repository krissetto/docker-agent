package runtime

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionEventHubClampsHugeSubscriberBuffers(t *testing.T) {
	h := newSessionEventHubWithLimits(1, 1024)
	_, compatibility, cancel := h.Subscribe("s", math.MaxInt)
	defer cancel()
	_, sequenced, cancelSequenced, _ := h.SubscribeSequenced("s", nil, math.MaxInt)
	defer cancelSequenced()
	assert.Equal(t, maxSessionEventSubscriberBuffer+1, cap(compatibility))
	assert.Equal(t, maxSessionEventSubscriberBuffer+1, cap(sequenced))
}

func TestSessionEventHubCapsCombinedSubscriberCount(t *testing.T) {
	h := newSessionEventHubWithLimits(1, 1024)
	var cancels []func()
	for i := range maxSessionEventSubscribers {
		if i%2 == 0 {
			_, _, cancel := h.Subscribe("compatibility", 1)
			cancels = append(cancels, cancel)
		} else {
			_, _, cancel, _ := h.SubscribeSequenced("sequenced", nil, 1)
			cancels = append(cancels, cancel)
		}
	}
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()
	assert.Equal(t, maxSessionEventSubscribers, h.subscriberCount)
	for range 100 {
		seed, compatibility, cancel := h.Subscribe("overflow", 1)
		require.Empty(t, seed)
		_, open := <-compatibility
		require.False(t, open)
		cancel()
		seedSequenced, sequenced, cancelSequenced, _ := h.SubscribeSequenced("overflow", nil, 1)
		require.Empty(t, seedSequenced)
		gap := <-sequenced
		require.True(t, gap.Gap)
		_, open = <-sequenced
		require.False(t, open)
		cancelSequenced()
	}
	assert.Len(t, h.subs, 1)
	assert.Len(t, h.seqSubs, 1)
	cancels[0]()
	_, _, cancel := h.Subscribe("replacement", 1)
	defer cancel()
	assert.Equal(t, maxSessionEventSubscribers, h.subscriberCount)
}

func TestSessionEventHubRejectsOversizedLivePayload(t *testing.T) {
	h := newSessionEventHubWithLimits(0, 0)
	_, compatibility, cancel := h.Subscribe("s", 4)
	defer cancel()
	_, sequenced, cancelSequenced, _ := h.SubscribeSequenced("s", nil, 4)
	defer cancelSequenced()
	h.Publish("s", AgentChoice("root", "s", strings.Repeat("x", maxSessionEventSubscriberBytes+1)))
	_, open := <-compatibility
	require.False(t, open, "oversized event is never queued")
	gap := <-sequenced
	assert.True(t, gap.Gap)
	assert.EqualValues(t, 1, gap.FirstAvailable)
	_, open = <-sequenced
	require.False(t, open)
	assert.Zero(t, h.subscriberCount)

	seed, compatibility, cancel := h.Subscribe("s", 1)
	defer cancel()
	require.Empty(t, seed, "oversized authoritative seed must not be cloned")
	_, open = <-compatibility
	require.False(t, open)
	seeds, sequenced, cancelSequenced, _ := h.SubscribeSequenced("s", nil, 1)
	defer cancelSequenced()
	require.Empty(t, seeds)
	assert.True(t, (<-sequenced).Gap)
	_, open = <-sequenced
	require.False(t, open)
	assert.Zero(t, h.subscriberCount)
}

func TestSessionEventHubQueuedBytesAndDrainRecovery(t *testing.T) {
	for _, drain := range []bool{false, true} {
		t.Run(map[bool]string{false: "backlog", true: "drained"}[drain], func(t *testing.T) {
			h := newSessionEventHubWithLimits(0, 0)
			_, compatibility, cancel := h.Subscribe("s", 10)
			defer cancel()
			_, sequenced, cancelSequenced, _ := h.SubscribeSequenced("s", nil, 10)
			defer cancelSequenced()
			event := AgentChoice("root", "s", strings.Repeat("x", maxSessionEventSubscriberBytes/2))
			h.Publish("s", event)
			if drain {
				<-compatibility
				<-sequenced
			}
			h.Publish("s", event)
			if drain {
				require.NotNil(t, <-compatibility)
				assert.False(t, (<-sequenced).Gap)
				assert.Equal(t, 2, h.subscriberCount, "drained bytes must release queue budget")
			} else {
				got := 0
				for range compatibility {
					got++
				}
				assert.Equal(t, 1, got)
				got = 0
				for event := range sequenced {
					got++
					if got == 2 {
						assert.True(t, event.Gap)
					}
				}
				assert.Equal(t, 2, got)
				assert.Zero(t, h.subscriberCount)
			}
		})
	}
}

func TestSessionEventHubRejectsOversizedReplayAdmission(t *testing.T) {
	h := newSessionEventHubWithLimits(10, 2*maxSessionEventSubscriberBytes)
	h.Publish("s", UserMessage(strings.Repeat("x", maxSessionEventSubscriberBytes+1), "s", nil))
	since := uint64(0)
	seed, events, cancel, cursor := h.SubscribeSequenced("s", &since, 1)
	defer cancel()
	assert.Empty(t, seed, "oversized replay must not be copied to a subscriber")
	assert.EqualValues(t, 1, cursor)
	assert.True(t, (<-events).Gap)
	_, open := <-events
	assert.False(t, open)
	assert.Zero(t, h.subscriberCount)
}
