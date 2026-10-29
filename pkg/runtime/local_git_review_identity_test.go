package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestLocalGitReviewIdentityFollowsRuntimeNotViewWrapper(t *testing.T) {
	newRuntime := func() *LocalRuntime {
		rt, err := NewLocalRuntime(t.Context(), viewEnvironmentTeam("root", "root-stable"), WithSessionStore(session.NewInMemorySessionStore()), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)
		owner := NewSessionRuntimeSupervisor(rt)
		t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
		return rt
	}
	first, second := newRuntime(), newRuntime()

	identityOf := func(rt SessionRuntime) string {
		identifier, ok := rt.(gitReviewBackend)
		require.True(t, ok)
		return identifier.GitReviewBackendIdentity()
	}
	view := NewSessionRuntimeSupervisor(first).Runtime()
	require.NotEmpty(t, identityOf(view))
	require.Equal(t, identityOf(view), identityOf(NewSessionRuntimeSupervisor(first).Runtime()), "fresh views of one runtime are one backend")
	require.Equal(t, identityOf(view), first.GitReviewBackendIdentity())
	require.NotEqual(t, identityOf(view), identityOf(NewSessionRuntimeSupervisor(second).Runtime()), "unrelated runtimes stay isolated")

	var nilRuntime *LocalRuntime
	require.Empty(t, nilRuntime.GitReviewBackendIdentity())
}
