package gitreview

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
)

// ErrStale reports a result superseded by a newer inventory or worktree revision.
var ErrStale = errors.New("git review result is stale")

var (
	errSharedClosed = fmt.Errorf("%w: git review provider is closed", ErrUnavailable)
	errTooManyScans = fmt.Errorf("%w: git review scans are already queued", ErrUnavailable)
	errTooManyReads = fmt.Errorf("%w: too many git review reads are pending", ErrUnavailable)
)

const (
	// maxLaneScans bounds unfinished inventory flights per lane. Joins never add to it.
	maxLaneScans = 3
	// maxConcurrentReads bounds concurrent inner status, directory, and document calls.
	maxConcurrentReads = 4
	// maxPendingReads bounds distinct pending status, directory, and document flights.
	maxPendingReads = 64
)

// DiscoveryContinuer is implemented by providers that can deepen a partial inventory.
type DiscoveryContinuer interface {
	CanContinueDiscovery() bool
	ContinueDiscovery(ctx context.Context) (Snapshot, error)
}

// LocalEditor is implemented only by providers whose files live on this machine.
type LocalEditor interface {
	EditablePath(worktreeID, path string) (string, error)
}

type worktreeRefresher interface {
	RefreshWorktree(ctx context.Context, worktreeID string) (Worktree, error)
}

// documentInvalidator is implemented by providers that cache document bytes.
type documentInvalidator interface {
	InvalidateDocuments()
}

// Capabilities reports optional provider features the wrapper forwards.
type Capabilities struct {
	Browse            bool
	ContinueDiscovery bool
	RefreshWorktree   bool
	LocalEditor       bool
}

// SharedOptions bounds the directory and document caches. Zero fields use defaults.
type SharedOptions struct {
	MaxEntries int // per cache; default 64
	MaxBytes   int // per cache; default MaxCacheBytes / 4
}

func (o SharedOptions) withDefaults() SharedOptions {
	if o.MaxEntries <= 0 {
		o.MaxEntries = 64
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = MaxCacheBytes / 4
	}
	return o
}

// Shared wraps a Provider with one authoritative inventory and bounded listing
// and document caches. Concurrent identical requests share one in-flight call.
// Worktree status is not cached here; the provider decides its own freshness.
type Shared struct {
	inner     Provider
	root      context.Context //nolint:containedctx // shared flights outlive individual waiters; Close cancels all
	stop      context.CancelFunc
	readSlots chan struct{}

	mu     sync.Mutex
	closed bool
	hasInv bool
	inv    Snapshot
	// epoch advances on each inventory publish and keys content results.
	// listEpoch advances on BrowseRoot and keys directory and document results.
	epoch      uint64
	listEpoch  uint64
	laneTail   [2]*invFlight // newest flight per lane; new flights queue behind it
	laneUndone [2]int        // unfinished flights per lane
	latest     [3]*invFlight // newest flight per mode; joinable while unfinished
	rev        map[string]uint64
	documents  *lru[DocumentKey, Document]
	dirs       *lru[dirKey, Directory]
	docFlights map[docFlightKey]*sharedFlight[Document]
	dirFlights map[dirFlightKey]*sharedFlight[Directory]
	wtFlights  map[wtFlightKey]*sharedFlight[Worktree]
}

type invMode int

const (
	invBrowse invMode = iota
	invFull
	invContinue
)

// laneOf keeps root-only browses out of the full-scan queue so Files never waits on a scan.
func laneOf(mode invMode) int {
	if mode == invBrowse {
		return 0
	}
	return 1
}

type dirKey struct{ worktree, path string }

type docFlightKey struct {
	doc              DocumentKey
	epoch, list, rev uint64
}

type dirFlightKey struct {
	dir              dirKey
	epoch, list, rev uint64
}

type wtFlightKey struct {
	id         string
	epoch, rev uint64
}

type sharedFlight[T any] struct {
	done     chan struct{}
	cancel   context.CancelFunc
	waiters  int
	canceled bool
	fresh    bool
	val      T
	err      error
}

type invFlight struct {
	sharedFlight[Snapshot]

	mode  invMode
	epoch uint64
	prev  *invFlight
}

