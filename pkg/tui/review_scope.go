package tui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/gitreview"
	"github.com/docker/docker-agent/pkg/runtime"
	view "github.com/docker/docker-agent/pkg/tui/components/review"
)

const (
	gitReviewIdentityTimeout = 10 * time.Second
	gitReviewPeerTimeout     = 30 * time.Second
	gitReviewSourceAttribute = "docker-agent.actor.source"
)

// sharedGitReview is the scope's single provider. The wrapper owns the inventory,
// worktree, directory and document caches and their in-flight requests. Local
// editing is an explicit capability, never a method every remote wrapper has.
type sharedGitReview struct {
	*gitreview.Shared

	editable gitreview.LocalEditor
}

// reviewLocalEditor reports whether provider's files are editable on this machine.
func reviewLocalEditor(provider gitreview.Provider) (gitreview.LocalEditor, bool) {
	if shared, ok := provider.(sharedGitReview); ok {
		return shared.editable, shared.editable != nil
	}
	editable, ok := provider.(gitreview.LocalEditor)
	return editable, ok
}

// gitReviewRegistry maps scope keys to scopes. Shutdown may close it from a
// command goroutine, so every access is serialised.
type gitReviewRegistry struct {
	mu     sync.Mutex
	closed bool
	scopes map[string]*gitReviewScope
}

// acquire returns the scope for key, creating it under parent when absent. After
// closeAll it returns nil, so late identity results cannot resurrect a scope.
func (r *gitReviewRegistry) acquire(key, backend string, parent context.Context) *gitReviewScope {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	if r.scopes == nil {
		r.scopes = map[string]*gitReviewScope{}
	}
	scope := r.scopes[key]
	if scope == nil {
		scope = newGitReviewScope(parent, backend)
		r.scopes[key] = scope
	}
	return scope
}

func (r *gitReviewRegistry) get(key string) *gitReviewScope {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.scopes[key]
}

func (r *gitReviewRegistry) drop(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.scopes, key)
}

// closeAll stops every scope and refuses new ones; shutdown is idempotent.
func (r *gitReviewRegistry) closeAll() {
	r.mu.Lock()
	scopes := r.scopes
	r.scopes, r.closed = nil, true
	r.mu.Unlock()
	for _, scope := range scopes {
		scope.closeAll()
	}
}

// gitReviewScope is the Git state of one canonical workspace on one backend.
// Attached workspaces share its provider; selection, drafts and presentation stay per workspace.
type gitReviewScope struct {
	mu      sync.Mutex
	root    context.Context //nolint:containedctx // last detach and shutdown cancel the scope lifetime
	stop    context.CancelFunc
	backend string
	// epoch advances on every fanout (Update goroutine only); peer content
	// scheduled under an older epoch is dropped on arrival.
	epoch   uint64
	wrapper sharedGitReview
	shared  *gitreview.Shared
	// remote is the route the scope reads through; its session moves when the route owner leaves.
	remote   *runtime.GitReviewRemote
	route    string
	attached map[string]string // anchor -> session ID
	// lease holds anchors whose intent is a full scan. While any lease exists the
	// scope keeps its in-flight scan alive, so an owner switch never restarts it.
	lease  map[string]bool
	retain context.CancelFunc
}

func newGitReviewScope(parent context.Context, backend string) *gitReviewScope {
	root, stop := context.WithCancel(parent)
	return &gitReviewScope{root: root, stop: stop, backend: backend, attached: map[string]string{}, lease: map[string]bool{}}
}

// gitReviewIdentity is one workspace's resolved sharing key.
type gitReviewIdentity struct {
	key     string
	backend string
}

// gitReviewIdentityMsg carries a resolved identity back to the UI goroutine, where
// scopes are attached and bootstrapped. Stale results are dropped by token.
type gitReviewIdentityMsg struct {
	owner       string
	token       uint64
	generation  uint64
	app         *app.App
	fingerprint string
	// browse is the intent captured when the request was made, not the mode at apply time.
	browse bool
	id     gitReviewIdentity
	ok     bool
}

