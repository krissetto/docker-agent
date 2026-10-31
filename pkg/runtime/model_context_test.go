package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/contracts"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

type contextProvider struct {
	configProvider

	lookup func(context.Context) (int64, error)
}

func (p *contextProvider) ContextWindow(ctx context.Context) (int64, error) { return p.lookup(ctx) }

// BaseConfig carries a rebuilder that returns the same fake, so the policy-A
// clone made for the compaction limit keeps its context-window resolver.
func (p *contextProvider) BaseConfig() base.Config {
	cfg := p.Config
	cfg.RebuildProvider = func(context.Context, *latest.ModelConfig, ...options.Opt) (contracts.Provider, error) {
		return p, nil
	}
	return cfg
}

func newContextProvider(endpoint string, lookup func(context.Context) (int64, error)) *contextProvider {
	return &contextProvider{
		configProvider: configProvider{Config: base.Config{BaseURL: endpoint, ModelConfig: latest.ModelConfig{Provider: "dmr", Model: "ai/qwen3"}}},
		lookup:         lookup,
	}
}

func TestResolveContextLimitDiscovery(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		explicit   int
		discovered int64
		err        error
		want       int64
		calls      int
	}{
		{name: "explicit wins", explicit: 4096, discovered: 8192, want: 4096},
		{name: "discovery before catalog", discovered: 8192, want: 8192, calls: 1},
		{name: "missing metadata falls back", want: 32768, calls: 1},
		{name: "failed discovery falls back", err: errors.New("unreachable"), want: 32768, calls: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			p := newContextProvider("http://local/engines/v1", func(context.Context) (int64, error) { calls++; return tt.discovered, tt.err })
			if tt.explicit > 0 {
				p.ModelConfig.ProviderOpts = map[string]any{"context_size": tt.explicit}
			}
			r := &LocalRuntime{modelsStore: stubModelStore{models: map[string]*modelsdev.Model{"dmr/ai/qwen3": {Limit: modelsdev.Limit{Context: 32768}}}}}
			for range 3 {
				assert.Equal(t, tt.want, r.resolveContextLimit(t.Context(), p, p.ID()))
			}
			assert.Equal(t, tt.calls, calls)
			if tt.explicit == 0 {
				assert.Nil(t, p.ModelConfig.ProviderOpts, "discovery must not rewrite configuration")
			}
		})
	}
}

func TestDiscoveredContextCacheScopeAndExpiry(t *testing.T) {
	t.Parallel()
	calls := 0
	lookup := func(context.Context) (int64, error) { calls++; return 8192, nil }
	now := time.Now()
	r := &LocalRuntime{now: func() time.Time { return now }}
	p := newContextProvider("http://one/engines/v1", lookup)
	assert.Equal(t, int64(8192), r.discoveredContextLimit(t.Context(), p))
	// A rebuilt provider at the same endpoint reuses the result.
	clone := newContextProvider("http://one/engines/v1", lookup)
	assert.Equal(t, int64(8192), r.discoveredContextLimit(t.Context(), clone))
	assert.Equal(t, 1, calls)
	other := newContextProvider("http://two/engines/v1", lookup)
	assert.Equal(t, int64(8192), r.discoveredContextLimit(t.Context(), other))
	assert.Equal(t, 2, calls)
	clone.ModelConfig.ProviderOpts = map[string]any{"runtime_flags": []string{"-c", "4096"}}
	assert.Equal(t, int64(8192), r.discoveredContextLimit(t.Context(), clone))
	assert.Equal(t, 3, calls, "different runtime options must not reuse an allocation")
	now = now.Add(time.Minute)
	assert.Equal(t, int64(8192), r.discoveredContextLimit(t.Context(), p))
	assert.Equal(t, 4, calls)
}

