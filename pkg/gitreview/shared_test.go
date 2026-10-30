package gitreview

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sharedFake counts inner provider calls. Gated calls block until their gate
// closes or their context is canceled.
type sharedFake struct {
	mu         sync.Mutex
	gen        uint64
	full       bool
	sameGen    bool // full scans and browses report the current generation
	ignoreCtx  bool // gated calls ignore cancellation, like a misbehaving provider
	version    int  // listing and document content version
	counts     map[string]int
	snapGate   chan struct{}
	browseGate chan struct{}
	loadGate   chan struct{}
	dirGate    chan struct{}
	docGate    chan struct{}
	active     int
	peak       int
	entered    chan string
	canceled   chan string
}

func (f *sharedFake) activeNow() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active
}

func (f *sharedFake) peakActive() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peak
}

func newSharedFake() *sharedFake {
	return &sharedFake{counts: map[string]int{}, version: 1, entered: make(chan string, 64), canceled: make(chan string, 64)}
}

func (f *sharedFake) advanceVersion() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.version++
}

func (f *sharedFake) count(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[op]
}

func (f *sharedFake) enter(ctx context.Context, op string, gate <-chan struct{}) error {
	if gate == nil {
		return ctx.Err()
	}
	if f.ignoreCtx {
		select {
		case f.entered <- op:
		default:
		}
		<-gate
		return nil
	}
	select {
	case f.entered <- op:
	default:
	}
	select {
	case <-gate:
		return nil
	case <-ctx.Done():
		select {
		case f.canceled <- op:
		default:
		}
		return ctx.Err()
	}
}

func sharedInventory(browse bool) Snapshot {
	wts := []Worktree{{ID: "a", Path: "a", Branch: "main", Current: true}}
	if !browse {
		wts = append(wts, Worktree{ID: "b", Path: "b", Branch: "dev"})
	}
	return Snapshot{Browse: browse, Scope: Scope{Label: "ws"}, Repositories: []Repository{{ID: "repo", Name: "repo", Kind: KindPrimary, Worktrees: wts}}}
}

func (f *sharedFake) Snapshot(ctx context.Context) (Snapshot, error) {
	f.mu.Lock()
	f.counts["snapshot"]++
	gate := f.snapGate
	f.mu.Unlock()
	if err := f.enter(ctx, "snapshot", gate); err != nil {
		return Snapshot{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.sameGen {
		f.gen++
	}
	f.full = true
	snap := sharedInventory(false)
	snap.Generation = f.gen
	return snap, nil
}

func (f *sharedFake) BrowseRoot(ctx context.Context) (Snapshot, error) {
	f.mu.Lock()
	f.counts["browse"]++
	gate := f.browseGate
	f.mu.Unlock()
	if err := f.enter(ctx, "browse", gate); err != nil {
		return Snapshot{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.full {
		// A gated browse that finishes after a full scan reports root-only, like a late result.
		snap := sharedInventory(gate != nil)
		snap.Generation = f.gen
		return snap, nil
	}
	if !f.sameGen {
		f.gen++
	}
	snap := sharedInventory(true)
	snap.Generation = f.gen
	return snap, nil
}

func (f *sharedFake) ContinueDiscovery(context.Context) (Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts["continue"]++
	f.gen++
	snap := sharedInventory(false)
	snap.Generation = f.gen
	return snap, nil
}

func (f *sharedFake) CanContinueDiscovery() bool { return true }

func (f *sharedFake) LoadWorktree(ctx context.Context, id string) (Worktree, error) {
	f.mu.Lock()
	f.counts["load:"+id]++
	gate, version := f.loadGate, f.version
	f.mu.Unlock()
	if err := f.enter(ctx, "load:"+id, gate); err != nil {
		return Worktree{}, err
	}
	return sharedWorktree(id, version), nil
}

func (f *sharedFake) RefreshWorktree(_ context.Context, id string) (Worktree, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts["refresh:"+id]++
	return sharedWorktree(id, f.version), nil
}

func sharedWorktree(id string, version int) Worktree {
	return Worktree{ID: id, Path: id, State: "clean", Files: []FileChange{{Path: fmt.Sprintf("%s-v%d.txt", id, version), Group: GroupUnstaged, Status: "M"}}}
}

func (f *sharedFake) Directory(ctx context.Context, worktreeID, path string) (Directory, error) {
	f.mu.Lock()
	f.counts["dir:"+worktreeID]++
	gate, version := f.dirGate, f.version
	f.mu.Unlock()
	if err := f.enter(ctx, "dir:"+worktreeID, gate); err != nil {
		return Directory{}, err
	}
	return Directory{WorktreeID: worktreeID, Path: path, State: StateReady, Entries: []DirectoryEntry{{Name: fmt.Sprintf("v%d", version)}}}, nil
}

func (f *sharedFake) Document(ctx context.Context, key DocumentKey) (Document, error) {
	f.mu.Lock()
	f.counts["doc:"+key.WorktreeID]++
	gate, version := f.docGate, f.version
	f.active++
	f.peak = max(f.peak, f.active)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()
	if err := f.enter(ctx, "doc:"+key.WorktreeID, gate); err != nil {
		return Document{}, err
	}
	return Document{Key: key, State: StateReady, Lines: []DocumentLine{{Kind: DiffAdd, New: 1, Text: fmt.Sprintf("v%d", version)}}}, nil
}

// editableFake adds the local-only capability.
type editableFake struct{ *sharedFake }

func (e editableFake) EditablePath(worktreeID, path string) (string, error) {
	return "/workspace/" + worktreeID + "/" + path, nil
}

func waitSharedCall(t *testing.T, f *sharedFake, op string) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case got := <-f.entered:
			if got == op {
				return
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s to start", op)
		}
	}
}

func waitSharedCanceled(t *testing.T, f *sharedFake, op string) {
	t.Helper()
	select {
	case got := <-f.canceled:
		require.Equal(t, op, got)
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s to observe cancellation", op)
	}
}

// sharedWaiters returns the waiter count of the newest unfinished flight for mode, or -1.
func sharedWaiters(s *Shared, mode invMode) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.latest[mode]
	if f == nil || isClosed(f.done) {
		return -1
	}
	return f.waiters
}

func sharedDocFlights(s *Shared) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.docFlights)
}

