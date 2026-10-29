package gitreview

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gitconfig "github.com/go-git/go-git/v5/plumbing/format/config"
)

const (
	MaxDepth                   = 8
	MaxEntries                 = 20_000
	MaxRepositories            = 100
	MaxDiscoveryDepth          = 8
	MaxDiscoveryEntries        = 100_000
	MaxDiscoveryRepositories   = 500
	DefaultDiscoveryDepth      = 4
	DefaultDiscoveryEntries    = 4_096
	DefaultDiscoveryTimeout    = 3 * time.Second
	MaxDiscoveryCandidateBytes = 2 << 20
	MaxStatusBytes             = 8 << 20
	MaxDirectoryItems          = 1_000
	MaxDiffBytes               = 2 << 20
	MaxDiffLines               = 10_000
	MaxCacheBytes              = 32 << 20
	MaxStatusCacheBytes        = 8 << 20
	MaxCountFileBytes          = 2 << 20
	MaxCountBytes              = 8 << 20
	MaxCountFiles              = 2_000
	CountTimeout               = 2 * time.Second
	DiscoveryTimeout           = 10 * time.Second
	MaxDiscoveryTimeout        = 30 * time.Second
	DiscoveryDepthStep         = 8
	DiscoveryEntriesStep       = 20_000
	DiscoveryRepositoriesStep  = 100
	DiscoveryTimeoutStep       = 10 * time.Second
	GitTimeout                 = 5 * time.Second
	statusCacheTTL             = 300 * time.Millisecond
)

var heavyDirectories = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true,
	"build": true, ".cache": true, ".venv": true, "venv": true,
	"__pycache__": true, ".gradle": true, "Library": true, "System": true,
	".Trash": true, ".terraform": true, "target": true, "Pods": true,
}

func pruneDiscoveryDirectory(name string) bool {
	return strings.HasPrefix(name, ".") || heavyDirectories[name]
}

func pathHasPrunedDiscoveryComponent(path string) bool {
	for _, component := range strings.Split(filepath.ToSlash(filepath.Clean(path)), "/") {
		if component != "." && pruneDiscoveryDirectory(component) {
			return true
		}
	}
	return false
}

// Local is bound to the immutable workspace scope belonging to one session.
// The workspace can be a checkout root or a subdirectory inside a checkout.
type Local struct {
	scope string
	label string

	mu                sync.Mutex
	generation        uint64
	snapshot          Snapshot
	worktrees         map[string]worktreeRef
	cache             map[DocumentKey]cachedDocument
	cacheBytes        int
	cacheOrder        []DocumentKey
	docFlights        map[documentFlightKey]*documentFlight
	statusFlights     map[string]*worktreeFlight
	statusCache       map[string]cachedStatus
	statusCacheBytes  int
	worktreeRevisions map[string]uint64
	docEpoch          uint64
	processes         chan struct{}
	discoveryLimits   discoveryLimits
}

type worktreeRef struct {
	root, gitDir, commonDir string
	repoID                  string
	kind                    Kind
	scopePath               string // relative to worktree; "." means all checkout files are in scope
}

type cachedDocument struct {
	document Document
	bytes    int
}

type documentFlightKey struct {
	documentKey DocumentKey
	generation  uint64
	revision    uint64
	epoch       uint64
}

type documentFlight struct {
	done     chan struct{}
	cancel   context.CancelFunc
	waiters  int
	canceled bool
	doc      Document
	err      error
}

type worktreeFlight struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	fresh   bool
	wt      Worktree
	err     error
}

type cachedStatus struct {
	generation  uint64
	revision    uint64
	fetchedAt   time.Time
	state       string
	countsState LoadState
	files       []FileChange
	bytes       int
}

func NewLocal(workspace string) (*Local, error) {
	if workspace == "" || !filepath.IsAbs(workspace) {
		return nil, fmt.Errorf("%w: workspace must be absolute", ErrUnavailable)
	}
	abs := filepath.Clean(workspace)
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	st, err := os.Stat(resolved)
	if err != nil || !st.IsDir() {
		return nil, fmt.Errorf("%w: workspace is not a directory", ErrUnavailable)
	}
	return &Local{scope: resolved, label: filepath.Base(resolved), cache: make(map[DocumentKey]cachedDocument), worktrees: make(map[string]worktreeRef), docFlights: make(map[documentFlightKey]*documentFlight), statusFlights: make(map[string]*worktreeFlight), statusCache: make(map[string]cachedStatus), worktreeRevisions: make(map[string]uint64), processes: make(chan struct{}, 2)}, nil
}

func (l *Local) CurrentSnapshot() Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	return cloneSnapshot(l.snapshot)
}

func (l *Local) Snapshot(ctx context.Context) (Snapshot, error) {
	l.mu.Lock()
	l.discoveryLimits = baseDiscoveryLimits()
	limits := l.discoveryLimits
	l.mu.Unlock()
	return l.scanSnapshot(ctx, limits)
}

// ContinueDiscovery repeats the bounded inventory with a larger limit for the
// specific reason the previous pass stopped. Each limit has a hard ceiling.
func (l *Local) ContinueDiscovery(ctx context.Context) (Snapshot, error) {
	l.mu.Lock()
	limits := l.discoveryLimits
	if limits.depth == 0 {
		limits = baseDiscoveryLimits()
	}
	switch l.snapshot.Scope.LimitReason {
	case DiscoveryLimitDepth:
		limits.depth = min(MaxDiscoveryDepth, limits.depth+2)
	case DiscoveryLimitEntries:
		limits.entries = min(MaxDiscoveryEntries, limits.entries+4_096)
	case DiscoveryLimitRepositories:
		limits.repositories = min(MaxDiscoveryRepositories, limits.repositories+DiscoveryRepositoriesStep)
	case DiscoveryLimitTimeout:
		limits.timeout = min(MaxDiscoveryTimeout, limits.timeout+5*time.Second)
	default:
		l.mu.Unlock()
		return l.CurrentSnapshot(), nil
	}
	if limits == l.discoveryLimits {
		l.mu.Unlock()
		return l.CurrentSnapshot(), nil
	}
	l.mu.Unlock()
	return l.scanSnapshot(ctx, limits)
}

// CanContinueDiscovery reports whether the last partial local scan can be
// deepened without exceeding a hard per-pass ceiling.
func (l *Local) CanContinueDiscovery() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	limits := l.discoveryLimits
	switch l.snapshot.Scope.LimitReason {
	case DiscoveryLimitDepth:
		return limits.depth < MaxDiscoveryDepth
	case DiscoveryLimitEntries:
		return limits.entries < MaxDiscoveryEntries
	case DiscoveryLimitBytes, DiscoveryLimitUnavailable:
		return false
	case DiscoveryLimitRepositories:
		return limits.repositories < MaxDiscoveryRepositories
	case DiscoveryLimitTimeout:
		return limits.timeout < MaxDiscoveryTimeout
	}
	return false
}

func (l *Local) scanSnapshot(ctx context.Context, limits discoveryLimits) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, limits.timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	owner, _ := owningCheckout(l.scope)
	refs, repos, reason, err := discover(ctx, l.scope, owner, l, limits)
	if err != nil {
		return Snapshot{}, err
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		reason = DiscoveryLimitTimeout
	}
	note := ""
	if reason != "" {
		note = discoveryLimitText(reason, limits)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if errors.Is(ctx.Err(), context.Canceled) {
		return Snapshot{}, ctx.Err()
	}
	l.discoveryLimits = limits
	return l.publishLocked(Snapshot{CapturedAt: time.Now(), Scope: Scope{Label: l.label, Note: note, LimitReason: reason, LimitValue: limitValue(reason, limits)}, Repositories: repos}, refs), nil
}

// BrowseRoot inventories only the scope's root checkout (or a synthetic workspace
// outside Git) without walking nearby repositories, listing untracked paths, or
// reading status. An existing inventory that already has the root is kept.
func (l *Local) BrowseRoot(ctx context.Context) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	owner, _ := owningCheckout(l.scope)
	refs, repo := rootInventory(l.scope, owner)
	rootID := repo.Worktrees[0].ID
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if l.snapshot.Generation != 0 {
		if _, ok := l.worktrees[rootID]; ok {
			return cloneSnapshot(l.snapshot), nil
		}
	}
	return l.publishLocked(Snapshot{CapturedAt: time.Now(), Browse: true, Scope: Scope{Label: l.label}, Repositories: []Repository{repo}}, refs), nil
}

// rootInventory uses the identities discover assigns so a later full Snapshot
// keeps the root IDs.
func rootInventory(scope string, owner *checkout) (map[string]worktreeRef, Repository) {
	refs := make(map[string]worktreeRef)
	if owner == nil {
		id := identity(scope + "\x00files")
		refs[id] = worktreeRef{root: scope, repoID: id, kind: KindNone, scopePath: "."}
		return refs, Repository{ID: id, Name: filepath.Base(scope), Kind: KindNone, Worktrees: []Worktree{{ID: id, Path: ".", Current: true, State: "clean"}}}
	}
	root := owner.root
	id := identity(owner.gitDir + "\x00" + root)
	scopeRel := "."
	if !samePath(root, scope) && within(root, scope) {
		scopeRel = mustRel(root, scope)
	}
	commonID := identity(owner.commonDir)
	refs[id] = worktreeRef{root: root, gitDir: owner.gitDir, commonDir: owner.commonDir, repoID: commonID, kind: KindPrimary, scopePath: scopeRel}
	wt := Worktree{ID: id, Path: displayPath(root, scope), Branch: owner.branch, Detached: owner.detached, Current: true, Linked: !samePath(owner.gitDir, owner.commonDir), State: "unscanned"}
	if owner.detached {
		if b, err := os.ReadFile(filepath.Join(owner.gitDir, "HEAD")); err == nil {
			wt.DetachedHead = strings.TrimSpace(string(b))
			if len(wt.DetachedHead) > 7 {
				wt.DetachedHead = wt.DetachedHead[:7]
			}
		}
	}
	return refs, Repository{ID: commonID, Name: canonicalCheckoutName(owner, nil), Kind: KindPrimary, Worktrees: []Worktree{wt}}
}