// gitReviewPeerContent refreshes an attached peer's listing or open document. It is
// requested after the scope's authoritative data changes.
type gitReviewPeerContent struct {
	anchor    string
	scopeKey  string
	scope     *gitReviewScope
	epoch     uint64
	directory *view.Directory
	document  *view.Document
}

// resolveGitReviewIdentity runs off the UI goroutine. Local roots are canonicalized
// with EvalSymlinks; remote roots are lexical because they name another machine's
// filesystem. Remote policy comes from the server's effective permissions.
func resolveGitReviewIdentity(ctx context.Context, a *app.App, workingDir string) (gitReviewIdentity, bool) {
	if a == nil || workingDir == "" {
		return gitReviewIdentity{}, false
	}
	rt, handle := a.SessionRuntime(), a.SessionHandle()
	_, remoteRuntime := rt.(runtime.SessionGitReview)
	_, transport := rt.(*runtime.SessionTransport)
	remote := remoteRuntime || transport || (handle != nil && !runtime.IsLocalSessionHandle(handle))
	backend := ""
	if id, ok := rt.(interface{ GitReviewBackendIdentity() string }); ok {
		backend = id.GitReviewBackendIdentity()
	}
	h := sha256.New()
	if remote {
		inspector, ok := handle.(runtime.SessionPermissionsInspector)
		if backend == "" || !ok {
			return gitReviewIdentity{}, false
		}
		callCtx, cancel := context.WithTimeout(ctx, gitReviewIdentityTimeout)
		info, err := inspector.EffectivePermissions(callCtx)
		cancel()
		if err != nil {
			return gitReviewIdentity{}, false
		}
		payload, err := json.Marshal(info)
		if err != nil {
			return gitReviewIdentity{}, false
		}
		fmt.Fprintf(h, "remote\x00%s\x00%s\x00", backend, filepath.Clean(workingDir))
		h.Write(payload)
	} else {
		abs, err := filepath.Abs(workingDir)
		if err != nil {
			return gitReviewIdentity{}, false
		}
		root, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return gitReviewIdentity{}, false
		}
		fmt.Fprintf(h, "local\x00%s\x00%s\x00", backend, root)
		if info := a.PermissionsInfo(); info != nil {
			fmt.Fprintf(h, "%q\x00%q\x00%q\x00", info.Allow, info.Ask, info.Deny)
		}
	}
	sess := a.Session()
	if sess == nil {
		return gitReviewIdentity{}, false
	}
	approved, policy, permissions := sess.SafetySettings()
	fmt.Fprintf(h, "source\x00%s\x00%t\x00%s\x00", sess.AttributesSnapshot()[gitReviewSourceAttribute], approved, policy)
	if encoded, err := json.Marshal(permissions); err == nil {
		h.Write(encoded)
	}
	return gitReviewIdentity{key: hex.EncodeToString(h.Sum(nil)), backend: backend}, true
}

// attachGitReviewScope binds ws to the scope for id, creating it when absent.
func (m *appModel) attachGitReviewScope(ws *gitReviewWorkspace, id gitReviewIdentity) *gitReviewScope {
	if ws.scopeKey != "" && ws.scopeKey != id.key {
		m.detachGitReviewScope(ws)
	}
	scope := m.gitReviewScopes.acquire(id.key, id.backend, m.ctx())
	if scope == nil {
		return nil
	}
	scope.attach(ws.anchor, ws.sessionID)
	ws.scopeKey = id.key
	return scope
}

// detachGitReviewScope releases ws. The last detach closes the shared provider and
// frees the registry entry, so bounded teardown follows the attached workspaces.
func (m *appModel) detachGitReviewScope(ws *gitReviewWorkspace) {
	if ws == nil || ws.scopeKey == "" {
		return
	}
	if scope := m.gitReviewScopes.get(ws.scopeKey); scope != nil && scope.release(ws.anchor) {
		m.gitReviewScopes.drop(ws.scopeKey)
	}
	ws.scopeKey = ""
}

