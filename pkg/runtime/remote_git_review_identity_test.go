package runtime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/gitreview"
)

func newIdentityClient(t *testing.T, address, token string) *Client {
	t.Helper()
	client, err := NewClient(address, WithAuthToken(token))
	require.NoError(t, err)
	return client
}

func TestGitReviewBackendIdentityIsSharedOnlyWithinOneBorrowedClient(t *testing.T) {
	const address = "https://agents.example.invalid:8443"
	client := newIdentityClient(t, address, "token-one")
	first, err := NewSessionTransport(client)
	require.NoError(t, err)
	second, err := NewSessionTransport(client)
	require.NoError(t, err)
	require.NotSame(t, first, second)
	require.NotEmpty(t, first.GitReviewBackendIdentity())
	require.Equal(t, first.GitReviewBackendIdentity(), second.GitReviewBackendIdentity(), "transports borrowing one Client are one backend")

	fresh, err := NewSessionTransport(newIdentityClient(t, address, "token-one"))
	require.NoError(t, err)
	require.NotEqual(t, first.GitReviewBackendIdentity(), fresh.GitReviewBackendIdentity(), "separately constructed clients stay isolated")

	var zero SessionTransport
	require.Empty(t, zero.GitReviewBackendIdentity(), "a transport without a client has no backend")
}

func TestGitReviewBackendIdentityIsEmptyForTypedNilTransport(t *testing.T) {
	var nilTransport *SessionTransport
	require.Empty(t, nilTransport.GitReviewBackendIdentity())
	var sessions SessionGitReview = nilTransport
	remote, err := NewGitReviewRemote(sessions, "session")
	require.NoError(t, err)
	require.Empty(t, remote.backend, "a typed-nil transport binds no backend")
}

func TestGitReviewRemoteRetargetKeepsGenerationAcrossEqualBackendPeers(t *testing.T) {
	var mu sync.Mutex
	var worktreePaths, worktreeGenerations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer shared-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/git-review/worktree") {
			var req api.GitReviewWorktreeRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			worktreePaths = append(worktreePaths, r.URL.Path)
			worktreeGenerations = append(worktreeGenerations, strconv.FormatUint(req.Generation, 10))
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(api.GitReviewWorktree{Generation: req.Generation, Worktree: gitreview.Worktree{ID: req.WorktreeID, State: "clean"}})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	const generation = uint64(7)
	shared := newIdentityClient(t, server.URL, "shared-token")
	original, err := NewSessionTransport(shared)
	require.NoError(t, err)
	peer, err := NewSessionTransport(shared)
	require.NoError(t, err)
	other, err := NewSessionTransport(newIdentityClient(t, server.URL, "shared-token"))
	require.NoError(t, err)

	provider, err := NewGitReviewRemote(original, "closed-session")
	require.NoError(t, err)
	provider.generation = generation

	require.Error(t, provider.Retarget("", peer.GitReviewBackendIdentity()))
	require.Error(t, provider.Retarget("", ""))
	require.Error(t, provider.Retarget("peer-session", other.GitReviewBackendIdentity()), "a separately constructed client is refused")
	require.NoError(t, provider.Retarget("peer-session", peer.GitReviewBackendIdentity()))
	require.Equal(t, generation, provider.generation, "retarget preserves the generation; no forced rescan")

	_, err = provider.LoadWorktree(t.Context(), "wt")
	require.NoError(t, err)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{api.SessionAPIPath + "/peer-session/git-review/worktree"}, worktreePaths)
	require.Equal(t, []string{"7"}, worktreeGenerations)
}
