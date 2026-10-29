package tui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/gitreview"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/server"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	view "github.com/docker/docker-agent/pkg/tui/components/review"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

// regGatedProvider holds every full scan inside Snapshot until release is closed.
// gone closes when a scan observes cancellation.
type regGatedProvider struct {
	*fakeReviewProvider

	entered   chan struct{}
	release   chan struct{}
	gone      chan struct{}
	scans     atomic.Int32
	cancelled atomic.Int32
}

func newRegGatedProvider(base *fakeReviewProvider) *regGatedProvider {
	return &regGatedProvider{fakeReviewProvider: base, entered: make(chan struct{}, 8), release: make(chan struct{}), gone: make(chan struct{})}
}

func (p *regGatedProvider) Snapshot(ctx context.Context) (gitreview.Snapshot, error) {
	p.scans.Add(1)
	p.entered <- struct{}{}
	select {
	case <-p.release:
		if ctx.Err() == nil {
			return p.fakeReviewProvider.Snapshot(ctx)
		}
	case <-ctx.Done():
	}
	if p.cancelled.Add(1) == 1 {
		close(p.gone)
	}
	return gitreview.Snapshot{}, ctx.Err()
}

// regVersionedProvider labels each directory listing with the inventory generation
// it was read at, so a stale delivery is observable.
type regVersionedProvider struct{ *fakeReviewProvider }

func (p regVersionedProvider) Directory(ctx context.Context, worktreeID, path string) (gitreview.Directory, error) {
	d, err := p.fakeReviewProvider.Directory(ctx, worktreeID, path)
	p.mu.Lock()
	generation := p.snapshots
	p.mu.Unlock()
	d.Entries = []gitreview.DirectoryEntry{{Name: fmt.Sprintf("gen-%d", generation)}}
	return d, err
}

// Document labels each document with the inventory generation it was read at.
func (p regVersionedProvider) Document(ctx context.Context, key gitreview.DocumentKey) (gitreview.Document, error) {
	d, err := p.fakeReviewProvider.Document(ctx, key)
	p.mu.Lock()
	generation := p.snapshots
	p.mu.Unlock()
	d.Lines = []gitreview.DocumentLine{{Kind: gitreview.DiffAdd, New: 1, Text: fmt.Sprintf("doc-gen-%d", generation)}}
	return d, err
}

// regFactory counts provider constructions; next supplies each one.
type regFactory struct{ calls atomic.Int32 }

func installRegFactory(h *reviewHarness, next func() gitreview.Provider) *regFactory {
	f := &regFactory{}
	h.m.gitReviewFactory = func(*app.App, string, string) (gitreview.Provider, *view.Snapshot, error) {
		f.calls.Add(1)
		return next(), nil, nil
	}
	return f
}

// regIdentityRuntime is a local runtime that names its backend.
type regIdentityRuntime struct{ backend string }

func (regIdentityRuntime) SessionByID(string) (runtime.SessionHandle, error) {
	return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound}
}

func (regIdentityRuntime) CreateSession(context.Context, *session.Session, runtime.SessionBinding) (runtime.SessionHandle, error) {
	return nil, errors.New("test runtime does not create sessions")
}

func (regIdentityRuntime) DeleteSession(context.Context, string) error { return nil }

func (r regIdentityRuntime) GitReviewBackendIdentity() string { return r.backend }

// regRemoteRuntime advertises the server-side review capability. Without a
// permissions inspector it must never join a local shared scope.
type regRemoteRuntime struct{ regIdentityRuntime }

func (regRemoteRuntime) GitReviewSnapshot(context.Context, string) (gitreview.Snapshot, error) {
	return gitreview.Snapshot{}, gitreview.ErrUnavailable
}

func (regRemoteRuntime) GitReviewWorktree(context.Context, string, uint64, string) (gitreview.Worktree, error) {
	return gitreview.Worktree{}, gitreview.ErrUnavailable
}

func (regRemoteRuntime) GitReviewDirectory(context.Context, string, uint64, string, string) (gitreview.Directory, error) {
	return gitreview.Directory{}, gitreview.ErrUnavailable
}

func (regRemoteRuntime) GitReviewDocument(context.Context, string, uint64, gitreview.DocumentKey) (gitreview.Document, error) {
	return gitreview.Document{}, gitreview.ErrUnavailable
}

// newRegHarness gives the owner session a real, canonicalizable folder.
func newRegHarness(t *testing.T) (*reviewHarness, string) {
	t.Helper()
	h := newReviewHarness(t, 140)
	dir := t.TempDir()
	h.sess.WorkingDir = dir
	return h, dir
}

// addRegPeer adds a second session to the harness supervisor, optionally on its own runtime.
func addRegPeer(t *testing.T, h *reviewHarness, dir string, rt runtime.SessionRuntime, mutate func(*session.Session)) *session.Session {
	t.Helper()
	const id = "s2"
	sess := &session.Session{ID: id, Title: id, WorkingDir: dir}
	if mutate != nil {
		mutate(sess)
	}
	a := app.New(t.Context(), rt, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	_, err := h.m.supervisor.AddSession(t.Context(), a, sess, dir, nil)
	require.NoError(t, err)
	h.m.chatPages[id] = &reviewChatPage{overlayWidth: 140}
	return sess
}

func switchRegWorkspace(t *testing.T, h *reviewHarness, id string) {
	t.Helper()
	h.m.paneWorkspaces.active = &paneWorkspace{anchor: id}
	h.m.supervisor.SwitchTo(id)
	h.pump(h.m.syncGitReview())
}

func regAwait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("the expected signal never arrived")
	}
}