func TestSharedEnsureSnapshotUpgradesBrowseAndReusesInventory(t *testing.T) {
	f := newSharedFake()
	s := NewShared(f, SharedOptions{})
	t.Cleanup(s.Close)

	browse, err := s.EnsureSnapshot(t.Context(), true)
	require.NoError(t, err)
	require.True(t, browse.Browse)
	require.Len(t, browse.Repositories[0].Worktrees, 1)
	_, err = s.EnsureSnapshot(t.Context(), true)
	require.NoError(t, err)
	require.Equal(t, 1, f.count("browse"), "a cached browse is reused")

	full, err := s.EnsureSnapshot(t.Context(), false)
	require.NoError(t, err)
	require.False(t, full.Browse, "a root-only inventory is upgraded to a full scan")
	require.Len(t, full.Repositories[0].Worktrees, 2)
	require.Equal(t, 1, f.count("snapshot"))

	_, err = s.EnsureSnapshot(t.Context(), false)
	require.NoError(t, err)
	reused, err := s.EnsureSnapshot(t.Context(), true)
	require.NoError(t, err)
	require.False(t, reused.Browse, "browse requests keep a cached full inventory")
	require.Equal(t, 1, f.count("snapshot"))
	require.Equal(t, 1, f.count("browse"))

	cur, ok := s.CurrentSnapshot()
	require.True(t, ok)
	require.Equal(t, full.Generation, cur.Generation)
}

func TestSharedSnapshotCoalescesConcurrentCallers(t *testing.T) {
	f := newSharedFake()
	f.snapGate = make(chan struct{})
	s := NewShared(f, SharedOptions{})
	t.Cleanup(s.Close)

	const callers = 8
	results := make(chan Snapshot, callers)
	errs := make(chan error, callers)
	for range callers {
		go func() {
			snap, err := s.Snapshot(t.Context())
			results <- snap
			errs <- err
		}()
	}
	require.Eventually(t, func() bool { return sharedWaiters(s, invFull) == callers }, 5*time.Second, time.Millisecond)
	close(f.snapGate)
	for range callers {
		require.NoError(t, <-errs)
		assert.Equal(t, uint64(1), (<-results).Generation)
	}
	require.Equal(t, 1, f.count("snapshot"))
}