// releaseGitReviewWorkspace drops a workspace whose session no longer exists.
func (m *appModel) releaseGitReviewWorkspace(anchor string) {
	ws := m.gitReviews[anchor]
	if ws == nil {
		return
	}
	m.detachGitReviewScope(ws)
	delete(m.gitReviews, anchor)
	if m.gitReviewOwner == anchor {
		m.cancelGitReviewRequests()
		m.gitReviewOwner = ""
		m.gitReviewGeneration++
	}
}

// sweepGitReviewWorkspaces releases workspaces whose sessions have been closed.
func (m *appModel) sweepGitReviewWorkspaces() {
	if m.supervisor == nil {
		return
	}
	for anchor := range m.gitReviews {
		if m.supervisor.GetRunner(anchor) == nil {
			m.releaseGitReviewWorkspace(anchor)
		}
	}
}

// open returns the scope's provider, creating it once. Concurrent callers wait for
// the first creation instead of constructing a second provider.
func (s *gitReviewScope) open(factory gitReviewProviderFactory, a *app.App, anchor, sessionID, workingDir string) (gitreview.Provider, *view.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root.Err() != nil {
		return nil, nil, context.Canceled
	}
	if s.shared != nil {
		return s.wrapper, nil, nil
	}
	provider, unavailable, err := factory(a, sessionID, workingDir)
	if err != nil || unavailable != nil {
		return nil, unavailable, err
	}
	if provider == nil {
		return nil, nil, errors.New("git review provider unavailable")
	}
	editable, _ := provider.(gitreview.LocalEditor)
	if remote, ok := provider.(*runtime.GitReviewRemote); ok {
		s.remote, s.route = remote, anchor
	}
	s.shared = gitreview.NewShared(provider, gitreview.SharedOptions{})
	s.wrapper = sharedGitReview{Shared: s.shared, editable: editable}
	s.retainLocked()
	return s.wrapper, nil, nil
}

// attach registers anchor as a consumer of the scope on sessionID.
func (s *gitReviewScope) attach(anchor, sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attached[anchor] = sessionID
}

// setFullLease records whether anchor still needs a full scan.
func (s *gitReviewScope) setFullLease(anchor string, full bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if full {
		s.lease[anchor] = true
	} else {
		delete(s.lease, anchor)
	}
	s.retainLocked()
}

// retainLocked keeps one scan running while any full lease exists. Its context ends
// with the last lease, so only the last consumer's departure cancels the scan.
func (s *gitReviewScope) retainLocked() {
	if len(s.lease) == 0 {
		if s.retain != nil {
			s.retain()
			s.retain = nil
		}
		return
	}
	if s.shared == nil || s.retain != nil {
		return
	}
	ctx, cancel := context.WithCancel(s.root)
	s.retain = cancel
	go func(shared *gitreview.Shared) {
		_, _ = shared.EnsureSnapshot(ctx, false)
	}(s.shared)
}

// release detaches anchor. It moves a remote route to a remaining session of the
// same backend, and reports whether the scope is now unused and closed.
func (s *gitReviewScope) release(anchor string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.attached, anchor)
	delete(s.lease, anchor)
	s.retainLocked()
	if len(s.attached) == 0 {
		// Cancelling the root stops any queued bootstrap that still holds this scope.
		if s.stop != nil {
			s.stop()
		}
		if s.shared != nil {
			s.shared.Close()
			s.shared = nil
		}
		s.remote, s.route = nil, ""
		return true
	}
	if s.remote != nil && s.route == anchor {
		next := ""
		for a := range s.attached {
			if next == "" || a < next {
				next = a
			}
		}
		s.route = next
		_ = s.remote.Retarget(s.attached[next], s.backend)
	}
	return false
}

// anchors returns the attached anchors in a stable order.
func (s *gitReviewScope) anchors() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.attached))
	for anchor := range s.attached {
		out = append(out, anchor)
	}
	slices.Sort(out)
	return out
}

