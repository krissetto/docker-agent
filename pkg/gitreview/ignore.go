package gitreview

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

const maxIgnoreFileBytes = 1 << 20

// ignoreRules models per-directory gitignore precedence without the process-
// global matcher cache used by general file completion. A refresh reads fresh
// rule files and nested rules take precedence over their parents.
type ignoreRules struct{ patterns []gitignore.Pattern }

func (r ignoreRules) child(root, dir, agentRoot string) (ignoreRules, error) {
	out := ignoreRules{patterns: append([]gitignore.Pattern(nil), r.patterns...)}
	names := []string{".gitignore"}
	if filepath.Clean(dir) == filepath.Clean(agentRoot) {
		names = append(names, ".agentsignore", ".cagentignore")
	}
	for _, name := range names {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return out, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			return out, err
		}
		data, err := io.ReadAll(io.LimitReader(f, maxIgnoreFileBytes+1))
		closeErr := f.Close()
		if err != nil {
			return out, err
		}
		if closeErr != nil {
			return out, closeErr
		}
		if len(data) > maxIgnoreFileBytes {
			return out, ErrTooLarge
		}
		domain := []string(nil)
		if name == ".gitignore" {
			rel, e := filepath.Rel(root, dir)
			if e != nil {
				return out, e
			}
			if rel != "." {
				domain = strings.Split(filepath.ToSlash(rel), "/")
			}
		}
		scanner := bufio.NewScanner(strings.NewReader(string(data)))
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			out.patterns = append(out.patterns, gitignore.ParsePattern(line, domain))
		}
		if err := scanner.Err(); err != nil {
			return out, err
		}
	}
	return out, nil
}

func (r ignoreRules) matches(root, path string, isDir bool) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	return gitignore.NewMatcher(r.patterns).Match(parts, isDir)
}

// walkStart is a directory whose children are inventoried; rules are inherited
// from its parent.
type walkStart struct {
	path, rel string
	rules     ignoreRules
	depth     int
}

const (
	// walkMaxOpenDirs bounds open directory handles per depth level.
	walkMaxOpenDirs = 32
	// walkBatchEntries bounds the children read from one directory per turn.
	walkBatchEntries = 128
)

type walkDirReader struct {
	walkStart

	f      *os.File
	done   bool
	closed bool
}

func (r *walkDirReader) close() {
	if !r.closed {
		r.closed = true
		_ = r.f.Close()
	}
}

type walkState struct {
	ruleRoot, agentRoot string
	maxEntries          int
	maxDepth            int
	visit               func(path, rel string, entry os.DirEntry, rules ignoreRules) (descend, stop bool, err error)
	entries             int
	limit               DiscoveryLimit
	levels              [][]walkStart
}

// walkContained inventories directories level by level. Up to walkMaxOpenDirs
// directories at one depth are read round-robin in bounded batches, so a wide
// directory among them cannot consume the shared entry budget before its open
// peers get a turn. Same-depth directories beyond that wait for a free slot and
// may end as an entries partial. Deeper levels run only after shallower ones.
func walkContained(ctx context.Context, starts []walkStart, ruleRoot, agentRoot string, maxEntries, maxDepth int, visit func(path, rel string, entry os.DirEntry, rules ignoreRules) (descend, stop bool, err error)) (DiscoveryLimit, error) {
	st := &walkState{ruleRoot: ruleRoot, agentRoot: agentRoot, maxEntries: maxEntries, maxDepth: maxDepth, visit: visit, levels: make([][]walkStart, maxDepth+1)}
	for _, start := range starts {
		if start.depth > maxDepth {
			if st.limit == "" {
				st.limit = DiscoveryLimitDepth
			}
			continue
		}
		st.levels[start.depth] = append(st.levels[start.depth], start)
	}
	for depth := range st.levels {
		stop, err := st.walkLevel(ctx, depth)
		if err != nil {
			return st.limit, err
		}
		if stop {
			break
		}
	}
	return st.limit, nil
}

// walkLevel reads every directory queued at depth. At most walkMaxOpenDirs are
// open at once; each open directory yields one batch per round.
func (st *walkState) walkLevel(ctx context.Context, depth int) (bool, error) {
	pending := st.levels[depth]
	st.levels[depth] = nil
	var active []*walkDirReader
	defer func() {
		for _, r := range active {
			r.close()
		}
	}()
	for len(pending) > 0 || len(active) > 0 {
		for len(active) < walkMaxOpenDirs && len(pending) > 0 {
			start := pending[0]
			pending = pending[1:]
			r, err := st.openReader(ctx, start)
			if err != nil {
				return false, err
			}
			if r != nil {
				active = append(active, r)
			}
		}
		var next []*walkDirReader
		for _, r := range active {
			stop, err := st.readBatch(ctx, r)
			if err != nil {
				return false, err
			}
			if stop {
				return true, nil
			}
			if r.done {
				r.close()
				continue
			}
			next = append(next, r)
		}
		active = next
	}
	return false, nil
}

func (st *walkState) openReader(ctx context.Context, start walkStart) (*walkDirReader, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rules, err := start.rules.child(st.ruleRoot, start.path, st.agentRoot)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(start.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	start.rules = rules
	return &walkDirReader{walkStart: start, f: f}, nil
}

// readBatch processes up to walkBatchEntries children of one open directory and
// reports stop when the budget or the visitor ends the whole walk.
func (st *walkState) readBatch(ctx context.Context, r *walkDirReader) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	batch, readErr := r.f.ReadDir(walkBatchEntries)
	slices.SortFunc(batch, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	for _, de := range batch {
		if pruneDiscoveryDirectory(de.Name()) || !de.IsDir() {
			continue
		}
		path := filepath.Join(r.path, de.Name())
		childRel := filepath.Join(r.rel, de.Name())
		if r.rules.matches(st.ruleRoot, path, true) {
			continue
		}
		if r.depth+1 > st.maxDepth {
			if st.limit == "" {
				st.limit = DiscoveryLimitDepth
			}
			continue
		}
		st.entries++
		if st.entries > st.maxEntries {
			st.limit = DiscoveryLimitEntries
			return true, nil
		}
		descend, stop, err := st.visit(path, childRel, de, r.rules)
		if err != nil {
			return false, err
		}
		if stop {
			return true, nil
		}
		if descend {
			st.levels[r.depth+1] = append(st.levels[r.depth+1], walkStart{path: path, rel: childRel, rules: r.rules, depth: r.depth + 1})
		}
	}
	if readErr == io.EOF {
		r.done = true
		return false, nil
	}
	return false, readErr
}

func ignoreRulesForPath(root, dir, agentRoot string) (ignoreRules, error) {
	if !within(root, dir) {
		return ignoreRules{}, ErrOutsideScope
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return ignoreRules{}, err
	}
	cur := root
	rules := ignoreRules{}
	rules, err = rules.child(root, root, agentRoot)
	if err != nil {
		return rules, err
	}
	if rel == "." {
		return rules, nil
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		rules, err = rules.child(root, cur, agentRoot)
		if err != nil {
			return rules, err
		}
	}
	return rules, nil
}