func TestSharedWaiterCancelKeepsFlightUntilLastWaiterLeaves(t *testing.T) {
	f := newSharedFake()
	f.snapGate = make(chan struct{})
	s := NewShared(f, SharedOptions{})
	t.Cleanup(s.Close)

	ctxA, cancelA := context.WithCancel(t.Context())
	defer cancelA()
	ctxB, cancelB := context.WithCancel(t.Context())
	defer cancelB()
	errA := make(chan error, 1)
	errB := make(chan error, 1)
	go func() { _, err := s.Snapshot(ctxA); errA <- err }()
	go func() { _, err := s.Snapshot(ctxB); errB <- err }()
	require.Eventually(t, func() bool { return sharedWaiters(s, invFull) == 2 }, 5*time.Second, time.Millisecond)

	cancelA()
	require.ErrorIs(t, <-errA, context.Canceled)
	select {
	case op := <-f.canceled:
		t.Fatalf("one waiter canceled the shared flight: %s", op)
	default:
	}
	require.Equal(t, 1, sharedWaiters(s, invFull))

	cancelB()
	require.ErrorIs(t, <-errB, context.Canceled)
	waitSharedCanceled(t, f, "snapshot")
	_, ok := s.CurrentSnapshot()
	require.False(t, ok, "a canceled inventory flight publishes nothing")
}

func TestSharedBrowseDoesNotWaitForPeerFullScan(t *testing.T) {
	f := newSharedFake()
	f.snapGate = make(chan struct{})
	s := NewShared(f, SharedOptions{})
	t.Cleanup(s.Close)

	peerErr := make(chan error, 1)
	peerSnap := make(chan Snapshot, 1)
	go func() {
		snap, err := s.Snapshot(t.Context())
		peerSnap <- snap
		peerErr <- err
	}()
	waitSharedCall(t, f, "snapshot")

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	browse, err := s.EnsureSnapshot(ctx, true)
	require.NoError(t, err, "Files must not wait behind a peer's full scan")
	require.True(t, browse.Browse)

	close(f.snapGate)
	require.NoError(t, <-peerErr)
	require.False(t, (<-peerSnap).Browse, "the peer's full scan survives and publishes")
	select {
	case op := <-f.canceled:
		t.Fatalf("browse canceled the peer scan: %s", op)
	default:
	}
	cur, ok := s.CurrentSnapshot()
	require.True(t, ok)
	require.False(t, cur.Browse)
}

func TestSharedLateBrowseNeverReplacesFullInventory(t *testing.T) {
	f := newSharedFake()
	f.browseGate = make(chan struct{})
	s := NewShared(f, SharedOptions{})
	t.Cleanup(s.Close)

	browseErr := make(chan error, 1)
	browseSnap := make(chan Snapshot, 1)
	go func() {
		snap, err := s.EnsureSnapshot(t.Context(), true)
		browseSnap <- snap
		browseErr <- err
	}()
	waitSharedCall(t, f, "browse")

	full, err := s.Snapshot(t.Context())
	require.NoError(t, err)
	require.False(t, full.Browse)

	close(f.browseGate)
	require.NoError(t, <-browseErr)
	require.False(t, (<-browseSnap).Browse, "a late root-only result returns the full inventory")
	cur, ok := s.CurrentSnapshot()
	require.True(t, ok)
	require.False(t, cur.Browse)
	require.Equal(t, full.Generation, cur.Generation)
}

func TestSharedEqualGenerationBrowseIsUpgradedByFullScan(t *testing.T) {
	f := newSharedFake()
	f.gen, f.sameGen = 1, true
	s := NewShared(f, SharedOptions{})
	t.Cleanup(s.Close)

	browse, err := s.EnsureSnapshot(t.Context(), true)
	require.NoError(t, err)
	require.True(t, browse.Browse)
	full, err := s.EnsureSnapshot(t.Context(), false)
	require.NoError(t, err)
	require.False(t, full.Browse, "an equal generation must not hide the upgrade")
	require.Equal(t, browse.Generation, full.Generation)
	require.Equal(t, 1, f.count("snapshot"))
}

