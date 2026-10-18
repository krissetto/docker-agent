package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/gitreview"
	"github.com/docker/docker-agent/pkg/session"
)

const gitReviewCacheEntries = 4
const gitReviewServerCacheEntries = 16

type gitReviewCacheEntry struct {
	key                string
	provider           *gitreview.Local
	snapshotGeneration uint64
	touched            time.Time
	inflight           chan struct{}
}

var newLocalGitReview = gitreview.NewLocal

var gitReviewCache = struct {
	sync.Mutex
	entries map[*Server]map[string]*gitReviewCacheEntry
}{entries: make(map[*Server]map[string]*gitReviewCacheEntry)}

func (s *Server) registerGitReviewRoutes(group *echo.Group) {
	group.GET("/:id/git-review", s.gitReviewSnapshot)
	group.POST("/:id/git-review/worktree", s.gitReviewWorktree)
	group.POST("/:id/git-review/directory", s.gitReviewDirectory)
	group.POST("/:id/git-review/document", s.gitReviewDocument)
}

// gitReviewScope re-resolves the authenticated session on every request. A
// cached provider is reusable only for this exact session/source/canonical
// workspace identity; callers never provide a root path.
func (s *Server) gitReviewScope(c echo.Context, generation uint64, refresh bool) (*gitreview.Local, string, error) {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return nil, "", sessionHTTPError(err)
	}
	metadata := handle.Metadata()
	if metadata.Historical || !metadata.ExecutionAvailable {
		return nil, "", echo.NewHTTPError(http.StatusNotFound, "Git review unavailable for historical session")
	}
	sess, err := handle.Snapshot(c.Request().Context())
	if err != nil {
		return nil, "", sessionHTTPError(err)
	}
	if sess == nil || sess.WorkingDir == "" || !filepath.IsAbs(sess.WorkingDir) {
		return nil, "", echo.NewHTTPError(http.StatusNotFound, "session workspace unavailable")
	}
	root, err := filepath.EvalSymlinks(filepath.Clean(sess.WorkingDir))
	if err != nil {
		return nil, "", echo.NewHTTPError(http.StatusNotFound, "session workspace unavailable")
	}
	if st, e := os.Stat(root); e != nil || !st.IsDir() {
		return nil, "", echo.NewHTTPError(http.StatusNotFound, "session workspace unavailable")
	}
	source := sess.AttributesSnapshot()["docker-agent.actor.source"]
	policy, _ := json.Marshal(struct {
		Permissions *session.PermissionsConfig
		Safety      session.SafetyPolicy
		Approved    bool
	}{sess.Permissions, sess.SafetyPolicy, sess.ToolsApproved})
	policyHash := sha256.Sum256(policy)
	key := sess.ID + "\x00" + source + "\x00" + root + "\x00" + fmt.Sprintf("%x", policyHash[:])
	gitReviewCache.Lock()
	entries := gitReviewCache.entries[s]
	if entries == nil {
		if len(gitReviewCache.entries) >= gitReviewServerCacheEntries {
			for owner, candidate := range gitReviewCache.entries {
				idle := true
				for _, cached := range candidate {
					if cached.inflight != nil {
						idle = false
						break
					}
				}
				if idle {
					delete(gitReviewCache.entries, owner)
					break
				}
			}
		}
		if len(gitReviewCache.entries) >= gitReviewServerCacheEntries {
			gitReviewCache.Unlock()
			return nil, key, echo.NewHTTPError(http.StatusServiceUnavailable, "Git review cache is busy")
		}
		entries = make(map[string]*gitReviewCacheEntry)
		gitReviewCache.entries[s] = entries
	}
	gitReviewCache.Unlock()
	for {
		gitReviewCache.Lock()
		entry := entries[key]
		if entry != nil && entry.inflight != nil {
			wait := entry.inflight
			gitReviewCache.Unlock()
			select {
			case <-wait:
				continue
			case <-c.Request().Context().Done():
				return nil, key, c.Request().Context().Err()
			}
		}
		if entry != nil && refresh {
			entry.inflight = make(chan struct{})
			provider := entry.provider
			gitReviewCache.Unlock()
			snapshot, e := provider.Snapshot(c.Request().Context())
			gitReviewCache.Lock()
			current := entries[key]
			if current != nil {
				current.snapshotGeneration = snapshot.Generation
				close(current.inflight)
				current.inflight = nil
				current.touched = time.Now()
			}
			gitReviewCache.Unlock()
			if e != nil {
				return nil, key, gitReviewHTTPError(e)
			}
			return provider, key, nil
		}
		if entry != nil {
			snapshot := entry.provider.CurrentSnapshot()
			if snapshot.Generation == 0 || generation == 0 || snapshot.Generation != generation {
				gitReviewCache.Unlock()
				return nil, key, echo.NewHTTPError(http.StatusConflict, "Git review snapshot expired; refresh to scan again")
			}
			entry.touched = time.Now()
			provider := entry.provider
			gitReviewCache.Unlock()
			return provider, key, nil
		}
		if !refresh {
			gitReviewCache.Unlock()
			return nil, key, echo.NewHTTPError(http.StatusConflict, "Git review snapshot expired; refresh to scan again")
		}
		if len(entries) >= gitReviewCacheEntries {
			oldest := ""
			var when time.Time
			for k, v := range entries {
				if v.inflight == nil && (oldest == "" || v.touched.Before(when)) {
					oldest, when = k, v.touched
				}
			}
			if oldest == "" {
				gitReviewCache.Unlock()
				return nil, key, echo.NewHTTPError(http.StatusServiceUnavailable, "Git review cache is busy")
			}
			delete(entries, oldest)
		}
		provider, e := newLocalGitReview(root)
		if e != nil {
			gitReviewCache.Unlock()
			return nil, key, echo.NewHTTPError(http.StatusNotFound, "Git review unavailable")
		}
		entry = &gitReviewCacheEntry{key: key, provider: provider, touched: time.Now(), inflight: make(chan struct{})}
		entries[key] = entry
		gitReviewCache.Unlock()
		snapshot, e := provider.Snapshot(c.Request().Context())
		gitReviewCache.Lock()
		current := entries[key]
		if current != nil {
			current.snapshotGeneration = snapshot.Generation
			close(current.inflight)
			current.inflight = nil
			current.touched = time.Now()
		}
		gitReviewCache.Unlock()
		if e != nil {
			return nil, key, gitReviewHTTPError(e)
		}
		return provider, key, nil
	}
}