// publishLocked replaces the inventory and drops caches and in-flight loads that
// may still refer to the previous one.
// localGenerations makes generations unique across Local lifetimes in this process,
// so a response from a recreated provider cannot match an old client generation.
var localGenerations atomic.Uint64

func (l *Local) publishLocked(snapshot Snapshot, refs map[string]worktreeRef) Snapshot {
	l.generation = localGenerations.Add(1)
	snapshot.Generation = l.generation
	l.snapshot = snapshot
	l.worktrees = refs
	l.cache = make(map[DocumentKey]cachedDocument)
	l.cacheOrder = nil
	l.cacheBytes = 0
	l.statusCache = make(map[string]cachedStatus)
	l.statusCacheBytes = 0
	l.worktreeRevisions = make(map[string]uint64)
	for _, flight := range l.docFlights {
		flight.canceled = true
		flight.cancel()
	}
	for _, flight := range l.statusFlights {
		flight.cancel()
	}
	l.statusFlights = make(map[string]*worktreeFlight)
	return cloneSnapshot(l.snapshot)
}

// EditablePath returns the absolute path of an existing regular file within
// this local provider's immutable workspace scope. It deliberately exists only
// on Local (not Provider), so remote providers cannot opt into local editing.
func (l *Local) EditablePath(worktreeID, path string) (string, error) {
	ref, ok := l.worktree(worktreeID)
	if !ok {
		return "", ErrNotFound
	}
	abs, checkoutRel, _, err := scopedPath(ref, path)
	if err != nil {
		return "", err
	}
	if checkoutRel == "." || checkoutRel == "" {
		return "", ErrOutsideScope
	}
	root, err := os.OpenRoot(ref.root)
	if err != nil {
		return "", err
	}
	defer root.Close()
	clean := filepath.Clean(checkoutRel)
	for parent := filepath.Dir(clean); parent != "." && parent != string(filepath.Separator); parent = filepath.Dir(parent) {
		info, err := root.Lstat(parent)
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", ErrOutsideScope
		}
	}
	info, err := root.Lstat(clean)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", ErrOutsideScope
	}
	file, err := root.Open(clean)
	if err != nil {
		return "", err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return "", ErrOutsideScope
	}
	return abs, nil
}

