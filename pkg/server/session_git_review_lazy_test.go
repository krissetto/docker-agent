package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/gitreview"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// lazyGitInit creates a minimal repository; discovery only needs the metadata.
func lazyGitInit(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, exec.CommandContext(t.Context(), "git", "-C", dir, "init", "--quiet", "-b", "main").Run())
}

// addNoise adds ordinary deep and bulky content that discovery must not turn
// into repositories or unbounded listings.
func addNoise(t *testing.T, repo string) {
	t.Helper()
	deep := filepath.Join(repo, "deep")
	for i := range 10 {
		deep = filepath.Join(deep, fmt.Sprintf("level-%02d", i))
	}
	require.NoError(t, os.MkdirAll(deep, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(deep, "leaf.txt"), []byte("leaf\n"), 0o600))
	bulk := filepath.Join(repo, "noise")
	require.NoError(t, os.MkdirAll(bulk, 0o755))
	for i := range 60 {
		require.NoError(t, os.WriteFile(filepath.Join(bulk, fmt.Sprintf("item-%03d.txt", i)), []byte("x\n"), 0o600))
	}
	modules := filepath.Join(repo, "node_modules", "pkg")
	require.NoError(t, os.MkdirAll(modules, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(modules, "index.js"), []byte("module.exports = {}\n"), 0o600))
}

// addLinkedWorktree records the same metadata `git worktree add` writes, so a
// linked checkout is a .git file pointing into the owning repository.
func addLinkedWorktree(t *testing.T, repo, linked, branch string) {
	t.Helper()
	meta := filepath.Join(repo, ".git", "worktrees", filepath.Base(linked))
	require.NoError(t, os.MkdirAll(meta, 0o755))
	require.NoError(t, os.MkdirAll(linked, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: "+meta+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(meta, "gitdir"), []byte(filepath.Join(linked, ".git")+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(meta, "commondir"), []byte("../..\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(meta, "HEAD"), []byte("ref: refs/heads/"+branch+"\n"), 0o600))
}

// TestRemoteGitReviewOverHTTPUsesRealProviderWithBoundedDiscovery drives the
// real server routes through the runtime remote adapter. Files opens one
// folder at a time, discovery stops at the default repository cap, and a linked
// worktree stays grouped under its owning repository.
func TestRemoteGitReviewOverHTTPUsesRealProviderWithBoundedDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name     string
		siblings int
		limited  bool
	}{
		{name: "100 siblings plus workspace complete", siblings: 100, limited: false},
		{name: "120 siblings are reported partial", siblings: 120, limited: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			lazyGitInit(t, root)
			for i := range tc.siblings {
				repo := filepath.Join(root, "repos", fmt.Sprintf("sibling-%03d", i))
				lazyGitInit(t, repo)
				if i%10 == 0 {
					addNoise(t, repo)
				}
			}
			linked := filepath.Join(root, "repos", "sibling-000-wt")
			addLinkedWorktree(t, filepath.Join(root, "repos", "sibling-000"), linked, "feature")
			require.NoError(t, os.WriteFile(filepath.Join(root, "docs.md"), []byte("docs\n"), 0o600))

			store := session.NewInMemorySessionStore()
			local, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "", agent.WithModel(sessionHTTPProvider{})))), runtime.WithSessionStore(store), runtime.WithWorkingDir(root))
			require.NoError(t, err)
			supervisor := runtime.NewSessionRuntimeSupervisor(local)
			t.Cleanup(func() { require.NoError(t, supervisor.Shutdown(context.WithoutCancel(t.Context()))) })
			_, err = supervisor.Runtime().CreateSession(t.Context(), session.New(session.WithID("git-review-lazy"), session.WithAgentName("root"), session.WithWorkingDir(root)), runtime.SessionBinding{AgentName: "root"})
			require.NoError(t, err)
			sm := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntime(supervisor.Runtime()))
			srv := NewWithManager(sm, "git-review-token")

			var mu sync.Mutex
			var directoryPaths []string
			recorded := func() []string {
				mu.Lock()
				defer mu.Unlock()
				return slices.Clone(directoryPaths)
			}
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/git-review/directory") {
					body, _ := io.ReadAll(r.Body)
					r.Body = io.NopCloser(bytes.NewReader(body))
					var req api.GitReviewDirectory
					_ = json.Unmarshal(body, &req)
					mu.Lock()
					directoryPaths = append(directoryPaths, req.Path)
					mu.Unlock()
				}
				srv.e.ServeHTTP(w, r)
			}))
			defer ts.Close()

			client, err := runtime.NewClient(ts.URL, runtime.WithHTTPClient(ts.Client()), runtime.WithAuthToken("git-review-token"))
			require.NoError(t, err)
			transport, err := runtime.NewSessionTransport(client)
			require.NoError(t, err)
			provider, err := runtime.NewGitReviewRemote(transport, "git-review-lazy")
			require.NoError(t, err)

			snapshot, err := provider.Snapshot(t.Context())
			require.NoError(t, err)
			if tc.limited {
				require.Equal(t, gitreview.DiscoveryLimitRepositories, snapshot.Scope.LimitReason)
				require.NotEmpty(t, snapshot.Repositories, "partial discovery still shows genuine repositories")
				require.LessOrEqual(t, len(snapshot.Repositories), 101, "100 discovered repositories plus the synthetic workspace")
			} else {
				require.Empty(t, snapshot.Scope.LimitReason, "100 nested repositories fit the default cap beside the workspace")
				require.Len(t, snapshot.Repositories, 101)
			}

			// The linked checkout is one worktree of its owning repository, never a
			// repository of its own.
			var owners []gitreview.Repository
			linkedWorktrees := 0
			for _, repo := range snapshot.Repositories {
				for _, wt := range repo.Worktrees {
					if wt.Linked && strings.Contains(wt.Path, "sibling-000-wt") {
						owners = append(owners, repo)
						linkedWorktrees++
					}
				}
			}
			require.LessOrEqual(t, linkedWorktrees, 1)
			if !tc.limited {
				require.Equal(t, 1, linkedWorktrees, "the linked checkout is discovered in complete scans")
			}
			for _, repo := range owners {
				require.Len(t, repo.Worktrees, 2, "the owning repository lists its main and linked checkouts")
			}
			require.Empty(t, recorded(), "opening the session lists no folders")

			var owner string
			for _, repo := range snapshot.Repositories {
				for _, wt := range repo.Worktrees {
					if wt.Current {
						owner = wt.ID
					}
				}
			}
			require.NotEmpty(t, owner)
			rootListing, err := provider.Directory(t.Context(), owner, "")
			require.NoError(t, err)
			require.Contains(t, entryNames(rootListing.Entries), "repos/")
			require.Equal(t, []string{""}, recorded(), "Files requests only the scope root")

			child, err := provider.Directory(t.Context(), owner, "repos")
			require.NoError(t, err)
			require.NotEmpty(t, child.Entries)
			require.Equal(t, []string{"", "repos"}, recorded(), "expanding one folder requests only that folder")
		})
	}
}