// regCollect runs cmd synchronously and returns every message it produces.
func regCollect(cmd tea.Cmd) []tea.Msg {
	var out []tea.Msg
	var walk func(tea.Cmd)
	walk = func(c tea.Cmd) {
		if c == nil {
			return
		}
		msg := c()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, next := range batch {
				walk(next)
			}
			return
		}
		out = append(out, msg)
	}
	walk(cmd)
	return out
}

// regDeliver feeds msgs into the model, holding peer content results instead of
// applying them so a test can deliver them later.
func regDeliver(h *reviewHarness, msgs []tea.Msg, held *[]gitReviewPeerContent) {
	for _, msg := range msgs {
		if content, ok := msg.(gitReviewPeerContent); ok {
			*held = append(*held, content)
			continue
		}
		_, next := h.m.Update(msg)
		regDeliver(h, regCollect(next), held)
	}
}

// Two workspaces on one scope share one provider. The peer attaches while the
// owner's full scan is blocked, joins that scan, and the owner's handoff never cancels it.
func TestConcurrentSameWorkspaceAttachJoinsOneBlockedScan(t *testing.T) {
	h, dir := newRegHarness(t)
	addRegPeer(t, h, dir, nil, nil)
	gate := newRegGatedProvider(h.provider)
	factory := installRegFactory(h, func() gitreview.Provider { return gate })

	owner := runInFlightWith(h, browseRequest(h, "s1", sidebar.GitShowChanges))
	regAwait(t, gate.entered)
	require.Equal(t, int32(1), factory.calls.Load())

	switchRegWorkspace(t, h, "s2")
	peer := runInFlightWith(h, browseRequest(h, "s2", sidebar.GitShowChanges))
	select {
	case <-peer:
		t.Fatal("the peer completed before the blocked scan was released")
	default:
	}

	close(gate.release)
	feedResults(h, awaitResults(t, owner))
	feedResults(h, awaitResults(t, peer))

	assert.Equal(t, int32(1), factory.calls.Load(), "one provider serves both attachments")
	assert.Equal(t, int32(1), gate.scans.Load(), "one scan serves both attachments")
	assert.Zero(t, gate.cancelled.Load(), "the owner's handoff never cancels a scan a peer still waits on")
	assert.Equal(t, 1, h.provider.snapshots)
	peerWS := h.m.gitReviews["s2"]
	require.NotNil(t, peerWS.snapshot)
	assert.EqualValues(t, 1, peerWS.snapshot.Generation)

	switchRegWorkspace(t, h, "s1")
	h.pump(browseRequest(h, "s1", sidebar.GitShowChanges))
	require.NotNil(t, h.m.gitReviews["s1"].snapshot, "the owner's superseded bootstrap is served from the shared inventory")
	assert.Equal(t, int32(1), gate.scans.Load(), "returning to the owner reuses the inventory")
	assert.Equal(t, int32(1), factory.calls.Load())
}

// Shutdown cancels the retained scan even when no further UI update arrives.
func TestShutdownCancelsRetainedScanWithoutFurtherUpdates(t *testing.T) {
	h, _ := newRegHarness(t)
	gate := newRegGatedProvider(h.provider)
	installRegFactory(h, func() gitreview.Provider { return gate })

	results := runInFlightWith(h, browseRequest(h, "s1", sidebar.GitShowChanges))
	regAwait(t, gate.entered)

	h.m.cleanupManagedResources()
	regAwait(t, gate.gone)
	awaitResults(t, results)
}

// Files attaches to a root read only: no workspace in the scope runs a repository scan.
func TestFilesAttachToSharedWorkspaceNeverSnapshots(t *testing.T) {
	h, dir := newRegHarness(t)
	addRegPeer(t, h, dir, nil, nil)
	factory := installRegFactory(h, func() gitreview.Provider { return h.provider })

	h.pump(browseRequest(h, "s1", sidebar.GitBrowse))
	require.Equal(t, int32(1), factory.calls.Load())
	require.Equal(t, 1, h.provider.browses)
	require.Equal(t, 0, h.provider.snapshots, "Files reads only the root")

	switchRegWorkspace(t, h, "s2")
	h.pump(browseRequest(h, "s2", sidebar.GitBrowse))
	assert.Equal(t, int32(1), factory.calls.Load(), "the peer attaches to the shared provider")
	assert.Equal(t, 1, h.provider.browses, "and reuses the cached root")
	assert.Equal(t, 0, h.provider.snapshots, "attaching in Files never scans")
	assert.Empty(t, h.provider.dirs, "attaching never lists folders")
	require.NotNil(t, h.m.gitReviews["s2"].snapshot)
	assert.True(t, h.m.gitReviews["s2"].snapshot.Browse)

	switchRegWorkspace(t, h, "s1")
	h.pump(browseRequest(h, "s1", sidebar.GitShowChanges))
	assert.Equal(t, 1, h.provider.snapshots, "only the explicit Changes selection scans")
	assert.False(t, h.m.gitReviews["s2"].snapshot.Browse, "the root-only peer receives the scanned inventory")
}

