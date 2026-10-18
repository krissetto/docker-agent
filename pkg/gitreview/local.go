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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	MaxDepth            = 8
	MaxEntries          = 20_000
	MaxRepositories     = 100
	MaxStatusBytes      = 8 << 20
	MaxDirectoryItems   = 1_000
	MaxDiffBytes        = 2 << 20
	MaxDiffLines        = 10_000
	MaxCacheBytes       = 32 << 20
	MaxStatusCacheBytes = 8 << 20
	DiscoveryTimeout    = 10 * time.Second
	GitTimeout          = 5 * time.Second
	statusCacheTTL      = 300 * time.Millisecond
)

var heavyDirectories = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true,
	"build": true, ".cache": true, ".venv": true, "venv": true,
	"__pycache__": true, ".gradle": true,
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
	processes         chan struct{}
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
	generation uint64
	revision   uint64
	fetchedAt  time.Time
	state      string
	files      []FileChange
	bytes      int
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
	ctx, cancel := context.WithTimeout(ctx, DiscoveryTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	owner, _ := owningCheckout(l.scope)
	refs, repos, partial, err := discover(ctx, l.scope, owner, l)
	if err != nil {
		return Snapshot{}, err
	}
	l.mu.Lock()
	l.generation++
	generation := l.generation
	note := ""
	if partial {
		note = "Discovery limited; some repositories were not scanned"
	}
	l.snapshot = Snapshot{Generation: generation, CapturedAt: time.Now(), Scope: Scope{Label: l.label, Note: note}, Repositories: repos}
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
	out := cloneSnapshot(l.snapshot)
	l.mu.Unlock()
	return out, nil
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
	flightKey := documentFlightKey{documentKey: key, generation: l.generation, revision: l.worktreeRevisions[key.WorktreeID]}
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
		if err == nil && flightKey.generation == l.generation && flightKey.revision == l.worktreeRevisions[key.WorktreeID] {
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
	var files []FileChange
	var err error
	if ref.gitDir != "" {
		if cached {
			state, files = statusResult.state, cloneFiles(statusResult.files)
		} else {
			state, files, err = status(ctx, ref.root, ref.scopePath, l)
		}
	}
	wt := Worktree{ID: worktreeID, Path: filepath.Base(ref.root), Current: false, Linked: !samePath(ref.gitDir, ref.commonDir), State: state, Files: files}
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
			l.storeStatusCacheLocked(worktreeID, cachedStatus{generation: generation, revision: revision, fetchedAt: time.Now(), state: state, files: cloneFiles(files)})
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

func discover(ctx context.Context, scope string, owner *checkout, l *Local) (map[string]worktreeRef, []Repository, bool, error) {
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
		repo = &Repository{ID: commonID, Name: filepath.Base(c.root), Kind: kind}
		repoMap[commonID] = repo
		paths, _, err := listWorktrees(ctx, c.root, l)
		if err != nil {
			paths = []worktreeListing{{path: c.root, branch: c.branch, detached: c.detached}}
		}
		seen := map[string]bool{}
		for _, wt := range paths {
			root, err := canonicalExisting(wt.path)
			if err != nil {
				continue
			}
			inScope := within(scope, root)
			ownerContainsScope := owner != nil && samePath(root, owner.root) && within(root, scope)
			if !inScope && !ownerContainsScope {
				repo.HiddenOutside++
				continue
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
			return nil, nil, false, err
		}
	}
	entries := 0
	repoCount := 0
	ruleRoot := scope
	if owner != nil {
		ruleRoot = owner.root
	}
	initialRules, err := ignoreRulesForPath(ruleRoot, scope, scope)
	if err != nil {
		return nil, nil, false, err
	}
	truncated, err := walkContained(ctx, scope, ruleRoot, scope, initialRules, MaxEntries, MaxDepth, func(path, rel string, d os.DirEntry, rules ignoreRules) (bool, error) {
		entries++
		if entries > MaxEntries || repoCount >= MaxRepositories {
			return false, nil
		}
		if path != scope {
			if c, e := openCheckout(path); e == nil {
				if owner != nil && identity(c.commonDir) == identity(owner.commonDir) {
					return false, nil
				}
				kind := KindNested
				if c.submodule {
					kind = KindSubmodule
				}
				repoCount++
				if err := addCheckout(c, kind); err != nil {
					return false, err
				}
				return true, nil
			}
		}
		return true, nil
	})
	if errors.Is(err, context.Canceled) {
		return nil, nil, false, err
	}
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return nil, nil, false, err
	}
	var repos []Repository
	for _, r := range repoMap {
		slices.SortFunc(r.Worktrees, func(a, b Worktree) int { return strings.Compare(a.Path, b.Path) })
		repos = append(repos, *r)
	}
	slices.SortFunc(repos, func(a, b Repository) int { return strings.Compare(a.Name, b.Name) })
	return refs, repos, truncated || errors.Is(err, context.DeadlineExceeded) || repoCount >= MaxRepositories || entries >= MaxEntries, nil
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
func status(ctx context.Context, root, scopeRel string, l *Local) (string, []FileChange, error) {
	args := []string{"-c", "core.quotepath=false", "status", "--porcelain=v2", "-z", "--untracked-files=all"}
	if scopeRel != "." {
		args = append(args, "--", ":(literal)"+filepath.ToSlash(scopeRel))
	}
	type statusResult struct {
		data []byte
		err  error
	}
	statusDone := make(chan statusResult, 1)
	go func() {
		data, err := l.git(ctx, root, MaxStatusBytes, args...)
		statusDone <- statusResult{data: data, err: err}
	}()
	binary, binErr := binaryPaths(ctx, root, scopeRel, l)
	status := <-statusDone
	if status.err != nil {
		return "unavailable", nil, status.err
	}
	files := parseStatus(status.data)
	if binErr == nil {
		for i := range files {
			files[i].Binary = binary[filepath.ToSlash(files[i].Path)] || binary[filepath.ToSlash(files[i].OldPath)]
		}
	}
	if scopeRel != "." {
		for i := range files {
			files[i].Path = trimScopePath(files[i].Path, scopeRel)
			files[i].OldPath = trimScopePath(files[i].OldPath, scopeRel)
		}
		files = slices.DeleteFunc(files, func(f FileChange) bool { return f.Path == "" })
	}
	return stateFor(files), files, nil
}

func binaryPaths(ctx context.Context, root, scopeRel string, l *Local) (map[string]bool, error) {
	out := make(map[string]bool)
	for _, args := range [][]string{{"diff", "--numstat", "-z"}, {"diff", "--cached", "--numstat", "-z"}} {
		cmd := append(args, "--")
		if scopeRel != "." {
			cmd = append(cmd, ":(literal)"+filepath.ToSlash(scopeRel))
		}
		data, err := l.git(ctx, root, MaxStatusBytes, cmd...)
		if err != nil {
			return nil, err
		}
		fields := bytes.Split(data, []byte{0})
		for _, field := range fields {
			parts := bytes.SplitN(field, []byte{'\t'}, 3)
			if len(parts) == 3 && (string(parts[0]) == "-" || string(parts[1]) == "-") {
				out[string(parts[2])] = true
			}
		}
	}
	return out, nil
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
			out = appendStatusXY(out, parts[9], parts[1])
			out[len(out)-1].OldPath = old
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
	return doc, nil
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
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	left := b.limit - b.Len()
	if left <= 0 {
		b.overflow = true
		return 0, ErrTooLarge
	}
	if len(p) > left {
		b.overflow = true
		_, _ = b.Buffer.Write(p[:left])
		return left, ErrTooLarge
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}
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
func cloneDocument(in Document) Document { out := in; out.Lines = slices.Clone(in.Lines); return out }
func documentBytes(doc Document) int {
	n := len(doc.Note) + len(doc.Err) + len(doc.PatchHeader)
	for _, line := range doc.Lines {
		n += len(line.Text) + 24
	}
	return n
}
