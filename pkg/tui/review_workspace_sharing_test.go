package tui

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/gitreview"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	view "github.com/docker/docker-agent/pkg/tui/components/review"
)

// Sessions in one workspace on the same backend attach to one provider, so the
// peer reuses the first bootstrap instead of constructing and scanning again.
func TestSameWorkspaceSessionsShareProviderBootstrap(t *testing.T) {
	h := newReviewHarness(t, 140)
	root := t.TempDir()
	h.sess.WorkingDir = root
	var factoryCalls atomic.Int32
	h.m.gitReviewFactory = func(*app.App, string, string) (gitreview.Provider, *view.Snapshot, error) {
		factoryCalls.Add(1)
		return h.provider, nil, nil
	}
	peer := &session.Session{ID: "s2", Title: "peer session", WorkingDir: root}
	peerApp := app.New(t.Context(), nil, peer, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	_, err := h.m.supervisor.AddSession(t.Context(), peerApp, peer, root, nil)
	require.NoError(t, err)
	h.m.chatPages["s2"] = &reviewChatPage{overlayWidth: 140}

	h.open(false)
	require.Equal(t, int32(1), factoryCalls.Load())
	require.Equal(t, 1, h.provider.snapshots)

	h.m.paneWorkspaces.active = &paneWorkspace{anchor: "s2"}
	h.m.supervisor.SwitchTo("s2")
	h.pump(h.m.syncGitReview())
	h.open(false)
	assert.Equal(t, int32(1), factoryCalls.Load(), "same workspace and backend: one provider is constructed")
	assert.Equal(t, 1, h.provider.snapshots, "the peer attaches to the cached snapshot instead of rescanning")
}

// gatedScan holds a full scan inside Snapshot until its context ends.
type gatedScan struct {
	entered   chan struct{}
	done      chan struct{}
	snapshots atomic.Int32
}

type gatedProvider struct {
	*fakeReviewProvider

	gate *gatedScan
}

func (p gatedProvider) Snapshot(ctx context.Context) (gitreview.Snapshot, error) {
	p.gate.snapshots.Add(1)
	p.gate.entered <- struct{}{}
	<-ctx.Done()
	p.gate.done <- struct{}{}
	return gitreview.Snapshot{}, ctx.Err()
}

// A scan stays alive while any attached peer still needs it, and is cancelled
// only when the last consumer releases the scope.
func TestScopeScanSurvivesOwnerHandoffUntilLastRelease(t *testing.T) {
	h := newReviewHarness(t, 140)
	gate := &gatedScan{entered: make(chan struct{}, 1), done: make(chan struct{}, 1)}
	provider := gatedProvider{fakeReviewProvider: h.provider, gate: gate}
	factory := func(*app.App, string, string) (gitreview.Provider, *view.Snapshot, error) { return provider, nil, nil }

	scope := newGitReviewScope(t.Context(), "local")
	scope.attach("A", "s1")
	_, _, err := scope.open(factory, nil, "A", "s1", "/work")
	require.NoError(t, err)
	scope.setFullLease("A", true)
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the scan never entered Snapshot")
	}

	scope.attach("B", "s2")
	scope.setFullLease("B", true)
	require.False(t, scope.release("A"), "B is still attached")
	select {
	case <-gate.done:
		t.Fatal("the scan was cancelled while peer B still needs it")
	default:
	}

	require.True(t, scope.release("B"), "the last release closes the scope")
	select {
	case <-gate.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the scan was not cancelled after the last release")
	}
	assert.Equal(t, int32(1), gate.snapshots.Load(), "peers share one scan")
}

// Each workspace key is isolated by root, policy and safety; equal inputs share.
func TestGitReviewScopeKeyIsolatesRootAndPolicy(t *testing.T) {
	newApp := func(id, dir string, approved bool, policy session.SafetyPolicy) *app.App {
		sess := &session.Session{ID: id, WorkingDir: dir, ToolsApproved: approved, SafetyPolicy: policy}
		return app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	}
	key := func(a *app.App, dir string) string {
		id, ok := resolveGitReviewIdentity(t.Context(), a, dir)
		require.True(t, ok)
		return id.key
	}
	root, other := t.TempDir(), t.TempDir()
	base := key(newApp("s1", root, false, ""), root)
	tests := []struct {
		name  string
		app   *app.App
		dir   string
		equal bool
	}{
		{name: "peer session same root and policy", app: newApp("s2", root, false, ""), dir: root, equal: true},
		{name: "lexically equal root", app: newApp("s3", root+"/", false, ""), dir: root + "/", equal: true},
		{name: "other root", app: newApp("s4", other, false, ""), dir: other},
		{name: "approval differs", app: newApp("s5", root, true, ""), dir: root},
		{name: "safety differs", app: newApp("s6", root, false, session.SafetyPolicyStrict), dir: root},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.equal, key(tt.app, tt.dir) == base)
		})
	}
}

// Ordinary updates reuse the attached scope without resolving identity again,
// so no identity or bootstrap work is scheduled after the review is attached.
func TestOrdinaryUpdatesScheduleNoIdentityWork(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.sess.WorkingDir = t.TempDir()
	var factoryCalls atomic.Int32
	h.m.gitReviewFactory = func(*app.App, string, string) (gitreview.Provider, *view.Snapshot, error) {
		factoryCalls.Add(1)
		return h.provider, nil, nil
	}
	h.open(false)
	require.Equal(t, int32(1), factoryCalls.Load())
	require.Equal(t, 1, h.provider.snapshots)

	for range 3 {
		h.pump(h.m.syncGitReview())
		assert.NotContains(t, h.m.gitReviewPending, "identity", "no identity request is scheduled by ordinary updates")
	}
	assert.Equal(t, int32(1), factoryCalls.Load())
	assert.Equal(t, 1, h.provider.snapshots)
}

// A same-session policy change releases the old scope and rebootstraps, so cached
// data from before the change never serves the new policy.
func TestSameSessionPolicyChangeRevalidatesScope(t *testing.T) {
	h := newReviewHarness(t, 140)
	h.sess.WorkingDir = t.TempDir()
	var factoryCalls atomic.Int32
	h.m.gitReviewFactory = func(*app.App, string, string) (gitreview.Provider, *view.Snapshot, error) {
		factoryCalls.Add(1)
		return h.provider, nil, nil
	}
	h.open(false)
	before := h.m.activeReview().scopeKey
	require.NotEmpty(t, before)
	require.Equal(t, int32(1), factoryCalls.Load())

	h.sess.SafetyPolicy = session.SafetyPolicyStrict
	h.pump(h.m.syncGitReview())

	after := h.m.activeReview()
	require.NotNil(t, after)
	assert.NotEqual(t, before, after.scopeKey, "the policy change moves the workspace to a new scope")
	assert.Equal(t, int32(2), factoryCalls.Load(), "the new scope bootstraps its own provider")
	assert.Nil(t, h.m.gitReviewScopes.get(before), "the old scope has no attachments left")
}
