package codemode

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

type cancellationTools struct {
	started    chan struct{}
	nativeDone chan struct{}
}

func (s cancellationTools) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{{Name: "signal", Parameters: tools.MustSchemaFor[map[string]any](), Handler: func(ctx context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
		close(s.started)
		if s.nativeDone != nil {
			<-ctx.Done()
			close(s.nativeDone)
			return nil, ctx.Err()
		}
		return tools.ResultSuccess("ready"), nil
	}}}, nil
}

func TestJavascriptCancellation(t *testing.T) {
	t.Run("pre-canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := Wrap().(*codeModeTool).runJavascript(ctx, tools.NopRuntime{}, `return "not run";`)
		require.ErrorIs(t, err, context.Canceled)
	})
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "bounded-cpu", true: "cooperative-native"}[native], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			inner := cancellationTools{started: make(chan struct{})}
			if native {
				inner.nativeDone = make(chan struct{})
			}
			c := Wrap(inner).(*codeModeTool)
			done := make(chan error, 1)
			go func() {
				_, err := c.runJavascript(ctx, tools.NopRuntime{}, `signal({}); const end=Date.now()+400; while(Date.now()<end){}; return "done";`)
				done <- err
			}()
			select {
			case <-inner.started:
			case <-time.After(time.Second):
				t.Fatal("native callback did not start")
			}
			cancel()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(time.Second):
				t.Fatal("bounded script did not return")
			}
			if native {
				select {
				case <-inner.nativeDone:
				default:
					t.Fatal("native callback was abandoned")
				}
			}
		})
	}
}

type ownerCleanupTools struct {
	owner       *tools.ResourceOwner
	stops       int
	globalStops int
	err         error
}

func (*ownerCleanupTools) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (s *ownerCleanupTools) Start(context.Context) error               { return s.err }
func (s *ownerCleanupTools) Stop(context.Context) error                { s.globalStops++; return nil }

func (s *ownerCleanupTools) StopResourceOwner(ctx context.Context) error {
	s.owner = tools.ResourceOwnerFromContext(ctx)
	s.stops++
	return s.err
}

func TestCodeModeOwnerCleanupIncludesFailedChildren(t *testing.T) {
	errA, errB := errors.New("a"), errors.New("b")
	a, b := &ownerCleanupTools{err: errA}, &ownerCleanupTools{err: errB}
	wrapped := tools.NewStartable(Wrap(a, b))
	require.Error(t, wrapped.Start(t.Context()))
	stopper, ok := tools.As[tools.ResourceOwnerStopper](wrapped)
	require.True(t, ok)
	owner := tools.NewResourceOwner()
	err := stopper.StopResourceOwner(tools.WithResourceOwner(t.Context(), owner))
	require.ErrorIs(t, err, errA)
	require.ErrorIs(t, err, errB)
	for _, inner := range []*ownerCleanupTools{a, b} {
		require.Same(t, owner, inner.owner)
		require.Equal(t, 1, inner.stops)
		require.Zero(t, inner.globalStops)
	}
}
