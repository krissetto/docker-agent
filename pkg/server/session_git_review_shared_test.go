package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/gitreview"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// fakePermissionsHandle overrides only the inspector; other SessionHandle methods are never called here.
type fakePermissionsHandle struct {
	runtime.SessionHandle

	info runtime.SessionPermissionsInfo
}

func (h fakePermissionsHandle) EffectivePermissions(context.Context) (runtime.SessionPermissionsInfo, error) {
	return h.info, nil
}

func newReviewSession(id string) *session.Session {
	sess := session.New(session.WithID(id), session.WithWorkingDir("/canonical/workspace"))
	sess.SetAttribute("docker-agent.actor.source", "agent.yaml")
	return sess
}

func TestGitReviewKeyIgnoresSessionIDButSeparatesEffectivePolicy(t *testing.T) {
	keyOf := func(handle runtime.SessionHandle, sess *session.Session, root string) string {
		key, err := gitReviewKey(t.Context(), handle, sess, root)
		require.NoError(t, err)
		return key
	}
	teamDeny := fakePermissionsHandle{info: runtime.SessionPermissionsInfo{Source: &runtime.PermissionsInfo{Deny: []string{"shell"}}}}
	base := keyOf(nil, newReviewSession("first"), "/canonical/workspace")
	require.Equal(t, base, keyOf(nil, newReviewSession("second"), "/canonical/workspace"), "peer sessions on one workspace share a provider")
	require.Equal(t, keyOf(teamDeny, newReviewSession("a"), "/canonical/workspace"), keyOf(teamDeny, newReviewSession("b"), "/canonical/workspace"), "equal effective rules share")
	require.NotEqual(t, base, keyOf(teamDeny, newReviewSession("c"), "/canonical/workspace"), "team rules change the effective policy")

	ask := fakePermissionsHandle{info: runtime.SessionPermissionsInfo{Source: &runtime.PermissionsInfo{Ask: []string{"shell"}}}}
	require.NotEqual(t, keyOf(teamDeny, newReviewSession("d"), "/canonical/workspace"), keyOf(ask, newReviewSession("d"), "/canonical/workspace"), "different team rules are different boundaries")

	approved := newReviewSession("approved")
	approved.ToolsApproved = true
	require.NotEqual(t, base, keyOf(nil, approved, "/canonical/workspace"), "approval changes the effective policy")

	otherSource := newReviewSession("source")
	otherSource.SetAttribute("docker-agent.actor.source", "other.yaml")
	require.NotEqual(t, base, keyOf(nil, otherSource, "/canonical/workspace"), "source is part of the boundary")
	require.NotEqual(t, base, keyOf(nil, newReviewSession("root"), "/other/workspace"), "canonical root is part of the boundary")
}

func newSharedGitReviewServer(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()
	store := session.NewInMemorySessionStore()
	local, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "", agent.WithModel(sessionHTTPProvider{})))), runtime.WithSessionStore(store), runtime.WithWorkingDir(root))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(local)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	for _, id := range []string{"shared-review-a", "shared-review-b"} {
		_, err = owner.Runtime().CreateSession(t.Context(), session.New(session.WithID(id), session.WithAgentName("root"), session.WithWorkingDir(root)), runtime.SessionBinding{AgentName: "root"})
		require.NoError(t, err)
	}
	sm := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntime(owner.Runtime()))
	return NewWithManager(sm, "git-review-token")
}

// countProviderBuilds counts provider constructions only; scan coalescing is
// covered by the gitreview.Shared tests.
func countProviderBuilds(t *testing.T) *atomic.Int32 {
	t.Helper()
	var builds atomic.Int32
	oldFactory := newLocalGitReview
	newLocalGitReview = func(path string) (*gitreview.Local, error) { builds.Add(1); return gitreview.NewLocal(path) }
	t.Cleanup(func() { newLocalGitReview = oldFactory })
	return &builds
}

func sharedReviewRequest(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer git-review-token")
	rec := httptest.NewRecorder()
	srv.e.ServeHTTP(rec, req)
	return rec
}

func decodeReviewSnapshot(t *testing.T, rec *httptest.ResponseRecorder) api.GitReviewSnapshot {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var snapshot api.GitReviewSnapshot
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snapshot))
	return snapshot
}

func TestGitReviewSessionIDsShareOneProviderAndGeneration(t *testing.T) {
	srv := newSharedGitReviewServer(t)
	builds := countProviderBuilds(t)

	browseA := decodeReviewSnapshot(t, sharedReviewRequest(t, srv, http.MethodPost, "/api/v2/sessions/shared-review-a/git-review/browse", ""))
	require.True(t, browseA.Snapshot.Browse)
	browseB := decodeReviewSnapshot(t, sharedReviewRequest(t, srv, http.MethodPost, "/api/v2/sessions/shared-review-b/git-review/browse", ""))
	require.Equal(t, browseA.Snapshot.Generation, browseB.Snapshot.Generation, "a peer session attaches to the cached inventory")
	require.Equal(t, int32(1), builds.Load())

	require.NotEmpty(t, browseA.Snapshot.Repositories)
	worktreeID := browseA.Snapshot.Repositories[0].Worktrees[0].ID
	directory := func(sessionID string, generation uint64) *httptest.ResponseRecorder {
		body, err := json.Marshal(api.GitReviewDirectory{Generation: generation, WorktreeID: worktreeID})
		require.NoError(t, err)
		return sharedReviewRequest(t, srv, http.MethodPost, "/api/v2/sessions/"+sessionID+"/git-review/directory", string(body))
	}
	require.Equal(t, http.StatusOK, directory("shared-review-b", browseB.Snapshot.Generation).Code)

	refreshed := decodeReviewSnapshot(t, sharedReviewRequest(t, srv, http.MethodGet, "/api/v2/sessions/shared-review-a/git-review", ""))
	require.Greater(t, refreshed.Snapshot.Generation, browseA.Snapshot.Generation, "an explicit refresh through one session moves the shared generation")
	require.Equal(t, http.StatusConflict, directory("shared-review-b", browseB.Snapshot.Generation).Code, "the peer's old generation is rejected")
	require.Equal(t, http.StatusOK, directory("shared-review-b", refreshed.Snapshot.Generation).Code, "the peer reads the refreshed generation")
	require.Equal(t, int32(1), builds.Load())
}

