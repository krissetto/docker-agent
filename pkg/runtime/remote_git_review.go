package runtime

import (
	"context"
	"crypto/rand"
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

// SessionGitReviewBrowser is optional: servers without it report folder browsing
// as unsupported instead of falling back to repository discovery.
type SessionGitReviewBrowser interface {
	GitReviewBrowse(ctx context.Context, sessionID string) (gitreview.Snapshot, error)
}

// ErrGitReviewBrowseUnsupported reports a remote runtime without root-only browsing.
var ErrGitReviewBrowseUnsupported = errors.New("git review folder browsing is not supported by this server")

type SessionGitReviewRefresher interface {
	GitReviewRefreshWorktree(ctx context.Context, sessionID string, generation uint64, worktreeID string) (gitreview.Worktree, error)
}

// gitReviewBackend names the authenticated backend a provider is bound to.
type gitReviewBackend interface {
	GitReviewBackendIdentity() string
}

type remoteWorktreeRefresh struct {
	done     chan struct{}
	cancel   context.CancelFunc
	waiters  int
	revision uint64
	worktree gitreview.Worktree
	err      error
}

// GitReviewBackendIdentity is opaque and process-local. Transports borrowing one
// Client share it; separately constructed clients stay isolated, even for the
// same endpoint and credentials.
func (r *SessionTransport) GitReviewBackendIdentity() string {
	if r == nil {
		return ""
	}
	return r.client.gitReviewBackendIdentity()
}

// newGitReviewIdentity names one local runtime. Every view of that runtime reports
// the same value; unrelated runtimes never collide.
func newGitReviewIdentity() string {
	return rand.Text()
}

// GitReviewBackendIdentity names the local runtime behind this view, so views of one
// runtime are one backend and unrelated runtimes stay isolated.
func (r *LocalRuntime) GitReviewBackendIdentity() string {
	if r == nil {
		return ""
	}
	return r.gitReviewIdentity
}

func (v *localSessionRuntimeView) GitReviewBackendIdentity() string {
	if v == nil {
		return ""
	}
	return v.runtime.GitReviewBackendIdentity()
}

func (c *Client) gitReviewBackendIdentity() string {
	if c == nil {
		return ""
	}
	return c.backendID
}

func (r *SessionTransport) GitReviewSnapshot(ctx context.Context, sessionID string) (gitreview.Snapshot, error) {
	var response api.GitReviewSnapshot
	err := r.client.sessionJSON(ctx, http.MethodGet, api.SessionAPIPath+"/"+url.PathEscape(sessionID)+"/git-review", nil, &response)
	return response.Snapshot, err
}

func (r *SessionTransport) GitReviewBrowse(ctx context.Context, sessionID string) (gitreview.Snapshot, error) {
	var response api.GitReviewSnapshot
	err := r.client.sessionJSON(ctx, http.MethodPost, api.SessionAPIPath+"/"+url.PathEscape(sessionID)+"/git-review/browse", nil, &response)
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

// BrowseRoot opens only the session root folder. Unsupported transports fail
// honestly; they never fall back to repository discovery or local I/O.
func (r *GitReviewRemote) BrowseRoot(ctx context.Context) (gitreview.Snapshot, error) {
	browser, ok := r.transport.(SessionGitReviewBrowser)
	if !ok {
		return gitreview.Snapshot{}, ErrGitReviewBrowseUnsupported
	}
	done := r.beginBrowse()
	req := r.request("")
	s, err := browser.GitReviewBrowse(ctx, req.sessionID)
	if err == nil {
		r.endBrowse(done, s.Generation)
	}
	return s, err
}

// Remote adapts the authenticated runtime transport to the shared Provider
// interface. Worktree and file requests carry identities, never absolute roots.
type GitReviewRemote struct {
	transport  SessionGitReview
	backend    string
	mu         sync.RWMutex
	sessionID  string
	generation uint64
	// Full Snapshot starts and completions; a Browse adopts only if no full completed since it began.
	fullStarted       uint64
	fullDone          uint64
	worktreeRevisions map[string]uint64
	worktreeRefreshes map[string]*remoteWorktreeRefresh
}

var _ gitreview.BrowseProvider = (*GitReviewRemote)(nil)

func NewGitReviewRemote(transport SessionGitReview, sessionID string) (*GitReviewRemote, error) {
	if transport == nil || sessionID == "" {
		return nil, errors.New("git review remote requires a session transport and session ID")
	}
	remote := &GitReviewRemote{transport: transport, sessionID: sessionID, worktreeRevisions: make(map[string]uint64), worktreeRefreshes: make(map[string]*remoteWorktreeRefresh)}
	if backend, ok := transport.(gitReviewBackend); ok {
		remote.backend = backend.GitReviewBackendIdentity()
	}
	return remote, nil
}

// Retarget moves the route to another session on the same backend. Only verified
// equal-scope peers may be targeted: they share the server provider, so the
// generation, worktree revisions, and in-flight work remain valid. The server
// still authorizes the new session on every request.
func (r *GitReviewRemote) Retarget(sessionID, backend string) error {
	if sessionID == "" {
		return errors.New("git review retarget requires a session ID")
	}
	if backend == "" || backend != r.backend {
		return errors.New("git review retarget requires the same backend")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessionID = sessionID
	return nil
}

func (r *GitReviewRemote) Snapshot(ctx context.Context) (gitreview.Snapshot, error) {
	seq := r.beginFull()
	req := r.request("")
	s, err := r.transport.GitReviewSnapshot(ctx, req.sessionID)
	r.endFull(seq, s.Generation, err == nil)
	return s, err
}
func (r *GitReviewRemote) LoadWorktree(ctx context.Context, id string) (gitreview.Worktree, error) {
	req := r.request(id)
	wt, err := r.transport.GitReviewWorktree(ctx, req.sessionID, req.generation, id)
	if err == nil && !r.current(req, id) {
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
		sessionID, generation := r.sessionID, r.generation
		r.worktreeRevisions[id]++
		revision := r.worktreeRevisions[id]
		workCtx, cancel := context.WithCancel(context.Background())
		flight = &remoteWorktreeRefresh{done: make(chan struct{}), cancel: cancel, revision: revision}
		r.worktreeRefreshes[id] = flight
		go r.runWorktreeRefresh(workCtx, sessionID, id, generation, revision, refresher, flight)
	}
	flight.waiters++
	r.mu.Unlock()
	return r.waitWorktreeRefresh(ctx, id, flight)
}

func (r *GitReviewRemote) runWorktreeRefresh(ctx context.Context, sessionID, id string, generation, revision uint64, refresher SessionGitReviewRefresher, flight *remoteWorktreeRefresh) {
	defer flight.cancel()
	wt, err := refresher.GitReviewRefreshWorktree(ctx, sessionID, generation, id)
	r.mu.Lock()
	current := r.worktreeRefreshes[id] == flight
	if current {
		delete(r.worktreeRefreshes, id)
	}
	if err == nil && (!current || r.generation != generation || r.worktreeRevisions[id] != revision) {
		err = errors.New("stale Git review worktree response")
	}
	if err == nil {
		r.worktreeRevisions[id]++
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
	req := r.request(id)
	dir, err := r.transport.GitReviewDirectory(ctx, req.sessionID, req.generation, id, path)
	if err == nil && !r.current(req, id) {
		err = errors.New("stale Git review directory response")
	}
	return dir, err
}

func (r *GitReviewRemote) Document(ctx context.Context, key gitreview.DocumentKey) (gitreview.Document, error) {
	req := r.request(key.WorktreeID)
	doc, err := r.transport.GitReviewDocument(ctx, req.sessionID, req.generation, key)
	if err == nil && !r.current(req, key.WorktreeID) {
		err = errors.New("stale Git review document response")
	}
	return doc, err
}

// remoteRequest captures the route and generation a request was issued under.
// Responses are accepted only while that generation and worktree revision stay current.
type remoteRequest struct {
	sessionID  string
	generation uint64
	revision   uint64
}

func (r *GitReviewRemote) request(worktreeID string) remoteRequest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return remoteRequest{sessionID: r.sessionID, generation: r.generation, revision: r.worktreeRevisions[worktreeID]}
}

func (r *GitReviewRemote) current(req remoteRequest, worktreeID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return req.generation == r.generation && req.revision == r.worktreeRevisions[worktreeID]
}

// beginFull numbers a full Snapshot. The newest full call that finishes successfully
// sets the generation, even when it is lower after a server cache reset.
func (r *GitReviewRemote) beginFull() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fullStarted++
	return r.fullStarted
}

func (r *GitReviewRemote) endFull(seq, generation uint64, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fullDone++
	if ok && seq == r.fullStarted {
		r.generation = generation
	}
}

// beginBrowse returns the count of completed full scans the Browse started under.
func (r *GitReviewRemote) beginBrowse() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fullDone
}

// endBrowse adopts a root-only generation provisionally while a full scan is still
// pending, so Files works at once. A full scan that completes since the Browse began
// always outranks it: its completion sets the generation after this check.
func (r *GitReviewRemote) endBrowse(done, generation uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if done == r.fullDone {
		r.generation = generation
	}
}