func (l *Local) Directory(ctx context.Context, worktreeID, path string) (Directory, error) {
	ref, ok := l.worktree(worktreeID)
	if !ok {
		return Directory{}, ErrNotFound
	}
	abs, rel, publicRel, err := scopedPath(ref, path)
	if err != nil {
		return Directory{}, err
	}
	st, err := os.Lstat(abs)
	if err != nil {
		return Directory{}, err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return Directory{}, ErrOutsideScope
	}
	f, err := os.Open(abs)
	if err != nil {
		return Directory{}, err
	}
	defer f.Close()
	rules, err := ignoreRulesForPath(ref.root, abs, l.scope)
	if err != nil {
		return Directory{}, err
	}
	out := Directory{WorktreeID: worktreeID, Path: filepath.ToSlash(publicRel), State: StateReady}
	seenEntries := 0
readLoop:
	for {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		entries, readErr := f.ReadDir(128)
		for _, e := range entries {
			seenEntries++
			if seenEntries > MaxEntries {
				out.Truncated = true
				break readLoop
			}
			if e.Name() == ".git" || heavyDirectories[e.Name()] {
				continue
			}
			if e.Type()&os.ModeSymlink != 0 {
				continue
			}
			info, infoErr := e.Info()
			if infoErr != nil {
				continue
			}
			childPath := filepath.Join(abs, e.Name())
			if rules.matches(ref.root, childPath, info.IsDir()) {
				continue
			}
			childRel := filepath.Join(rel, e.Name())
			if !scopeAllows(ref, childRel) {
				continue
			}
			if len(out.Entries) >= MaxDirectoryItems {
				out.Truncated = true
				break readLoop
			}
			out.Entries = append(out.Entries, DirectoryEntry{Name: e.Name(), Dir: e.IsDir()})
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return out, readErr
		}
	}
	slices.SortFunc(out.Entries, func(a, b DirectoryEntry) int {
		if a.Dir != b.Dir {
			if a.Dir {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return out, nil
}

func (l *Local) Document(ctx context.Context, key DocumentKey) (Document, error) {
	if err := ctx.Err(); err != nil {
		return Document{}, err
	}
	ref, ok := l.worktree(key.WorktreeID)
	if !ok {
		return Document{}, ErrNotFound
	}
	key.Path = filepath.ToSlash(filepath.Clean(filepath.FromSlash(key.Path)))
	abs, _, publicRel, err := scopedPath(ref, key.Path)
	if err != nil {
		return Document{}, err
	}
	key.Path = filepath.ToSlash(publicRel)

	l.mu.Lock()
	if cached, ok := l.cache[key]; ok {
		l.mu.Unlock()
		return cloneDocument(cached.document), nil
	}
	flightKey := documentFlightKey{documentKey: key, generation: l.generation, revision: l.worktreeRevisions[key.WorktreeID], epoch: l.docEpoch}
	flight := l.docFlights[flightKey]
	if flight == nil || flight.canceled || flightCtxErr(flight) != nil {
		workCtx, cancel := context.WithCancel(context.Background())
		flight = &documentFlight{done: make(chan struct{}), cancel: cancel}
		l.docFlights[flightKey] = flight
		go l.runDocumentFlight(workCtx, cancel, flightKey, flight, ref, key, abs)
	}
	flight.waiters++
	l.mu.Unlock()

	select {
	case <-flight.done:
		if flight.err != nil {
			return flight.doc, flight.err
		}
		return cloneDocument(flight.doc), nil
	case <-ctx.Done():
		l.mu.Lock()
		if !isClosed(flight.done) {
			flight.waiters--
			if flight.waiters == 0 {
				if l.docFlights[flightKey] == flight {
					delete(l.docFlights, flightKey)
				}
				flight.canceled = true
				flight.cancel()
			}
		}
		l.mu.Unlock()
		return Document{}, ctx.Err()
	}
}

// InvalidateDocuments drops cached and in-flight document reads so the next read
// touches the disk again. Status, inventory, and directory reads are unaffected.
func (l *Local) InvalidateDocuments() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.docEpoch++
	l.cache = make(map[DocumentKey]cachedDocument)
	l.cacheOrder = nil
	l.cacheBytes = 0
	for key, flight := range l.docFlights {
		flight.canceled = true
		flight.cancel()
		delete(l.docFlights, key)
	}
}

func (l *Local) runDocumentFlight(ctx context.Context, cancel context.CancelFunc, flightKey documentFlightKey, flight *documentFlight, ref worktreeRef, key DocumentKey, abs string) {
	defer cancel()
	var doc Document
	var err error
	switch key.Mode {
	case "changes":
		doc, err = l.changeDocument(ctx, ref, key, abs)
	case "files":
		doc, err = l.fileDocument(ctx, ref, key, abs)
	default:
		err = fmt.Errorf("invalid document mode")
	}
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	l.mu.Lock()
	flight.doc, flight.err = doc, err
	if l.docFlights[flightKey] == flight {
		delete(l.docFlights, flightKey)
		if err == nil && flightKey.generation == l.generation && flightKey.revision == l.worktreeRevisions[key.WorktreeID] && flightKey.epoch == l.docEpoch {
			n := documentBytes(doc)
			if n <= MaxCacheBytes {
				for l.cacheBytes+n > MaxCacheBytes && len(l.cacheOrder) > 0 {
					oldest := l.cacheOrder[0]
					l.cacheOrder = l.cacheOrder[1:]
					if c, ok := l.cache[oldest]; ok {
						l.cacheBytes -= c.bytes
						delete(l.cache, oldest)
					}
				}
				l.cache[key] = cachedDocument{cloneDocument(doc), n}
				l.cacheOrder = append(l.cacheOrder, key)
				l.cacheBytes += n
			}
		}
	}
	close(flight.done)
	l.mu.Unlock()
}

func flightCtxErr(flight *documentFlight) error {
	select {
	case <-flight.done:
		return errors.New("completed")
	default:
	}
	return nil
}

func isClosed(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// LoadWorktree obtains the bounded status for one checkout. Discovery itself
// only reads repository metadata; status is intentionally lazy. Repeated
// requests reuse status briefly; callers that need fresh status use RefreshWorktree.
func (l *Local) LoadWorktree(ctx context.Context, worktreeID string) (Worktree, error) {
	if err := ctx.Err(); err != nil {
		return Worktree{}, err
	}
	l.mu.Lock()
	if _, ok := l.worktrees[worktreeID]; !ok {
		l.mu.Unlock()
		return Worktree{}, ErrNotFound
	}
	flight := l.statusFlights[worktreeID]
	if flight == nil {
		statusResult, cached := l.statusCache[worktreeID]
		if cached && statusResult.generation == l.generation && statusResult.revision == l.worktreeRevisions[worktreeID] && time.Since(statusResult.fetchedAt) < statusCacheTTL {
			l.mu.Unlock()
			return l.loadWorktree(ctx, worktreeID, false)
		}
		workCtx, cancel := context.WithCancel(context.Background())
		flight = &worktreeFlight{done: make(chan struct{}), cancel: cancel}
		l.statusFlights[worktreeID] = flight
		go l.runWorktreeLoad(workCtx, worktreeID, flight)
	}
	flight.waiters++
	l.mu.Unlock()
	return l.waitWorktreeFlight(ctx, worktreeID, flight)
}

// RefreshWorktree invalidates status and documents for one checkout before
// reading its current status. It is an explicit freshness boundary for auto-refresh.
func (l *Local) RefreshWorktree(ctx context.Context, worktreeID string) (Worktree, error) {
	if err := ctx.Err(); err != nil {
		return Worktree{}, err
	}
	l.mu.Lock()
	if _, ok := l.worktrees[worktreeID]; !ok {
		l.mu.Unlock()
		return Worktree{}, ErrNotFound
	}
	flight := l.statusFlights[worktreeID]
	if flight == nil || !flight.fresh {
		if flight != nil {
			if l.statusFlights[worktreeID] == flight {
				delete(l.statusFlights, worktreeID)
			}
			flight.cancel()
		}
		l.worktreeRevisions[worktreeID]++
		l.deleteStatusCacheLocked(worktreeID)
		l.evictWorktreeDocumentsLocked(worktreeID)
		for key, docFlight := range l.docFlights {
			if key.documentKey.WorktreeID == worktreeID {
				if l.docFlights[key] == docFlight {
					delete(l.docFlights, key)
				}
				docFlight.canceled = true
				docFlight.cancel()
			}
		}
		workCtx, cancel := context.WithCancel(context.Background())
		flight = &worktreeFlight{done: make(chan struct{}), cancel: cancel, fresh: true}
		l.statusFlights[worktreeID] = flight
		go l.runWorktreeRefresh(workCtx, worktreeID, flight)
	}
	flight.waiters++
	l.mu.Unlock()
	return l.waitWorktreeFlight(ctx, worktreeID, flight)
}

func (l *Local) waitWorktreeFlight(ctx context.Context, worktreeID string, flight *worktreeFlight) (Worktree, error) {
	select {
	case <-flight.done:
		if flight.err != nil {
			return flight.wt, flight.err
		}
		return cloneWorktree(flight.wt), nil
	case <-ctx.Done():
		l.mu.Lock()
		if !isClosed(flight.done) {
			flight.waiters--
			if flight.waiters == 0 {
				if l.statusFlights[worktreeID] == flight {
					delete(l.statusFlights, worktreeID)
				}
				flight.cancel()
			}
		}
		l.mu.Unlock()
		return Worktree{}, ctx.Err()
	}
}

func (l *Local) runWorktreeLoad(ctx context.Context, worktreeID string, flight *worktreeFlight) {
	defer flight.cancel()
	wt, err := l.loadWorktree(ctx, worktreeID, false)
	l.mu.Lock()
	flight.wt, flight.err = wt, err
	if l.statusFlights[worktreeID] == flight {
		delete(l.statusFlights, worktreeID)
	}
	close(flight.done)
	l.mu.Unlock()
}

func (l *Local) runWorktreeRefresh(ctx context.Context, worktreeID string, flight *worktreeFlight) {
	defer flight.cancel()
	wt, err := l.loadWorktree(ctx, worktreeID, true)
	l.mu.Lock()
	flight.wt, flight.err = wt, err
	if l.statusFlights[worktreeID] == flight {
		delete(l.statusFlights, worktreeID)
		if err == nil {
			l.worktreeRevisions[worktreeID]++
			if cached, ok := l.statusCache[worktreeID]; ok {
				cached.revision = l.worktreeRevisions[worktreeID]
				l.statusCache[worktreeID] = cached
			}
			l.evictWorktreeDocumentsLocked(worktreeID)
			for key, docFlight := range l.docFlights {
				if key.documentKey.WorktreeID == worktreeID {
					if l.docFlights[key] == docFlight {
						delete(l.docFlights, key)
					}
					docFlight.canceled = true
					docFlight.cancel()
				}
			}
		}
	}
	close(flight.done)
	l.mu.Unlock()
}

func cloneWorktree(wt Worktree) Worktree {
	wt.Files = cloneFiles(wt.Files)
	return wt
}

func (l *Local) loadWorktree(ctx context.Context, worktreeID string, fresh bool) (Worktree, error) {
	l.mu.Lock()
	ref, ok := l.worktrees[worktreeID]
	generation := l.generation
	revision := l.worktreeRevisions[worktreeID]
	if !ok {
		l.mu.Unlock()
		return Worktree{}, ErrNotFound
	}
	statusResult, cached := l.statusCache[worktreeID]
	if fresh || !cached || statusResult.generation != generation || statusResult.revision != revision || time.Since(statusResult.fetchedAt) >= statusCacheTTL {
		cached = false
	}
	l.mu.Unlock()

	state := "clean"
	countsState := StateReady
	var files []FileChange
	var err error
	if ref.gitDir != "" {
		if cached {
			state, countsState, files = statusResult.state, statusResult.countsState, cloneFiles(statusResult.files)
		} else {
			state, files, countsState, err = status(ctx, ref.root, ref.scopePath, l)
		}
	}
	wt := Worktree{ID: worktreeID, Path: filepath.Base(ref.root), Current: false, Linked: !samePath(ref.gitDir, ref.commonDir), State: state, CountsState: countsState, Files: files}
	if ctx.Err() != nil {
		return Worktree{}, ctx.Err()
	}
	if err != nil {
		wt.State = "error"
		wt.Err = err.Error()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.worktrees[worktreeID] != ref || l.generation != generation {
		return Worktree{}, ErrNotFound
	}
	if l.worktreeRevisions[worktreeID] != revision {
		return Worktree{}, fmt.Errorf("%w: worktree status superseded by refresh", ErrUnavailable)
	}
	if err == nil {
		if !cached {
			l.storeStatusCacheLocked(worktreeID, cachedStatus{generation: generation, revision: revision, fetchedAt: time.Now(), state: state, countsState: countsState, files: cloneFiles(files)})
		}
		for ri := range l.snapshot.Repositories {
			for wi := range l.snapshot.Repositories[ri].Worktrees {
				if l.snapshot.Repositories[ri].Worktrees[wi].ID == worktreeID {
					prior := l.snapshot.Repositories[ri].Worktrees[wi]
					wt.Path = prior.Path
					wt.Branch = prior.Branch
					wt.Detached = prior.Detached
					wt.DetachedHead = prior.DetachedHead
					wt.Current = prior.Current
					l.snapshot.Repositories[ri].Worktrees[wi] = wt
					return wt, nil
				}
			}
		}
	} else {
		for ri := range l.snapshot.Repositories {
			for wi := range l.snapshot.Repositories[ri].Worktrees {
				if l.snapshot.Repositories[ri].Worktrees[wi].ID == worktreeID {
					wt.Path = l.snapshot.Repositories[ri].Worktrees[wi].Path
					wt.Branch = l.snapshot.Repositories[ri].Worktrees[wi].Branch
					wt.Current = l.snapshot.Repositories[ri].Worktrees[wi].Current
					wt.Detached = l.snapshot.Repositories[ri].Worktrees[wi].Detached
					wt.DetachedHead = l.snapshot.Repositories[ri].Worktrees[wi].DetachedHead
					return wt, err
				}
			}
		}
	}
	return Worktree{}, ErrNotFound
}

func cloneFiles(files []FileChange) []FileChange {
	return append([]FileChange(nil), files...)
}

func (l *Local) deleteStatusCacheLocked(worktreeID string) {
	if cached, ok := l.statusCache[worktreeID]; ok {
		l.statusCacheBytes -= cached.bytes
		delete(l.statusCache, worktreeID)
	}
}

func (l *Local) storeStatusCacheLocked(worktreeID string, cached cachedStatus) {
	cached.bytes = 64
	for _, file := range cached.files {
		cached.bytes += 64 + len(file.Path) + len(file.OldPath) + len(file.Status)
	}
	if cached.bytes > MaxStatusCacheBytes {
		return
	}
	l.deleteStatusCacheLocked(worktreeID)
	for id, previous := range l.statusCache {
		if time.Since(previous.fetchedAt) >= statusCacheTTL {
			l.deleteStatusCacheLocked(id)
		}
	}
	for l.statusCacheBytes+cached.bytes > MaxStatusCacheBytes {
		oldestID := ""
		var oldest time.Time
		for id, previous := range l.statusCache {
			if oldestID == "" || previous.fetchedAt.Before(oldest) {
				oldestID, oldest = id, previous.fetchedAt
			}
		}
		if oldestID == "" {
			break
		}
		l.deleteStatusCacheLocked(oldestID)
	}
	l.statusCache[worktreeID] = cached
	l.statusCacheBytes += cached.bytes
}

func (l *Local) evictWorktreeDocumentsLocked(worktreeID string) {
	for key, cached := range l.cache {
		if key.WorktreeID == worktreeID {
			l.cacheBytes -= cached.bytes
			delete(l.cache, key)
		}
	}
	order := l.cacheOrder[:0]
	for _, key := range l.cacheOrder {
		if key.WorktreeID != worktreeID {
			order = append(order, key)
		}
	}
	l.cacheOrder = order
}

func (l *Local) worktree(id string) (worktreeRef, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.worktrees[id]
	return r, ok
}

func boundedErrorText(err error, maxBytes int) string {
	if err == nil {
		return "unknown error"
	}
	text := strings.TrimSpace(err.Error())
	if len(text) > maxBytes {
		text = text[:maxBytes] + "…"
	}
	return text
}

type discoveryLimits struct {
	depth, entries, repositories int
	timeout                      time.Duration
}

func baseDiscoveryLimits() discoveryLimits {
	return discoveryLimits{depth: DefaultDiscoveryDepth, entries: DefaultDiscoveryEntries, repositories: MaxRepositories, timeout: DefaultDiscoveryTimeout}
}

func limitValue(reason DiscoveryLimit, limits discoveryLimits) int {
	switch reason {
	case DiscoveryLimitDepth:
		return limits.depth
	case DiscoveryLimitEntries:
		return limits.entries
	case DiscoveryLimitBytes:
		return MaxDiscoveryCandidateBytes
	case DiscoveryLimitRepositories:
		return limits.repositories
	case DiscoveryLimitTimeout:
		return int(limits.timeout.Seconds())
	}
	return 0
}

func discoveryLimitText(reason DiscoveryLimit, limits discoveryLimits) string {
	value := limitValue(reason, limits)
	switch reason {
	case DiscoveryLimitDepth:
		return fmt.Sprintf("Partial scan: directory depth limit reached (%d).", value)
	case DiscoveryLimitEntries:
		return fmt.Sprintf("Partial scan: discovery entry limit reached (%s entries).", strconv.Itoa(value))
	case DiscoveryLimitBytes:
		return fmt.Sprintf("Partial scan: Git metadata output limit reached (%s bytes).", strconv.Itoa(value))
	case DiscoveryLimitUnavailable:
		return "Partial scan: nested repository metadata is unavailable; only discovered workspaces are shown."
	case DiscoveryLimitRepositories:
		return fmt.Sprintf("Partial scan: nested repository limit reached (%d repositories).", value)
	case DiscoveryLimitTimeout:
		return fmt.Sprintf("Partial scan: discovery time limit reached (%d seconds).", value)
	}
	return ""
}

var errStopDiscovery = errors.New("git review discovery stopped")

func mainWorktreePath(path, commonDir string) bool {
	return samePath(filepath.Join(path, ".git"), commonDir)
}

func sortDiscoveredRepositories(repos []Repository) {
	for i := range repos {
		slices.SortStableFunc(repos[i].Worktrees, func(a, b Worktree) int {
			if a.Linked != b.Linked {
				if !a.Linked {
					return -1
				}
				return 1
			}
			return strings.Compare(a.Path, b.Path)
		})
	}
	slices.SortFunc(repos, func(a, b Repository) int {
		if cmp := strings.Compare(a.Name, b.Name); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.ID, b.ID)
	})
}

func canonicalCheckoutName(c *checkout, paths []worktreeListing) string {
	for _, path := range paths {
		if mainWorktreePath(path.path, c.commonDir) {
			return filepath.Base(filepath.Clean(path.path))
		}
	}
	if filepath.Base(c.commonDir) == ".git" {
		return filepath.Base(filepath.Dir(c.commonDir))
	}
	return filepath.Base(c.root)
}

func safeDiscoveryDirectory(root, rel string) bool {
	if rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	cur := root
	for part := range strings.SplitSeq(filepath.Clean(rel), string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	return true
}

func checkoutWorktrees(ctx context.Context, c *checkout, l *Local) ([]worktreeListing, []string, error) {
	if samePath(c.gitDir, c.commonDir) {
		f, err := os.Open(filepath.Join(c.commonDir, "worktrees"))
		if errors.Is(err, os.ErrNotExist) {
			return []worktreeListing{{path: c.root, branch: c.branch, detached: c.detached}}, nil, nil
		}
		if err != nil {
			return nil, nil, err
		}
		entries, readErr := f.ReadDir(1)
		_ = f.Close()
		if readErr == io.EOF || readErr == nil && len(entries) == 0 {
			return []worktreeListing{{path: c.root, branch: c.branch, detached: c.detached}}, nil, nil
		}
		if readErr != nil && readErr != io.EOF {
			return nil, nil, readErr
		}
	}
	return listWorktrees(ctx, c.root, l)
}

func discover(ctx context.Context, scope string, owner *checkout, l *Local, limits discoveryLimits) (map[string]worktreeRef, []Repository, DiscoveryLimit, error) {
	refs := make(map[string]worktreeRef)
	repoMap := make(map[string]*Repository)
	if owner == nil {
		id := identity(scope + "\x00files")
		refs[id] = worktreeRef{root: scope, repoID: id, kind: KindNone, scopePath: "."}
		repoMap[id] = &Repository{ID: id, Name: filepath.Base(scope), Kind: KindNone, Worktrees: []Worktree{{ID: id, Path: ".", Current: true, State: "clean"}}}
	}
	addCheckout := func(c *checkout, kind Kind) error {
		commonID := identity(c.commonDir)
		repo := repoMap[commonID]
		if repo != nil {
			return nil
		} // One common Git directory is one project; inventory all its checkouts once.
		paths, _, err := checkoutWorktrees(ctx, c, l)
		if err != nil {
			paths = []worktreeListing{{path: c.root, branch: c.branch, detached: c.detached}}
		}
		name := canonicalCheckoutName(c, paths)
		repo = &Repository{ID: commonID, Name: name, Kind: kind}
		repoMap[commonID] = repo
		slices.SortStableFunc(paths, func(a, b worktreeListing) int {
			aMain := mainWorktreePath(a.path, c.commonDir)
			bMain := mainWorktreePath(b.path, c.commonDir)
			if aMain != bMain {
				if aMain {
					return -1
				}
				return 1
			}
			return strings.Compare(a.path, b.path)
		})
		seen := map[string]bool{}
		for _, wt := range paths {
			listedPath, err := filepath.Abs(wt.path)
			if err != nil {
				continue
			}
			ownerContainsScope := owner != nil && samePath(listedPath, owner.root) && within(owner.root, scope)
			if !within(scope, listedPath) && !ownerContainsScope {
				repo.HiddenOutside++
				continue
			}
			root := listedPath
			if ownerContainsScope {
				root = owner.root
			} else {
				safe := samePath(scope, listedPath)
				if !safe {
					safe = safeDiscoveryDirectory(scope, mustRel(scope, listedPath))
				}
				if !safe {
					continue
				}
				root, err = canonicalExisting(listedPath)
				if err != nil {
					continue
				}
			}
			if seen[root] {
				continue
			}
			seen[root] = true
			meta, err := openCheckout(root)
			if err != nil {
				continue
			}
			id := identity(meta.gitDir + "\x00" + root)
			scopeRel := "."
			if !samePath(root, scope) && within(root, scope) {
				scopeRel = mustRel(root, scope)
			}
			ref := worktreeRef{root: root, gitDir: meta.gitDir, commonDir: meta.commonDir, repoID: commonID, kind: kind, scopePath: scopeRel}
			refs[id] = ref
			w := Worktree{ID: id, Path: displayPath(root, scope), Branch: wt.branch, Detached: wt.detached, DetachedHead: func() string {
				if wt.detached {
					if c, e := openCheckout(root); e == nil {
						if b, e := os.ReadFile(filepath.Join(c.gitDir, "HEAD")); e == nil {
							s := strings.TrimSpace(string(b))
							if len(s) > 7 {
								return s[:7]
							}
							return s
						}
					}
				}
				return ""
			}(), Current: owner != nil && samePath(root, owner.root), Linked: !samePath(meta.gitDir, meta.commonDir), State: "unscanned"}
			repo.Worktrees = append(repo.Worktrees, w)
		}
		return nil
	}
	if owner != nil {
		if err := addCheckout(owner, KindPrimary); err != nil {
			return nil, nil, "", err
		}
	}
	repoCount := 0
	var stopReason DiscoveryLimit
	var walkReason DiscoveryLimit
	ruleRoot := scope
	if owner != nil {
		ruleRoot = owner.root
	}
	if owner == nil {
		initial, err := ignoreRulesForPath(scope, scope, scope)
		if err != nil {
			return nil, nil, "", err
		}
		walkReason, err = walkContained(ctx, []walkStart{{path: scope, rel: ".", rules: initial}}, scope, scope, limits.entries, limits.depth, func(path, rel string, entry os.DirEntry, rules ignoreRules) (bool, bool, error) {
			if c, openErr := openCheckout(path); openErr == nil {
				if _, exists := repoMap[identity(c.commonDir)]; exists {
					return false, false, nil
				}
				if repoCount >= limits.repositories {
					stopReason = DiscoveryLimitRepositories
					return false, true, errStopDiscovery
				}
				if err := addCheckout(c, KindNested); err != nil {
					return false, true, err
				}
				repoCount++
				return false, false, nil
			}
			return true, false, nil
		})
		if errors.Is(err, errStopDiscovery) {
			err = nil
		}
		if errors.Is(err, context.Canceled) {
			return nil, nil, "", err
		}
		if errors.Is(err, context.DeadlineExceeded) {
			walkReason = DiscoveryLimitTimeout
		} else if err != nil {
			walkReason = DiscoveryLimitUnavailable
			if workspace := repoMap[identity(scope+"\x00files")]; workspace != nil {
				workspace.Err = "nested repository discovery unavailable: " + boundedErrorText(err, 512)
			}
		}
		var repos []Repository
		for _, repo := range repoMap {
			repos = append(repos, *repo)
		}
		sortDiscoveredRepositories(repos)
		if stopReason != "" {
			walkReason = stopReason
		}
		return refs, repos, walkReason, nil
	}
	var err error
	walkReason, err = discoverNestedMetadata(ctx, scope, ruleRoot, owner, l, limits, func(c *checkout) error {
		if _, exists := repoMap[identity(c.commonDir)]; exists {
			return nil
		}
		if repoCount >= limits.repositories {
			stopReason = DiscoveryLimitRepositories
			return errStopDiscovery
		}
		kind := KindNested
		if c.submodule {
			kind = KindSubmodule
		}
		repoCount++
		return addCheckout(c, kind)
	})
	if errors.Is(err, errStopDiscovery) {
		err = nil
	}
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			return nil, nil, "", err
		case errors.Is(err, context.DeadlineExceeded):
			walkReason = DiscoveryLimitTimeout
			err = nil
		default:
			if owner != nil {
				if primary := repoMap[identity(owner.commonDir)]; primary != nil {
					primary.Err = "nested repository discovery unavailable: " + boundedErrorText(err, 512)
				}
			}
			walkReason = DiscoveryLimitUnavailable
			err = nil
		}
	}

	if errors.Is(err, context.Canceled) {
		return nil, nil, "", err
	}
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return nil, nil, "", err
	}
	var repos []Repository
	for _, r := range repoMap {
		repos = append(repos, *r)
	}
	sortDiscoveredRepositories(repos)
	reason := stopReason
	if reason == "" {
		reason = walkReason
	}
	if errors.Is(err, context.DeadlineExceeded) {
		reason = DiscoveryLimitTimeout
	}
	return refs, repos, reason, nil
}

func discoverNestedMetadata(ctx context.Context, scope, ruleRoot string, owner *checkout, l *Local, limits discoveryLimits, add func(*checkout) error) (DiscoveryLimit, error) {
	if owner != nil {
		return discoverOwnedNested(ctx, owner.root, scope, owner, l, limits, add)
	}
	initialRules, err := ignoreRulesForPath(ruleRoot, scope, scope)
	if err != nil {
		return "", err
	}
	limit, err := walkContained(ctx, []walkStart{{path: scope, rel: ".", rules: initialRules}}, ruleRoot, scope, limits.entries, limits.depth, func(path, rel string, entry os.DirEntry, rules ignoreRules) (bool, bool, error) {
		if c, openErr := openCheckout(path); openErr == nil {
			if owner != nil && samePath(c.commonDir, owner.commonDir) {
				return false, false, nil
			}
			if err := add(c); err != nil {
				return false, true, err
			}
			return false, false, nil
		}
		return true, false, nil
	})
	return limit, err
}

func discoverOwnedNested(ctx context.Context, root, scope string, owner *checkout, l *Local, limits discoveryLimits, add func(*checkout) error) (DiscoveryLimit, error) {
	scopeRel, err := filepath.Rel(root, scope)
	if err != nil || scopeRel == ".." || filepath.IsAbs(scopeRel) || strings.HasPrefix(scopeRel, ".."+string(filepath.Separator)) {
		return DiscoveryLimitUnavailable, ErrOutsideScope
	}
	if scopeRel == "" {
		scopeRel = "."
	}
	candidates := make(map[string]struct{}, min(limits.entries, 4096))
	reason := DiscoveryLimit("")
	addCandidate := func(path string) {
		path = filepath.Clean(filepath.FromSlash(path))
		if path == "." || filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
			return
		}
		if scopeRel != "." && !within(scopeRel, path) {
			return
		}
		discoveryRel := path
		if scopeRel != "." {
			discoveryRel, _ = filepath.Rel(scopeRel, path)
		}
		if pathHasPrunedDiscoveryComponent(discoveryRel) {
			return
		}
		if _, exists := candidates[path]; exists {
			return
		}
		if len(candidates) >= limits.entries {
			reason = DiscoveryLimitEntries
			return
		}
		candidates[path] = struct{}{}
	}
	modules, moduleLimit, moduleErr := submodulePaths(root)
	if moduleLimit != "" {
		reason = moduleLimit
	}
	if moduleErr != nil && reason == "" {
		if errors.Is(moduleErr, ErrTooLarge) {
			reason = DiscoveryLimitBytes
		} else {
			reason = DiscoveryLimitUnavailable
		}
	}
	for _, path := range modules {
		addCandidate(path)
	}
	args := []string{"ls-files", "--others", "--directory", "--exclude-standard", "-z", "--", ":(glob)**/"}
	untracked, err := l.git(ctx, scope, MaxDiscoveryCandidateBytes, args...)
	if errors.Is(err, ErrTooLarge) {
		reason = DiscoveryLimitBytes
	} else if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		if reason == "" {
			reason = DiscoveryLimitUnavailable
		}
	}
	if len(untracked) > 0 && untracked[len(untracked)-1] != 0 && reason == "" {
		reason = DiscoveryLimitUnavailable
	}
	completeRecords := untracked[:bytes.LastIndexByte(untracked, 0)+1]
	for record := range bytes.SplitSeq(completeRecords, []byte{0}) {
		if len(record) > 0 && record[len(record)-1] == '/' {
			rel := filepath.FromSlash(strings.TrimSuffix(string(record), "/"))
			if scopeRel != "." {
				if rel == "." {
					rel = scopeRel
				} else {
					rel = filepath.Join(scopeRel, rel)
				}
			}
			addCandidate(rel)
		}
	}
	paths := make([]string, 0, len(candidates))
	for path := range candidates {
		paths = append(paths, path)
	}
	// Shallow candidates first so the repository cap cannot hide a top-level peer
	// behind many deep candidates that sort earlier lexically.
	slices.SortFunc(paths, func(a, b string) int {
		if da, db := scopeRelDepth(scopeRel, a), scopeRelDepth(scopeRel, b); da != db {
			return da - db
		}
		return strings.Compare(a, b)
	})
	remaining := limits.entries
	var starts []walkStart
	for _, candidate := range paths {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if remaining <= 0 {
			reason = DiscoveryLimitEntries
			break
		}
		remaining--
		if !safeDiscoveryDirectory(root, candidate) {
			continue
		}
		abs := filepath.Join(root, candidate)
		parentRules, err := ignoreRulesForPath(root, filepath.Dir(abs), scope)
		if err != nil {
			return "", err
		}
		if parentRules.matches(root, abs, true) {
			continue
		}
		depth := scopeRelDepth(scopeRel, candidate)
		if depth > limits.depth {
			if reason == "" {
				reason = DiscoveryLimitDepth
			}
			continue
		}
		if c, err := openCheckout(abs); err == nil {
			if !samePath(c.commonDir, owner.commonDir) {
				if err := add(c); err != nil {
					return "", err
				}
			}
			continue
		}
		initial, err := ignoreRulesForPath(root, abs, scope)
		if err != nil {
			return "", err
		}
		starts = append(starts, walkStart{path: abs, rel: candidate, rules: initial, depth: depth})
	}
	// One level-order walk shares the remaining budget across every candidate, so
	// a deep or noisy candidate cannot starve shallower peers.
	walkLimit, err := walkContained(ctx, starts, root, scope, remaining, limits.depth, func(path, rel string, entry os.DirEntry, rules ignoreRules) (bool, bool, error) {
		if c, openErr := openCheckout(path); openErr == nil {
			if samePath(c.commonDir, owner.commonDir) {
				return false, false, nil
			}
			if err := add(c); err != nil {
				return false, true, err
			}
			return false, false, nil
		}
		return true, false, nil
	})
	if err != nil {
		return "", err
	}
	if walkLimit != "" && (reason == "" || walkLimit != DiscoveryLimitDepth) {
		reason = walkLimit
	}
	return reason, nil
}