func TestDiscoveredContextDoesNotCacheCancellation(t *testing.T) {
	t.Parallel()
	calls := 0
	p := newContextProvider("http://local", func(ctx context.Context) (int64, error) {
		calls++
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 4096, nil
	})
	r := &LocalRuntime{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.Zero(t, r.discoveredContextLimit(ctx, p))
	assert.Equal(t, int64(4096), r.discoveredContextLimit(t.Context(), p))
	assert.Equal(t, 2, calls)
}

func TestDiscoveredContextConcurrent(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	p := newContextProvider("http://local", func(context.Context) (int64, error) { calls.Add(1); return 4096, nil })
	r := &LocalRuntime{}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { assert.Equal(t, int64(4096), r.discoveredContextLimit(t.Context(), p)) })
	}
	wg.Wait()
	assert.Equal(t, int32(1), calls.Load())
}

func TestCompactionUsesDiscoveredWindow(t *testing.T) {
	t.Parallel()
	p := newContextProvider("http://local", func(context.Context) (int64, error) { return 8192, nil })
	a := agent.New("root", "test", agent.WithModel(p))
	r := &LocalRuntime{modelsStore: stubModelStore{}}
	require.Equal(t, int64(8192), r.compactionContextLimit(t.Context(), a))
}

// Two sessions pin different providers (different endpoints and models) on the
// same agent. Each session must resolve its own window, concurrently, with no
// cross-talk through the runtime-level cache.
func TestDiscoveredContextSessionsResolveOwnLimits(t *testing.T) {
	t.Parallel()
	var oneCalls, twoCalls atomic.Int32
	one := newContextProvider("http://one/engines/v1", func(context.Context) (int64, error) { oneCalls.Add(1); return 8192, nil })
	two := newContextProvider("http://two/engines/v1", func(context.Context) (int64, error) { twoCalls.Add(1); return 4096, nil })
	two.ModelConfig.Model = "ai/gemma3"
	a := agent.New("root", "test", agent.WithModel(one))
	r := &LocalRuntime{modelsStore: stubModelStore{}}
	ctxOne := agent.WithContextModels(t.Context(), "root", []provider.Provider{one})
	ctxTwo := agent.WithContextModels(t.Context(), "root", []provider.Provider{two})

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			assert.Equal(t, int64(8192), r.compactionContextLimit(ctxOne, a))
		})
		wg.Go(func() {
			assert.Equal(t, int64(4096), r.compactionContextLimit(ctxTwo, a))
		})
	}
	wg.Wait()
	assert.Equal(t, int32(1), oneCalls.Load())
	assert.Equal(t, int32(1), twoCalls.Load())
}

// A gateway client never reports a window (its resolver returns zero), so it
// must not share a cache entry with a direct client for the same endpoint,
// model and options.
func TestDiscoveredContextGatewayIsKeyed(t *testing.T) {
	t.Parallel()
	direct := newContextProvider("http://local", func(context.Context) (int64, error) { return 8192, nil })
	gateway := newContextProvider("http://local", func(context.Context) (int64, error) { return 0, nil })
	gateway.ModelOptions = options.Apply(options.WithGateway("http://gateway.example"))
	r := &LocalRuntime{}
	assert.Equal(t, int64(0), r.discoveredContextLimit(t.Context(), gateway))
	assert.Equal(t, int64(8192), r.discoveredContextLimit(t.Context(), direct), "a gateway entry must not satisfy a direct client")
}

// A failed discovery is cached as zero for at most the TTL: it is reused within
// the window and retried once the window has passed, never kept permanently.
func TestDiscoveredContextFailureExpiresWithTTL(t *testing.T) {
	t.Parallel()
	calls := 0
	p := newContextProvider("http://local", func(context.Context) (int64, error) {
		calls++
		return 0, errors.New("unreachable")
	})
	now := time.Now()
	r := &LocalRuntime{now: func() time.Time { return now }}
	assert.Zero(t, r.discoveredContextLimit(t.Context(), p))
	assert.Zero(t, r.discoveredContextLimit(t.Context(), p))
	assert.Equal(t, 1, calls, "the failure is reused within the TTL")

	now = now.Add(time.Minute - time.Second)
	assert.Zero(t, r.discoveredContextLimit(t.Context(), p))
	assert.Equal(t, 1, calls, "still inside the TTL")

	now = now.Add(time.Second)
	assert.Zero(t, r.discoveredContextLimit(t.Context(), p))
	assert.Equal(t, 2, calls, "the failure expires at the TTL and is retried")
}