// NewShared wraps p. The wrapper never polls; results change only on explicit
// calls or when the inner provider's generation changes.
func NewShared(p Provider, opts SharedOptions) *Shared {
	opts = opts.withDefaults()
	root, stop := context.WithCancel(context.Background())
	return &Shared{
		inner:      p,
		root:       root,
		stop:       stop,
		readSlots:  make(chan struct{}, maxConcurrentReads),
		rev:        make(map[string]uint64),
		documents:  newLRU[DocumentKey, Document](opts.MaxEntries, opts.MaxBytes),
		dirs:       newLRU[dirKey, Directory](opts.MaxEntries, opts.MaxBytes),
		docFlights: make(map[docFlightKey]*sharedFlight[Document]),
		dirFlights: make(map[dirFlightKey]*sharedFlight[Directory]),
		wtFlights:  make(map[wtFlightKey]*sharedFlight[Worktree]),
	}
}

// Close cancels in-flight work and drops every cache. Later calls fail with ErrUnavailable.
func (s *Shared) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.stop()
	s.hasInv = false
	s.inv = Snapshot{}
	s.laneTail = [2]*invFlight{}
	s.latest = [3]*invFlight{}
	s.resetContentLocked()
}

// CurrentSnapshot returns the cached inventory without starting any work.
func (s *Shared) CurrentSnapshot() (Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasInv {
		return Snapshot{}, false
	}
	return cloneSnapshot(s.inv), true
}

// Snapshot always starts an explicit full refresh unless an identical one is already in flight.
func (s *Shared) Snapshot(ctx context.Context) (Snapshot, error) {
	return s.inventory(ctx, invFull, false)
}

// EnsureSnapshot is the attach path: it returns the cached inventory when it
// satisfies the request. browse=false upgrades a root-only inventory to a full scan.
func (s *Shared) EnsureSnapshot(ctx context.Context, browse bool) (Snapshot, error) {
	if browse {
		return s.inventory(ctx, invBrowse, true)
	}
	return s.inventory(ctx, invFull, true)
}

// BrowseRoot is the explicit Files refresh. It re-reads the root through the provider
// (never a Git scan) and drops cached directory and document results first.
func (s *Shared) BrowseRoot(ctx context.Context) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Snapshot{}, errSharedClosed
	}
	if s.hasInv {
		s.invalidateListingLocked()
	}
	s.mu.Unlock()
	return s.inventory(ctx, invBrowse, false)
}

// CanContinueDiscovery reports whether the inner provider can deepen discovery.
func (s *Shared) CanContinueDiscovery() bool {
	c, ok := s.inner.(DiscoveryContinuer)
	return ok && c.CanContinueDiscovery()
}

// ContinueDiscovery fails with ErrStale if the inventory changes before it runs.
func (s *Shared) ContinueDiscovery(ctx context.Context) (Snapshot, error) {
	if _, ok := s.inner.(DiscoveryContinuer); !ok {
		return Snapshot{}, fmt.Errorf("%w: discovery continuation is unavailable", ErrUnavailable)
	}
	return s.inventory(ctx, invContinue, false)
}

// LoadWorktree coalesces concurrent status reads. Results are not cached here,
// so each explicit read reaches the provider and its own freshness rules.
func (s *Shared) LoadWorktree(ctx context.Context, worktreeID string) (Worktree, error) {
	if err := ctx.Err(); err != nil {
		return Worktree{}, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Worktree{}, errSharedClosed
	}
	key := wtFlightKey{id: worktreeID, epoch: s.epoch, rev: s.rev[worktreeID]}
	f := s.wtFlights[key]
	if f == nil {
		if len(s.wtFlights) >= maxPendingReads {
			s.mu.Unlock()
			return Worktree{}, errTooManyReads
		}
		f = s.startWorktreeLocked(key, false)
	}
	f.waiters++
	s.mu.Unlock()
	return await(s, ctx, f, func() {
		if s.wtFlights[key] == f {
			delete(s.wtFlights, key)
		}
	}, cloneWorktree)
}

// RefreshWorktree invalidates this worktree's documents and directories before
// reading fresh status. Other worktrees keep their caches.
func (s *Shared) RefreshWorktree(ctx context.Context, worktreeID string) (Worktree, error) {
	if err := ctx.Err(); err != nil {
		return Worktree{}, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Worktree{}, errSharedClosed
	}
	if !s.hasWorktreeLocked(worktreeID) {
		s.mu.Unlock()
		return Worktree{}, ErrNotFound
	}
	key := wtFlightKey{id: worktreeID, epoch: s.epoch, rev: s.rev[worktreeID]}
	f := s.wtFlights[key]
	if f == nil || !f.fresh {
		if len(s.wtFlights) >= maxPendingReads {
			s.mu.Unlock()
			return Worktree{}, errTooManyReads
		}
		s.invalidateWorktreeLocked(worktreeID)
		key.rev = s.rev[worktreeID]
		f = s.startWorktreeLocked(key, true)
	}
	f.waiters++
	s.mu.Unlock()
	return await(s, ctx, f, func() {
		if s.wtFlights[key] == f {
			delete(s.wtFlights, key)
		}
	}, cloneWorktree)
}

