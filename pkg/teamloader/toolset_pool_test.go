package teamloader

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestToolsetPoolFailedCreateAndStartDoNotPoisonNextAcquisition(t *testing.T) {
	t.Parallel()
	pool, err := NewToolsetLeasePool("scope")
	require.NoError(t, err)
	material := []byte("same-key")
	var attempts atomic.Int32
	_, _, err = pool.acquire(t.Context(), material, func() (tools.ToolSet, error) { attempts.Add(1); return nil, errors.New("partial create") }, func(context.Context, tools.ToolSet) error { return nil })
	require.ErrorContains(t, err, "partial create")
	raw := new(poolProbeToolset)
	tool, lease, err := pool.acquire(t.Context(), []byte("valid-key"), func() (tools.ToolSet, error) { return raw, nil }, func(context.Context, tools.ToolSet) error { return nil })
	require.NoError(t, err)
	start, ok := tools.As[tools.Startable](tool)
	require.True(t, ok)
	require.NoError(t, start.Start(t.Context()))
	require.NoError(t, lease.Close())
	require.NoError(t, pool.Close(t.Context()))
}

type poolProbeToolset struct {
	starts atomic.Int32
	closes atomic.Int32
}

func (p *poolProbeToolset) Start(context.Context) error                 { p.starts.Add(1); return nil }
func (p *poolProbeToolset) Stop(context.Context) error                  { return nil }
func (p *poolProbeToolset) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (p *poolProbeToolset) Close() error                                { p.closes.Add(1); return nil }

type flakyStartPoolToolset struct {
	poolProbeToolset
	fail atomic.Bool
}

func (p *flakyStartPoolToolset) Start(context.Context) error {
	p.starts.Add(1)
	if p.fail.Swap(false) {
		return errors.New("injected start failure")
	}
	return nil
}

func TestToolsetLeasePoolFailedRawStartCanBeRetriedByOtherLease(t *testing.T) {
	t.Parallel()
	pool, err := NewToolsetLeasePool("source-runtime")
	require.NoError(t, err)
	raw := new(flakyStartPoolToolset)
	raw.fail.Store(true)
	create := func() (tools.ToolSet, error) { return raw, nil }
	dispose := func(context.Context, tools.ToolSet) error { return nil }
	first, firstClose, err := pool.acquire(t.Context(), []byte("key"), create, dispose)
	require.NoError(t, err)
	startable, ok := tools.As[tools.Startable](first)
	require.True(t, ok)
	require.ErrorContains(t, startable.Start(t.Context()), "injected start failure")
	second, secondClose, err := pool.acquire(t.Context(), []byte("key"), create, dispose)
	require.NoError(t, err)
	secondStart, ok := tools.As[tools.Startable](second)
	require.True(t, ok)
	require.NoError(t, secondStart.Start(t.Context()))
	require.NoError(t, firstClose.Close())
	require.NoError(t, secondClose.Close())
	require.NoError(t, pool.Close(t.Context()))
	require.EqualValues(t, 2, raw.starts.Load())
}