func scopeRelDepth(scopeRel, candidate string) int {
	if scopeRel == "." || !within(scopeRel, candidate) {
		return pathDepth(candidate)
	}
	return pathDepth(mustRel(scopeRel, candidate))
}

func pathDepth(path string) int {
	clean := filepath.Clean(path)
	if clean == "." {
		return 0
	}
	return len(strings.Split(filepath.ToSlash(clean), "/"))
}

func submodulePaths(root string) ([]string, DiscoveryLimit, error) {
	path := filepath.Join(root, ".gitmodules")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, DiscoveryLimitUnavailable, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, DiscoveryLimitUnavailable, ErrOutsideScope
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, DiscoveryLimitUnavailable, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, MaxDiscoveryCandidateBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, DiscoveryLimitUnavailable, readErr
	}
	if closeErr != nil {
		return nil, DiscoveryLimitUnavailable, closeErr
	}
	if len(data) > MaxDiscoveryCandidateBytes {
		return nil, DiscoveryLimitBytes, ErrTooLarge
	}
	paths, err := parseSubmodulePaths(data)
	if err != nil {
		return nil, DiscoveryLimitUnavailable, err
	}
	return paths, "", nil
}

// parseSubmodulePaths reads [submodule] sections with go-git config.ReadConfig's
// rules without linking its full config package: a later section with the same
// name replaces an earlier one, and names or paths go-git rejects are dropped.
// Empty paths are returned as-is and filtered here.
func parseSubmodulePaths(data []byte) ([]string, error) {
	raw := gitconfig.New()
	if err := gitconfig.NewDecoder(bytes.NewReader(data)).Decode(raw); err != nil {
		return nil, err
	}
	byName := make(map[string]string)
	for _, sub := range raw.Section("submodule").Subsections {
		path, url := sub.Option("path"), sub.Option("url")
		// go-git's Validate skips bad names and, only when a URL is set, dot-dot paths.
		if !validSubmoduleName(sub.Name) || (url != "" && submoduleDotDotPath.MatchString(path)) {
			continue
		}
		byName[sub.Name] = path
	}
	paths := make([]string, 0, len(byName))
	for _, path := range byName {
		if path != "" {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	return paths, nil
}

var submoduleDotDotPath = regexp.MustCompile(`(^|[/\\])\.\.([/\\]|$)`)

// validSubmoduleName mirrors go-git's check: no empty or "." name, no segment that is
// ".." on HFS+ or NTFS, no NUL, no leading or trailing separator, and no drive prefix.
func validSubmoduleName(name string) bool {
	if name == "" || name == "." {
		return false
	}
	for _, seg := range strings.FieldsFunc(name, isSubmoduleSep) {
		if hfsDotDot(seg) || ntfsDotDot(seg) {
			return false
		}
	}
	if strings.ContainsRune(name, 0) || isSubmoduleSep(rune(name[0])) || isSubmoduleSep(rune(name[len(name)-1])) {
		return false
	}
	return len(name) < 2 || name[1] != ':'
}

func isSubmoduleSep(r rune) bool { return r == '/' || r == '\\' }

// hfsDotDot reports whether seg is ".." once HFS+ ignorable code points are dropped.
func hfsDotDot(seg string) bool {
	return strings.Map(func(r rune) rune {
		if hfsIgnoredRune(r) {
			return -1
		}
		return r
	}, seg) == ".."
}

func hfsIgnoredRune(r rune) bool {
	switch r {
	case 0x200c, 0x200d, 0x200e, 0x200f, 0x202a, 0x202b, 0x202c, 0x202d, 0x202e,
		0x206a, 0x206b, 0x206c, 0x206d, 0x206e, 0x206f, 0xfeff:
		return true
	}
	return false
}

// ntfsDotDot reports whether seg is ".." followed only by spaces, periods, or an ADS colon.
func ntfsDotDot(seg string) bool {
	if len(seg) < 2 || seg[0] != '.' || seg[1] != '.' {
		return false
	}
	for i := 2; i < len(seg); i++ {
		switch seg[i] {
		case ':':
			return true
		case ' ', '.':
		default:
			return false
		}
	}
	return true
}

type checkout struct {
	root, gitDir, commonDir, branch string
	detached, submodule             bool
}

type worktreeListing struct {
	path, branch string
	detached     bool
}

func owningCheckout(scope string) (*checkout, error) {
	for d := scope; ; d = filepath.Dir(d) {
		if c, e := openCheckout(d); e == nil {
			return c, nil
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	return nil, nil
}
func openCheckout(root string) (*checkout, error) {
	meta := filepath.Join(root, ".git")
	st, err := os.Lstat(meta)
	if err != nil {
		return nil, err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return nil, ErrOutsideScope
	}
	gitDir := meta
	submodule := false
	if !st.IsDir() {
		data, err := os.ReadFile(meta)
		if err != nil || len(data) > 4096 {
			return nil, ErrNotFound
		}
		line := strings.TrimSpace(string(data))
		target, ok := strings.CutPrefix(line, "gitdir: ")
		if !ok {
			return nil, ErrNotFound
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(root, target)
		}
		gitDir, err = filepath.EvalSymlinks(target)
		if err != nil {
			return nil, err
		}
		submodule = true
	}
	if !isGitDir(gitDir) {
		return nil, ErrNotFound
	}
	common := gitDir
	if data, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		v := strings.TrimSpace(string(data))
		if !filepath.IsAbs(v) {
			v = filepath.Join(gitDir, v)
		}
		if resolved, e := filepath.EvalSymlinks(v); e == nil {
			common = resolved
		}
	}
	if filepath.Base(common) == ".git" {
		submodule = false
	}
	branch, detached := readBranch(filepath.Join(gitDir, "HEAD"))
	return &checkout{root: root, gitDir: gitDir, commonDir: common, branch: branch, detached: detached, submodule: submodule}, nil
}
func isGitDir(path string) bool {
	_, headErr := os.Stat(filepath.Join(path, "HEAD"))
	if headErr != nil {
		return false
	}
	if _, commonErr := os.Stat(filepath.Join(path, "commondir")); commonErr == nil {
		return true // linked worktree gitdirs keep objects/refs in their common dir
	}
	_, objectsErr := os.Stat(filepath.Join(path, "objects"))
	_, refsErr := os.Stat(filepath.Join(path, "refs"))
	return objectsErr == nil && refsErr == nil
}
func readBranch(path string) (string, bool) {
	b, e := os.ReadFile(path)
	if e != nil {
		return "", true
	}
	s := strings.TrimSpace(string(b))
	if v, ok := strings.CutPrefix(s, "ref: refs/heads/"); ok {
		return v, false
	}
	return "", true
}
func listWorktrees(ctx context.Context, root string, l *Local) ([]worktreeListing, []string, error) {
	out, err := l.git(ctx, root, 8<<20, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, nil, err
	}
	fields := bytes.Split(out, []byte{0})
	var result []worktreeListing
	var current *worktreeListing
	for _, field := range fields {
		if len(field) == 0 {
			if current != nil {
				result = append(result, *current)
				current = nil
			}
			continue
		}
		s := string(field)
		if v, ok := strings.CutPrefix(s, "worktree "); ok {
			if current != nil {
				result = append(result, *current)
			}
			current = &worktreeListing{path: v}
			continue
		}
		if current == nil {
			continue
		}
		if v, ok := strings.CutPrefix(s, "branch refs/heads/"); ok {
			current.branch = v
		}
		if s == "detached" {
			current.detached = true
		}
	}
	if current != nil {
		result = append(result, *current)
	}
	if len(result) == 0 {
		return nil, nil, ErrNotFound
	}
	return result, nil, nil
}
func status(ctx context.Context, root, scopeRel string, l *Local) (string, []FileChange, LoadState, error) {
	args := []string{"-c", "core.quotepath=false", "status", "--porcelain=v2", "-z", "--untracked-files=all"}
	if scopeRel != "." {
		args = append(args, "--", ":(literal)"+filepath.ToSlash(scopeRel))
	}
	type commandResult struct {
		data []byte
		err  error
	}
	type numstatResult struct {
		counts map[string]numstatCount
		err    error
	}
	statusDone := make(chan commandResult, 1)
	stagedDone := make(chan numstatResult, 1)
	unstagedDone := make(chan numstatResult, 1)
	go func() {
		data, err := l.git(ctx, root, MaxStatusBytes, args...)
		statusDone <- commandResult{data: data, err: err}
	}()
	go func() {
		counts, err := diffNumstat(ctx, root, scopeRel, true, l)
		stagedDone <- numstatResult{counts: counts, err: err}
	}()
	go func() {
		counts, err := diffNumstat(ctx, root, scopeRel, false, l)
		unstagedDone <- numstatResult{counts: counts, err: err}
	}()
	loadedStatus := <-statusDone
	loadedStaged := <-stagedDone
	loadedUnstaged := <-unstagedDone
	if loadedStatus.err != nil {
		return "unavailable", nil, StateError, loadedStatus.err
	}
	files := parseStatus(loadedStatus.data)
	countsState := StateReady
	if loadedStaged.err != nil || loadedUnstaged.err != nil {
		countsState = StateError
	}
	staged := loadedStaged.counts
	unstaged := loadedUnstaged.counts
	countCtx, cancelCounts := context.WithTimeout(ctx, CountTimeout)
	defer cancelCounts()
	var untrackedBytes, untrackedFiles int
	untrackedPaths := make([]string, 0)
	for _, file := range files {
		if file.Group == GroupUntracked && len(untrackedPaths) < MaxCountFiles {
			untrackedPaths = append(untrackedPaths, filepath.ToSlash(file.Path))
		}
	}
	attrs := map[string]string{}
	if len(untrackedPaths) > 0 {
		var attrErr error
		attrs, attrErr = untrackedDiffAttributes(countCtx, root, untrackedPaths, l)
		if attrErr != nil {
			countsState = StateError
		}
	}
	var rootDir *os.Root
	if len(files) > 0 {
		rootDir, _ = os.OpenRoot(root)
		if rootDir != nil {
			defer rootDir.Close()
		}
	}
	for i := range files {
		f := &files[i]
		f.Added, f.Removed = -1, -1
		switch f.Group {
		case GroupStaged:
			applyNumstat(f, staged, &countsState)
		case GroupUnstaged:
			applyNumstat(f, unstaged, &countsState)
		case GroupUntracked:
			untrackedFiles++
			if untrackedFiles > MaxCountFiles || rootDir == nil || countCtx.Err() != nil {
				countsState = StateError
				continue
			}
			diffAttr, ok := attrs[filepath.ToSlash(f.Path)]
			if !ok || diffAttr == "unknown" {
				countsState = StateError
				continue
			}
			added, binary, n, err := countUntracked(countCtx, rootDir, f.Path, MaxCountBytes-untrackedBytes, diffAttr)
			untrackedBytes += n
			if err != nil {
				countsState = StateError
				continue
			}
			f.Binary = binary
			if !binary {
				f.Added, f.Removed = added, 0
			}
		case GroupConflicts:
			// A conflicted path has no single authoritative two-sided line delta.
			countsState = StateError
		}
	}
	if scopeRel != "." {
		for i := range files {
			files[i].Path = trimScopePath(files[i].Path, scopeRel)
			files[i].OldPath = trimScopePath(files[i].OldPath, scopeRel)
		}
		files = slices.DeleteFunc(files, func(f FileChange) bool { return f.Path == "" })
	}
	return stateFor(files), files, countsState, nil
}

type numstatCount struct {
	added, removed int
	binary         bool
}

// untrackedDiffAttributes asks Git for one bounded batch of diff attributes.
// Attribute drivers are reported unknown because their diff.<driver>.binary
// configuration is not loaded here; numeric counts remain unavailable for them.
func untrackedDiffAttributes(ctx context.Context, root string, paths []string, l *Local) (map[string]string, error) {
	var input bytes.Buffer
	for _, path := range paths {
		input.WriteString(path)
		input.WriteByte(0)
	}
	data, err := l.gitInput(ctx, root, MaxStatusBytes, input.Bytes(), "check-attr", "-z", "--stdin", "diff")
	if err != nil {
		return nil, err
	}
	fields := bytes.Split(data, []byte{0})
	out := make(map[string]string, len(paths))
	for i := 0; i+2 < len(fields); i += 3 {
		if len(fields[i]) == 0 {
			continue
		}
		path, attr, value := string(fields[i]), string(fields[i+1]), string(fields[i+2])
		if attr != "diff" {
			continue
		}
		switch value {
		case "unset", "set", "unspecified":
			out[filepath.ToSlash(path)] = value
		default:
			out[filepath.ToSlash(path)] = "unknown"
		}
	}
	return out, nil
}

// diffNumstat asks Git for line-count metadata only. It never requests a patch
// or retains diff contents; output and execution time use the normal Git bounds.
func diffNumstat(ctx context.Context, root, scopeRel string, staged bool, l *Local) (map[string]numstatCount, error) {
	args := []string{"diff"}
	if staged {
		args = append(args, "--cached")
	}
	args = append(args, "--no-ext-diff", "--no-textconv", "--numstat", "-z", "-M")
	if scopeRel != "." {
		args = append(args, "--", ":(literal)"+filepath.ToSlash(scopeRel))
	}
	data, err := l.git(ctx, root, MaxStatusBytes, args...)
	if err != nil {
		return nil, err
	}
	return parseNumstat(data), nil
}

// parseNumstat handles Git's NUL-delimited rename form, including unusual paths.
func parseNumstat(data []byte) map[string]numstatCount {
	out := make(map[string]numstatCount)
	for len(data) > 0 {
		end := bytes.IndexByte(data, 0)
		if end < 0 {
			break
		}
		field := data[:end]
		data = data[end+1:]
		parts := bytes.SplitN(field, []byte{'\t'}, 3)
		if len(parts) != 3 {
			continue
		}
		added, removed := -1, -1
		binary := string(parts[0]) == "-" || string(parts[1]) == "-"
		if !binary {
			added, _ = strconv.Atoi(string(parts[0]))
			removed, _ = strconv.Atoi(string(parts[1]))
		}
		path := string(parts[2])
		if path == "" {
			oldEnd := bytes.IndexByte(data, 0)
			if oldEnd < 0 {
				break
			}
			data = data[oldEnd+1:]
			newEnd := bytes.IndexByte(data, 0)
			if newEnd < 0 {
				break
			}
			path = string(data[:newEnd])
			data = data[newEnd+1:]
		}
		out[filepath.ToSlash(path)] = numstatCount{added: added, removed: removed, binary: binary}
	}
	return out
}

func applyNumstat(file *FileChange, counts map[string]numstatCount, state *LoadState) {
	count, ok := counts[filepath.ToSlash(file.Path)]
	if !ok {
		*state = StateError
		return
	}
	file.Added, file.Removed, file.Binary = count.added, count.removed, count.binary
}

func countUntracked(ctx context.Context, root *os.Root, rel string, budget int, diffAttr string) (int, bool, int, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, 0, err
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return 0, false, 0, ErrOutsideScope
	}
	for parent := filepath.Dir(clean); parent != "." && parent != string(filepath.Separator); parent = filepath.Dir(parent) {
		if err := ctx.Err(); err != nil {
			return 0, false, 0, err
		}
		parentInfo, err := root.Lstat(parent)
		if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
			return 0, false, 0, ErrOutsideScope
		}
	}
	info, err := root.Lstat(clean)
	if err != nil {
		return 0, false, 0, err
	}
	if diffAttr == "unset" {
		return 0, true, 0, nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := root.Readlink(clean)
		if err != nil {
			return 0, false, 0, err
		}
		if len(target) > min(MaxCountFileBytes, budget) {
			return 0, false, len(target), ErrTooLarge
		}
		return countTextLines([]byte(target)), false, len(target), nil
	}
	if !info.Mode().IsRegular() {
		return 0, false, 0, ErrOutsideScope
	}
	limit := min(MaxCountFileBytes, budget)
	if limit < 0 || info.Size() > int64(limit) {
		return 0, false, 0, ErrTooLarge
	}
	f, err := root.Open(clean)
	if err != nil {
		return 0, false, 0, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return 0, false, 0, ErrOutsideScope
	}
	var data []byte
	buf := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return 0, false, len(data), err
		}
		if len(data) > limit {
			return 0, false, len(data), ErrTooLarge
		}
		n, readErr := f.Read(buf)
		if n > 0 {
			if len(data)+n > limit {
				return 0, false, len(data) + n, ErrTooLarge
			}
			data = append(data, buf[:n]...)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return 0, false, len(data), readErr
		}
	}
	if diffAttr != "set" && bytes.IndexByte(data[:min(8_000, len(data))], 0) >= 0 {
		return 0, true, len(data), nil
	}
	return countTextLines(data), false, len(data), nil
}

