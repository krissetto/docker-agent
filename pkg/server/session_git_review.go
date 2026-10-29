package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/gitreview"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

const gitReviewCacheEntries = 4
const gitReviewServerCacheEntries = 16

// gitReviewEntry is one shared provider for a canonical workspace under one
// server, source, and effective policy. refs pins it while a request uses it.
type gitReviewEntry struct {
	provider *gitreview.Shared
	refs     int
	touched  time.Time
}

var newLocalGitReview = gitreview.NewLocal

var gitReviewCache = struct {
	sync.Mutex
	entries map[*Server]map[string]*gitReviewEntry
}{entries: make(map[*Server]map[string]*gitReviewEntry)}

func (s *Server) registerGitReviewRoutes(group *echo.Group) {
	group.GET("/:id/git-review", s.gitReviewSnapshot)
	group.POST("/:id/git-review/browse", s.gitReviewBrowse)
	group.POST("/:id/git-review/worktree", s.gitReviewWorktree)
	group.POST("/:id/git-review/directory", s.gitReviewDirectory)
	group.POST("/:id/git-review/document", s.gitReviewDocument)
}

// gitReviewScope re-resolves the authenticated session on every request. Providers
// are shared by canonical workspace, source, and effective policy within this Server,
// never by session ID, and callers never provide a root path. The returned release
// must be called once the response no longer uses the provider.
func (s *Server) gitReviewScope(c echo.Context, generation uint64, create bool) (*gitreview.Shared, func(), error) {
	ctx := c.Request().Context()
	handle, err := s.sessionByID(ctx, c.Param("id"))
	if err != nil {
		return nil, nil, sessionHTTPError(err)
	}
	metadata := handle.Metadata()
	if metadata.Historical || !metadata.ExecutionAvailable {
		return nil, nil, echo.NewHTTPError(http.StatusNotFound, "Git review unavailable for historical session")
	}
	sess, err := handle.Snapshot(ctx)
	if err != nil {
		return nil, nil, sessionHTTPError(err)
	}
	if sess == nil || sess.WorkingDir == "" || !filepath.IsAbs(sess.WorkingDir) {
		return nil, nil, echo.NewHTTPError(http.StatusNotFound, "session workspace unavailable")
	}
	root, err := filepath.EvalSymlinks(filepath.Clean(sess.WorkingDir))
	if err != nil {
		return nil, nil, echo.NewHTTPError(http.StatusNotFound, "session workspace unavailable")
	}
	if st, e := os.Stat(root); e != nil || !st.IsDir() {
		return nil, nil, echo.NewHTTPError(http.StatusNotFound, "session workspace unavailable")
	}
	key, err := gitReviewKey(ctx, handle, sess, root)
	if err != nil {
		return nil, nil, sessionHTTPError(err)
	}
	provider, release, err := s.gitReviewAcquire(key, root, create)
	if err != nil {
		return nil, nil, err
	}
	if !create {
		if err := gitReviewCurrent(provider, generation); err != nil {
			release()
			return nil, nil, err
		}
	}
	return provider, release, nil
}

// gitReviewKey includes the effective permissions (team plus session overrides) so
// sessions that would see different policy never share a provider.
func gitReviewKey(ctx context.Context, handle runtime.SessionHandle, sess *session.Session, root string) (string, error) {
	var effective any
	if inspector, ok := handle.(runtime.SessionPermissionsInspector); ok {
		info, err := inspector.EffectivePermissions(ctx)
		if err != nil {
			return "", err
		}
		effective = info
	}
	policy, err := json.Marshal(struct {
		Permissions *session.PermissionsConfig
		Safety      session.SafetyPolicy
		Approved    bool
		Effective   any
	}{sess.Permissions, sess.SafetyPolicy, sess.ToolsApproved, effective})
	if err != nil {
		return "", err
	}
	policyHash := sha256.Sum256(policy)
	source := sess.AttributesSnapshot()["docker-agent.actor.source"]
	return source + "\x00" + root + "\x00" + hex.EncodeToString(policyHash[:]), nil
}