// Directory returns a cached listing for the worktree's current revision and listing epoch.
func (s *Shared) Directory(ctx context.Context, worktreeID, path string) (Directory, error) {
	if err := ctx.Err(); err != nil {
		return Directory{}, err
	}
	key := dirKey{worktree: worktreeID, path: path}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Directory{}, errSharedClosed
	}
	if dir, ok := s.dirs.get(key); ok {
		s.mu.Unlock()
		return cloneDirectory(dir), nil
	}
	fk := dirFlightKey{dir: key, epoch: s.epoch, list: s.listEpoch, rev: s.rev[worktreeID]}
	f := s.dirFlights[fk]
	if f == nil {
		if len(s.dirFlights) >= maxPendingReads {
			s.mu.Unlock()
			return Directory{}, errTooManyReads
		}
		f = s.startDirectoryLocked(fk)
	}
	f.waiters++
	s.mu.Unlock()
	return await(s, ctx, f, func() {
		if s.dirFlights[fk] == f {
			delete(s.dirFlights, fk)
		}
	}, cloneDirectory)
}

// Document returns a cached document for the worktree's current revision and listing epoch.
func (s *Shared) Document(ctx context.Context, key DocumentKey) (Document, error) {
	if err := ctx.Err(); err != nil {
		return Document{}, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Document{}, errSharedClosed
	}
	if doc, ok := s.documents.get(key); ok {
		s.mu.Unlock()
		return cloneDocument(doc), nil
	}
	fk := docFlightKey{doc: key, epoch: s.epoch, list: s.listEpoch, rev: s.rev[key.WorktreeID]}
	f := s.docFlights[fk]
	if f == nil {
		if len(s.docFlights) >= maxPendingReads {
			s.mu.Unlock()
			return Document{}, errTooManyReads
		}
		f = s.startDocumentLocked(fk)
	}
	f.waiters++
	s.mu.Unlock()
	return await(s, ctx, f, func() {
		if s.docFlights[fk] == f {
			delete(s.docFlights, fk)
		}
	}, cloneDocument)
}

// LocalEditor exposes EditablePath only when the inner provider is local.
func (s *Shared) LocalEditor() (LocalEditor, bool) {
	e, ok := s.inner.(LocalEditor)
	return e, ok
}

// Capabilities reports which optional features the inner provider implements.
func (s *Shared) Capabilities() Capabilities {
	_, browse := s.inner.(BrowseProvider)
	_, cont := s.inner.(DiscoveryContinuer)
	_, refresh := s.inner.(worktreeRefresher)
	_, edit := s.inner.(LocalEditor)
	return Capabilities{Browse: browse, ContinueDiscovery: cont, RefreshWorktree: refresh, LocalEditor: edit}
}

// inventory joins or starts a flight. With ensure, a cached inventory that satisfies
// the mode is returned under the same lock, so a scan published in between is never rescanned.
func (s *Shared) inventory(ctx context.Context, mode invMode, ensure bool) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Snapshot{}, errSharedClosed
	}
	if ensure && s.hasInv && (mode == invBrowse || !s.inv.Browse) {
		snap := cloneSnapshot(s.inv)
		s.mu.Unlock()
		return snap, nil
	}
	f := s.latest[mode]
	if f == nil || f.canceled || isClosed(f.done) {
		var err error
		f, err = s.startInventoryLocked(mode)
		if err != nil {
			s.mu.Unlock()
			return Snapshot{}, err
		}
	}
	f.waiters++
	s.mu.Unlock()
	return await(s, ctx, &f.sharedFlight, nil, cloneSnapshot)
}

// startInventoryLocked queues behind the newest flight in the same lane.
func (s *Shared) startInventoryLocked(mode invMode) (*invFlight, error) {
	lane := laneOf(mode)
	if s.laneUndone[lane] >= maxLaneScans {
		return nil, errTooManyScans
	}
	f := &invFlight{mode: mode, epoch: s.epoch, prev: s.laneTail[lane]}
	f.done = make(chan struct{})
	ctx, cancel := context.WithCancel(s.root) //nolint:gosec // flight owns cancel; runFlight and last waiter both release it
	f.cancel = cancel
	s.laneTail[lane] = f
	s.latest[mode] = f
	s.laneUndone[lane]++
	go s.runInventory(ctx, f)
	return f, nil
}