func countTextLines(data []byte) int {
	lines := bytes.Count(data, []byte{'\n'})
	if len(data) > 0 && data[len(data)-1] != '\n' {
		lines++
	}
	return lines
}

func parseStatus(data []byte) []FileChange {
	fields := bytes.Split(data, []byte{0})
	var out []FileChange
	for i := 0; i < len(fields); i++ {
		if len(fields[i]) == 0 {
			continue
		}
		line := string(fields[i])
		switch line[0] {
		case '#':
			continue
		case '?':
			p := strings.TrimPrefix(line, "? ")
			out = append(out, FileChange{Path: p, Group: GroupUntracked, Status: "??", Added: -1, Removed: -1})
		case 'u':
			parts := strings.SplitN(line, " ", 11)
			if len(parts) >= 11 {
				out = append(out, FileChange{Path: parts[10], Group: GroupConflicts, Status: parts[1], Added: -1, Removed: -1})
			}
		case '1':
			parts := strings.SplitN(line, " ", 9)
			if len(parts) < 9 {
				continue
			}
			out = appendStatusXY(out, parts[8], parts[1])
		case '2':
			parts := strings.SplitN(line, " ", 10)
			if len(parts) < 10 {
				continue
			}
			old := ""
			if i+1 < len(fields) {
				i++
				old = string(fields[i])
			}
			start := len(out)
			out = appendStatusXY(out, parts[9], parts[1])
			for j := start; j < len(out); j++ {
				out[j].OldPath = old
			}
		}
	}
	slices.SortFunc(out, func(a, b FileChange) int { return strings.Compare(a.Path, b.Path) })
	return out
}
func appendStatusXY(out []FileChange, path, xy string) []FileChange {
	if strings.Contains(xy, "U") {
		return append(out, FileChange{Path: path, Group: GroupConflicts, Status: xy, Added: -1, Removed: -1})
	}
	staged := len(xy) > 0 && xy[0] != '.'
	unstaged := len(xy) > 1 && xy[1] != '.'
	if staged {
		out = append(out, FileChange{Path: path, Group: GroupStaged, Status: string(xy[0]), Added: -1, Removed: -1})
	}
	if unstaged {
		out = append(out, FileChange{Path: path, Group: GroupUnstaged, Status: string(xy[1]), Added: -1, Removed: -1})
	}
	return out
}
func stateFor(files []FileChange) string {
	if len(files) == 0 {
		return "clean"
	}
	return "changed"
}

