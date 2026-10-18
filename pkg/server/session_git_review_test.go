package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

func TestCanonicalGitReviewRoutesRequireAuthAndSession(t *testing.T) {
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{})
	srv := NewWithManager(sm, "git-review-secret")
	for _, testCase := range []struct{ path, method, body string }{{"/api/v2/sessions/missing/git-review", http.MethodGet, ""}, {"/api/v2/sessions/missing/git-review/worktree", http.MethodPost, `{"generation":1,"worktree_id":"wt"}`}} {
		path, method, body := testCase.path, testCase.method, testCase.body
		req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		unauthorized := httptest.NewRecorder()
		srv.e.ServeHTTP(unauthorized, req)
		require.Equal(t, http.StatusUnauthorized, unauthorized.Code)
		req = httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer git-review-secret")
		rec := httptest.NewRecorder()
		srv.e.ServeHTTP(rec, req)
		require.Equal(t, http.StatusNotFound, rec.Code)
	}
}

func TestCanonicalGitReviewReadsSessionWorkspaceAndRejectsEscape(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command("git", "-C", root, "init", "--quiet", "-b", "main")
	require.NoError(t, cmd.Run())
	for _, pair := range [][2]string{{"user.email", "review@example.invalid"}, {"user.name", "review test"}} {
		cmd = exec.Command("git", "-C", root, "config", pair[0], pair[1])
		require.NoError(t, cmd.Run())
	}
	tracked := filepath.Join(root, "Dockerfile.dev")
	require.NoError(t, os.WriteFile(tracked, []byte("FROM base\nRUN one\n"), 0o600))
	cmd = exec.Command("git", "-C", root, "add", "Dockerfile.dev")
	require.NoError(t, cmd.Run())
	cmd = exec.Command("git", "-C", root, "commit", "--quiet", "-m", "initial")
	require.NoError(t, cmd.Run())
	require.NoError(t, os.WriteFile(tracked, []byte("FROM base\nRUN two\n"), 0o600))
	outside := filepath.Join(filepath.Dir(root), "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("secret\n"), 0o600))

	store := session.NewInMemorySessionStore()
	local, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "", agent.WithModel(sessionHTTPProvider{})))), runtime.WithSessionStore(store), runtime.WithWorkingDir(root))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(local)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	_, err = owner.Runtime().CreateSession(t.Context(), session.New(session.WithID("git-review-session"), session.WithAgentName("root"), session.WithWorkingDir(root)), runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	sm := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntime(owner.Runtime()))
	srv := NewWithManager(sm, "git-review-token")
	request := func(method, path, body, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		srv.e.ServeHTTP(rec, req)
		return rec
	}
	unauthorized := request(http.MethodGet, "/api/v2/sessions/git-review-session/git-review", "", "")
	require.Equal(t, http.StatusUnauthorized, unauthorized.Code)
	providerBuilds := 0
	oldFactory := newLocalGitReview
	newLocalGitReview = func(path string) (*gitreview.Local, error) { providerBuilds++; return gitreview.NewLocal(path) }
	t.Cleanup(func() { newLocalGitReview = oldFactory })
	snapRec := request(http.MethodGet, "/api/v2/sessions/git-review-session/git-review", "", "git-review-token")
	require.Equal(t, http.StatusOK, snapRec.Code, snapRec.Body.String())
	var snapshot api.GitReviewSnapshot
	require.NoError(t, json.Unmarshal(snapRec.Body.Bytes(), &snapshot))
	require.NotZero(t, snapshot.Snapshot.Generation)
	require.Len(t, snapshot.Snapshot.Repositories, 1)
	wt := snapshot.Snapshot.Repositories[0].Worktrees[0]
	statusBody, _ := json.Marshal(api.GitReviewWorktreeRequest{Generation: snapshot.Snapshot.Generation, WorktreeID: wt.ID})
	statusRec := request(http.MethodPost, "/api/v2/sessions/git-review-session/git-review/worktree", string(statusBody), "git-review-token")
	require.Equal(t, http.StatusOK, statusRec.Code, statusRec.Body.String())
	var status api.GitReviewWorktree
	require.NoError(t, json.Unmarshal(statusRec.Body.Bytes(), &status))
	require.Equal(t, snapshot.Snapshot.Generation, status.Generation)
	require.Len(t, status.Worktree.Files, 1)
	key := gitreview.DocumentKey{Mode: "changes", WorktreeID: wt.ID, Path: "Dockerfile.dev", Group: gitreview.GroupUnstaged}
	body, _ := json.Marshal(api.GitReviewDocument{Generation: snapshot.Snapshot.Generation, Key: key})
	docRec := request(http.MethodPost, "/api/v2/sessions/git-review-session/git-review/document", string(body), "git-review-token")
	require.Equal(t, http.StatusOK, docRec.Code, docRec.Body.String())
	require.Equal(t, 1, providerBuilds, "worktree and document reads reuse the explicit-snapshot provider")
	unknownRefreshField := request(http.MethodPost, "/api/v2/sessions/git-review-session/git-review/worktree", `{"generation":`+strconv.FormatUint(snapshot.Snapshot.Generation, 10)+`,"worktree_id":"`+wt.ID+`","refresh":false,"unexpected":true}`, "git-review-token")
	require.Equal(t, http.StatusBadRequest, unknownRefreshField.Code, "strict request decoding rejects unknown fields")
	var document api.GitReviewDocumentResult
	require.NoError(t, json.Unmarshal(docRec.Body.Bytes(), &document))
	require.Equal(t, gitreview.StateReady, document.Document.State)
	require.Contains(t, document.Document.Lines, gitreview.DocumentLine{Kind: gitreview.DiffRemove, Old: 2, Text: "RUN one"})
	require.Contains(t, document.Document.Lines, gitreview.DocumentLine{Kind: gitreview.DiffAdd, New: 2, Text: "RUN two"})

	// Targeted refresh reads only this worktree, preserving discovery generation.
	require.NoError(t, os.WriteFile(tracked, []byte("FROM base\nRUN three\n"), 0o600))
	refreshBody, _ := json.Marshal(api.GitReviewWorktreeRequest{Generation: snapshot.Snapshot.Generation, WorktreeID: wt.ID, Refresh: true})
	refreshRec := request(http.MethodPost, "/api/v2/sessions/git-review-session/git-review/worktree", string(refreshBody), "git-review-token")
	require.Equal(t, http.StatusOK, refreshRec.Code, refreshRec.Body.String())
	var refreshed api.GitReviewWorktree
	require.NoError(t, json.Unmarshal(refreshRec.Body.Bytes(), &refreshed))
	require.Equal(t, snapshot.Snapshot.Generation, refreshed.Generation)
	require.Equal(t, "changed", refreshed.Worktree.State)
	require.Equal(t, 1, providerBuilds, "targeted refresh must not rebuild provider or rediscover workspace")
	body, _ = json.Marshal(api.GitReviewDocument{Generation: snapshot.Snapshot.Generation, Key: key})
	docRec = request(http.MethodPost, "/api/v2/sessions/git-review-session/git-review/document", string(body), "git-review-token")
	require.Equal(t, http.StatusOK, docRec.Code, docRec.Body.String())
	require.NoError(t, json.Unmarshal(docRec.Body.Bytes(), &document))
	require.Contains(t, document.Document.Lines, gitreview.DocumentLine{Kind: gitreview.DiffAdd, New: 2, Text: "RUN three"})
	require.NotContains(t, document.Document.Lines, gitreview.DocumentLine{Kind: gitreview.DiffAdd, New: 2, Text: "RUN two"})

	body, _ = json.Marshal(api.GitReviewDocument{Generation: snapshot.Snapshot.Generation + 1, Key: key})
	stale := request(http.MethodPost, "/api/v2/sessions/git-review-session/git-review/document", string(body), "git-review-token")
	require.Equal(t, http.StatusConflict, stale.Code)
	key.Mode = "files"
	key.Path = "../outside.txt"
	body, _ = json.Marshal(api.GitReviewDocument{Generation: snapshot.Snapshot.Generation, Key: key})
	escape := request(http.MethodPost, "/api/v2/sessions/git-review-session/git-review/document", string(body), "git-review-token")
	require.Equal(t, http.StatusBadRequest, escape.Code)
}
