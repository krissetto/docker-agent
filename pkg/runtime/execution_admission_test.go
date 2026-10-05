package runtime

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func admissionRuntime(executions, toolCalls int) *LocalRuntime {
	r := &LocalRuntime{policy: SessionResourcePolicy{MaxExecutions: executions, MaxTools: toolCalls}}
	r.applyResourcePolicy()
	return r
}

func TestExecutionAdmissionCapacityAndCancellation(t *testing.T) {
	r := admissionRuntime(1, 1)
	release, err := r.acquireExecution(t.Context(), "parent")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = r.acquireExecution(ctx, "child")
	require.ErrorIs(t, err, context.Canceled)
	release()
	release()
	release, err = r.acquireExecution(t.Context(), "child")
	require.NoError(t, err)
	release()
	assert.Zero(t, r.admission().active)

	for _, limit := range []int{0, -1} {
		_, err := admissionRuntime(limit, limit).acquireExecution(t.Context(), "disabled")
		require.ErrorIs(t, err, ErrSessionCapacity)
		_, err = admissionRuntime(limit, limit).acquireTool(t.Context(), "disabled", "shell")
		assert.ErrorIs(t, err, ErrSessionCapacity)
	}
}

func TestExecutionAdmissionNestedSinglePermit(t *testing.T) {
	r := admissionRuntime(1, 1)
	parentRelease, err := r.acquireExecution(t.Context(), "parent")
	require.NoError(t, err)
	defer parentRelease()
	resumeParent, err := r.suspendExecution(t.Context(), "parent")
	require.NoError(t, err)
	childRelease, err := r.acquireExecution(t.Context(), "child")
	require.NoError(t, err)
	resumeChild, err := r.suspendExecution(t.Context(), "child")
	require.NoError(t, err)
	grandchildRelease, err := r.acquireExecution(t.Context(), "grandchild")
	require.NoError(t, err)
	grandchildRelease()
	require.NoError(t, resumeChild(t.Context()))
	assert.Equal(t, 1, r.admission().active)
	childRelease()
	require.NoError(t, resumeParent(t.Context()))
	require.NoError(t, resumeParent(t.Context()), "resume is idempotent")
	assert.Equal(t, 1, r.admission().active)
}

func TestExecutionResumeCanceledDoesNotLeak(t *testing.T) {
	r := admissionRuntime(1, 1)
	release, err := r.acquireExecution(t.Context(), "parent")
	require.NoError(t, err)
	resume, err := r.suspendExecution(t.Context(), "parent")
	require.NoError(t, err)
	childRelease, err := r.acquireExecution(t.Context(), "child")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, resume(ctx), context.Canceled)
	release()
	childRelease()
	assert.Empty(t, r.admission().leases)
	assert.Zero(t, r.admission().active)
}

func TestExecutionAndToolAdmissionMaximumConcurrency(t *testing.T) {
	for _, kind := range []string{"execution", "tool"} {
		t.Run(kind, func(t *testing.T) {
			r := admissionRuntime(3, 3)
			var active, peak, admitted atomic.Int32
			firstWave := make(chan struct{})
			var wg sync.WaitGroup
			for i := range 18 {
				wg.Go(func() {
					var release func()
					var err error
					if kind == "execution" {
						release, err = r.acquireExecution(t.Context(), string(rune('a'+i)))
					} else {
						release, err = r.acquireTool(t.Context(), "session", "shell")
					}
					if err != nil {
						t.Error(err)
						return
					}
					n := active.Add(1)
					for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
					}
					if admitted.Add(1) == 3 {
						close(firstWave)
					}
					<-firstWave
					active.Add(-1)
					release()
				})
			}
			wg.Wait()
			assert.EqualValues(t, 3, peak.Load())
			assert.Zero(t, active.Load())
		})
	}
}

func TestExecutionConcurrentDelegationResumesAfterBothChildren(t *testing.T) {
	r := admissionRuntime(1, 1)
	release, err := r.acquireExecution(t.Context(), "parent")
	require.NoError(t, err)
	defer release()
	first, err := r.suspendExecution(t.Context(), "parent")
	require.NoError(t, err)
	second, err := r.suspendExecution(t.Context(), "parent")
	require.NoError(t, err)
	firstDone := make(chan error, 1)
	go func() { firstDone <- first(t.Context()) }()
	childRelease, err := r.acquireExecution(t.Context(), "child")
	require.NoError(t, err)
	childRelease()
	require.NoError(t, second(t.Context()))
	require.NoError(t, <-firstDone)
	assert.Equal(t, 1, r.admission().active)
}

func TestToolAdmissionWaitingCancellation(t *testing.T) {
	r := admissionRuntime(1, 1)
	release, err := r.acquireTool(t.Context(), "parent", "shell")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := r.acquireTool(ctx, "child", "shell")
		done <- err
	}()
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	release()
	assert.Empty(t, r.toolPermits)
}