func (s *Shared) runInventory(ctx context.Context, f *invFlight) {
	if prev := f.prev; prev != nil {
		select {
		case <-prev.done:
		case <-ctx.Done():
		}
	}
	f.prev = nil
	lane := laneOf(f.mode)
	runFlight(s, ctx, &f.sharedFlight, func(ctx context.Context) (Snapshot, error) {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		s.mu.Lock()
		stale := f.mode == invContinue && f.epoch != s.epoch
		s.mu.Unlock()
		if stale {
			return Snapshot{}, ErrStale
		}
		return s.loadInventory(ctx, f.mode)
	}, func(snap Snapshot, err error) (Snapshot, error) {
		s.laneUndone[lane]--
		return s.finishInventoryLocked(f, snap, err)
	})
}

func (s *Shared) loadInventory(ctx context.Context, mode invMode) (Snapshot, error) {
	switch mode {
	case invBrowse:
		b, ok := s.inner.(BrowseProvider)
		if !ok {
			return Snapshot{}, fmt.Errorf("%w: folder browsing is not available for this workspace", ErrUnavailable)
		}
		return b.BrowseRoot(ctx)
	case invContinue:
		c, ok := s.inner.(DiscoveryContinuer)
		if !ok {
			return Snapshot{}, fmt.Errorf("%w: discovery continuation is unavailable", ErrUnavailable)
		}
		return c.ContinueDiscovery(ctx)
	default:
		return s.inner.Snapshot(ctx)
	}
}

func (s *Shared) finishInventoryLocked(f *invFlight, snap Snapshot, err error) (Snapshot, error) {
	switch {
	case s.closed:
		return Snapshot{}, errSharedClosed
	case f.canceled:
		// A provider may ignore ctx; a canceled scan must not replace the inventory.
		return Snapshot{}, context.Canceled
	case err != nil:
		return Snapshot{}, err
	case f.mode == invBrowse && s.hasInv && !s.inv.Browse:
		// A full inventory outranks a root-only browse result.
		return cloneSnapshot(s.inv), nil
	case f.mode == invContinue && f.epoch != s.epoch:
		return Snapshot{}, ErrStale
	case s.hasInv && snap.Generation < s.inv.Generation:
		return Snapshot{}, ErrStale
	}
	s.publishInventoryLocked(snap)
	return cloneSnapshot(snap), nil
}

// publishInventoryLocked always advances the epoch, so equal provider generations
// (e.g. a root-only browse upgraded by a full scan) still invalidate dependent results.
func (s *Shared) publishInventoryLocked(snap Snapshot) {
	s.epoch++
	s.inv, s.hasInv = snap, true
	s.resetContentLocked()
}

// invalidateListingLocked drops directory and document results, including the
// inner provider's cached document bytes, so the next Files read reaches the disk.
// Called under s.mu; the inner provider never calls back into the wrapper.
func (s *Shared) invalidateListingLocked() {
	s.listEpoch++
	s.documents.clear()
	s.dirs.clear()
	cancelSharedFlights(s.docFlights, func(docFlightKey) bool { return true })
	cancelSharedFlights(s.dirFlights, func(dirFlightKey) bool { return true })
	if inv, ok := s.inner.(documentInvalidator); ok {
		inv.InvalidateDocuments()
	}
}

func (s *Shared) hasWorktreeLocked(id string) bool {
	for _, repo := range s.inv.Repositories {
		for _, wt := range repo.Worktrees {
			if wt.ID == id {
				return true
			}
		}
	}
	return false
}

// patchInventoryLocked keeps CurrentSnapshot's status in step with the last loaded worktree.
func (s *Shared) patchInventoryLocked(wt Worktree) {
	for ri := range s.inv.Repositories {
		for wi := range s.inv.Repositories[ri].Worktrees {
			w := &s.inv.Repositories[ri].Worktrees[wi]
			if w.ID == wt.ID {
				w.State, w.CountsState, w.Err = wt.State, wt.CountsState, wt.Err
				w.Files = slices.Clone(wt.Files)
				return
			}
		}
	}
}