func TestSharedBrowseRootRefreshesFilesWithoutScanningGit(t *testing.T) {
	f := newSharedFake()
	s := NewShared(f, SharedOptions{})
	t.Cleanup(s.Close)
	_, err := s.Snapshot(t.Context())
	require.NoError(t, err)

	entry := func() string {
		t.Helper()
		d, err := s.Directory(t.Context(), "a", ".")
		require.NoError(t, err)
		return d.Entries[0].Name
	}
	doc := func() string {
		t.Helper()
		d, err := s.Document(t.Context(), DocumentKey{Mode: "files", WorktreeID: "a", Path: "main.go"})
		require.NoError(t, err)
		return d.Lines[0].Text
	}
	require.Equal(t, "v1", entry())
	require.Equal(t, "v1", doc())

	f.advanceVersion()
	require.Equal(t, "v1", entry(), "cached listing is reused until an explicit refresh")
	require.Equal(t, "v1", doc())

	_, err = s.EnsureSnapshot(t.Context(), true)
	require.NoError(t, err)
	require.Equal(t, "v1", entry(), "attach-style browse reuses the cache")

	root, err := s.BrowseRoot(t.Context())
	require.NoError(t, err)
	require.False(t, root.Browse, "the explicit refresh keeps the full inventory")
	require.Equal(t, "v2", entry())
	require.Equal(t, "v2", doc())
	require.Equal(t, 2, f.count("dir:a"))
	require.Equal(t, 1, f.count("browse"), "BrowseRoot re-reads the root without a Git scan")
	require.Equal(t, 1, f.count("snapshot"))

	_, err = s.EnsureSnapshot(t.Context(), true)
	require.NoError(t, err)
	require.Equal(t, "v2", entry())
	require.Equal(t, 2, f.count("dir:a"))
	require.Equal(t, 1, f.count("browse"), "attach reuses the cached inventory")
	require.Equal(t, 1, f.count("snapshot"))
}

func TestSharedDirectoryReadSupersededByBrowseRootIsRejected(t *testing.T) {
	f := newSharedFake()
	f.dirGate = make(chan struct{})
	s := NewShared(f, SharedOptions{})
	t.Cleanup(s.Close)
	_, err := s.Snapshot(t.Context())
	require.NoError(t, err)

	dirErr := make(chan error, 1)
	go func() { _, err := s.Directory(t.Context(), "a", "."); dirErr <- err }()
	waitSharedCall(t, f, "dir:a")

	_, err = s.BrowseRoot(t.Context())
	require.NoError(t, err)
	require.ErrorIs(t, <-dirErr, ErrStale)

	close(f.dirGate)
	d, err := s.Directory(t.Context(), "a", ".")
	require.NoError(t, err)
	require.Len(t, d.Entries, 1)
	require.Equal(t, 2, f.count("dir:a"), "the superseded listing was not cached")
}

func TestSharedCanceledScanIgnoringContextNeverPublishes(t *testing.T) {
	f := newSharedFake()
	f.snapGate = make(chan struct{})
	f.ignoreCtx = true
	s := NewShared(f, SharedOptions{})
	t.Cleanup(s.Close)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errs := make(chan error, 1)
	go func() { _, err := s.Snapshot(ctx); errs <- err }()
	waitSharedCall(t, f, "snapshot")
	s.mu.Lock()
	flight := s.latest[invFull]
	s.mu.Unlock()

	cancel()
	require.ErrorIs(t, <-errs, context.Canceled)
	close(f.snapGate)
	<-flight.done
	_, ok := s.CurrentSnapshot()
	require.False(t, ok, "a canceled scan that returns successfully must not publish")

	_, err := s.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, 2, f.count("snapshot"))
}

func TestSharedRefreshWorktreeInvalidatesOnlyItsCaches(t *testing.T) {
	f := newSharedFake()
	s := NewShared(f, SharedOptions{})
	t.Cleanup(s.Close)
	_, err := s.Snapshot(t.Context())
	require.NoError(t, err)

	read := func(id string) {
		t.Helper()
		_, err := s.Document(t.Context(), DocumentKey{Mode: "files", WorktreeID: id, Path: "main.go"})
		require.NoError(t, err)
		_, err = s.Directory(t.Context(), id, ".")
		require.NoError(t, err)
		_, err = s.LoadWorktree(t.Context(), id)
		require.NoError(t, err)
	}
	read("a")
	read("b")
	read("a")
	read("b")
	require.Equal(t, 1, f.count("doc:a"))
	require.Equal(t, 1, f.count("doc:b"))
	require.Equal(t, 1, f.count("dir:a"))
	require.Equal(t, 1, f.count("dir:b"))
	require.Equal(t, 2, f.count("load:a"), "status reads reach the provider on every explicit read")
	require.Equal(t, 2, f.count("load:b"))

	_, err = s.RefreshWorktree(t.Context(), "a")
	require.NoError(t, err)
	read("a")
	read("b")
	require.Equal(t, 1, f.count("refresh:a"))
	require.Equal(t, 2, f.count("doc:a"), "refreshed worktree re-reads its documents")
	require.Equal(t, 2, f.count("dir:a"), "refreshed worktree re-reads its directories")
	require.Equal(t, 3, f.count("load:a"))
	require.Equal(t, 1, f.count("doc:b"), "other worktrees keep their caches")
	require.Equal(t, 1, f.count("dir:b"))
	require.Equal(t, 3, f.count("load:b"))
}