func (s *Server) gitReviewSnapshot(c echo.Context) error {
	provider, _, err := s.gitReviewScope(c, 0, true)
	if err != nil {
		return err
	}
	snapshot := provider.CurrentSnapshot()
	snapshot.Scope.Remote, snapshot.Scope.RemoteLabel = true, filepath.Base(snapshot.Scope.Label)
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, api.GitReviewSnapshot{Snapshot: snapshot})
}

func (s *Server) gitReviewWorktree(c echo.Context) error {
	var req api.GitReviewWorktreeRequest
	if decodeSessionJSON(c, &req) != nil || req.WorktreeID == "" {
		return sessionRequestError("worktree_id is required")
	}
	provider, _, err := s.gitReviewScope(c, req.Generation, false)
	if err != nil {
		return err
	}
	var wt gitreview.Worktree
	if req.Refresh {
		wt, err = provider.RefreshWorktree(c.Request().Context(), req.WorktreeID)
	} else {
		wt, err = provider.LoadWorktree(c.Request().Context(), req.WorktreeID)
	}
	if err != nil {
		return gitReviewHTTPError(err)
	}
	return c.JSON(http.StatusOK, api.GitReviewWorktree{Generation: provider.CurrentSnapshot().Generation, Worktree: wt})
}
func (s *Server) gitReviewDirectory(c echo.Context) error {
	var req api.GitReviewDirectory
	if decodeSessionJSON(c, &req) != nil || req.WorktreeID == "" {
		return sessionRequestError("worktree_id is required")
	}
	provider, _, err := s.gitReviewScope(c, req.Generation, false)
	if err != nil {
		return err
	}
	dir, err := provider.Directory(c.Request().Context(), req.WorktreeID, req.Path)
	if err != nil {
		return gitReviewHTTPError(err)
	}
	return c.JSON(http.StatusOK, api.GitReviewDirectoryResult{Generation: provider.CurrentSnapshot().Generation, Directory: dir})
}
func (s *Server) gitReviewDocument(c echo.Context) error {
	var req api.GitReviewDocument
	if decodeSessionJSON(c, &req) != nil || req.Key.WorktreeID == "" || req.Key.Path == "" {
		return sessionRequestError("worktree_id and path are required")
	}
	provider, _, err := s.gitReviewScope(c, req.Generation, false)
	if err != nil {
		return err
	}
	doc, err := provider.Document(c.Request().Context(), req.Key)
	if err != nil {
		return gitReviewHTTPError(err)
	}
	return c.JSON(http.StatusOK, api.GitReviewDocumentResult{Generation: provider.CurrentSnapshot().Generation, Document: doc})
}
func gitReviewHTTPError(err error) error {
	switch {
	case errors.Is(err, gitreview.ErrNotFound):
		return echo.NewHTTPError(http.StatusNotFound, "Git review item not found")
	case errors.Is(err, gitreview.ErrOutsideScope):
		return echo.NewHTTPError(http.StatusBadRequest, "path is outside the session workspace")
	case errors.Is(err, gitreview.ErrTooLarge):
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "Git review result is too large")
	case errors.Is(err, context.DeadlineExceeded):
		return echo.NewHTTPError(http.StatusGatewayTimeout, "Git review timed out")
	default:
		return echo.NewHTTPError(http.StatusBadRequest, "Git review unavailable: "+strings.TrimSpace(err.Error()))
	}
}