// An explicit refresh reaches attached peers' inventory and folder listings, while
// their drafts, selection, open document and sidebar expansion stay as they were.
func TestExplicitRefreshFanoutLeavesPeerPresentationAndDraftsAlone(t *testing.T) {
	h, dir := newRegHarness(t)
	addRegPeer(t, h, dir, nil, nil)
	installRegFactory(h, func() gitreview.Provider { return h.provider })
	h.pump(browseRequest(h, "s1", sidebar.GitShowChanges))
	require.Equal(t, 1, h.provider.snapshots)

	switchRegWorkspace(t, h, "s2")
	h.pump(browseRequest(h, "s2", sidebar.GitShowChanges))
	_, cmd := h.m.Update(sidebar.GitRequestMsg{Owner: "s2", Action: sidebar.GitOpen, WorktreeID: "w1", Path: "Dockerfile.dev", Group: view.GroupUnstaged})
	h.pump(cmd)
	h.comment("peer note")
	_, cmd = h.m.Update(sidebar.GitRequestMsg{Owner: "s2", Action: sidebar.GitLoadDirectory, WorktreeID: "w1", Path: "src"})
	h.pump(cmd)
	require.Equal(t, 1, countString(h.provider.dirs, "src"))
	page, ok := h.m.gitPage("s2")
	require.True(t, ok)
	page.(*reviewChatPage).watch = sidebar.GitWatchMsg{Owner: "s2", Open: true, Mode: view.ModeChanges, ExpandedWorktrees: []string{"w2"}}

	peer := h.m.gitReviews["s2"]
	draft := peer.draft.Snapshot()
	require.Len(t, draft.Comments, 1, "the peer holds a draft comment")
	selection := peer.current().Selection()
	openKey, open := peer.models[view.ModeChanges].OpenKey()
	require.True(t, open, "the peer has a document open")

	switchRegWorkspace(t, h, "s1")
	h.pump(browseRequest(h, "s1", sidebar.GitRefresh))
	require.Equal(t, 2, h.provider.snapshots)

	assert.EqualValues(t, 2, peer.snapshot.Generation, "the peer receives the refreshed inventory")
	assert.Equal(t, draft, peer.draft.Snapshot(), "the peer's draft is untouched")
	assert.Equal(t, selection, peer.current().Selection(), "the peer's selection is untouched")
	key, stillOpen := peer.models[view.ModeChanges].OpenKey()
	assert.True(t, stillOpen)
	assert.Equal(t, openKey, key, "the peer's open document is untouched")
	page, _ = h.m.gitPage("s2")
	assert.Equal(t, []string{"w2"}, page.GitWatchState().ExpandedWorktrees, "the peer's sidebar expansion is untouched")

	switchRegWorkspace(t, h, "s2")
	_, cmd = h.m.Update(sidebar.GitRequestMsg{Owner: "s2", Action: sidebar.GitLoadDirectory, WorktreeID: "w1", Path: "src"})
	h.pump(cmd)
	assert.Equal(t, 2, countString(h.provider.dirs, "src"), "a folder listing from before the refresh is never reused")
}

// A peer content result read before a newer refresh must not overwrite the newer listing.
func TestDelayedPeerContentFromSupersededGenerationIsDropped(t *testing.T) {
	h, dir := newRegHarness(t)
	addRegPeer(t, h, dir, nil, nil)
	installRegFactory(h, func() gitreview.Provider { return regVersionedProvider{h.provider} })
	h.pump(browseRequest(h, "s1", sidebar.GitShowChanges))

	switchRegWorkspace(t, h, "s2")
	h.pump(browseRequest(h, "s2", sidebar.GitShowChanges))
	_, cmd := h.m.Update(sidebar.GitRequestMsg{Owner: "s2", Action: sidebar.GitLoadDirectory, WorktreeID: "w1", Path: "src"})
	h.pump(cmd)
	peer := h.m.gitReviews["s2"]
	require.Equal(t, "gen-1", peer.directories["w1\x00src"].Entries[0].Name)

	switchRegWorkspace(t, h, "s1")
	var held []gitReviewPeerContent
	regDeliver(h, regCollect(browseRequest(h, "s1", sidebar.GitRefresh)), &held)
	require.NotEmpty(t, held, "the first refresh produced peer content that has not been delivered")

	h.pump(browseRequest(h, "s1", sidebar.GitRefresh))
	require.Equal(t, "gen-3", peer.directories["w1\x00src"].Entries[0].Name, "the newest refresh reached the peer")

	for _, content := range held {
		h.pump(func() tea.Msg { return content })
	}
	assert.Equal(t, "gen-3", peer.directories["w1\x00src"].Entries[0].Name, "superseded peer content never overwrites the newer listing")
}

