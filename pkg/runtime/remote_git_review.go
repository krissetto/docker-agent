package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/gitreview"
)

// SessionGitReview is a server-side, read-only filesystem capability. The
// workspace is always resolved from the identified session, never from a
// client-supplied path.
type SessionGitReview interface {
	GitReviewSnapshot(ctx context.Context, sessionID string) (gitreview.Snapshot, error)
	GitReviewWorktree(ctx context.Context, sessionID string, generation uint64, worktreeID string) (gitreview.Worktree, error)
	GitReviewDirectory(ctx context.Context, sessionID string, generation uint64, worktreeID, path string) (gitreview.Directory, error)
	GitReviewDocument(ctx context.Context, sessionID string, generation uint64, key gitreview.DocumentKey) (gitreview.Document, error)
}

type SessionGitReviewRefresher interface {
	GitReviewRefreshWorktree(ctx context.Context, sessionID string, generation uint64, worktreeID string) (gitreview.Worktree, error)
}

type remoteWorktreeRefresh struct {
	done     chan struct{}
	cancel   context.CancelFunc
	waiters  int
	revision uint64
	worktree gitreview.Worktree
	err      error
}

func (r *SessionTransport) GitReviewSnapshot(ctx context.Context, sessionID string) (gitreview.Snapshot, error) {
	var response api.GitReviewSnapshot
	err := r.client.sessionJSON(ctx, http.MethodGet, api.SessionAPIPath+"/"+url.PathEscape(sessionID)+"/git-review", nil, &response)
	return response.Snapshot, err
}
func (r *SessionTransport) GitReviewWorktree(ctx context.Context, sessionID string, generation uint64, worktreeID string) (gitreview.Worktree, error) {
	var response api.GitReviewWorktree
	err := r.client.sessionJSON(ctx, http.MethodPost, api.SessionAPIPath+"/"+url.PathEscape(sessionID)+"/git-review/worktree", api.GitReviewWorktreeRequest{Generation: generation, WorktreeID: worktreeID}, &response)
	if err == nil && response.Generation != generation {
		err = errors.New("stale Git review worktree response")
	}
	return response.Worktree, err
}
func (r *SessionTransport) GitReviewRefreshWorktree(ctx context.Context, sessionID string, generation uint64, worktreeID string) (gitreview.Worktree, error) {
	var response api.GitReviewWorktree
	err := r.client.sessionJSON(ctx, http.MethodPost, api.SessionAPIPath+"/"+url.PathEscape(sessionID)+"/git-review/worktree", api.GitReviewWorktreeRequest{Generation: generation, WorktreeID: worktreeID, Refresh: true}, &response)
	if err == nil && response.Generation != generation {
		err = errors.New("stale Git review worktree response")
	}
	return response.Worktree, err
}
func (r *SessionTransport) GitReviewDirectory(ctx context.Context, sessionID string, generation uint64, worktreeID, path string) (gitreview.Directory, error) {
	var response api.GitReviewDirectoryResult
	err := r.client.sessionJSON(ctx, http.MethodPost, api.SessionAPIPath+"/"+url.PathEscape(sessionID)+"/git-review/directory", api.GitReviewDirectory{Generation: generation, WorktreeID: worktreeID, Path: path}, &response)
	if err == nil && response.Generation != generation {
		err = errors.New("stale Git review directory response")
	}
	return response.Directory, err
}
func (r *SessionTransport) GitReviewDocument(ctx context.Context, sessionID string, generation uint64, key gitreview.DocumentKey) (gitreview.Document, error) {
	var response api.GitReviewDocumentResult
	err := r.client.sessionJSON(ctx, http.MethodPost, api.SessionAPIPath+"/"+url.PathEscape(sessionID)+"/git-review/document", api.GitReviewDocument{Generation: generation, Key: key}, &response)
	if err == nil && response.Generation != generation {
		err = errors.New("stale Git review document response")
	}
	return response.Document, err
}

// Remote adapts the authenticated runtime transport to the shared Provider
// interface. Worktree and file requests carry identities, never absolute roots.
type GitReviewRemote struct {
	transport         SessionGitReview
	sessionID         string
	mu                sync.RWMutex
	generation        uint64
	worktreeRevisions map[string]uint64
	worktreeRefreshes map[string]*remoteWorktreeRefresh
}