func (l *Local) changeDocument(ctx context.Context, ref worktreeRef, key DocumentKey, abs string) (Document, error) {
	doc := Document{Key: key, State: StateReady}
	rel, err := filepath.Rel(ref.root, abs)
	if err != nil || !scopeAllows(ref, rel) {
		return doc, ErrOutsideScope
	}
	var args []string
	switch key.Group {
	case GroupUntracked:
		return l.fileDocument(ctx, ref, key, abs)
	case GroupStaged:
		args = []string{"diff", "--cached"}
	case GroupUnstaged:
		args = []string{"diff"}
	case GroupConflicts:
		args = []string{"diff", "--cc"}
	default:
		args = []string{"diff"}
	}
	args = append(args, "--no-ext-diff", "--no-textconv", "--no-color", "--unified=3", "--", ":(literal)"+filepath.ToSlash(rel))
	data, err := l.git(ctx, ref.root, MaxDiffBytes, args...)
	if err != nil {
		return doc, err
	}
	doc = parsePatch(key, data)
	return doc, nil
}
func (l *Local) fileDocument(ctx context.Context, ref worktreeRef, key DocumentKey, abs string) (Document, error) {
	doc := Document{Key: key, State: StateReady}
	st, err := os.Lstat(abs)
	if err != nil {
		return doc, err
	}
	if !st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0 {
		return doc, ErrOutsideScope
	}
	f, err := os.Open(abs)
	if err != nil {
		return doc, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxDiffBytes+1))
	if err != nil {
		return doc, err
	}
	if len(data) > MaxDiffBytes {
		data = data[:MaxDiffBytes]
		doc.Truncated = true
	}
	if bytes.IndexByte(data, 0) >= 0 {
		doc.Binary = true
		doc.Note = "Binary file; contents are not displayed."
		return doc, nil
	}
	lines := bytes.Split(data, []byte{'\n'})
	if len(lines) > MaxDiffLines {
		lines = lines[:MaxDiffLines]
		doc.Truncated = true
	}
	for i, line := range lines {
		if i == len(lines)-1 && len(line) == 0 {
			break
		}
		kind := DiffContext
		oldNum := i + 1
		if key.Mode == "changes" && key.Group == GroupUntracked {
			kind = DiffAdd
			oldNum = 0
		}
		doc.Lines = append(doc.Lines, DocumentLine{Kind: kind, Old: oldNum, New: i + 1, Text: string(line)})
	}
	if key.Mode == "files" {
		doc.Indicators, _ = l.fileIndicators(ctx, ref, abs, data)
	}
	return doc, nil
}