// A benign sync after attach resolves no identity again.
func TestBenignSyncNeverRepeatsIdentityResolution(t *testing.T) {
	h, dir := newRegHarness(t)
	addRegPeer(t, h, dir, nil, nil)
	factory := installRegFactory(h, func() gitreview.Provider { return h.provider })
	h.pump(browseRequest(h, "s1", sidebar.GitShowChanges))
	require.Equal(t, int32(1), factory.calls.Load())

	for _, msg := range regCollect(h.m.syncGitReview()) {
		_, isIdentity := msg.(gitReviewIdentityMsg)
		assert.False(t, isIdentity, "a sync with no scope change never re-resolves identity")
	}
	assert.Equal(t, int32(1), factory.calls.Load())
}

// Scope keys isolate backend, team source, folder, safety, permissions and remote
// sessions. Only a peer with an identical scope key shares the owner's provider.
func TestSharedScopeKeyIsolatesBackendSourcePolicyAndFolder(t *testing.T) {
	cases := []struct {
		name   string
		other  bool
		slash  bool
		rt     runtime.SessionRuntime
		mutate func(*session.Session)
		shared bool
	}{
		{name: "same folder, backend and policy", shared: true},
		{name: "same folder spelled with a trailing slash", slash: true, shared: true},
		{name: "other folder", other: true},
		{name: "backend identity", rt: regIdentityRuntime{backend: "backend-b"}},
		{name: "team source attribute", mutate: func(s *session.Session) { s.SetAttribute(gitReviewSourceAttribute, "team-b") }},
		{name: "tools approved", mutate: func(s *session.Session) { s.ToolsApproved = true }},
		{name: "safety policy", mutate: func(s *session.Session) { s.SafetyPolicy = session.SafetyPolicyStrict }},
		{name: "session permissions", mutate: func(s *session.Session) {
			s.Permissions = &session.PermissionsConfig{Deny: []string{"shell"}}
		}},
		{name: "remote runtime without a shared scope", rt: regRemoteRuntime{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, dir := newRegHarness(t)
			peerDir := dir
			switch {
			case tc.other:
				peerDir = t.TempDir()
			case tc.slash:
				peerDir = dir + "/"
			}
			addRegPeer(t, h, peerDir, tc.rt, tc.mutate)
			var made []*fakeReviewProvider

			factory := installRegFactory(h, func() gitreview.Provider {
				p := &fakeReviewProvider{}
				if len(made) == 0 {
					p = h.provider
				}
				made = append(made, p)
				return p
			})

			h.pump(browseRequest(h, "s1", sidebar.GitShowChanges))
			switchRegWorkspace(t, h, "s2")
			h.pump(browseRequest(h, "s2", sidebar.GitShowChanges))
			switchRegWorkspace(t, h, "s1")
			h.pump(browseRequest(h, "s1", sidebar.GitRefresh))

			peer := h.m.gitReviews["s2"]
			require.NotNil(t, peer.snapshot)
			assert.Equal(t, 2, h.provider.snapshots, "the owner's refresh scans its own provider")
			if tc.shared {
				assert.Equal(t, int32(1), factory.calls.Load(), "one provider serves the scope")
				assert.EqualValues(t, 2, peer.snapshot.Generation, "the refresh reaches the attached peer")
				return
			}
			require.Equal(t, int32(2), factory.calls.Load(), "an unshared scope constructs its own provider")
			require.Len(t, made, 2)
			assert.EqualValues(t, 1, peer.snapshot.Generation, "an unshared peer keeps its own inventory")
			assert.Equal(t, 1, made[1].snapshots, "and is never rescanned by the owner's refresh")
		})
	}
}

// The provider is closed only when its last attached workspace leaves its scope.
func TestSharedProviderClosesOnlyAfterLastWorkspaceLeaves(t *testing.T) {
	h, dir := newRegHarness(t)
	peer := addRegPeer(t, h, dir, nil, nil)
	installRegFactory(h, func() gitreview.Provider { return h.provider })
	h.pump(browseRequest(h, "s1", sidebar.GitShowChanges))
	shared := h.m.gitReviews["s1"].provider
	require.NotNil(t, shared)

	switchRegWorkspace(t, h, "s2")
	h.pump(browseRequest(h, "s2", sidebar.GitShowChanges))
	oldKey := h.m.gitReviews["s2"].scopeKey
	peer.WorkingDir = t.TempDir()
	h.pump(browseRequest(h, "s2", sidebar.GitShowChanges))
	_, err := shared.LoadWorktree(t.Context(), "w1")
	require.NoError(t, err, "a remaining attached workspace keeps the shared provider open")

	switchRegWorkspace(t, h, "s1")
	h.sess.WorkingDir = t.TempDir()
	h.pump(browseRequest(h, "s1", sidebar.GitShowChanges))
	_, err = shared.LoadWorktree(t.Context(), "w1")
	require.ErrorIs(t, err, gitreview.ErrUnavailable, "the last workspace to leave closes the shared provider")
	require.NotEmpty(t, oldKey)
	assert.Nil(t, h.m.gitReviewScopes.get(oldKey), "the old folder's scope is dropped")
	assert.NotNil(t, h.m.gitReviewScopes.get(h.m.gitReviews["s2"].scopeKey), "the peer's moved folder keeps its scope")
}

