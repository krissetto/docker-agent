package supervisor

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/stretchr/testify/require"
)

func policyRuntime(t *testing.T) *runtime.LocalRuntime {
	t.Helper()
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "fixture", agent.WithModel(policyProvider{})))))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

func TestSubagentsPreferenceIncludesRetainedUniqueOwnersAndSaveFailure(t *testing.T) {
	s := New(nil)
	first, idle := policyRuntime(t), policyRuntime(t)
	a := app.New(t.Context(), nil, session.New(), runtime.SessionBinding{}, app.WithRuntimeServices(first))
	s.runners["root"] = &SessionTab{App: a}
	s.runners["attached"] = &SessionTab{App: a}
	s.ownerResources = []*viewOwner{{resources: ViewOwnerResources{Services: idle}, retained: true}}
	saves := 0
	require.ErrorContains(t, s.SaveUseSubagents(false, first, func(bool) error { return errors.New("disk full") }), "disk full")
	require.True(t, first.UseSubagents())
	require.True(t, idle.UseSubagents())
	require.NoError(t, s.SaveUseSubagents(false, first, func(enabled bool) error { saves++; require.False(t, enabled); return nil }))
	require.Equal(t, 1, saves)
	require.False(t, first.UseSubagents())
	require.False(t, idle.UseSubagents())
	require.ErrorContains(t, s.SaveUseSubagents(true, nil, func(bool) error { t.Fatal("unsupported must not save"); return nil }), "without policy support")
	require.False(t, first.UseSubagents())
}

func TestSubagentsPreferenceConcurrentOwnerRegistrationInheritsSavedPolicy(t *testing.T) {
	s := New(nil)
	scope := new(ViewOwnerScope)
	require.NoError(t, s.ConfigureSessionViews(t.Context(), HostViewConfig{Resolve: func(context.Context, string) (ViewOwnerIdentity, error) { return ViewOwnerIdentity{}, nil }}))
	current, late := policyRuntime(t), policyRuntime(t)
	entered, release, saved := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		saved <- s.SaveUseSubagents(false, current, func(bool) error { close(entered); <-release; return nil })
	}()
	<-entered
	registered := make(chan error, 1)
	go func() {
		registered <- s.RegisterSessionOwner(ViewOwnerIdentity{Scope: scope, RootSessionID: "late"}, ViewOwnerResources{Services: late, Sessions: &ownerTestRuntime{}, NewApp: func(context.Context, runtime.CommittedSessionView) (*app.App, error) { return nil, nil }}, false)
	}()
	close(release)
	require.NoError(t, <-saved)
	require.NoError(t, <-registered)
	require.False(t, late.UseSubagents())
}

type policyProvider struct{}

func (policyProvider) ID() modelsdev.ID        { return modelsdev.NewID("test", "fixture") }
func (policyProvider) BaseConfig() base.Config { return base.Config{} }
func (policyProvider) MaxTokens() int          { return 0 }
func (policyProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return nil, errors.New("no live provider")
}

// A decorator may expose the capability without exposing a concrete runtime.
type policyDecorator struct {
	app.Services
	runtime.SubagentPolicy
}

func TestSubagentsPreferenceOptionalCapabilityDecorator(t *testing.T) {
	s := New(nil)
	rt := policyRuntime(t)
	wrapped := policyDecorator{Services: rt, SubagentPolicy: rt}
	require.NoError(t, s.SaveUseSubagents(false, wrapped, func(bool) error { return nil }))
	require.False(t, rt.UseSubagents())
	unsupported := struct{ app.Services }{Services: rt}
	require.ErrorContains(t, s.SaveUseSubagents(true, unsupported, func(bool) error { t.Fatal("unsupported must not save"); return nil }), "without policy support")
	require.False(t, rt.UseSubagents())
}
