package runtime

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	ragtypes "github.com/docker/docker-agent/pkg/rag/types"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type (
	subscriberCounts struct{ tools, rag, releases atomic.Int32 }
	valueSubscribers struct {
		counts  *subscriberCounts
		payload any
	}
)

func (valueSubscribers) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (valueSubscribers) Name() string                                { return "fake" }
func (s valueSubscribers) SubscribeToolsChanged(func()) func() {
	s.counts.tools.Add(1)
	var once atomic.Bool
	return func() {
		if once.CompareAndSwap(false, true) {
			s.counts.releases.Add(1)
		}
	}
}

func (s valueSubscribers) SubscribeEvents(ragtypes.EventCallback) func() {
	s.counts.rag.Add(1)
	var once atomic.Bool
	return func() {
		if once.CompareAndSwap(false, true) {
			s.counts.releases.Add(1)
		}
	}
}

type nonComparableSubscribers struct {
	valueSubscribers

	data []string
}

func TestSubscriberIdentityAcceptsNonComparableValues(t *testing.T) {
	for _, kind := range []string{"slice-value", "nested-interface", "pointer-alias"} {
		t.Run(kind, func(t *testing.T) {
			counts := &subscriberCounts{}
			var ts tools.ToolSet
			want := int32(2)
			switch kind {
			case "slice-value":
				ts = nonComparableSubscribers{valueSubscribers: valueSubscribers{counts: counts}, data: []string{"x"}}
			case "nested-interface":
				ts = valueSubscribers{counts: counts, payload: []string{"x"}}
			case "pointer-alias":
				ts = &valueSubscribers{counts: counts, payload: []string{"x"}}
				want = 1
			}
			a := agent.New("root", "prompt", agent.WithModel(&mockProvider{id: "test/model"}), agent.WithToolSets(ts, ts))
			r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(a)), WithModelStore(mockModelStore{}))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, r.Close()) })
			require.NotPanics(t, func() { r.OnToolsChanged(func(Event) {}) })
			require.Equal(t, want, counts.tools.Load())
			var release func()
			require.NotPanics(t, func() {
				release = r.subscribeRAGChanges(session.New(session.WithExtraToolSets([]tools.ToolSet{ts})), NewChannelSink(make(chan Event, 1)))
			})
			ragWant := int32(3)
			if kind == "pointer-alias" {
				ragWant = 1
			}
			require.Equal(t, ragWant, counts.rag.Load())
			r.OnToolsChanged(nil)
			release()
			require.Equal(t, want+ragWant, counts.releases.Load())
		})
	}
}

func TestOnToolsChangedSubscribesRetainedTeamRevisions(t *testing.T) {
	oldCounts, newCounts := &subscriberCounts{}, &subscriberCounts{}
	oldAgent := agent.New("root", "old", agent.WithModel(&mockProvider{id: "test/old-tools"}), agent.WithToolSets(&valueSubscribers{counts: oldCounts}))
	oldTeam := team.New(team.WithAgents(oldAgent))
	oldEnv, err := NewTeamEnvironment("r1", "source", "baseline", "team", oldTeam, ModelSwitcherConfig{}, func(context.Context) error { return nil })
	require.NoError(t, err)
	environments, err := NewTeamEnvironmentStore(oldEnv)
	require.NoError(t, err)
	r, err := NewLocalRuntime(t.Context(), oldTeam, WithTeamEnvironmentStore(environments), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	oldLease, err := environments.AcquireCurrent()
	require.NoError(t, err)
	newAgent := agent.New("root", "new", agent.WithModel(&mockProvider{id: "test/new-tools"}), agent.WithToolSets(&valueSubscribers{counts: newCounts}))
	newTeam := team.New(team.WithAgents(newAgent))
	newEnv, err := NewTeamEnvironment("r2", "source", "baseline", "team", newTeam, ModelSwitcherConfig{}, func(context.Context) error { return nil })
	require.NoError(t, err)
	require.NoError(t, environments.Publish("source", "r1", newEnv))
	r.OnToolsChanged(func(Event) {})
	require.EqualValues(t, 1, oldCounts.tools.Load())
	require.EqualValues(t, 1, newCounts.tools.Load())
	r.OnToolsChanged(nil)
	require.EqualValues(t, 1, oldCounts.releases.Load())
	require.EqualValues(t, 1, newCounts.releases.Load())
	require.NoError(t, oldLease.Release(t.Context()))
	require.NoError(t, r.Close())
}