// regRequestCounter counts HTTP requests per review route, so a test can assert
// how often the server was scanned, listed or read.
type regRequestCounter struct {
	base  http.RoundTripper
	mu    sync.Mutex
	calls map[string]int
}

func (c *regRequestCounter) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.calls[regRoute(r)]++
	c.mu.Unlock()
	return c.base.RoundTrip(r)
}

func (c *regRequestCounter) count(route string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[route]
}

func regRoute(r *http.Request) string {
	path := r.URL.Path
	if i := strings.Index(path, "/git-review"); i >= 0 {
		return r.Method + " " + path[i:]
	}
	if strings.HasSuffix(path, "/permissions") {
		return r.Method + " /permissions"
	}
	return r.Method + " " + path
}

// regServerToken is the owner's valid credential; clients dial with it or with a wrong one.
const regServerToken = "reg-owner-token"

// regRemote is one client's view of a loopback session API served by a real owner runtime.
type regRemote struct {
	transport *runtime.SessionTransport
	calls     *regRequestCounter
	url       string
	owner     runtime.SessionRuntime
	dir       string
}

func newRegRemote(t *testing.T, dir string) *regRemote {
	t.Helper()
	store := session.NewInMemorySessionStore()
	local, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(
		agent.New("root", "root", agent.WithModel(tuiModelProvider{})),
	)), runtime.WithSessionStore(store), runtime.WithWorkingDir(dir))
	require.NoError(t, err)
	supervised := runtime.NewSessionRuntimeSupervisor(local)
	t.Cleanup(func() { require.NoError(t, supervised.Shutdown(context.WithoutCancel(t.Context()))) })

	serverCtx, stopServer := context.WithCancel(t.Context())
	t.Cleanup(stopServer)
	listener, err := (&net.ListenConfig{}).Listen(serverCtx, "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	api := server.NewWithManager(server.NewSessionManager(serverCtx, nil, store, 0, nil, server.WithSessionRuntime(supervised.Runtime())), regServerToken)
	go func() { _ = api.Serve(serverCtx, listener) }()

	owned := &regRemote{url: "http://" + listener.Addr().String(), owner: supervised.Runtime(), dir: dir}
	return owned.dial(t, regServerToken)
}

// dial opens a client with its own backend identity and request counter.
func (r *regRemote) dial(t *testing.T, token string) *regRemote {
	t.Helper()
	counter := &regRequestCounter{base: http.DefaultTransport, calls: map[string]int{}}
	client, err := runtime.NewClient(r.url, runtime.WithAuthToken(token), runtime.WithHTTPClient(&http.Client{Transport: counter}))
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	return &regRemote{transport: transport, calls: counter, url: r.url, owner: r.owner, dir: r.dir}
}