func (s *Shared) startWorktreeLocked(key wtFlightKey, fresh bool) *sharedFlight[Worktree] {
	f := newSharedFlight[Worktree](fresh)
	s.wtFlights[key] = f
	ctx, cancel := context.WithCancel(s.root) //nolint:gosec // flight owns cancel; runFlight and last waiter both release it
	f.cancel = cancel
	go runFlight(s, ctx, f, limitReads(s, func(ctx context.Context) (Worktree, error) {
		if r, ok := s.inner.(worktreeRefresher); ok && fresh {
			return r.RefreshWorktree(ctx, key.id)
		}
		return s.inner.LoadWorktree(ctx, key.id)
	}), func(wt Worktree, err error) (Worktree, error) {
		if s.wtFlights[key] == f {
			delete(s.wtFlights, key)
		}
		switch {
		case s.closed:
			return Worktree{}, errSharedClosed
		case !s.currentWorktreeLocked(key):
			return Worktree{}, ErrStale
		case f.canceled:
			return Worktree{}, context.Canceled
		case err != nil:
			return Worktree{}, err
		}
		s.patchInventoryLocked(wt)
		return wt, nil
	})
	return f
}

func (s *Shared) startDirectoryLocked(fk dirFlightKey) *sharedFlight[Directory] {
	f := newSharedFlight[Directory](false)
	s.dirFlights[fk] = f
	ctx, cancel := context.WithCancel(s.root) //nolint:gosec // flight owns cancel; runFlight and last waiter both release it
	f.cancel = cancel
	go runFlight(s, ctx, f, limitReads(s, func(ctx context.Context) (Directory, error) {
		return s.inner.Directory(ctx, fk.dir.worktree, fk.dir.path)
	}), func(dir Directory, err error) (Directory, error) {
		if s.dirFlights[fk] == f {
			delete(s.dirFlights, fk)
		}
		switch {
		case s.closed:
			return Directory{}, errSharedClosed
		case !s.currentDirLocked(fk):
			return Directory{}, ErrStale
		case f.canceled:
			return Directory{}, context.Canceled
		case err != nil:
			return Directory{}, err
		}
		s.dirs.put(fk.dir, cloneDirectory(dir), directoryBytes(dir))
		return dir, nil
	})
	return f
}

func (s *Shared) startDocumentLocked(fk docFlightKey) *sharedFlight[Document] {
	f := newSharedFlight[Document](false)
	s.docFlights[fk] = f
	ctx, cancel := context.WithCancel(s.root) //nolint:gosec // flight owns cancel; runFlight and last waiter both release it
	f.cancel = cancel
	go runFlight(s, ctx, f, limitReads(s, func(ctx context.Context) (Document, error) {
		return s.inner.Document(ctx, fk.doc)
	}), func(doc Document, err error) (Document, error) {
		if s.docFlights[fk] == f {
			delete(s.docFlights, fk)
		}
		switch {
		case s.closed:
			return Document{}, errSharedClosed
		case !s.currentDocLocked(fk):
			return Document{}, ErrStale
		case f.canceled:
			return Document{}, context.Canceled
		case err != nil:
			return Document{}, err
		}
		s.documents.put(fk.doc, cloneDocument(doc), documentBytes(doc))
		return doc, nil
	})
	return f
}

func (s *Shared) currentWorktreeLocked(key wtFlightKey) bool {
	return s.epoch == key.epoch && s.rev[key.id] == key.rev
}

func (s *Shared) currentDirLocked(fk dirFlightKey) bool {
	return s.epoch == fk.epoch && s.listEpoch == fk.list && s.rev[fk.dir.worktree] == fk.rev
}

func (s *Shared) currentDocLocked(fk docFlightKey) bool {
	return s.epoch == fk.epoch && s.listEpoch == fk.list && s.rev[fk.doc.WorktreeID] == fk.rev
}

// invalidateWorktreeLocked bumps the worktree revision so in-flight results for
// it become stale, then drops its cached listings and documents.
func (s *Shared) invalidateWorktreeLocked(id string) {
	cancelSharedFlights(s.docFlights, func(k docFlightKey) bool { return k.doc.WorktreeID == id })
	cancelSharedFlights(s.dirFlights, func(k dirFlightKey) bool { return k.dir.worktree == id })
	cancelSharedFlights(s.wtFlights, func(k wtFlightKey) bool { return k.id == id })
	s.documents.removeIf(func(k DocumentKey) bool { return k.WorktreeID == id })
	s.dirs.removeIf(func(k dirKey) bool { return k.worktree == id })
	s.rev[id]++
}