func TestSharedRefreshSupersedesInFlightLoad(t *testing.T) {
	f := newSharedFake()
	f.loadGate = make(chan struct{})
	s := NewShared(f, SharedOptions{})
	t.Cleanup(s.Close)
	_, err := s.Snapshot(t.Context())
	require.NoError(t, err)

	loadErr := make(chan error, 1)
	go func() { _, err := s.LoadWorktree(t.Context(), "a"); loadErr <- err }()
	waitSharedCall(t, f, "load:a")

	_, err = s.RefreshWorktree(t.Context(), "a")
	require.NoError(t, err)
	require.ErrorIs(t, <-loadErr, ErrStale)
	waitSharedCanceled(t, f, "load:a")

	close(f.loadGate)
	_, err = s.LoadWorktree(t.Context(), "a")
	require.NoError(t, err)
	require.Equal(t, 2, f.count("load:a"), "the superseded load is not reused; the next read reaches the provider")
}

func TestSharedDocumentCacheIsBoundedAndIsolated(t *testing.T) {
	f := newSharedFake()
	s := NewShared(f, SharedOptions{MaxEntries: 2})
	t.Cleanup(s.Close)
	_, err := s.Snapshot(t.Context())
	require.NoError(t, err)

	doc := func(path string) Document {
		t.Helper()
		d, err := s.Document(t.Context(), DocumentKey{Mode: "files", WorktreeID: "a", Path: path})
		require.NoError(t, err)
		return d
	}
	doc("p1")
	doc("p2")
	doc("p3")
	doc("p3")
	require.Equal(t, 3, f.count("doc:a"), "the least recently used entry was evicted")
	doc("p1")
	require.Equal(t, 4, f.count("doc:a"))

	got := doc("p1")
	got.Lines[0].Text = "mutated"
	require.Equal(t, "v1", doc("p1").Lines[0].Text, "callers cannot mutate cached documents")
	require.Equal(t, 4, f.count("doc:a"))
}

func TestSharedCloseCancelsInFlightWorkAndRejectsLaterCalls(t *testing.T) {
	f := newSharedFake()
	f.snapGate = make(chan struct{})
	s := NewShared(f, SharedOptions{})
	errs := make(chan error, 1)
	go func() { _, err := s.Snapshot(t.Context()); errs <- err }()
	waitSharedCall(t, f, "snapshot")

	s.Close()
	require.ErrorIs(t, <-errs, ErrUnavailable)
	waitSharedCanceled(t, f, "snapshot")

	_, err := s.Snapshot(t.Context())
	require.ErrorIs(t, err, ErrUnavailable)
	_, err = s.Document(t.Context(), DocumentKey{Mode: "files", WorktreeID: "a", Path: "x"})
	require.ErrorIs(t, err, ErrUnavailable)
	_, ok := s.CurrentSnapshot()
	require.False(t, ok)
}

func TestSharedContinueDiscoveryRejectsInventoryChangedBeforeItRuns(t *testing.T) {
	f := newSharedFake()
	s := NewShared(f, SharedOptions{})
	t.Cleanup(s.Close)
	_, err := s.Snapshot(t.Context())
	require.NoError(t, err)

	f.snapGate = make(chan struct{})
	snapErr := make(chan error, 1)
	go func() { _, err := s.Snapshot(t.Context()); snapErr <- err }()
	waitSharedCall(t, f, "snapshot")

	contErr := make(chan error, 1)
	go func() { _, err := s.ContinueDiscovery(t.Context()); contErr <- err }()
	require.Eventually(t, func() bool { return sharedWaiters(s, invContinue) == 1 }, 5*time.Second, time.Millisecond)
	close(f.snapGate)
	require.NoError(t, <-snapErr)
	require.ErrorIs(t, <-contErr, ErrStale)
	require.Equal(t, 0, f.count("continue"))
}