func (l *Local) fileIndicators(ctx context.Context, ref worktreeRef, abs string, current []byte) ([]DocumentIndicator, error) {
	if ref.gitDir == "" {
		return nil, nil
	}
	rel, err := filepath.Rel(ref.root, abs)
	if err != nil || !scopeAllows(ref, rel) {
		return nil, ErrOutsideScope
	}
	gitPath := filepath.ToSlash(rel)
	tracked, err := l.git(ctx, ref.root, MaxStatusBytes, "ls-files", "-z", "--", ":(literal)"+gitPath)
	if err != nil {
		return nil, err
	}
	if len(tracked) == 0 {
		// An untracked file (or a path not present in HEAD) has no baseline.
		return addedIndicators(current), nil
	}
	if _, err := l.git(ctx, ref.root, 128, "rev-parse", "--verify", "HEAD"); err != nil {
		return addedIndicators(current), nil
	}
	patch, err := l.git(ctx, ref.root, MaxDiffBytes, "diff", "--no-ext-diff", "--no-textconv", "--no-color", "--unified=0", "HEAD", "--", ":(literal)"+gitPath)
	if err != nil {
		return nil, err
	}
	indicators := parseWholeFileIndicators(patch)
	if len(current) == 0 {
		for i := range indicators {
			if indicators[i].Kind == ChangeDeleted {
				indicators[i].Line = 0
			}
		}
	}
	return indicators, nil
}

func addedIndicators(data []byte) []DocumentIndicator {
	lines := min(textLineCount(data), MaxDiffLines)
	indicators := make([]DocumentIndicator, 0, lines)
	for line := 1; line <= lines; line++ {
		indicators = append(indicators, DocumentIndicator{Line: line, Kind: ChangeAdded})
	}
	if lines == 0 && len(data) == 0 {
		return []DocumentIndicator{{Line: 0, Kind: ChangeAdded}}
	}
	return indicators
}

func textLineCount(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	lines := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		lines++
	}
	return lines
}

func parseWholeFileIndicators(patch []byte) []DocumentIndicator {
	byLine := make(map[int]ChangeKind)
	lines := strings.Split(string(patch), "\n")
	for i := 0; i < len(lines); {
		if !strings.HasPrefix(lines[i], "@@ ") {
			i++
			continue
		}
		oldCount, newStart, newCount, ok := parseUnifiedHunkRange(lines[i])
		i++
		if !ok {
			continue
		}
		var removed, added int
		for i < len(lines) && !strings.HasPrefix(lines[i], "@@ ") {
			line := lines[i]
			if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
				removed++
			} else if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
				added++
			}
			i++
		}
		// Trust the hunk counts rather than patch text if Git omitted a hunk body.
		removed = max(removed, oldCount)
		added = max(added, newCount)
		paired := min(removed, added)
		for offset := 0; offset < paired; offset++ {
			byLine[newStart+offset] = ChangeModified
		}
		for offset := paired; offset < added; offset++ {
			byLine[newStart+offset] = ChangeAdded
		}
		if removed > added {
			anchor := max(1, newStart+paired)
			if _, occupied := byLine[anchor]; !occupied {
				byLine[anchor] = ChangeDeleted
			}
		}
	}
	lineNumbers := make([]int, 0, len(byLine))
	for line := range byLine {
		lineNumbers = append(lineNumbers, line)
	}
	slices.Sort(lineNumbers)
	out := make([]DocumentIndicator, 0, len(lineNumbers))
	for _, line := range lineNumbers {
		out = append(out, DocumentIndicator{Line: line, Kind: byLine[line]})
	}
	return out
}

