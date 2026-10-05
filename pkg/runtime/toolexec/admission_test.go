package toolexec

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInvocationAdmissionNestedHandoffAndCancellation(t *testing.T) {
	permits := make(chan struct{}, 1)
	d := &Dispatcher{AcquireTool: func(ctx context.Context, _, _ string) (func(), error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		select {
		case permits <- struct{}{}:
			return func() { <-permits }, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	for _, canceled := range []bool{false, true} {
		err := d.invokeAdmitted(t.Context(), "parent", "callback", func(ctx context.Context) {
			assert.Len(t, permits, 1)
			resume := SuspendInvocation(ctx)
			assert.Empty(t, permits)
			require.NoError(t, d.invokeAdmitted(ctx, "child", "shell", func(context.Context) {
				assert.Len(t, permits, 1)
			}))
			resumeCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			if canceled {
				cancel()
				require.ErrorIs(t, resume(resumeCtx), context.Canceled)
				assert.Empty(t, permits)
			} else {
				require.NoError(t, resume(resumeCtx))
				require.NoError(t, resume(resumeCtx))
				assert.Len(t, permits, 1)
			}
		})
		require.NoError(t, err)
		assert.Empty(t, permits)
	}
}