func TestSharedOptionalCapabilitiesAreForwardedOnlyWhenImplemented(t *testing.T) {
	f := newSharedFake()

	local := NewShared(editableFake{f}, SharedOptions{})
	t.Cleanup(local.Close)
	require.Equal(t, Capabilities{Browse: true, ContinueDiscovery: true, RefreshWorktree: true, LocalEditor: true}, local.Capabilities())
	editor, ok := local.LocalEditor()
	require.True(t, ok)
	path, err := editor.EditablePath("a", "main.go")
	require.NoError(t, err)
	require.Equal(t, "/workspace/a/main.go", path)

	remote := NewShared(remoteProvider{Provider: f}, SharedOptions{})
	t.Cleanup(remote.Close)
	require.Equal(t, Capabilities{}, remote.Capabilities())
	_, ok = remote.LocalEditor()
	require.False(t, ok)
	_, ok = any(remote).(LocalEditor)
	require.False(t, ok, "remote providers must not expose local editing through the wrapper")
	require.False(t, remote.CanContinueDiscovery())
	_, err = remote.ContinueDiscovery(t.Context())
	require.ErrorIs(t, err, ErrUnavailable)
	_, err = remote.EnsureSnapshot(t.Context(), true)
	require.ErrorIs(t, err, ErrUnavailable, "root-only browse is unavailable without BrowseRoot")
	full, err := remote.EnsureSnapshot(t.Context(), false)
	require.NoError(t, err)
	require.False(t, full.Browse)
}

// remoteProvider hides every optional method of the wrapped provider.
type remoteProvider struct{ Provider }

func TestSharedAlternatingScanRequestsShareOneQueuedScan(t *testing.T) {
	f := newSharedFake()
	f.snapGate = make(chan struct{})
	s := NewShared(f, SharedOptions{})
	t.Cleanup(s.Close)

	snapErrs := make(chan error, 6)
	contErrs := make(chan error, 6)
	startSnap := func() {
		go func() { _, err := s.Snapshot(t.Context()); snapErrs <- err }()
	}
	startCont := func() {
		go func() { _, err := s.ContinueDiscovery(t.Context()); contErrs <- err }()
	}

	startSnap()
	waitSharedCall(t, f, "snapshot")
	startCont()
	require.Eventually(t, func() bool { return sharedWaiters(s, invContinue) == 1 }, 5*time.Second, time.Millisecond)
	for range 5 {
		startSnap()
		startCont()
	}
	require.Eventually(t, func() bool {
		return sharedWaiters(s, invFull) == 6 && sharedWaiters(s, invContinue) == 6
	}, 5*time.Second, time.Millisecond)
	close(f.snapGate)
	for range 6 {
		require.NoError(t, <-snapErrs)
	}
	for range 6 {
		require.ErrorIs(t, <-contErrs, ErrStale, "the queued continuation was superseded by the scan ahead of it")
	}
	require.Equal(t, 1, f.count("snapshot"), "alternating requests join; they do not queue more scans")
	require.Equal(t, 0, f.count("continue"))
}

func TestSharedContentReadsAreBoundedConcurrency(t *testing.T) {
	f := newSharedFake()
	f.docGate = make(chan struct{})
	s := NewShared(f, SharedOptions{})
	t.Cleanup(s.Close)
	_, err := s.Snapshot(t.Context())
	require.NoError(t, err)

	const reads = 3 * maxConcurrentReads
	errs := make(chan error, reads)
	for i := range reads {
		go func() {
			_, err := s.Document(t.Context(), DocumentKey{Mode: "files", WorktreeID: "a", Path: fmt.Sprintf("p%d", i)})
			errs <- err
		}()
	}
	require.Eventually(t, func() bool { return f.activeNow() == maxConcurrentReads }, 5*time.Second, time.Millisecond)
	close(f.docGate)
	for range reads {
		require.NoError(t, <-errs)
	}
	require.Equal(t, maxConcurrentReads, f.peakActive(), "inner reads never exceed the concurrency bound")
}

