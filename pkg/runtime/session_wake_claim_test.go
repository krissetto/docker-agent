package runtime

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

func TestWakeClaimStaleSubmitDoesNotStartEmptyGeneration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		provider := coordinationReply("answer")
		provider.call = func(context.Context, []chat.Message) (chat.MessageStream, error) {
			calls.Add(1)
			return newStreamBuilder().AddContent("answer").AddStopWithUsage(1, 1).Build(), nil
		}
		_, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), provider, coordinationReply("done"))
		h := coordinationCreate(t, owner.Runtime(), "root", "")
		// Park the creation-triggered scheduler before arranging the competing wake.
		synctest.Wait()
		d := h.(*sessionHandle).driver
		entered, release := make(chan struct{}), make(chan struct{})
		defer func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}()
		d.beforePrepareStart = func() { close(entered); <-release }
		done := make(chan error, 1)
		go func() { _, err := h.Submit(t.Context(), TurnInput{Content: "one input"}); done <- err }()
		coordinationWait(t, entered)
		require.True(t, d.WakePending())
		d.Wait()
		d.mu.Lock()
		generation := d.generation
		d.mu.Unlock()
		close(release)
		require.NoError(t, <-done)
		d.Wait()
		d.mu.Lock()
		assert.Equal(t, generation, d.generation)
		assert.False(t, d.running)
		assert.Empty(t, d.pending)
		d.mu.Unlock()
		assert.Equal(t, int32(1), calls.Load())
	})
}

func TestWakeClaimRechecksInputAfterGate(t *testing.T) {
	r := newDriverTestRuntime(t)
	d := r.sessionDrivers.Get(session.New(session.WithID("gate")))
	d.pending = []QueuedMessage{{Content: "removed during admission"}}
	var aborted atomic.Int32
	d.SetPreStartGate(func() bool {
		d.DrainPending()
		return true
	}, func() { aborted.Add(1) })
	runCtx, generation, callbacks, err := d.prepareStart(t.Context(), true)
	require.Error(t, err)
	assert.Nil(t, runCtx)
	assert.Zero(t, generation)
	assert.Empty(t, callbacks)
	assert.Equal(t, int32(1), aborted.Load())
	assert.Zero(t, d.generation)
	assert.False(t, d.running)
	assert.False(t, d.starting)
}

func TestEmptyStopWarningDoesNotInventProviderCause(t *testing.T) {
	warning := emptyTurnWarning(streamResult{}, false, "openai/gpt-6-astra", chat.FinishReasonStop)
	assert.Equal(t, "Model openai/gpt-6-astra returned an empty response (stop reason: stop).", warning)
	assert.NotContains(t, warning, "rate-limit")
	assert.NotContains(t, warning, "token limit")
	assert.Contains(t, emptyTurnWarning(streamResult{}, false, "model", chat.FinishReasonLength), "output token limit")
	assert.Contains(t, emptyTurnWarning(streamResult{ReasoningContent: "thinking"}, false, "model", chat.FinishReasonStop), "only reasoning")
	assert.Empty(t, emptyTurnWarning(streamResult{}, true, "model", chat.FinishReasonStop))
}