// current returns the authoritative snapshot cached by the shared provider.
func (s *gitReviewScope) current() (gitreview.Snapshot, bool) {
	s.mu.Lock()
	shared := s.shared
	s.mu.Unlock()
	if shared == nil {
		return gitreview.Snapshot{}, false
	}
	return shared.CurrentSnapshot()
}

// sharedProvider returns the scope's provider once it exists.
func (s *gitReviewScope) sharedProvider() (sharedGitReview, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wrapper, s.shared != nil
}

// setGitReviewLease updates ws's full-scan intent on its scope.
func (m *appModel) setGitReviewLease(ws *gitReviewWorkspace, full bool) {
	if ws == nil || ws.scopeKey == "" {
		return
	}
	if scope := m.gitReviewScopes.get(ws.scopeKey); scope != nil {
		scope.setFullLease(ws.anchor, full)
	}
}

// Inactive peers must not receive old-scope data while waiting to be reopened.
func (m *appModel) discardChangedGitReviewScope(ws *gitReviewWorkspace) (tea.Cmd, bool) {
	if ws == nil || ws.scopeKey == "" {
		return nil, false
	}
	a := m.gitReviewApp(ws.anchor)
	if a != nil {
		sess := a.Session()
		route, ok := m.supervisor.RouteGeneration(ws.anchor)
		if ok && route == ws.route && sess.ID == ws.sessionID &&
			reviewPolicyFingerprint(a, sess.WorkingDir) == ws.policyFingerprint {
			return nil, false
		}
	}
	m.detachGitReviewScope(ws)
	if m.gitReviewOwner == ws.anchor {
		m.cancelGitReviewRequests()
		m.gitReviewGeneration++
	}
	if ws.inspecting {
		if page, ok := m.gitPage(ws.anchor); ok {
			page.SetContentOverlay(nil)
		}
		ws.inspecting = false
	}
	ws.each(func(model *view.Model) {
		model.StopAnimation()
		delete(m.reviewAnimations, model)
	})
	clear(ws.models)
	clear(ws.directories)
	ws.provider, ws.snapshot = nil, nil
	ws.policyFingerprint = ""
	ws.prewarm = gitSidebarPrewarm{}
	ws.directoryGeneration++
	m.viewCacheValid = false
	return tea.Batch(m.clearReviewHint(ws), m.pushGitSidebar(ws)), true
}

// fanoutGitReviewScope pushes the scope's authoritative snapshot to attached peers
// and refetches their listings and open documents. Peers keep their selections,
// drafts and disclosures; only data they show is refreshed.
func (m *appModel) fanoutGitReviewScope(ws *gitReviewWorkspace) tea.Cmd {
	if ws == nil || ws.scopeKey == "" {
		return nil
	}
	scope := m.gitReviewScopes.get(ws.scopeKey)
	if scope == nil {
		return nil
	}
	snap, ok := scope.current()
	provider, shared := scope.sharedProvider()
	if !ok || !shared {
		return nil
	}
	current := reviewSnapshot(snap)
	scope.epoch++
	var cmds []tea.Cmd
	for _, anchor := range scope.anchors() {
		peer := m.gitReviews[anchor]
		if anchor == ws.anchor || peer == nil || peer.scopeKey != ws.scopeKey || peer.provider == nil {
			continue
		}
		if cmd, changed := m.discardChangedGitReviewScope(peer); changed {
			cmds = append(cmds, cmd)
			continue
		}
		next := cloneReviewSnapshot(current)
		peer.snapshot = &next
		peer.each(func(model *view.Model) { cmds = append(cmds, model.SetSnapshot(next)) })
		cmds = append(cmds, m.peerContentCmds(peer, scope, provider)...)
		cmds = append(cmds, m.pushGitSidebar(peer))
	}
	return tea.Batch(cmds...)
}