func entryNames(entries []gitreview.DirectoryEntry) []string {
	var out []string
	for _, e := range entries {
		name := e.Name
		if e.Dir {
			name += "/"
		}
		out = append(out, name)
	}
	return out
}

// Files bootstraps through the authenticated root-only route: nearby repositories
// exist, yet no full snapshot runs until the Git side explicitly asks for one.
func TestRemoteGitReviewBrowseIsRootOnlyUntilExplicitScan(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	lazyGitInit(t, root)
	for i := range 3 {
		lazyGitInit(t, filepath.Join(root, "repos", fmt.Sprintf("sibling-%03d", i)))
	}
	store := session.NewInMemorySessionStore()
	local, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "", agent.WithModel(sessionHTTPProvider{})))), runtime.WithSessionStore(store), runtime.WithWorkingDir(root))
	require.NoError(t, err)
	supervisor := runtime.NewSessionRuntimeSupervisor(local)
	t.Cleanup(func() { require.NoError(t, supervisor.Shutdown(context.WithoutCancel(t.Context()))) })
	_, err = supervisor.Runtime().CreateSession(t.Context(), session.New(session.WithID("git-review-browse"), session.WithAgentName("root"), session.WithWorkingDir(root)), runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	sm := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntime(supervisor.Runtime()))
	srv := NewWithManager(sm, "git-review-token")

	var mu sync.Mutex
	var calls []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, api.SessionAPIPath) || strings.Contains(r.URL.Path, "/git-review") {
			mu.Lock()
			calls = append(calls, r.Method+" "+r.URL.Path)
			mu.Unlock()
		}
		srv.e.ServeHTTP(w, r)
	}))
	defer ts.Close()

	unauthorized := httptest.NewRecorder()
	srv.e.ServeHTTP(unauthorized, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v2/sessions/git-review-browse/git-review/browse", nil))
	require.Equal(t, http.StatusUnauthorized, unauthorized.Code)

	client, err := runtime.NewClient(ts.URL, runtime.WithHTTPClient(ts.Client()), runtime.WithAuthToken("git-review-token"))
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	provider, err := runtime.NewGitReviewRemote(transport, "git-review-browse")
	require.NoError(t, err)

	first, err := provider.BrowseRoot(t.Context())
	require.NoError(t, err)
	require.True(t, first.Browse)
	require.Len(t, first.Repositories, 1, "the root bootstrap never inventories nearby repositories")
	require.NotZero(t, first.Generation)
	second, err := provider.BrowseRoot(t.Context())
	require.NoError(t, err)
	require.Equal(t, first.Generation, second.Generation, "a repeated browse keeps the same generation")
	var owner string
	for _, wt := range first.Repositories[0].Worktrees {
		if wt.Current {
			owner = wt.ID
		}
	}
	require.NotEmpty(t, owner)
	listing, err := provider.Directory(t.Context(), owner, "")
	require.NoError(t, err)
	require.Contains(t, entryNames(listing.Entries), "repos/")

	mu.Lock()
	require.Equal(t, []string{
		"POST /api/v2/sessions/git-review-browse/git-review/browse",
		"POST /api/v2/sessions/git-review-browse/git-review/browse",
		"POST /api/v2/sessions/git-review-browse/git-review/directory",
	}, calls, "Files issues root and directory requests only")
	mu.Unlock()

	full, err := provider.Snapshot(t.Context())
	require.NoError(t, err)
	require.False(t, full.Browse)
	require.Greater(t, len(full.Repositories), 1, "the explicit Git scan discovers nearby repositories")
}