func TestSharedPendingReadsAreBounded(t *testing.T) {
	f := newSharedFake()
	f.docGate = make(chan struct{})
	s := NewShared(f, SharedOptions{})
	t.Cleanup(s.Close)
	_, err := s.Snapshot(t.Context())
	require.NoError(t, err)

	errs := make(chan error, maxPendingReads)
	for i := range maxPendingReads {
		go func() {
			_, err := s.Document(t.Context(), DocumentKey{Mode: "files", WorktreeID: "a", Path: fmt.Sprintf("p%d", i)})
			errs <- err
		}()
	}
	require.Eventually(t, func() bool { return sharedDocFlights(s) == maxPendingReads }, 5*time.Second, time.Millisecond)
	_, err = s.Document(t.Context(), DocumentKey{Mode: "files", WorktreeID: "a", Path: "overflow"})
	require.ErrorIs(t, err, ErrUnavailable, "distinct reads beyond the pending bound fail fast")
	close(f.docGate)
	for range maxPendingReads {
		require.NoError(t, <-errs)
	}
}

// Real Local: the explicit Files refresh must see a rewritten file, without a Git scan.
func TestSharedBrowseRootReadsLocalFilesWithoutGitScan(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "notes.txt")
	require.NoError(t, os.WriteFile(path, []byte("A\n"), 0o600))
	local, err := NewLocal(root)
	require.NoError(t, err)
	s := NewShared(local, SharedOptions{})
	t.Cleanup(s.Close)

	browse, err := s.BrowseRoot(t.Context())
	require.NoError(t, err)
	require.True(t, browse.Browse)
	key := DocumentKey{Mode: "files", WorktreeID: browse.Repositories[0].Worktrees[0].ID, Path: "notes.txt"}
	doc, err := s.Document(t.Context(), key)
	require.NoError(t, err)
	require.Equal(t, "A", doc.Lines[0].Text)
	generation := local.CurrentSnapshot().Generation

	require.NoError(t, os.WriteFile(path, []byte("B\n"), 0o600))
	_, err = s.BrowseRoot(t.Context())
	require.NoError(t, err)
	doc, err = s.Document(t.Context(), key)
	require.NoError(t, err)
	require.Equal(t, "B", doc.Lines[0].Text, "the explicit Files refresh drops the provider's cached document")
	require.Equal(t, generation, local.CurrentSnapshot().Generation, "no inventory publish or Git scan")
}

// Real Local: concurrent scans and root reads must leave the wrapper and Local on the same full inventory.
func TestSharedLocalOverlapKeepsGenerationsAligned(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	local, err := NewLocal(root)
	require.NoError(t, err)
	s := NewShared(local, SharedOptions{})
	t.Cleanup(s.Close)

	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for range 8 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			_, err := s.Snapshot(t.Context())
			errs <- err
		}()
		go func() {
			defer wg.Done()
			_, err := s.EnsureSnapshot(t.Context(), true)
			errs <- err
		}()
		go func() {
			defer wg.Done()
			_, err := s.BrowseRoot(t.Context())
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	cur, ok := s.CurrentSnapshot()
	require.True(t, ok)
	require.False(t, cur.Browse)
	inner := local.CurrentSnapshot()
	require.False(t, inner.Browse)
	require.Equal(t, inner.Generation, cur.Generation)
}

func TestLocalCanceledScanDoesNotPublish(t *testing.T) {
	local, err := NewLocal(t.TempDir())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = local.scanSnapshot(ctx, baseDiscoveryLimits())
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, local.CurrentSnapshot().Generation)
}

// onlyDocFlight returns the single pending document flight. Caller holds s.mu.
func onlyDocFlight(s *Shared) *sharedFlight[Document] {
	for _, f := range s.docFlights {
		return f
	}
	return nil
}