// peerContentCmds refetches a peer's visible listings and open documents from the
// scope's cache, so nothing waits for the peer to become active.
func (m *appModel) peerContentCmds(peer *gitReviewWorkspace, scope *gitReviewScope, provider sharedGitReview) []tea.Cmd {
	ctx := m.ctx()
	anchor, scopeKey, epoch := peer.anchor, peer.scopeKey, scope.epoch
	var cmds []tea.Cmd
	for _, dir := range peer.directories {
		worktreeID, path := dir.WorktreeID, dir.Path
		cmds = append(cmds, func() tea.Msg {
			callCtx, cancel := context.WithTimeout(ctx, gitReviewPeerTimeout)
			defer cancel()
			d, err := provider.Directory(callCtx, worktreeID, path)
			if err != nil {
				return nil
			}
			v := reviewDirectory(d)
			v.WorktreeID, v.Path = worktreeID, path
			return gitReviewPeerContent{anchor: anchor, scopeKey: scopeKey, scope: scope, epoch: epoch, directory: &v}
		})
	}
	for _, model := range peer.models {
		key, open := model.OpenKey()
		if !open {
			continue
		}
		cmds = append(cmds, func() tea.Msg {
			callCtx, cancel := context.WithTimeout(ctx, gitReviewPeerTimeout)
			defer cancel()
			d, err := provider.Document(callCtx, gitReviewKey(key))
			if err != nil {
				return nil
			}
			v := reviewDocument(d)
			return gitReviewPeerContent{anchor: anchor, scopeKey: scopeKey, scope: scope, epoch: epoch, document: &v}
		})
	}
	return cmds
}

// applyGitReviewPeerContent updates only what the peer still shows.
func (m *appModel) applyGitReviewPeerContent(msg gitReviewPeerContent) tea.Cmd {
	peer := m.gitReviews[msg.anchor]
	if peer == nil || peer.scopeKey == "" || peer.scopeKey != msg.scopeKey || peer.provider == nil {
		return nil
	}
	scope := m.gitReviewScopes.get(msg.scopeKey)
	if scope == nil || scope != msg.scope || scope.epoch != msg.epoch {
		return nil
	}
	if cmd, changed := m.discardChangedGitReviewScope(peer); changed {
		return cmd
	}
	var cmds []tea.Cmd
	if msg.directory != nil {
		key := msg.directory.WorktreeID + "\x00" + msg.directory.Path
		if _, shown := peer.directories[key]; shown {
			peer.directories[key] = *msg.directory
			if files := peer.models[view.ModeFiles]; files != nil {
				cmds = append(cmds, files.SetDirectory(*msg.directory))
			}
			cmds = append(cmds, m.pushGitSidebar(peer))
		}
	}
	if msg.document != nil {
		for _, model := range peer.models {
			if key, open := model.OpenKey(); open && key == msg.document.Key {
				cmds = append(cmds, model.SetDocument(*msg.document))
			}
		}
	}
	m.viewCacheValid = false
	return tea.Batch(cmds...)
}

// reviewBootstrapSnapshot reuses the scope's cached inventory; only an explicit
// refresh reads Git again.
func reviewBootstrapSnapshot(provider gitreview.Provider, browse bool) func(context.Context) (gitreview.Snapshot, error) {
	if shared, ok := provider.(sharedGitReview); ok {
		return func(ctx context.Context) (gitreview.Snapshot, error) { return shared.EnsureSnapshot(ctx, browse) }
	}
	return reviewScanSnapshot(provider, browse)
}

// loadGitReviewIdentity resolves ws's sharing identity off the UI goroutine.
func (m *appModel) loadGitReviewIdentity(anchor string, a *app.App) tea.Cmd {
	ws := m.gitReviews[anchor]
	fingerprint := reviewPolicyFingerprint(a, ws.workingDir)
	browse := ws.sidebarMode == view.ModeFiles
	ctx, token := m.beginGitReviewRequest("identity")
	generation, workingDir := m.gitReviewGeneration, ws.workingDir
	return func() tea.Msg {
		id, ok := resolveGitReviewIdentity(ctx, a, workingDir)
		return gitReviewIdentityMsg{owner: anchor, token: token, generation: generation, app: a, fingerprint: fingerprint, browse: browse, id: id, ok: ok}
	}
}