func parseUnifiedHunkRange(line string) (oldCount, newStart, newCount int, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 3 || !strings.HasPrefix(fields[1], "-") || !strings.HasPrefix(fields[2], "+") {
		return 0, 0, 0, false
	}
	parse := func(field string) (int, int, bool) {
		value := field[1:]
		parts := strings.SplitN(value, ",", 2)
		start, err := strconv.Atoi(parts[0])
		if err != nil {
			return 0, 0, false
		}
		count := 1
		if len(parts) == 2 {
			count, err = strconv.Atoi(parts[1])
			if err != nil {
				return 0, 0, false
			}
		}
		return start, count, true
	}
	_, oldCount, oldOK := parse(fields[1])
	newStart, newCount, newOK := parse(fields[2])
	return oldCount, newStart, newCount, oldOK && newOK
}

func parsePatch(key DocumentKey, data []byte) Document {
	doc := Document{Key: key, State: StateReady}
	if len(data) == 0 {
		return doc
	}
	if bytes.Contains(data, []byte("GIT binary patch")) || bytes.Contains(data, []byte("Binary files ")) {
		doc.Binary = true
		doc.Note = "Binary file changes are not displayed."
		return doc
	}
	scan := bufio.NewScanner(bytes.NewReader(data))
	scan.Buffer(make([]byte, 4096), MaxDiffBytes)
	oldNum, newNum := 0, 0
	lines := 0
	inHunk := false
	var header strings.Builder
	for scan.Scan() {
		s := scan.Text()
		if strings.HasPrefix(s, "@@ ") {
			if doc.PatchHeader == "" {
				doc.PatchHeader = strings.TrimRight(header.String(), "\n")
			}
			inHunk = true
			oldNum, newNum = parseHunkNumbers(s)
			if lines >= MaxDiffLines {
				doc.Truncated = true
				break
			}
			lines++
			doc.Lines = append(doc.Lines, DocumentLine{Kind: DiffHunk, Old: oldNum, New: newNum, Text: s})
			continue
		}
		if !inHunk {
			header.WriteString(s)
			header.WriteByte('\n')
			continue
		}
		if lines >= MaxDiffLines {
			doc.Truncated = true
			break
		}
		lines++
		line := DocumentLine{Text: s}
		switch {
		case strings.HasPrefix(s, "+"):
			line.Kind = DiffAdd
			line.New = newNum
			newNum++
			line.Text = strings.TrimPrefix(s, "+")
		case strings.HasPrefix(s, "-"):
			line.Kind = DiffRemove
			line.Old = oldNum
			oldNum++
			line.Text = strings.TrimPrefix(s, "-")
		case strings.HasPrefix(s, "\\"):
			line.Kind = DiffNote
		default:
			line.Kind = DiffContext
			line.Old = oldNum
			line.New = newNum
			oldNum++
			newNum++
			line.Text = strings.TrimPrefix(s, " ")
		}
		doc.Lines = append(doc.Lines, line)
	}
	if scan.Err() != nil {
		doc.Truncated = true
	}
	return doc
}

func parseHunkNumbers(s string) (int, int) {
	var oldStart, newStart int
	if fields := strings.Fields(s); len(fields) >= 3 {
		if old, ok := strings.CutPrefix(fields[1], "-"); ok {
			oldStart, _ = strconv.Atoi(strings.Split(old, ",")[0])
		}
		if next, ok := strings.CutPrefix(fields[2], "+"); ok {
			newStart, _ = strconv.Atoi(strings.Split(next, ",")[0])
		}
	}
	return oldStart, newStart
}

func (l *Local) gitInput(ctx context.Context, dir string, maxBytes int, input []byte, args ...string) ([]byte, error) {
	select {
	case l.processes <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-l.processes }()
	cmdCtx, cancel := context.WithTimeout(ctx, GitTimeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "git", append([]string{"-C", dir, "-c", "core.fsmonitor=false", "-c", "core.pager=cat"}, args...)...)
	cmd.Env = gitEnv()
	cmd.Stdin = bytes.NewReader(input)
	var stdout limitedBuffer
	stdout.limit = maxBytes
	cmd.Stdout = &stdout
	var stderr limitedBuffer
	stderr.limit = 64 << 10
	cmd.Stderr = &stderr
	err := cmd.Run()
	if stdout.overflow {
		return stdout.Bytes(), ErrTooLarge
	}
	if cmdCtx.Err() != nil {
		return nil, cmdCtx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(stderr.Bytes())), err)
	}
	return stdout.Bytes(), nil
}

func (l *Local) git(ctx context.Context, dir string, maxBytes int, args ...string) ([]byte, error) {
	select {
	case l.processes <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-l.processes }()
	cmdCtx, cancel := context.WithTimeout(ctx, GitTimeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "git", append([]string{"-C", dir, "-c", "core.fsmonitor=false", "-c", "core.pager=cat"}, args...)...)
	cmd.Env = gitEnv()
	var stdout limitedBuffer
	stdout.limit = maxBytes
	cmd.Stdout = &stdout
	var stderr limitedBuffer
	stderr.limit = 64 << 10
	cmd.Stderr = &stderr
	err := cmd.Run()
	if stdout.overflow {
		return stdout.Bytes(), ErrTooLarge
	}
	if cmdCtx.Err() != nil {
		return nil, cmdCtx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(stderr.Bytes())), err)
	}
	return stdout.Bytes(), nil
}

type limitedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	left := b.limit - b.buffer.Len()
	if left <= 0 {
		b.overflow = true
		return 0, ErrTooLarge
	}
	if len(p) > left {
		b.overflow = true
		_, _ = b.buffer.Write(p[:left])
		return left, ErrTooLarge
	}
	return b.buffer.Write(p)
}

func (b *limitedBuffer) ReadFrom(r io.Reader) (int64, error) {
	var total int64
	buf := make([]byte, 32<<10)
	for {
		left := b.limit - b.buffer.Len()
		if left < len(buf) {
			buf = buf[:max(1, left+1)]
		}
		n, readErr := r.Read(buf)
		if n > 0 {
			written, writeErr := b.Write(buf[:n])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
		}
		if readErr == io.EOF {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
		if n == 0 {
			return total, io.ErrNoProgress
		}
	}
}

func (b *limitedBuffer) Len() int      { return b.buffer.Len() }
func (b *limitedBuffer) Bytes() []byte { return b.buffer.Bytes() }

func gitEnv() []string {
	env := os.Environ()
	blocked := map[string]bool{"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true, "GIT_EXTERNAL_DIFF": true, "GIT_PAGER": true, "GIT_OPTIONAL_LOCKS": true, "GIT_TERMINAL_PROMPT": true, "GIT_PAGER_IN_USE": true}
	out := make([]string, 0, len(env)+4)
	for _, v := range env {
		k, _, _ := strings.Cut(v, "=")
		if !blocked[k] && !strings.HasPrefix(k, "GIT_CONFIG_") && k != "GIT_CONFIG" {
			out = append(out, v)
		}
	}
	return append(out, "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
}

func scopedPath(ref worktreeRef, path string) (abs, checkoutRel, publicRel string, err error) {
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == "." {
		clean = ""
	}
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", "", "", ErrOutsideScope
	}
	publicRel = clean
	checkoutRel = clean
	if ref.scopePath != "." {
		checkoutRel = filepath.Join(ref.scopePath, clean)
	}
	abs, checkoutRel, err = safePath(ref.root, checkoutRel)
	if err != nil || !scopeAllows(ref, checkoutRel) {
		return "", "", "", ErrOutsideScope
	}
	return abs, checkoutRel, publicRel, nil
}

func trimScopePath(path, scope string) string {
	path = filepath.Clean(filepath.FromSlash(path))
	if path == scope {
		return ""
	}
	if within(scope, path) {
		rel, _ := filepath.Rel(scope, path)
		return rel
	}
	return path
}

func safePath(root, path string) (string, string, error) {
	if strings.ContainsRune(path, 0) {
		return "", "", ErrOutsideScope
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", "", ErrOutsideScope
	}
	abs := filepath.Join(root, clean)
	if !within(root, abs) {
		return "", "", ErrOutsideScope
	}
	for cur := abs; within(root, cur); cur = filepath.Dir(cur) {
		st, e := os.Lstat(cur)
		if e == nil && st.Mode()&os.ModeSymlink != 0 {
			return "", "", ErrOutsideScope
		}
		if cur == root {
			break
		}
	}
	return abs, clean, nil
}
func scopeAllows(ref worktreeRef, rel string) bool {
	if ref.scopePath == "." {
		return true
	}
	rel = filepath.Clean(rel)
	return samePath(rel, ref.scopePath) || within(ref.scopePath, rel)
}
func within(root, path string) bool {
	r, e := filepath.Rel(root, path)
	if e != nil {
		return false
	}
	return r == "." || r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}
func samePath(a, b string) bool {
	aa, _ := filepath.Abs(a)
	bb, _ := filepath.Abs(b)
	return filepath.Clean(aa) == filepath.Clean(bb)
}
func canonicalExisting(path string) (string, error) {
	abs, e := filepath.Abs(path)
	if e != nil {
		return "", e
	}
	return filepath.EvalSymlinks(abs)
}
func identity(value string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(value)))
	return hex.EncodeToString(sum[:12])
}
func displayPath(path, scope string) string {
	if samePath(path, scope) {
		return "."
	}
	if within(scope, path) {
		r, _ := filepath.Rel(scope, path)
		return filepath.ToSlash(r)
	}
	if within(path, scope) {
		return filepath.Base(path)
	}
	return filepath.Base(path)
}
func mustRel(root, path string) string {
	r, e := filepath.Rel(root, path)
	if e != nil {
		return "."
	}
	return r
}
func cloneSnapshot(in Snapshot) Snapshot {
	out := in
	out.Repositories = slices.Clone(in.Repositories)
	for i := range out.Repositories {
		out.Repositories[i].Worktrees = slices.Clone(in.Repositories[i].Worktrees)
		for j := range out.Repositories[i].Worktrees {
			out.Repositories[i].Worktrees[j].Files = slices.Clone(in.Repositories[i].Worktrees[j].Files)
		}
	}
	return out
}
func cloneDocument(in Document) Document {
	out := in
	out.Lines = slices.Clone(in.Lines)
	out.Indicators = slices.Clone(in.Indicators)
	return out
}
func documentBytes(doc Document) int {
	n := len(doc.Note) + len(doc.Err) + len(doc.PatchHeader)
	for _, line := range doc.Lines {
		n += len(line.Text) + 24
	}
	return n
}