// gitReviewAcquire pins an existing provider, or creates one only for explicit
// attach or refresh requests. Read paths never create a provider.
func (s *Server) gitReviewAcquire(key, root string, create bool) (*gitreview.Shared, func(), error) {
	gitReviewCache.Lock()
	defer gitReviewCache.Unlock()
	entries := gitReviewCache.entries[s]
	entry := entries[key]
	if entry == nil {
		if !create {
			return nil, nil, echo.NewHTTPError(http.StatusConflict, "Git review snapshot expired; refresh to scan again")
		}
		if entries == nil {
			if len(gitReviewCache.entries) >= gitReviewServerCacheEntries && !gitReviewEvictIdleServerLocked() {
				return nil, nil, echo.NewHTTPError(http.StatusServiceUnavailable, "Git review cache is busy")
			}
			entries = make(map[string]*gitReviewEntry)
			gitReviewCache.entries[s] = entries
		}
		if len(entries) >= gitReviewCacheEntries && !gitReviewEvictIdleEntryLocked(entries) {
			return nil, nil, echo.NewHTTPError(http.StatusServiceUnavailable, "Git review cache is busy")
		}
		local, err := newLocalGitReview(root)
		if err != nil {
			return nil, nil, echo.NewHTTPError(http.StatusNotFound, "Git review unavailable")
		}
		entry = &gitReviewEntry{provider: gitreview.NewShared(local, gitreview.SharedOptions{})}
		entries[key] = entry
	}
	entry.refs++
	entry.touched = time.Now()
	provider := entry.provider
	return provider, func() { s.gitReviewRelease(entry) }, nil
}

func (s *Server) gitReviewRelease(entry *gitReviewEntry) {
	gitReviewCache.Lock()
	defer gitReviewCache.Unlock()
	entry.refs--
	entry.touched = time.Now()
}

// gitReviewEvictIdleEntryLocked closes the least recently used unpinned provider.
func gitReviewEvictIdleEntryLocked(entries map[string]*gitReviewEntry) bool {
	oldest := ""
	var when time.Time
	for key, entry := range entries {
		if entry.refs == 0 && (oldest == "" || entry.touched.Before(when)) {
			oldest, when = key, entry.touched
		}
	}
	if oldest == "" {
		return false
	}
	entries[oldest].provider.Close()
	delete(entries, oldest)
	return true
}

// gitReviewEvictIdleServerLocked drops one server whose providers are all unpinned.
func gitReviewEvictIdleServerLocked() bool {
	for owner, entries := range gitReviewCache.entries {
		idle := true
		for _, entry := range entries {
			if entry.refs != 0 {
				idle = false
				break
			}
		}
		if idle {
			for _, entry := range entries {
				entry.provider.Close()
			}
			delete(gitReviewCache.entries, owner)
			return true
		}
	}
	return false
}

// gitReviewCurrent rejects a generation that no longer matches the provider's
// inventory. Callers check it before and after reading, so a response never mixes generations.
func gitReviewCurrent(provider *gitreview.Shared, generation uint64) error {
	snapshot, ok := provider.CurrentSnapshot()
	if !ok || snapshot.Generation == 0 || snapshot.Generation != generation {
		return echo.NewHTTPError(http.StatusConflict, "Git review snapshot expired; refresh to scan again")
	}
	return nil
}

// remoteGitReviewSnapshot marks the response as served over the API.
func remoteGitReviewSnapshot(snapshot gitreview.Snapshot) gitreview.Snapshot {
	snapshot.Scope.Remote, snapshot.Scope.RemoteLabel = true, filepath.Base(snapshot.Scope.Label)
	return snapshot
}