func TestGitReviewConcurrentAttachReusesOneProvider(t *testing.T) {
	srv := newSharedGitReviewServer(t)
	builds := countProviderBuilds(t)

	// Warm the session so the burst measures provider sharing, not the cold session open.
	require.Equal(t, http.StatusOK, sharedReviewRequest(t, srv, http.MethodPost, "/api/v2/sessions/shared-review-a/git-review/browse", "").Code)
	const attaches = 8
	codes := make([]int, attaches)
	var wg sync.WaitGroup
	for i := range attaches {
		wg.Go(func() {
			codes[i] = sharedReviewRequest(t, srv, http.MethodPost, "/api/v2/sessions/shared-review-a/git-review/browse", "").Code
		})
	}
	wg.Wait()
	for _, code := range codes {
		require.Equal(t, http.StatusOK, code)
	}
	require.Equal(t, int32(1), builds.Load(), "concurrent attaches pin one provider")
}

func TestGitReviewReadPathsNeverCreateProviders(t *testing.T) {
	srv := newSharedGitReviewServer(t)
	builds := countProviderBuilds(t)

	rec := sharedReviewRequest(t, srv, http.MethodPost, "/api/v2/sessions/shared-review-a/git-review/worktree", `{"generation":1,"worktree_id":"wt"}`)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, int32(0), builds.Load())
}

func TestGitReviewProviderPinsBoundCacheEviction(t *testing.T) {
	srv := newSharedGitReviewServer(t)
	type pinned struct {
		provider *gitreview.Shared
		release  func()
	}
	held := make([]pinned, 0, gitReviewCacheEntries)
	for i := range gitReviewCacheEntries {
		provider, release, err := srv.gitReviewAcquire(fmt.Sprintf("key-%d", i), t.TempDir(), true)
		require.NoError(t, err)
		held = append(held, pinned{provider, release})
	}
	defer func() {
		for _, p := range held {
			p.release()
		}
	}()

	_, _, err := srv.gitReviewAcquire("key-overflow", t.TempDir(), true)
	var httpErr *echo.HTTPError
	require.ErrorAs(t, err, &httpErr)
	require.Equal(t, http.StatusServiceUnavailable, httpErr.Code, "pinned providers are never evicted")

	first := held[0].provider
	held[0].release()
	held = held[1:]
	_, release, err := srv.gitReviewAcquire("key-overflow", t.TempDir(), true)
	require.NoError(t, err)
	release()
	_, err = first.Snapshot(t.Context())
	require.Error(t, err, "the evicted unpinned provider is closed")
}

// Two provider lifetimes for one scope: after eviction and rebuild, a generation
// from the first lifetime must not validate against the second.
func TestGitReviewRebuiltProviderRejectsPreviousLifetimeGeneration(t *testing.T) {
	srv := newSharedGitReviewServer(t)
	countProviderBuilds(t)

	browse := func() api.GitReviewSnapshot {
		return decodeReviewSnapshot(t, sharedReviewRequest(t, srv, http.MethodPost, "/api/v2/sessions/shared-review-a/git-review/browse", ""))
	}
	first := browse()
	worktreeID := first.Snapshot.Repositories[0].Worktrees[0].ID
	directory := func(generation uint64) int {
		body, err := json.Marshal(api.GitReviewDirectory{Generation: generation, WorktreeID: worktreeID})
		require.NoError(t, err)
		return sharedReviewRequest(t, srv, http.MethodPost, "/api/v2/sessions/shared-review-a/git-review/directory", string(body)).Code
	}
	require.Equal(t, http.StatusOK, directory(first.Snapshot.Generation))

	gitReviewCache.Lock()
	var scopeKey string
	for key := range gitReviewCache.entries[srv] {
		scopeKey = key
	}
	gitReviewCache.entries[srv][scopeKey].touched = time.Time{}
	gitReviewCache.Unlock()

	var pins []func()
	for i := range gitReviewCacheEntries - 1 {
		_, release, err := srv.gitReviewAcquire(fmt.Sprintf("pin-%d", i), t.TempDir(), true)
		require.NoError(t, err)
		pins = append(pins, release)
	}
	_, release, err := srv.gitReviewAcquire("pin-overflow", t.TempDir(), true)
	require.NoError(t, err)
	release()
	for _, unpin := range pins {
		unpin()
	}
	gitReviewCache.Lock()
	_, stillCached := gitReviewCache.entries[srv][scopeKey]
	gitReviewCache.Unlock()
	require.False(t, stillCached, "the unpinned scope provider was evicted")

	second := browse()
	require.NotEqual(t, first.Snapshot.Generation, second.Snapshot.Generation, "a rebuilt provider takes a fresh generation")
	require.Equal(t, http.StatusConflict, directory(first.Snapshot.Generation), "the previous lifetime's generation is rejected")
	require.Equal(t, http.StatusOK, directory(second.Snapshot.Generation), "the rebuilt provider serves its own generation")
}