// session creates a root session with a fixed ID on the owner runtime, in the remote's folder.
func (r *regRemote) session(t *testing.T, id string) *session.Session {
	t.Helper()
	sess := session.New(session.WithID(id), session.WithAgentName("root"), session.WithWorkingDir(r.dir))
	_, err := r.owner.CreateSession(t.Context(), sess, runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	return sess
}

// app is a TUI-side view of a server session, borrowing this client's transport.
func (r *regRemote) app(t *testing.T, sess *session.Session) *app.App {
	t.Helper()
	return app.New(t.Context(), r.transport, sess.Clone(), runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(stubRuntime{}))
}

// regInitRepo makes dir a git checkout with one tracked file and one unstaged change.
func regInitRepo(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for the remote review fixture")
	}
	git := func(args ...string) {
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	file := filepath.Join(dir, "Dockerfile.dev")
	git("init", "-q")
	require.NoError(t, os.WriteFile(file, []byte("FROM golang:1.22\n"), 0o644))
	git("add", "-A")
	git("commit", "-q", "-m", "init")
	require.NoError(t, os.WriteFile(file, []byte("FROM golang:1.27\n"), 0o644))
}

// newRegRemoteHarness makes the harness's focused workspace a remote session.
func newRegRemoteHarness(t *testing.T, remote *regRemote, sess *session.Session) *reviewHarness {
	t.Helper()
	h := newReviewHarness(t, 140)
	h.m.gitReviewFactory = nil
	a := remote.app(t, sess)
	sv := supervisor.New(nil)
	_, err := sv.AddSession(t.Context(), a, sess, remote.dir, nil)
	require.NoError(t, err)
	h.m.supervisor, h.m.application = sv, a
	h.m.sessionState = service.NewSessionState(sess)
	h.sess = sess
	return h
}

// addRegRemotePeer adds a session through a separately constructed app.
func addRegRemotePeer(t *testing.T, h *reviewHarness, id string, remote *regRemote, sess *session.Session) {
	t.Helper()
	_, err := h.m.supervisor.AddSession(t.Context(), remote.app(t, sess), sess, remote.dir, nil)
	require.NoError(t, err)
	h.m.chatPages[id] = &reviewChatPage{overlayWidth: 140}
}

// attachRegRemotePeer opens a server session as a peer through the production
// PrepareSessionView, Commit and NewResolvedFromTemplate path, borrowing the focused template's transport.
func attachRegRemotePeer(t *testing.T, h *reviewHarness, remote *regRemote, id string) {
	t.Helper()
	remote.session(t, id)
	prepared, err := remote.transport.PrepareSessionView(t.Context(), id)
	require.NoError(t, err)
	t.Cleanup(prepared.Abort)
	committed, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	peer, err := app.NewResolvedFromTemplate(t.Context(), remote.transport, committed, h.m.application)
	require.NoError(t, err)
	require.Equal(t, id, peer.Session().ID)
	_, err = h.m.supervisor.AddSession(t.Context(), peer, peer.Session(), remote.dir, nil)
	require.NoError(t, err)
	h.m.chatPages[id] = &reviewChatPage{overlayWidth: 140}
}

func regCurrentWorktree(t *testing.T, ws *gitReviewWorkspace) string {
	t.Helper()
	require.NotNil(t, ws.snapshot)
	id := reviewCurrentWorktree(*ws.snapshot)
	require.NotEmpty(t, id, "the snapshot names a current worktree")
	return id
}

func regChangedFile(t *testing.T, ws *gitReviewWorkspace) view.FileChange {
	t.Helper()
	require.NotNil(t, ws.snapshot)
	for _, repo := range ws.snapshot.Repositories {
		for _, wt := range repo.Worktrees {
			for _, file := range wt.Files {
				return file
			}
		}
	}
	t.Fatal("the snapshot lists no changed file")
	return view.FileChange{}
}

// Equal-scope remote peers share one server inventory, one folder listing and
// one document read, while their drafts stay separate.
func TestRemoteEqualScopePeersShareOneServerScanAndContent(t *testing.T) {
	dir := t.TempDir()
	regInitRepo(t, dir)
	remote := newRegRemote(t, dir)
	h := newRegRemoteHarness(t, remote, remote.session(t, "s1"))
	attachRegRemotePeer(t, h, remote, "s2")

	h.pump(browseRequest(h, "s1", sidebar.GitShowChanges))
	require.Equal(t, 1, remote.calls.count("GET /git-review"), "the first Changes bootstrap reads the server inventory once")
	ownerWS := h.m.gitReviews["s1"]
	require.NotEmpty(t, ownerWS.scopeKey, "a remote identity joins a shared scope")
	wt := regCurrentWorktree(t, ownerWS)

	switchRegWorkspace(t, h, "s2")
	h.pump(browseRequest(h, "s2", sidebar.GitShowChanges))
	peerWS := h.m.gitReviews["s2"]
	assert.Equal(t, ownerWS.scopeKey, peerWS.scopeKey, "equal backend, folder and policy share one scope")
	assert.Equal(t, 1, remote.calls.count("GET /git-review"), "the peer attaches to the cached inventory without a second scan")
	require.NotNil(t, peerWS.snapshot)
	assert.Equal(t, ownerWS.snapshot.Generation, peerWS.snapshot.Generation)

	_, cmd := h.m.Update(sidebar.GitRequestMsg{Owner: "s2", Action: sidebar.GitLoadDirectory, WorktreeID: wt, Path: ""})
	h.pump(cmd)
	listings := remote.calls.count("POST /git-review/directory")
	require.Equal(t, 1, listings)
	switchRegWorkspace(t, h, "s1")
	_, cmd = h.m.Update(sidebar.GitRequestMsg{Owner: "s1", Action: sidebar.GitLoadDirectory, WorktreeID: wt, Path: ""})
	h.pump(cmd)
	assert.Equal(t, listings, remote.calls.count("POST /git-review/directory"), "the owner's listing is served from the shared cache")

	file := regChangedFile(t, ownerWS)
	_, cmd = h.m.Update(sidebar.GitRequestMsg{Owner: "s1", Action: sidebar.GitOpen, WorktreeID: wt, Path: file.Path, Group: file.Group})
	h.pump(cmd)
	documents := remote.calls.count("POST /git-review/document")
	require.Equal(t, 1, documents)
	switchRegWorkspace(t, h, "s2")
	_, cmd = h.m.Update(sidebar.GitRequestMsg{Owner: "s2", Action: sidebar.GitOpen, WorktreeID: wt, Path: file.Path, Group: file.Group})
	h.pump(cmd)
	assert.Equal(t, documents, remote.calls.count("POST /git-review/document"), "the peer's identical document comes from the shared cache")

	require.NoError(t, ownerWS.draft.SetGeneral("owner note"))
	require.NoError(t, peerWS.draft.SetGeneral("peer note"))

	switchRegWorkspace(t, h, "s1")
	h.pump(browseRequest(h, "s1", sidebar.GitRefresh))
	assert.Equal(t, 2, remote.calls.count("GET /git-review"), "one explicit refresh reads the server inventory once for the whole scope")
	assert.Equal(t, documents+1, remote.calls.count("POST /git-review/document"), "the peer's open document is re-read once after the refresh")
	assert.Equal(t, ownerWS.snapshot.Generation, peerWS.snapshot.Generation, "the peer shows the refreshed inventory")
	assert.Equal(t, "owner note", ownerWS.draft.Snapshot().General, "the owner's draft is its own")
	assert.Equal(t, "peer note", peerWS.draft.Snapshot().General, "the peer's draft is its own")
}

// A second server is a different backend: the same folder there never shares a scope.
func TestRemoteScopesNeverSpanServers(t *testing.T) {
	dir := t.TempDir()
	regInitRepo(t, dir)
	first := newRegRemote(t, dir)
	second := newRegRemote(t, dir)
	h := newRegRemoteHarness(t, first, first.session(t, "s1"))
	addRegRemotePeer(t, h, "s2", second, second.session(t, "s2"))

	h.pump(browseRequest(h, "s1", sidebar.GitShowChanges))
	switchRegWorkspace(t, h, "s2")
	h.pump(browseRequest(h, "s2", sidebar.GitShowChanges))

	assert.NotEmpty(t, h.m.gitReviews["s1"].scopeKey)
	assert.NotEmpty(t, h.m.gitReviews["s2"].scopeKey)
	assert.NotEqual(t, h.m.gitReviews["s1"].scopeKey, h.m.gitReviews["s2"].scopeKey, "a different server is a different backend")
	assert.Equal(t, 1, first.calls.count("GET /git-review"), "the first server is scanned once")
	assert.Equal(t, 1, second.calls.count("GET /git-review"), "the second server is scanned on its own")
}

// A client with a wrong credential never joins the owner's scope or reads its inventory.
func TestRemoteAuthIdentitiesNeverShareScopeOrInventory(t *testing.T) {
	dir := t.TempDir()
	regInitRepo(t, dir)
	owner := newRegRemote(t, dir)
	h := newRegRemoteHarness(t, owner, owner.session(t, "s1"))
	h.pump(browseRequest(h, "s1", sidebar.GitShowChanges))
	require.NotEmpty(t, h.m.gitReviews["s1"].scopeKey)

	intruder := owner.dial(t, "wrong-token")
	addRegRemotePeer(t, h, "s2", intruder, intruder.session(t, "s2"))
	switchRegWorkspace(t, h, "s2")
	h.pump(browseRequest(h, "s2", sidebar.GitShowChanges))

	intruderWS := h.m.gitReviews["s2"]
	assert.Empty(t, intruderWS.scopeKey, "an unauthorized client never joins the owner's scope")
	require.NotNil(t, intruderWS.snapshot)
	assert.Empty(t, intruderWS.snapshot.Repositories, "an unauthorized client never receives the owner's inventory")
	assert.NotEmpty(t, intruderWS.snapshot.Scope.Unavailable, "the failed read is reported")
	assert.GreaterOrEqual(t, intruder.calls.count("GET /git-review"), 1, "the intruder did attempt its own read")
	assert.Equal(t, 1, owner.calls.count("GET /git-review"), "the owner's inventory is scanned once; the failed attempt never reuses it")

	switchRegWorkspace(t, h, "s1")
	assert.NotEmpty(t, h.m.gitReviews["s1"].snapshot.Repositories, "the owner keeps its own inventory")
}

// The remote review reads only the server's folder: a path that exists only on the
// TUI's machine is never read, and the server's inventory still arrives.
func TestRemoteWorkspaceNeverReadsLocalFiles(t *testing.T) {
	dir := t.TempDir()
	regInitRepo(t, dir)
	remote := newRegRemote(t, dir)
	local := remote.session(t, "s1").Clone()
	local.WorkingDir = filepath.Join(t.TempDir(), "not-on-this-machine")
	h := newRegRemoteHarness(t, remote, local)

	h.pump(browseRequest(h, "s1", sidebar.GitShowChanges))
	ws := h.m.gitReviews["s1"]
	require.NotNil(t, ws.snapshot)
	assert.Empty(t, ws.snapshot.Scope.Unavailable, "the review does not fail on a folder that exists only locally")
	assert.NotEmpty(t, ws.snapshot.Repositories, "the server's inventory arrives")
	assert.NotEmpty(t, ws.scopeKey, "the remote identity resolves without local paths")
	assert.Equal(t, 1, remote.calls.count("GET /git-review"))
}

// A peer whose policy or source changes after sharing detaches at its next ensure
// boundary, bootstraps its own provider, and never receives the other workspace's refresh.
func TestPolicyChangeOnSharedPeerDetachesBeforeFanout(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*session.Session)
	}{
		{name: "tools approved", mutate: func(s *session.Session) { s.ToolsApproved = true }},
		{name: "safety policy", mutate: func(s *session.Session) { s.SafetyPolicy = session.SafetyPolicyStrict }},
		{name: "source attribute", mutate: func(s *session.Session) { s.SetAttribute(gitReviewSourceAttribute, "team-b") }},
		{name: "session permissions", mutate: func(s *session.Session) {
			s.Permissions = &session.PermissionsConfig{Deny: []string{"shell"}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, dir := newRegHarness(t)
			peer := addRegPeer(t, h, dir, nil, nil)
			var made []*fakeReviewProvider

			installRegFactory(h, func() gitreview.Provider {
				p := &fakeReviewProvider{}
				if len(made) == 0 {
					p = h.provider
				}
				made = append(made, p)
				return p
			})

			h.pump(browseRequest(h, "s1", sidebar.GitShowChanges))
			switchRegWorkspace(t, h, "s2")
			h.pump(browseRequest(h, "s2", sidebar.GitShowChanges))
			require.Len(t, made, 1, "the two equal-scope workspaces share one provider")
			shared := h.m.gitReviews["s1"].scopeKey
			require.Equal(t, shared, h.m.gitReviews["s2"].scopeKey)

			tc.mutate(peer)
			h.pump(browseRequest(h, "s2", sidebar.GitShowChanges))
			assert.Equal(t, 1, h.provider.snapshots, "the changed peer never reads through the old shared provider")
			require.Len(t, made, 2, "the changed peer bootstraps a provider of its own")
			peerWS := h.m.gitReviews["s2"]
			assert.NotEqual(t, shared, peerWS.scopeKey, "the changed peer leaves the shared scope")
			require.NotNil(t, peerWS.snapshot)
			peerGeneration := peerWS.snapshot.Generation

			switchRegWorkspace(t, h, "s1")
			h.pump(browseRequest(h, "s1", sidebar.GitRefresh))
			assert.Equal(t, 2, h.provider.snapshots, "the owner refreshes its own provider")
			assert.Equal(t, 1, made[1].snapshots, "the owner's refresh never scans the changed peer's provider")
			assert.Equal(t, peerGeneration, peerWS.snapshot.Generation, "the changed peer never receives the owner's fanout")
		})
	}
}