// gitReviewSnapshot is the explicit refresh boundary. Concurrent identical refreshes
// share one in-flight scan inside the shared provider.
func (s *Server) gitReviewSnapshot(c echo.Context) error {
	provider, release, err := s.gitReviewScope(c, 0, true)
	if err != nil {
		return err
	}
	defer release()
	snapshot, err := provider.Snapshot(c.Request().Context())
	if err != nil {
		return gitReviewHTTPError(err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, api.GitReviewSnapshot{Snapshot: remoteGitReviewSnapshot(snapshot)})
}

// gitReviewBrowse opens only the session root folder. It never runs repository
// discovery, so the Files surface can list a folder without a Git scan.
func (s *Server) gitReviewBrowse(c echo.Context) error {
	provider, release, err := s.gitReviewScope(c, 0, true)
	if err != nil {
		return err
	}
	defer release()
	snapshot, err := provider.BrowseRoot(c.Request().Context())
	if err != nil {
		return gitReviewHTTPError(err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, api.GitReviewSnapshot{Snapshot: remoteGitReviewSnapshot(snapshot)})
}

func (s *Server) gitReviewWorktree(c echo.Context) error {
	var req api.GitReviewWorktreeRequest
	if decodeSessionJSON(c, &req) != nil || req.WorktreeID == "" {
		return sessionRequestError("worktree_id is required")
	}
	provider, release, err := s.gitReviewScope(c, req.Generation, false)
	if err != nil {
		return err
	}
	defer release()
	var wt gitreview.Worktree
	if req.Refresh {
		wt, err = provider.RefreshWorktree(c.Request().Context(), req.WorktreeID)
	} else {
		wt, err = provider.LoadWorktree(c.Request().Context(), req.WorktreeID)
	}
	if err != nil {
		return gitReviewHTTPError(err)
	}
	if err := gitReviewCurrent(provider, req.Generation); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, api.GitReviewWorktree{Generation: req.Generation, Worktree: wt})
}

func (s *Server) gitReviewDirectory(c echo.Context) error {
	var req api.GitReviewDirectory
	if decodeSessionJSON(c, &req) != nil || req.WorktreeID == "" {
		return sessionRequestError("worktree_id is required")
	}
	provider, release, err := s.gitReviewScope(c, req.Generation, false)
	if err != nil {
		return err
	}
	defer release()
	dir, err := provider.Directory(c.Request().Context(), req.WorktreeID, req.Path)
	if err != nil {
		return gitReviewHTTPError(err)
	}
	if err := gitReviewCurrent(provider, req.Generation); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, api.GitReviewDirectoryResult{Generation: req.Generation, Directory: dir})
}

func (s *Server) gitReviewDocument(c echo.Context) error {
	var req api.GitReviewDocument
	if decodeSessionJSON(c, &req) != nil || req.Key.WorktreeID == "" || req.Key.Path == "" {
		return sessionRequestError("worktree_id and path are required")
	}
	provider, release, err := s.gitReviewScope(c, req.Generation, false)
	if err != nil {
		return err
	}
	defer release()
	doc, err := provider.Document(c.Request().Context(), req.Key)
	if err != nil {
		return gitReviewHTTPError(err)
	}
	if err := gitReviewCurrent(provider, req.Generation); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, api.GitReviewDocumentResult{Generation: req.Generation, Document: doc})
}

func gitReviewHTTPError(err error) error {
	switch {
	case errors.Is(err, gitreview.ErrNotFound):
		return echo.NewHTTPError(http.StatusNotFound, "Git review item not found")
	case errors.Is(err, gitreview.ErrOutsideScope):
		return echo.NewHTTPError(http.StatusBadRequest, "path is outside the session workspace")
	case errors.Is(err, gitreview.ErrTooLarge):
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "Git review result is too large")
	case errors.Is(err, gitreview.ErrStale):
		return echo.NewHTTPError(http.StatusConflict, "Git review snapshot expired; refresh to scan again")
	case errors.Is(err, context.DeadlineExceeded):
		return echo.NewHTTPError(http.StatusGatewayTimeout, "Git review timed out")
	default:
		return echo.NewHTTPError(http.StatusBadRequest, "Git review unavailable: "+strings.TrimSpace(err.Error()))
	}
}
