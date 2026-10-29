package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/gitreview"
)

// legacyGitReviewTransport implements only the original Git review routes.
type legacyGitReviewTransport struct{ snapshots int }

func (l *legacyGitReviewTransport) GitReviewSnapshot(context.Context, string) (gitreview.Snapshot, error) {
	l.snapshots++
	return gitreview.Snapshot{Generation: 1}, nil
}

func (l *legacyGitReviewTransport) GitReviewWorktree(context.Context, string, uint64, string) (gitreview.Worktree, error) {
	return gitreview.Worktree{}, nil
}

func (l *legacyGitReviewTransport) GitReviewDirectory(context.Context, string, uint64, string, string) (gitreview.Directory, error) {
	return gitreview.Directory{}, nil
}

func (l *legacyGitReviewTransport) GitReviewDocument(context.Context, string, uint64, gitreview.DocumentKey) (gitreview.Document, error) {
	return gitreview.Document{}, nil
}

func TestGitReviewRemoteBrowseWithoutServerSupportFailsWithoutScan(t *testing.T) {
	transport := &legacyGitReviewTransport{}
	provider, err := NewGitReviewRemote(transport, "session")
	require.NoError(t, err)
	_, err = provider.BrowseRoot(t.Context())
	require.ErrorIs(t, err, ErrGitReviewBrowseUnsupported)
	require.Zero(t, transport.snapshots, "an unsupported browse never falls back to a repository scan")
}

func TestGitReviewRemoteBrowseHTTPNotFoundDoesNotFallBackToSnapshot(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		http.NotFound(w, r)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithHTTPClient(server.Client()), WithAuthToken("review-token"))
	require.NoError(t, err)
	transport, err := NewSessionTransport(client)
	require.NoError(t, err)
	provider, err := NewGitReviewRemote(transport, "session")
	require.NoError(t, err)
	_, err = provider.BrowseRoot(t.Context())
	require.Error(t, err)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"POST " + api.SessionAPIPath + "/session/git-review/browse"}, paths, "only the root bootstrap route is attempted")
}

func TestGitReviewRemoteBrowseRootSetsGenerationForDirectories(t *testing.T) {
	const generation = uint64(7)
	var mu sync.Mutex
	var browseGenerations []uint64
	var directoryGenerations []uint64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer review-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case api.SessionAPIPath + "/session/git-review/browse":
			mu.Lock()
			browseGenerations = append(browseGenerations, generation)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(api.GitReviewSnapshot{Snapshot: gitreview.Snapshot{Generation: generation, Browse: true, Repositories: []gitreview.Repository{{ID: "owner", Name: "owner", Kind: gitreview.KindNone, Worktrees: []gitreview.Worktree{{ID: "owner-wt", Path: ".", Current: true, State: "unscanned"}}}}}})
		case api.SessionAPIPath + "/session/git-review/directory":
			var req api.GitReviewDirectory
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			directoryGenerations = append(directoryGenerations, req.Generation)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(api.GitReviewDirectoryResult{Generation: req.Generation, Directory: gitreview.Directory{WorktreeID: req.WorktreeID, Path: req.Path, State: gitreview.StateReady}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithHTTPClient(server.Client()), WithAuthToken("review-token"))
	require.NoError(t, err)
	transport, err := NewSessionTransport(client)
	require.NoError(t, err)
	provider, err := NewGitReviewRemote(transport, "session")
	require.NoError(t, err)
	root, err := provider.BrowseRoot(t.Context())
	require.NoError(t, err)
	require.True(t, root.Browse)
	require.Len(t, root.Repositories, 1, "the browse bootstrap carries only the root")
	_, err = provider.Directory(t.Context(), "owner-wt", "")
	require.NoError(t, err)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []uint64{generation}, browseGenerations)
	require.Equal(t, []uint64{generation}, directoryGenerations, "directory reads carry the browse generation")
}