// A document read before a newer refresh must not replace the newer content the peer shows.
// The peer's own Changes model is read directly, so focus on another page cannot hide it.
func TestDelayedDocumentFromSupersededGenerationIsDropped(t *testing.T) {
	h, dir := newRegHarness(t)
	addRegPeer(t, h, dir, nil, nil)
	installRegFactory(h, func() gitreview.Provider { return regVersionedProvider{h.provider} })
	h.pump(browseRequest(h, "s1", sidebar.GitShowChanges))

	switchRegWorkspace(t, h, "s2")
	h.pump(browseRequest(h, "s2", sidebar.GitShowChanges))
	_, cmd := h.m.Update(sidebar.GitRequestMsg{Owner: "s2", Action: sidebar.GitOpen, WorktreeID: "w1", Path: "Dockerfile.dev", Group: view.GroupUnstaged})
	h.pump(cmd)
	peer := h.m.gitReviews["s2"]
	require.Contains(t, regRendered(peer), "doc-gen-1")

	switchRegWorkspace(t, h, "s1")
	var held []gitReviewPeerContent
	regDeliver(h, regCollect(browseRequest(h, "s1", sidebar.GitRefresh)), &held)
	require.NotEmpty(t, held, "the first refresh produced peer content that has not been delivered")
	h.pump(browseRequest(h, "s1", sidebar.GitRefresh))
	require.Contains(t, regRendered(peer), "doc-gen-3", "the newest refresh reached the open document")

	for _, content := range held {
		h.pump(func() tea.Msg { return content })
	}
	frame := regRendered(peer)
	assert.Contains(t, frame, "doc-gen-3", "superseded document content never replaces the newer one")
	assert.NotContains(t, frame, "doc-gen-2")
}