// resetContentLocked drops every content result and flight when the inventory is replaced.
func (s *Shared) resetContentLocked() {
	cancelSharedFlights(s.docFlights, func(docFlightKey) bool { return true })
	cancelSharedFlights(s.dirFlights, func(dirFlightKey) bool { return true })
	cancelSharedFlights(s.wtFlights, func(wtFlightKey) bool { return true })
	s.documents.clear()
	s.dirs.clear()
	s.rev = make(map[string]uint64)
}

func newSharedFlight[T any](fresh bool) *sharedFlight[T] {
	return &sharedFlight[T]{done: make(chan struct{}), fresh: fresh}
}

func cancelSharedFlights[K comparable, T any](flights map[K]*sharedFlight[T], match func(K) bool) {
	for key, f := range flights {
		if match(key) {
			f.canceled = true
			f.cancel()
			delete(flights, key)
		}
	}
}

// limitReads bounds concurrent inner calls. A queued read stops waiting when its flight is canceled.
func limitReads[T any](s *Shared, work func(context.Context) (T, error)) func(context.Context) (T, error) {
	return func(ctx context.Context) (T, error) {
		var zero T
		select {
		case s.readSlots <- struct{}{}:
		case <-ctx.Done():
			return zero, ctx.Err()
		}
		defer func() { <-s.readSlots }()
		return work(ctx)
	}
}

// runFlight runs work outside the lock, then records the result under the lock.
// finish decides the final value or error, including stale and closed checks.
func runFlight[T any](s *Shared, ctx context.Context, f *sharedFlight[T], work func(context.Context) (T, error), finish func(T, error) (T, error)) {
	defer f.cancel()
	val, err := work(ctx)
	if err == nil && ctx.Err() != nil {
		// A provider may ignore ctx; a canceled flight never publishes its result.
		err = ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f.val, f.err = finish(val, err)
	close(f.done)
}

// await waits for f. The flight is canceled only when its last waiter leaves.
func await[T any](s *Shared, ctx context.Context, f *sharedFlight[T], forget func(), clone func(T) T) (T, error) {
	select {
	case <-f.done:
		if f.err != nil {
			var zero T
			return zero, f.err
		}
		return clone(f.val), nil
	case <-ctx.Done():
		var zero T
		s.mu.Lock()
		if !isClosed(f.done) {
			f.waiters--
			if f.waiters == 0 {
				f.canceled = true
				f.cancel()
				if forget != nil {
					forget()
				}
			}
		}
		s.mu.Unlock()
		return zero, ctx.Err()
	}
}

type lruEntry[K comparable, V any] struct {
	key   K
	value V
	bytes int
}

// lru is a count- and byte-bounded cache. It is not safe for concurrent use.
type lru[K comparable, V any] struct {
	maxEntries, maxBytes int
	bytes                int
	order                *list.List
	items                map[K]*list.Element
}

func newLRU[K comparable, V any](maxEntries, maxBytes int) *lru[K, V] {
	return &lru[K, V]{maxEntries: maxEntries, maxBytes: maxBytes, order: list.New(), items: make(map[K]*list.Element)}
}

func (c *lru[K, V]) get(key K) (V, bool) {
	el, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*lruEntry[K, V]).value, true
}

func (c *lru[K, V]) put(key K, value V, bytes int) {
	c.remove(key)
	if bytes > c.maxBytes {
		return
	}
	for c.order.Len() > 0 && (c.order.Len() >= c.maxEntries || c.bytes+bytes > c.maxBytes) {
		c.removeElement(c.order.Back())
	}
	c.items[key] = c.order.PushFront(&lruEntry[K, V]{key: key, value: value, bytes: bytes})
	c.bytes += bytes
}

func (c *lru[K, V]) remove(key K) {
	if el, ok := c.items[key]; ok {
		c.removeElement(el)
	}
}

func (c *lru[K, V]) removeIf(match func(K) bool) {
	for key, el := range c.items {
		if match(key) {
			c.removeElement(el)
		}
	}
}

func (c *lru[K, V]) removeElement(el *list.Element) {
	e := c.order.Remove(el).(*lruEntry[K, V])
	delete(c.items, e.key)
	c.bytes -= e.bytes
}

func (c *lru[K, V]) clear() {
	c.order.Init()
	c.items = make(map[K]*list.Element)
	c.bytes = 0
}

func cloneDirectory(d Directory) Directory {
	d.Entries = slices.Clone(d.Entries)
	return d
}

func directoryBytes(d Directory) int {
	n := 64 + len(d.Path) + len(d.Err)
	for _, e := range d.Entries {
		n += len(e.Name) + 16
	}
	return n
}
