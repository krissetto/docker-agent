package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/gitreview"
)

func TestSessionTransportGitReviewLoadsReadyDocumentWithAuthAndGeneration(t *testing.T) {
	var requests []string
	snapshot := gitreview.Snapshot{Generation: 7, Scope: gitreview.Scope{Label: "workspace"}, Repositories: []gitreview.Repository{{ID: "repo", Name: "repo", Kind: gitreview.KindPrimary, Worktrees: []gitreview.Worktree{{ID: "wt", Path: ".", Current: true, State: "unscanned"}}}}}
	document := gitreview.Document{Key: gitreview.DocumentKey{Mode: "changes", WorktreeID: "wt", Path: "Dockerfile.dev", Group: gitreview.GroupUnstaged}, State: gitreview.StateReady, PatchHeader: "@@ -1 +1 @@", Lines: []gitreview.DocumentLine{{Kind: gitreview.DiffRemove, Old: 1, Text: "FROM old"}, {Kind: gitreview.DiffAdd, New: 1, Text: "FROM new"}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer review-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		requests = append(requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case api.SessionAPIPath + "/session/git-review":
			_ = json.NewEncoder(w).Encode(api.GitReviewSnapshot{Snapshot: snapshot})
		case api.SessionAPIPath + "/session/git-review/worktree":
			var req api.GitReviewWorktreeRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			require.Equal(t, uint64(7), req.Generation)
			_ = json.NewEncoder(w).Encode(api.GitReviewWorktree{Generation: 7, Worktree: snapshot.Repositories[0].Worktrees[0]})
		case api.SessionAPIPath + "/session/git-review/document":
			var req api.GitReviewDocument
			_ = json.NewDecoder(r.Body).Decode(&req)
			require.Equal(t, uint64(7), req.Generation)
			require.Equal(t, "Dockerfile.dev", req.Key.Path)
			_ = json.NewEncoder(w).Encode(api.GitReviewDocumentResult{Generation: 7, Document: document})
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
	gotSnapshot, err := provider.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, uint64(7), gotSnapshot.Generation)
	gotWT, err := provider.LoadWorktree(t.Context(), "wt")
	require.NoError(t, err)
	require.Equal(t, "wt", gotWT.ID)
	gotDoc, err := provider.Document(t.Context(), document.Key)
	require.NoError(t, err)
	require.Equal(t, gitreview.StateReady, gotDoc.State)
	require.Equal(t, document.Lines, gotDoc.Lines)
	require.Contains(t, gotDoc.Lines, gitreview.DocumentLine{Kind: gitreview.DiffRemove, Old: 1, Text: "FROM old"})
	require.Contains(t, gotDoc.Lines, gitreview.DocumentLine{Kind: gitreview.DiffAdd, New: 1, Text: "FROM new"})
	require.Equal(t, []string{"GET " + api.SessionAPIPath + "/session/git-review", "POST " + api.SessionAPIPath + "/session/git-review/worktree", "POST " + api.SessionAPIPath + "/session/git-review/document"}, requests)
}

func TestSessionTransportGitReviewRejectsStaleResponseAndPropagatesCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == api.SessionAPIPath+"/s/git-review" {
			_ = json.NewEncoder(w).Encode(api.GitReviewSnapshot{Snapshot: gitreview.Snapshot{Generation: 2}})
			return
		}
		if r.URL.Path == api.SessionAPIPath+"/s/git-review/document" {
			_ = json.NewEncoder(w).Encode(api.GitReviewDocumentResult{Generation: 1})
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := NewSessionTransport(client)
	require.NoError(t, err)
	provider, err := NewGitReviewRemote(transport, "s")
	require.NoError(t, err)
	_, err = provider.Snapshot(t.Context())
	require.NoError(t, err)
	_, err = provider.Document(t.Context(), gitreview.DocumentKey{Mode: "files", WorktreeID: "wt", Path: "a"})
	require.ErrorContains(t, err, "stale Git review document response")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = transport.GitReviewDirectory(ctx, "s", 2, "wt", ".")
	require.Error(t, err)
}

func TestGitReviewRemoteRefreshInvalidatesInflightDocuments(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case api.SessionAPIPath + "/s/git-review":
			_ = json.NewEncoder(w).Encode(api.GitReviewSnapshot{Snapshot: gitreview.Snapshot{Generation: 7}})
		case api.SessionAPIPath + "/s/git-review/worktree":
			var req api.GitReviewWorktreeRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			require.True(t, req.Refresh)
			require.Equal(t, uint64(7), req.Generation)
			_ = json.NewEncoder(w).Encode(api.GitReviewWorktree{Generation: 7, Worktree: gitreview.Worktree{ID: req.WorktreeID, State: "changed"}})
		case api.SessionAPIPath + "/s/git-review/document":
			close(entered)
			<-release
			_ = json.NewEncoder(w).Encode(api.GitReviewDocumentResult{Generation: 7, Document: gitreview.Document{State: gitreview.StateReady}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := NewSessionTransport(client)
	require.NoError(t, err)
	provider, err := NewGitReviewRemote(transport, "s")
	require.NoError(t, err)
	_, err = provider.Snapshot(t.Context())
	require.NoError(t, err)

	docDone := make(chan error, 1)
	go func() {
		_, docErr := provider.Document(t.Context(), gitreview.DocumentKey{Mode: "files", WorktreeID: "wt", Path: "file.txt"})
		docDone <- docErr
	}()
	<-entered
	wt, err := provider.RefreshWorktree(t.Context(), "wt")
	require.NoError(t, err)
	require.Equal(t, "wt", wt.ID)
	close(release)
	require.ErrorContains(t, <-docDone, "stale Git review document response")
}

func TestGitReviewRemoteCoalescesOverlappingRefreshForWorktree(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var refreshCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case api.SessionAPIPath + "/s/git-review":
			_ = json.NewEncoder(w).Encode(api.GitReviewSnapshot{Snapshot: gitreview.Snapshot{Generation: 7}})
		case api.SessionAPIPath + "/s/git-review/worktree":
			refreshCalls.Add(1)
			<-entered
			<-release
			_ = json.NewEncoder(w).Encode(api.GitReviewWorktree{Generation: 7, Worktree: gitreview.Worktree{ID: "wt", State: "changed"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := NewSessionTransport(client)
	require.NoError(t, err)
	provider, err := NewGitReviewRemote(transport, "s")
	require.NoError(t, err)
	_, err = provider.Snapshot(t.Context())
	require.NoError(t, err)

	firstDone := make(chan error, 1)
	go func() {
		wt, refreshErr := provider.RefreshWorktree(t.Context(), "wt")
		if refreshErr == nil && wt.State != "changed" {
			refreshErr = errors.New("unexpected worktree status")
		}
		firstDone <- refreshErr
	}()
	deadline := time.After(time.Second)
	for refreshCalls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("refresh request did not start")
		case <-time.After(time.Millisecond):
		}
	}
	secondDone := make(chan error, 1)
	go func() {
		wt, refreshErr := provider.RefreshWorktree(t.Context(), "wt")
		if refreshErr == nil && wt.State != "changed" {
			refreshErr = errors.New("unexpected worktree status")
		}
		secondDone <- refreshErr
	}()
	close(entered)
	time.Sleep(20 * time.Millisecond)
	close(release)
	require.NoError(t, <-firstDone)
	require.NoError(t, <-secondDone)
	require.EqualValues(t, 1, refreshCalls.Load(), "overlapping refreshes of one worktree share one request")
}