// regRendered is what a workspace's own Changes model shows, independent of the focused page.
func regRendered(ws *gitReviewWorkspace) string {
	return ansi.Strip(ws.current().View())
}

func TestInactivePeerPolicyChangeRejectsFanoutWithoutReopen(t *testing.T) {
	for _, delayed := range []bool{false, true} {
		t.Run(fmt.Sprintf("delayed=%t", delayed), func(t *testing.T) {
			h, dir := newRegHarness(t)
			sess := addRegPeer(t, h, dir, nil, nil)
			installRegFactory(h, func() gitreview.Provider { return regVersionedProvider{h.provider} })
			h.pump(browseRequest(h, "s1", sidebar.GitShowChanges))
			switchRegWorkspace(t, h, "s2")
			h.pump(browseRequest(h, "s2", sidebar.GitShowChanges))
			_, cmd := h.m.Update(sidebar.GitRequestMsg{Owner: "s2", Action: sidebar.GitOpen, WorktreeID: "w1", Path: "Dockerfile.dev", Group: view.GroupUnstaged})
			h.pump(cmd)
			peer := h.m.gitReviews["s2"]
			require.Contains(t, regRendered(peer), "doc-gen-1")
			require.NoError(t, peer.draft.SetGeneral("keep this draft"))
			switchRegWorkspace(t, h, "s1")

			var held []gitReviewPeerContent
			if delayed {
				regDeliver(h, regCollect(browseRequest(h, "s1", sidebar.GitRefresh)), &held)
				require.NotEmpty(t, held)
			}
			sess.SetAttribute(gitReviewSourceAttribute, "new-policy-source")
			if delayed {
				for _, content := range held {
					h.pump(func() tea.Msg { return content })
				}
			} else {
				h.pump(browseRequest(h, "s1", sidebar.GitRefresh))
			}
			assert.Empty(t, peer.scopeKey, "an inactive changed peer leaves the old sharing scope without reopening")
			assert.Nil(t, peer.provider)
			assert.Nil(t, peer.snapshot)
			assert.Empty(t, peer.models, "old-scope document content is discarded")
			assert.Empty(t, peer.directories)
			assert.Equal(t, "keep this draft", peer.draft.Snapshot().General)
			require.NotNil(t, h.m.gitReviews["s1"].snapshot, "the unchanged owner retains its data")
		})
	}
}
