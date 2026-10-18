package review

import (
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

type rowKind int

const (
	rowNote rowKind = iota
	rowRepo
	rowWorktree
	rowHidden
	rowGroup
	rowFile
	rowDir
	rowEntry
)

type row struct {
	kind       rowKind
	key        string
	depth      int
	repo, wt   int
	group      Group
	file       FileChange
	path       string
	label      string
	tone       noteTone
	expandable bool
	expanded   bool
	// merged marks a worktree row standing in for its single-worktree repository.
	merged bool
}

type noteTone int

const (
	toneMuted noteTone = iota
	toneError
)

func (r row) selectable() bool { return r.kind != rowNote }

func (r row) structural() bool {
	return r.kind == rowRepo || r.kind == rowWorktree || r.kind == rowGroup || r.kind == rowDir
}

func (r row) header() bool { return r.kind == rowRepo || r.kind == rowWorktree }

func (r row) leaf() bool { return r.kind == rowFile || r.kind == rowEntry }

func repoKey(id string) string           { return "repo:" + id }
func worktreeKey(id string) string       { return "wt:" + id }
func groupKey(wt string, g Group) string { return "grp:" + wt + ":" + strconv.Itoa(int(g)) }
func dirKey(wt, p string) string         { return "dir:" + wt + ":" + p }
func fileKey(wt string, g Group, p string) string {
	return "file:" + wt + ":" + strconv.Itoa(int(g)) + ":" + p
}
func entryKey(wt, p string) string { return "entry:" + wt + ":" + p }

type dirID struct{ worktree, path string }

// matcher implements smart-case substring matching.
type matcher struct {
	query string
	fold  bool
}

func newMatcher(query string) matcher {
	return matcher{query: query, fold: query == strings.ToLower(query)}
}

func (m matcher) active() bool { return m.query != "" }

func (m matcher) match(s string) bool {
	if m.query == "" {
		return true
	}
	if m.fold {
		s = strings.ToLower(s)
	}
	return strings.Contains(s, m.query)
}

// index returns byte ranges of every non-overlapping match in s.
func (m matcher) index(s string) [][2]int {
	if m.query == "" {
		return nil
	}
	hay := s
	if m.fold {
		hay = strings.ToLower(s)
		if len(hay) != len(s) {
			return nil
		}
	}
	var out [][2]int
	for from := 0; from < len(hay); {
		i := strings.Index(hay[from:], m.query)
		if i < 0 {
			break
		}
		out = append(out, [2]int{from + i, from + i + len(m.query)})
		from += i + len(m.query)
	}
	return out
}

type rowBuilder struct {
	m     *Model
	rows  []row
	f     matcher
	width int
}

func (m *Model) buildRows() []row {
	b := rowBuilder{m: m, f: newMatcher(m.filterQuery()), width: m.listWidth()}
	snap := m.snapshot
	if snap.Scope.Unavailable != "" {
		b.note(snap.Scope.Unavailable, 0, toneMuted)
		return b.rows
	}
	if len(snap.Repositories) == 0 {
		if snap.Generation == 0 {
			b.note("Not scanned yet · press r to scan", 0, toneMuted)
		} else {
			label := "No Git repositories"
			if m.mode == ModeFiles {
				label = "No workspace folders available"
			}
			if snap.Scope.Label != "" {
				label += " under " + snap.Scope.Label
			}
			b.note(label, 0, toneMuted)
		}
		return b.rows
	}
	for ri := range snap.Repositories {
		b.repository(ri)
	}
	if snap.Scope.Note != "" && !b.f.active() {
		b.note(snap.Scope.Note, 0, toneMuted)
	}
	if b.f.active() && len(b.rows) == 0 {
		b.note("No matches for “"+m.filterQuery()+"” in "+m.filterScope(), 0, toneMuted)
	}
	return b.rows
}

// note wraps explanatory text so messages are never cut off.
func (b *rowBuilder) note(label string, depth int, tone noteTone) {
	width := max(8, b.width-2-depth*2)
	for line := range strings.SplitSeq(ansi.Wrap(sanitize(label), width, ""), "\n") {
		b.rows = append(b.rows, row{kind: rowNote, label: line, depth: depth, tone: tone, key: "note:" + strconv.Itoa(len(b.rows))})
	}
}

func (b *rowBuilder) repository(ri int) {
	repo := b.m.snapshot.Repositories[ri]
	merged := len(repo.Worktrees) == 1 && repo.HiddenOutside == 0
	start := len(b.rows)
	depth := 0
	if !merged {
		r := row{kind: rowRepo, key: repoKey(repo.ID), repo: ri, wt: -1, expandable: true}
		r.expanded = b.m.isExpanded(r.key) || b.f.active()
		b.rows = append(b.rows, r)
		if !r.expanded {
			return
		}
		depth = 1
		if repo.Err != "" {
			b.note("⚠ "+repo.Err, depth, toneError)
		}
	}
	matched := false
	for wi := range repo.Worktrees {
		if b.worktree(ri, wi, depth, merged) {
			matched = true
		}
	}
	if !merged && repo.HiddenOutside > 0 && !b.f.active() {
		label := "+" + strconv.Itoa(repo.HiddenOutside) + " linked outside · not shown"
		b.rows = append(b.rows, row{kind: rowHidden, key: "hidden:" + repo.ID, depth: depth, repo: ri, wt: -1, label: label})
	}
	if b.f.active() && !matched {
		b.rows = b.rows[:start]
	}
}

// worktree appends a worktree and its children; it reports whether anything matched the filter.
func (b *rowBuilder) worktree(ri, wi, depth int, merged bool) bool {
	wt := b.m.snapshot.Repositories[ri].Worktrees[wi]
	r := row{kind: rowWorktree, key: worktreeKey(wt.ID), repo: ri, wt: wi, depth: depth, expandable: true, merged: merged}
	r.expanded = b.m.isExpanded(r.key) || b.f.active()
	start := len(b.rows)
	b.rows = append(b.rows, r)
	if !r.expanded {
		return !b.f.active()
	}
	var matched bool
	if b.m.mode == ModeFiles {
		matched = b.directory(ri, wi, "", depth+1)
	} else {
		matched = b.changes(ri, wi, depth+1)
	}
	if b.f.active() && !matched {
		b.rows = b.rows[:start]
	}
	return matched || !b.f.active()
}

func (b *rowBuilder) changes(ri, wi, depth int) bool {
	wt := b.m.snapshot.Repositories[ri].Worktrees[wi]
	switch wt.State {
	case StateUnscanned:
		label := "not scanned · r"
		if b.m.loadPending[worktreeKey(wt.ID)] {
			label = "scanning…"
		}
		b.note(label, depth, toneMuted)
		return false
	case StateLoading:
		b.note("scanning…", depth, toneMuted)
		return false
	case StateError:
		b.note("⚠ "+fallback(wt.Err, "scan failed"), depth, toneError)
		return false
	}
	if len(wt.Files) == 0 {
		if !b.f.active() {
			b.note("working tree clean", depth, toneMuted)
		}
		return false
	}
	matched := false
	for _, g := range changeGroups {
		var files []FileChange
		for _, f := range wt.Files {
			if f.Group == g && (b.f.match(f.Path) || (f.OldPath != "" && b.f.match(f.OldPath))) {
				files = append(files, f)
			}
		}
		if len(files) == 0 {
			continue
		}
		matched = true
		r := row{kind: rowGroup, key: groupKey(wt.ID, g), repo: ri, wt: wi, depth: depth, group: g, expandable: true, label: strconv.Itoa(len(files))}
		r.expanded = b.m.isExpanded(r.key) || b.f.active()
		b.rows = append(b.rows, r)
		if !r.expanded {
			continue
		}
		for _, f := range files {
			b.rows = append(b.rows, row{kind: rowFile, key: fileKey(wt.ID, g, f.Path), repo: ri, wt: wi, depth: depth + 1, group: g, file: f, path: f.Path})
		}
	}
	return matched
}

func (b *rowBuilder) directory(ri, wi int, dir string, depth int) bool {
	wt := b.m.snapshot.Repositories[ri].Worktrees[wi]
	d, ok := b.m.dirs[dirID{wt.ID, dir}]
	if !ok || d.State == StateUnscanned || d.State == StateLoading {
		if !b.f.active() {
			b.note("loading…", depth, toneMuted)
		}
		return false
	}
	if d.State == StateError {
		b.note("⚠ "+fallback(d.Err, "cannot read folder"), depth, toneError)
		return false
	}
	entries := slices.Clone(d.Entries)
	slices.SortStableFunc(entries, func(a, c Entry) int {
		if a.Dir != c.Dir {
			if a.Dir {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(c.Name))
	})
	if len(entries) == 0 && !b.f.active() {
		b.note("empty folder", depth, toneMuted)
	}
	matched := false
	for _, e := range entries {
		p := path.Join(dir, e.Name)
		if e.Dir {
			r := row{kind: rowDir, key: dirKey(wt.ID, p), repo: ri, wt: wi, depth: depth, path: p, label: sanitize(e.Name), expandable: true}
			_, loaded := b.m.dirs[dirID{wt.ID, p}]
			r.expanded = b.m.isExpanded(r.key) || (b.f.active() && loaded)
			start := len(b.rows)
			b.rows = append(b.rows, r)
			child := false
			if r.expanded {
				child = b.directory(ri, wi, p, depth+1)
			}
			if b.f.active() && !child && !b.f.match(p) {
				b.rows = b.rows[:start]
				continue
			}
			matched = true
			continue
		}
		if !b.f.match(p) {
			continue
		}
		matched = true
		b.rows = append(b.rows, row{kind: rowEntry, key: entryKey(wt.ID, p), repo: ri, wt: wi, depth: depth, path: p, label: sanitize(e.Name)})
	}
	if d.Truncated && !b.f.active() {
		b.note("… more entries not listed", depth, toneMuted)
	}
	return matched
}

func fallback(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