func NewGitReviewRemote(transport SessionGitReview, sessionID string) (*GitReviewRemote, error) {
	if transport == nil || sessionID == "" {
		return nil, errors.New("git review remote requires a session transport and session ID")
	}
	return &GitReviewRemote{transport: transport, sessionID: sessionID, worktreeRevisions: make(map[string]uint64), worktreeRefreshes: make(map[string]*remoteWorktreeRefresh)}, nil
}
func (r *GitReviewRemote) Snapshot(ctx context.Context) (gitreview.Snapshot, error) {
	s, err := r.transport.GitReviewSnapshot(ctx, r.sessionID)
	if err == nil {
		r.mu.Lock()
		r.generation = s.Generation
		r.mu.Unlock()
	}
	return s, err
}
func (r *GitReviewRemote) LoadWorktree(ctx context.Context, id string) (gitreview.Worktree, error) {
	r.mu.RLock()
	g := r.generation
	revision := r.worktreeRevisions[id]
	r.mu.RUnlock()
	wt, err := r.transport.GitReviewWorktree(ctx, r.sessionID, g, id)
	r.mu.RLock()
	stale := g != r.generation || revision != r.worktreeRevisions[id]
	r.mu.RUnlock()
	if stale && err == nil {
		err = errors.New("stale Git review worktree response")
	}
	return wt, err
}
func (r *GitReviewRemote) RefreshWorktree(ctx context.Context, id string) (gitreview.Worktree, error) {
	if err := ctx.Err(); err != nil {
		return gitreview.Worktree{}, err
	}
	refresher, ok := r.transport.(SessionGitReviewRefresher)
	if !ok {
		return gitreview.Worktree{}, errors.New("Git review transport does not support targeted refresh")
	}
	r.mu.Lock()
	flight := r.worktreeRefreshes[id]
	if flight == nil {
		generation := r.generation
		r.worktreeRevisions[id]++
		revision := r.worktreeRevisions[id]
		workCtx, cancel := context.WithCancel(context.Background())
		flight = &remoteWorktreeRefresh{done: make(chan struct{}), cancel: cancel, revision: revision}
		r.worktreeRefreshes[id] = flight
		go r.runWorktreeRefresh(workCtx, id, generation, revision, refresher, flight)
	}
	flight.waiters++
	r.mu.Unlock()
	return r.waitWorktreeRefresh(ctx, id, flight)
}

func (r *GitReviewRemote) runWorktreeRefresh(ctx context.Context, id string, generation, revision uint64, refresher SessionGitReviewRefresher, flight *remoteWorktreeRefresh) {
	defer flight.cancel()
	wt, err := refresher.GitReviewRefreshWorktree(ctx, r.sessionID, generation, id)
	r.mu.Lock()
	if r.worktreeRefreshes[id] == flight {
		if err == nil && r.generation == generation && r.worktreeRevisions[id] == revision {
			r.worktreeRevisions[id]++
		} else if err == nil {
			err = errors.New("stale Git review worktree response")
		}
		delete(r.worktreeRefreshes, id)
	}
	flight.worktree, flight.err = wt, err
	close(flight.done)
	r.mu.Unlock()
}

func (r *GitReviewRemote) waitWorktreeRefresh(ctx context.Context, id string, flight *remoteWorktreeRefresh) (gitreview.Worktree, error) {
	select {
	case <-flight.done:
		return flight.worktree, flight.err
	case <-ctx.Done():
		r.mu.Lock()
		if !remoteRefreshDone(flight.done) {
			flight.waiters--
			if flight.waiters == 0 {
				if r.worktreeRefreshes[id] == flight {
					delete(r.worktreeRefreshes, id)
				}
				flight.cancel()
			}
		}
		r.mu.Unlock()
		return gitreview.Worktree{}, ctx.Err()
	}
}

func remoteRefreshDone(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func (r *GitReviewRemote) Directory(ctx context.Context, id, path string) (gitreview.Directory, error) {
	r.mu.RLock()
	g := r.generation
	r.mu.RUnlock()
	return r.transport.GitReviewDirectory(ctx, r.sessionID, g, id, path)
}
func (r *GitReviewRemote) Document(ctx context.Context, key gitreview.DocumentKey) (gitreview.Document, error) {
	r.mu.RLock()
	g := r.generation
	revision := r.worktreeRevisions[key.WorktreeID]
	r.mu.RUnlock()
	doc, err := r.transport.GitReviewDocument(ctx, r.sessionID, g, key)
	r.mu.RLock()
	stale := g != r.generation || revision != r.worktreeRevisions[key.WorktreeID]
	r.mu.RUnlock()
	if stale && err == nil {
		err = errors.New("stale Git review document response")
	}
	return doc, err
}