// A provider that ignores cancellation must not overwrite a newer result for the same key.
func TestSharedCanceledDocumentFlightCannotOverwriteNewerResult(t *testing.T) {
	f := newSharedFake()
	f.docGate = make(chan struct{})
	f.ignoreCtx = true
	s := NewShared(f, SharedOptions{})
	t.Cleanup(s.Close)
	_, err := s.Snapshot(t.Context())
	require.NoError(t, err)

	key := DocumentKey{Mode: "files", WorktreeID: "a", Path: "main.go"}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	oldErr := make(chan error, 1)
	go func() { _, err := s.Document(ctx, key); oldErr <- err }()
	waitSharedCall(t, f, "doc:a")
	s.mu.Lock()
	oldFlight := onlyDocFlight(s)
	s.mu.Unlock()
	cancel()
	require.ErrorIs(t, <-oldErr, context.Canceled)

	gate := f.docGate
	f.advanceVersion()
	f.mu.Lock()
	f.docGate = nil
	f.mu.Unlock()
	fresh, err := s.Document(t.Context(), key)
	require.NoError(t, err)
	require.Equal(t, "v2", fresh.Lines[0].Text)

	close(gate)
	<-oldFlight.done
	cached, err := s.Document(t.Context(), key)
	require.NoError(t, err)
	require.Equal(t, "v2", cached.Lines[0].Text, "the canceled flight never replaces the newer cache entry")
	require.Equal(t, 2, f.count("doc:a"), "the cached result served the last read")
}

func TestSharedCanceledDirectoryFlightCannotOverwriteNewerResult(t *testing.T) {
	f := newSharedFake()
	f.dirGate = make(chan struct{})
	f.ignoreCtx = true
	s := NewShared(f, SharedOptions{})
	t.Cleanup(s.Close)
	_, err := s.Snapshot(t.Context())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	oldErr := make(chan error, 1)
	go func() { _, err := s.Directory(ctx, "a", "."); oldErr <- err }()
	waitSharedCall(t, f, "dir:a")
	s.mu.Lock()
	var oldFlight *sharedFlight[Directory]
	for _, df := range s.dirFlights {
		oldFlight = df
	}
	s.mu.Unlock()
	cancel()
	require.ErrorIs(t, <-oldErr, context.Canceled)

	gate := f.dirGate
	f.advanceVersion()
	f.mu.Lock()
	f.dirGate = nil
	f.mu.Unlock()
	fresh, err := s.Directory(t.Context(), "a", ".")
	require.NoError(t, err)
	require.Equal(t, "v2", fresh.Entries[0].Name)

	close(gate)
	<-oldFlight.done
	cached, err := s.Directory(t.Context(), "a", ".")
	require.NoError(t, err)
	require.Equal(t, "v2", cached.Entries[0].Name, "the canceled listing never replaces the newer cache entry")
	require.Equal(t, 2, f.count("dir:a"))
}

func TestSharedCanceledWorktreeFlightCannotOverwriteNewerStatus(t *testing.T) {
	f := newSharedFake()
	f.loadGate = make(chan struct{})
	f.ignoreCtx = true
	s := NewShared(f, SharedOptions{})
	t.Cleanup(s.Close)
	_, err := s.Snapshot(t.Context())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	oldErr := make(chan error, 1)
	go func() { _, err := s.LoadWorktree(ctx, "a"); oldErr <- err }()
	waitSharedCall(t, f, "load:a")
	s.mu.Lock()
	var oldFlight *sharedFlight[Worktree]
	for _, wf := range s.wtFlights {
		oldFlight = wf
	}
	s.mu.Unlock()
	cancel()
	require.ErrorIs(t, <-oldErr, context.Canceled)

	gate := f.loadGate
	f.advanceVersion()
	f.mu.Lock()
	f.loadGate = nil
	f.mu.Unlock()
	fresh, err := s.LoadWorktree(t.Context(), "a")
	require.NoError(t, err)
	require.Equal(t, "a-v2.txt", fresh.Files[0].Path)

	close(gate)
	<-oldFlight.done
	cur, ok := s.CurrentSnapshot()
	require.True(t, ok)
	require.Equal(t, "a-v2.txt", cur.Repositories[0].Worktrees[0].Files[0].Path, "the canceled status never patches the inventory")
}

// Concurrent attach calls on an empty cache share one scan. A caller that checked
// before the scan published must return the cached inventory, not start another scan.
func TestSharedEnsureSnapshotNeverRescansAfterConcurrentPublish(t *testing.T) {
	for round := range 200 {
		f := newSharedFake()
		s := NewShared(f, SharedOptions{})
		var wg sync.WaitGroup
		errs := make(chan error, 32)
		for range 32 {
			wg.Go(func() {
				_, err := s.EnsureSnapshot(t.Context(), false)
				errs <- err
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
		require.Equal(t, 1, f.count("snapshot"), "round %d", round)
		s.Close()
	}
}