// applyGitReviewIdentity attaches the workspace to its scope and starts the
// bootstrap. A current provider with an unchanged key is kept as is; a changed key
// releases the old scope and rebootstraps.
func (m *appModel) applyGitReviewIdentity(msg gitReviewIdentityMsg) tea.Cmd {
	req, ok := m.gitReviewPending["identity"]
	if !ok || req.token != msg.token {
		return nil
	}
	delete(m.gitReviewPending, "identity")
	req.cancel()
	if !m.validGitReviewScope(msg.owner, msg.generation) {
		return nil
	}
	ws := m.gitReviews[msg.owner]
	if ws == nil {
		return nil
	}
	ws.policyFingerprint = msg.fingerprint
	if ws.provider != nil {
		if ws.scopeKey == "" || (msg.ok && msg.id.key == ws.scopeKey) {
			return nil
		}
		m.detachGitReviewScope(ws)
		ws.provider, ws.snapshot = nil, nil
		clear(ws.directories)
	}
	var scope *gitReviewScope
	if msg.ok {
		scope = m.attachGitReviewScope(ws, msg.id)
		if scope == nil {
			return nil
		}
	} else {
		m.detachGitReviewScope(ws)
	}
	return m.startGitReviewBootstrap(ws, scope, msg.app, msg.browse)
}

// reviewPolicyFingerprint is the cheap, in-memory policy and identity metadata of a
// workspace. It never performs I/O; a change schedules full identity resolution.
func reviewPolicyFingerprint(a *app.App, workingDir string) string {
	if a == nil {
		return ""
	}
	h := sha256.New()
	backend := ""
	if id, ok := a.SessionRuntime().(interface{ GitReviewBackendIdentity() string }); ok {
		backend = id.GitReviewBackendIdentity()
	}
	fmt.Fprintf(h, "%s\x00%s\x00", backend, filepath.Clean(workingDir))
	if sess := a.Session(); sess != nil {
		approved, policy, permissions := sess.SafetySettings()
		fmt.Fprintf(h, "%t\x00%s\x00%s\x00", approved, policy, sess.AttributesSnapshot()[gitReviewSourceAttribute])
		if encoded, err := json.Marshal(permissions); err == nil {
			h.Write(encoded)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// revalidateGitReviewScope schedules identity resolution only when a shared
// workspace's policy metadata changed since it was attached.
func (m *appModel) revalidateGitReviewScope(ws *gitReviewWorkspace, a *app.App) (tea.Cmd, bool) {
	if ws == nil || ws.provider == nil || ws.scopeKey == "" {
		return nil, false
	}
	if !m.validGitReviewScope(ws.anchor, m.gitReviewGeneration) {
		return nil, false
	}
	if _, resolving := m.gitReviewPending["identity"]; resolving {
		return nil, false
	}
	if reviewPolicyFingerprint(a, ws.workingDir) == ws.policyFingerprint {
		return nil, false
	}
	return m.loadGitReviewIdentity(ws.anchor, a), true
}

// gitReviewApp returns the running app behind an anchor, if its session is still open.
func (m *appModel) gitReviewApp(anchor string) *app.App {
	if m.supervisor == nil {
		return nil
	}
	runner := m.supervisor.GetRunner(anchor)
	if runner == nil || runner.App == nil || runner.App.Session() == nil {
		return nil
	}
	return runner.App
}

// closeAll cancels the retained scan and closes the shared provider; idempotent.
func (s *gitReviewScope) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.lease)
	clear(s.attached)
	if s.stop != nil {
		s.stop()
	}
	s.retain = nil
	if s.shared != nil {
		s.shared.Close()
		s.shared = nil
	}
	s.remote, s.route = nil, ""
}
