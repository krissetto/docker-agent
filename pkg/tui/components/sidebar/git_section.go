package sidebar

import (
	"path"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/components/review"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// GitChanges is the host's Git state for the sidebar section. The section
// never performs IO: expanding emits GitRequestMsg and the host replies here.
type GitChanges struct {
	// Owner identifies the page/session the section belongs to; requests carry it.
	Owner     string
	Available bool
	Loading   bool
	// Scanning lists worktrees whose status request is in flight.
	Scanning                       map[string]bool
	Err                            string
	Snapshot                       *review.Snapshot
	DiscoveryContinuationSupported bool
	ContinueDiscovery              bool
	ContinuingDiscovery            bool
	// Open is the file shown in the inspector, highlighted in the tree.
	Open                review.DocumentKey
	Mode                review.Mode
	Directories         []review.Directory
	DirectoryGeneration uint64
}

type GitAction int

const (
	// GitLoad asks for the first bounded discovery (on first expand).
	GitLoad GitAction = iota
	GitRefresh
	GitLoadWorktree
	GitContinueDiscovery
	GitOpen
	GitBrowse
	GitShowChanges
	GitLoadDirectory
	GitOpenFile
)

type GitRequestMsg struct {
	Owner      string
	Action     GitAction
	WorktreeID string
	Path       string
	Group      review.Group
}

// GitWatchMsg reports the visible Git tree's refresh scope without starting IO.
type GitWatchMsg struct {
	Owner             string
	Open              bool
	Mode              review.Mode
	ExpandedWorktrees []string
}

type gitRowKind int

const (
	gitRowNote gitRowKind = iota
	gitRowRepo
	gitRowWorktree
	gitRowGroup
	gitRowFile
	gitRowContinue
	gitRowBrowse
)

type gitRow struct {
	kind     gitRowKind
	key      string
	guides   string
	label    string
	tail     string
	worktree string
	path     string
	group    review.Group
	file     review.FileChange
	status   string
	warning  bool
	// counts reports whether the worktree's line counts and binary flags are known.
	counts bool
	branch bool
	open   bool
}

// gitSection renders like the subagent tree: a summary row, then indented
// rows where top-level repositories sit flush and nested rows carry ├/│/└
// guides. Everything starts collapsed and expansions are kept by stable
// identity across snapshots.
type gitSection struct {
	data                   GitChanges
	expanded               map[string]bool
	rows                   []gitRow
	displayRows            []gitRow
	displayToSource        []int
	cursor                 int
	focused                bool
	revision               uint64
	hovered                int
	previousHovered        int
	directoryPending       map[string]bool
	directoryGeneration    uint64
	directoryGenerationSet bool
	spinnerFrame           string
	summaryLoading         bool
}

const gitSectionKey = "git"

func (g *gitSection) set(data GitChanges) {
	if g.directoryGenerationSet && g.directoryGeneration != data.DirectoryGeneration {
		clear(g.directoryPending)
	}
	g.directoryGeneration, g.directoryGenerationSet = data.DirectoryGeneration, true
	selected := ""
	if g.cursor > 0 && g.cursor-1 < len(g.displayRows) {
		selected = g.displayRows[g.cursor-1].key
	}
	if g.directoryPending == nil {
		g.directoryPending = map[string]bool{}
	}
	for _, dir := range data.Directories {
		if dir.State != review.StateLoading {
			delete(g.directoryPending, directoryIdentity(dir.WorktreeID, dir.Path))
		}
	}
	g.data = data
	g.rebuild()
	if selected != "" {
		for i, row := range g.displayRows {
			if row.key == selected {
				g.cursor = i + 1
				break
			}
		}
	}
}

func (g *gitSection) open() bool { return g.expanded[gitSectionKey] }

func (g *gitSection) visible() bool { return g.data.Available }

func (g *gitSection) rebuild() {
	selected := ""
	if g.cursor > 0 && g.cursor-1 < len(g.displayRows) {
		selected = g.displayRows[g.cursor-1].key
	}
	g.revision++
	g.rows = g.rows[:0]
	g.summaryLoading = false
	if g.data.Snapshot != nil {
		for _, repo := range g.data.Snapshot.Repositories {
			for _, wt := range repo.Worktrees {
				g.summaryLoading = g.summaryLoading || wt.State == review.StateUnscanned || wt.State == review.StateLoading
			}
		}
	}
	if !g.open() {
		g.clamp()
		return
	}
	snap := g.data.Snapshot
	switch {
	case g.data.Err != "" && snap == nil:
		g.rows = append(g.rows, gitRow{kind: gitRowNote, label: styles.ErrorStyle.Render("⚠ " + sanitizeGit(g.data.Err))})
	case snap == nil && g.data.Loading:
		frame := g.spinnerFrame
		if frame == "" {
			frame = "⠋"
		}
		g.rows = append(g.rows, gitRow{kind: gitRowNote, label: styles.MutedStyle.Render(frame)})
	case snap == nil:
		// An interrupted scan never leaves the section spinning.
		g.rows = append(g.rows, gitRow{kind: gitRowNote, label: styles.MutedStyle.Render("not loaded · r to refresh")})
	case snap.Scope.Unavailable != "":
		g.rows = append(g.rows, gitRow{kind: gitRowNote, label: styles.MutedStyle.Render(sanitizeGit(snap.Scope.Unavailable))})
	case len(snap.Repositories) == 0:
		if snap.Scope.LimitReason == "" {
			g.rows = append(g.rows, gitRow{kind: gitRowNote, label: styles.MutedStyle.Render("no Git repositories")})
		}
	default:
		if g.data.Mode == review.ModeFiles {
			g.browse(snap)
		} else {
			for _, repo := range snap.Repositories {
				g.repository(repo)
			}
		}
	}
	// Files lists the browsed tree only; global Git discovery limits describe repositories.
	if g.data.Mode != review.ModeFiles {
		g.discoveryRows(snap)
	}
	g.prepareDisplayRows(40)
	if selected != "" {
		for i, row := range g.displayRows {
			if row.key == selected {
				g.cursor = i + 1
				break
			}
		}
	}
	g.clamp()
}

func (g *gitSection) discoveryRows(snap *review.Snapshot) {
	if snap == nil {
		return
	}
	if snap.Scope.LimitReason != "" {
		label := discoveryLimitSummary(snap.Scope.LimitReason, snap.Scope.LimitValue)
		if !g.data.ContinueDiscovery {
			if snap.Scope.Remote {
				label += " · remote"
			} else if !g.data.DiscoveryContinuationSupported {
				label += " · final"
			}
		}
		g.rows = append(g.rows, gitRow{kind: gitRowNote, key: "discovery-limit", label: label, warning: true})
		if g.data.ContinuingDiscovery {
			g.rows = append(g.rows, gitRow{kind: gitRowContinue, key: "continue-scan", label: styles.MutedStyle.Render("⠋ Scanning deeper…")})
		} else if g.data.ContinueDiscovery {
			g.rows = append(g.rows, gitRow{kind: gitRowContinue, key: "continue-scan", label: styles.InfoStyle.Render("Continue scan")})
		}
		return
	}
	if snap.Scope.Note != "" {
		label := sanitizeGit(snap.Scope.Note)
		if snap.Scope.Remote {
			label = "Incomplete · remote"
		}
		g.rows = append(g.rows, gitRow{kind: gitRowNote, key: "discovery-note", label: label, warning: true})
	}
}

func (g *gitSection) currentScopeWorktrees() []review.Worktree {
	if g.data.Snapshot == nil {
		return nil
	}
	var current []review.Worktree
	for _, repo := range g.data.Snapshot.Repositories {
		for _, wt := range repo.Worktrees {
			if wt.Current {
				current = append(current, wt)
			}
		}
	}
	return current
}

func (g *gitSection) requestCurrentScopeDirectories() tea.Cmd {
	var cmds []tea.Cmd
	for _, wt := range g.currentScopeWorktrees() {
		cmds = append(cmds, g.requestDirectory(wt.ID, ""))
	}
	return tea.Batch(cmds...)
}

func browseDirKey(worktreeID, dir string) string      { return "browse:dir:" + worktreeID + ":" + dir }
func browseWorktreeKey(worktreeID string) string      { return "browse:wt:" + worktreeID }
func browseRepoKey(repoID string) string              { return "browse:repo:" + repoID }
func directoryIdentity(worktreeID, dir string) string { return worktreeID + "\x00" + dir }

func (g *gitSection) directory(worktreeID, dir string) (review.Directory, bool) {
	for _, item := range g.data.Directories {
		if item.WorktreeID == worktreeID && item.Path == dir {
			return item, true
		}
	}
	return review.Directory{}, false
}

func (g *gitSection) requestDirectory(worktreeID, dir string, retry ...bool) tea.Cmd {
	item, ok := g.directory(worktreeID, dir)
	forceRetry := len(retry) > 0 && retry[0]
	if ok && item.State != review.StateUnscanned && !(forceRetry && item.State == review.StateError) {
		return nil
	}
	key := directoryIdentity(worktreeID, dir)
	if g.directoryPending == nil {
		g.directoryPending = map[string]bool{}
	}
	if g.directoryPending[key] {
		return nil
	}
	g.directoryPending[key] = true
	g.revision++
	return core.CmdHandler(GitRequestMsg{Owner: g.data.Owner, Action: GitLoadDirectory, WorktreeID: worktreeID, Path: dir})
}

func (g *gitSection) browse(snapshot *review.Snapshot) {
	directoryByID := make(map[string]review.Directory, len(g.data.Directories))
	for _, dir := range g.data.Directories {
		directoryByID[directoryIdentity(dir.WorktreeID, dir.Path)] = dir
		if dir.State != review.StateLoading {
			delete(g.directoryPending, directoryIdentity(dir.WorktreeID, dir.Path))
		}
	}
	statusByWorktree := map[string]map[string]string{}
	changesByWorktree := map[string]map[string]review.FileChange{}
	countsByWorktree := map[string]bool{}
	changedCounts := map[string]map[string]bool{}
	for _, repo := range snapshot.Repositories {
		for _, wt := range repo.Worktrees {
			statusByWorktree[wt.ID] = map[string]string{}
			changesByWorktree[wt.ID] = map[string]review.FileChange{}
			countsByWorktree[wt.ID] = countsKnown(wt)
			changedCounts[wt.ID] = map[string]bool{}
			for _, file := range wt.Files {
				statusByWorktree[wt.ID][file.Path] = file.Status
				changesByWorktree[wt.ID][file.Path] = file
				for parent := path.Dir(file.Path); parent != "." && parent != "/"; parent = path.Dir(parent) {
					changedCounts[wt.ID][parent] = true
				}
			}
		}
	}
	appendWorktree := func(repo review.Repository, wt review.Worktree, guides, child string) {
		key := browseWorktreeKey(wt.ID)
		label := checkoutPath(repo, wt)
		metadata := checkoutMetadata(wt)
		if metadata != "" {
			label += "  " + metadata
		}
		open := g.expanded[key]
		g.rows = append(g.rows, gitRow{kind: gitRowWorktree, key: key, guides: guides, label: label, worktree: wt.ID, branch: true, open: open})
		if !open {
			return
		}
		dir, loaded := directoryByID[directoryIdentity(wt.ID, "")]
		if !loaded || dir.State == review.StateUnscanned || dir.State == review.StateLoading {
			text := "Loading…"
			if g.directoryPending[directoryIdentity(wt.ID, "")] {
				text = "Loading…"
			}
			g.rows = append(g.rows, gitRow{kind: gitRowNote, guides: child, label: styles.MutedStyle.Render(text)})
			return
		}
		if dir.Err != "" || dir.State == review.StateError {
			g.rows = append(g.rows, gitRow{kind: gitRowNote, guides: child, label: styles.ErrorStyle.Render("⚠ " + sanitizeGit(dir.Err))})
			return
		}
		g.browseEntries(wt.ID, "", child, dir, directoryByID, statusByWorktree[wt.ID], changesByWorktree[wt.ID], countsByWorktree[wt.ID], changedCounts[wt.ID])
	}
	for _, repo := range snapshot.Repositories {
		var otherWorktrees []review.Worktree
		for _, wt := range repo.Worktrees {
			if !wt.Current {
				otherWorktrees = append(otherWorktrees, wt)
				continue
			}
			dir, loaded := directoryByID[directoryIdentity(wt.ID, "")]
			switch {
			case !loaded || dir.State == review.StateUnscanned || dir.State == review.StateLoading:
				g.rows = append(g.rows, gitRow{kind: gitRowNote, label: styles.MutedStyle.Render("Loading…")})
			case dir.Err != "" || dir.State == review.StateError:
				g.rows = append(g.rows, gitRow{kind: gitRowNote, label: styles.ErrorStyle.Render("⚠ " + sanitizeGit(dir.Err))})
			default:
				g.browseEntries(wt.ID, "", "", dir, directoryByID, statusByWorktree[wt.ID], changesByWorktree[wt.ID], countsByWorktree[wt.ID], changedCounts[wt.ID])
			}
		}
		if len(otherWorktrees) == 0 {
			continue
		}
		key := browseRepoKey(repo.ID)
		open := g.expanded[key]
		g.rows = append(g.rows, gitRow{kind: gitRowRepo, key: key, label: styles.TabPrimaryStyle.Render(sanitizeGit(repo.Name)), branch: true, open: open})
		if !open {
			continue
		}
		for i, wt := range otherWorktrees {
			guides, child := connector("", i == len(otherWorktrees)-1)
			appendWorktree(repo, wt, guides, child)
		}
	}
}

func (g *gitSection) discoveredWorktreeAt(scopeWorktreeID, rel string) bool {
	if g.data.Snapshot == nil {
		return false
	}
	currentScope := false
	for _, wt := range g.currentScopeWorktrees() {
		currentScope = currentScope || wt.ID == scopeWorktreeID
	}
	if !currentScope {
		return false
	}
	rel = path.Clean(rel)
	for _, repo := range g.data.Snapshot.Repositories {
		for _, wt := range repo.Worktrees {
			if !wt.Current && wt.Path != "" && wt.Path != "." && path.Clean(wt.Path) == rel {
				return true
			}
		}
	}
	return false
}

func (g *gitSection) browseEntries(worktreeID, parent, guides string, dir review.Directory, all map[string]review.Directory, statuses map[string]string, changes map[string]review.FileChange, counts bool, changedDirs map[string]bool) {
	entries := make([]review.Entry, 0, len(dir.Entries))
	for _, entry := range dir.Entries {
		if entry.Dir && g.discoveredWorktreeAt(worktreeID, path.Join(parent, entry.Name)) {
			continue
		}
		entries = append(entries, entry)
	}
	slices.SortFunc(entries, func(a, b review.Entry) int {
		if a.Dir != b.Dir {
			if a.Dir {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	for i, entry := range entries {
		rel := path.Join(parent, entry.Name)
		last := i == len(entries)-1
		rowGuides, child := connector(guides, last)
		if parent == "" && guides == "" {
			rowGuides, child = "", "  "
		}
		if entry.Dir {
			key := browseDirKey(worktreeID, rel)
			open := g.expanded[key]
			changed := 0
			for file := range statuses {
				if strings.HasPrefix(file, rel+"/") {
					changed++
				}
			}
			if changed == 0 && changedDirs[rel] {
				changed = 1
			}
			tail := ""
			if changed > 0 {
				tail = strconv.Itoa(changed) + " changed"
			}
			g.rows = append(g.rows, gitRow{kind: gitRowBrowse, key: key, guides: rowGuides, label: styles.TabPrimaryStyle.Render(sanitizeGit(entry.Name + "/")), tail: tail, worktree: worktreeID, path: rel, branch: true, open: open})
			if !open {
				continue
			}
			nested, loaded := all[directoryIdentity(worktreeID, rel)]
			if !loaded || nested.State == review.StateUnscanned || nested.State == review.StateLoading {
				text := "Loading…"
				if !g.directoryPending[directoryIdentity(worktreeID, rel)] {
					text = "Select to load"
				}
				g.rows = append(g.rows, gitRow{kind: gitRowNote, guides: child, label: styles.MutedStyle.Render(text)})
			} else if nested.Err != "" || nested.State == review.StateError {
				g.rows = append(g.rows, gitRow{kind: gitRowNote, guides: child, label: styles.ErrorStyle.Render("⚠ " + sanitizeGit(nested.Err))})
			} else {
				g.browseEntries(worktreeID, rel, child, nested, all, statuses, changes, counts, changedDirs)
			}
			continue
		}
		change, changed := changes[rel]
		status := statuses[rel]
		selected := g.data.Open.Mode == review.ModeFiles && g.data.Open.WorktreeID == worktreeID && g.data.Open.Path == rel
		name := entry.Name
		if selected {
			name = "▸ " + name
		}
		row := gitRow{kind: gitRowBrowse, key: "browse:file:" + worktreeID + ":" + rel, guides: rowGuides, label: styles.TabPrimaryStyle.Render(sanitizeGit(name)), worktree: worktreeID, path: rel, status: status}
		if changed {
			row.file, row.counts = change, counts
		}
		g.rows = append(g.rows, row)
	}
	if dir.Truncated {
		g.rows = append(g.rows, gitRow{kind: gitRowNote, guides: guides, label: styles.WarningStyle.Render("More entries not shown")})
	}
}

func (g *gitSection) setMode(mode review.Mode) tea.Cmd {
	if g.data.Mode == mode {
		return nil
	}
	g.data.Mode = mode
	g.focused, g.cursor = true, 0
	var cmds []tea.Cmd
	if mode == review.ModeFiles {
		if g.expanded == nil {
			g.expanded = map[string]bool{}
		}
		g.expanded[gitSectionKey] = true
	}
	if mode == review.ModeFiles {
		cmds = append(cmds, g.requestCurrentScopeDirectories(), core.CmdHandler(GitRequestMsg{Owner: g.data.Owner, Action: GitBrowse}))
	} else {
		cmds = append(cmds, core.CmdHandler(GitRequestMsg{Owner: g.data.Owner, Action: GitShowChanges}))
	}
	g.rebuild()
	cmds = append(cmds, core.CmdHandler(g.watchState()))
	return tea.Batch(cmds...)
}

func (g *gitSection) clamp() {
	g.cursor = max(0, min(g.cursor, len(g.displayRows)))
}

func connector(prefix string, last bool) (string, string) {
	if last {
		return prefix + "└ ", prefix + "  "
	}
	return prefix + "├ ", prefix + "│ "
}

// repository emits a top-level row; each checkout branches beneath it.
func (g *gitSection) repository(repo review.Repository) {
	key := "repo:" + repo.ID
	g.rows = append(g.rows, gitRow{kind: gitRowRepo, key: key, label: styles.TabPrimaryStyle.Render(sanitizeGit(repo.Name)), branch: true, open: g.expanded[key]})
	if !g.expanded[key] {
		return
	}
	for i, wt := range repo.Worktrees {
		guides, child := connector("", i == len(repo.Worktrees)-1)
		g.worktree(repo, wt, guides, child)
	}
	if repo.HiddenOutside > 0 {
		g.rows = append(g.rows, gitRow{kind: gitRowNote, guides: "  ", label: styles.MutedStyle.Render("⊘ +" + strconv.Itoa(repo.HiddenOutside) + " outside · not shown")})
	}
}

// worktree appends a row drawn with guides and nests its children under child.
func (g *gitSection) worktree(repo review.Repository, wt review.Worktree, guides, child string) {
	key := "wt:" + wt.ID
	metadata := checkoutMetadata(wt)
	label := styles.TabPrimaryStyle.Render(checkoutPath(repo, wt))
	if wt.Current {
		label = "● " + label
	}
	if metadata != "" {
		label += "  " + styles.MutedStyle.Render(metadata)
	}
	tail := ""
	if wt.State == review.StateReady && len(wt.Files) > 0 {
		tail = changeTail(wt.Files, countsKnown(wt))
	}
	g.rows = append(g.rows, gitRow{kind: gitRowWorktree, key: key, guides: guides, label: label, tail: tail, worktree: wt.ID, branch: true, open: g.expanded[key]})
	if !g.expanded[key] {
		return
	}
	switch wt.State {
	case review.StateUnscanned, review.StateLoading:
		label := "not scanned · r to refresh"
		if g.data.Scanning[wt.ID] {
			label = ""
		}
		g.rows = append(g.rows, gitRow{kind: gitRowNote, guides: child + "  ", label: styles.MutedStyle.Render(label)})
		return
	case review.StateError:
		g.rows = append(g.rows, gitRow{kind: gitRowNote, guides: child + "  ", label: styles.ErrorStyle.Render("⚠ " + sanitizeGit(wt.Err))})
		return
	}
	var groups []review.Group
	for _, grp := range []review.Group{review.GroupConflicts, review.GroupStaged, review.GroupUnstaged, review.GroupUntracked} {
		for _, f := range wt.Files {
			if f.Group == grp {
				groups = append(groups, grp)
				break
			}
		}
	}
	if len(groups) == 0 {
		g.rows = append(g.rows, gitRow{kind: gitRowNote, guides: child + "  ", label: styles.MutedStyle.Render("clean")})
		return
	}
	for i, grp := range groups {
		g.group(wt, grp, child, i == len(groups)-1)
	}
}

func (g *gitSection) group(wt review.Worktree, grp review.Group, prefix string, last bool) {
	guides, child := connector(prefix, last)
	key := "grp:" + wt.ID + ":" + strconv.Itoa(int(grp))
	var files []review.FileChange
	for _, f := range wt.Files {
		if f.Group == grp {
			files = append(files, f)
		}
	}
	counts := countsKnown(wt)
	g.rows = append(g.rows, gitRow{kind: gitRowGroup, key: key, guides: guides, label: grp.String(), tail: changeTail(files, counts), worktree: wt.ID, group: grp, branch: true, open: g.expanded[key]})
	if !g.expanded[key] {
		return
	}
	for i, f := range files {
		fg, _ := connector(child, i == len(files)-1)
		g.rows = append(g.rows, gitRow{kind: gitRowFile, key: "file:" + wt.ID + ":" + strconv.Itoa(int(grp)) + ":" + f.Path, guides: fg, worktree: wt.ID, group: grp, file: f, counts: counts})
	}
}

// countsKnown reports whether per-file line counts and binary flags are
// complete; until then the tree shows file counts only, never zeros.
func countsKnown(wt review.Worktree) bool {
	return wt.CountsState == review.StateReady
}

// changeTail is the muted file count followed, when known, by the summed
// green/red line deltas: "3 +12 −4". Binary files add nothing to the sums.
func changeTail(files []review.FileChange, counts bool) string {
	tail := styles.MutedStyle.Render(strconv.Itoa(len(files)))
	if !counts {
		return tail
	}
	added, removed := 0, 0
	for _, f := range files {
		if f.Binary {
			continue
		}
		added += f.Added
		removed += f.Removed
	}
	if stats := lineStats(added, removed); stats != "" {
		tail += " " + stats
	}
	return tail
}

// lineStats renders "+N −N", omitting zero sides.
func lineStats(added, removed int) string {
	stats := ""
	if added > 0 {
		stats = styles.SuccessStyle.Render("+" + strconv.Itoa(added))
	}
	if removed > 0 {
		if stats != "" {
			stats += " "
		}
		stats += styles.ErrorStyle.Render("−" + strconv.Itoa(removed))
	}
	return stats
}

// fileStats mirrors the review panel: "bin" for binaries, else line deltas,
// and nothing while counts are still unknown.
func fileStats(f review.FileChange, counts bool) string {
	switch {
	case !counts:
		return ""
	case f.Binary:
		return styles.MutedStyle.Render("bin")
	}
	return lineStats(f.Added, f.Removed)
}

func discoveryLimitSummary(reason string, value int) string {
	limit := "limit"
	switch reason {
	case "depth":
		limit = "depth"
	case "entries":
		limit = "entries"
	case "repositories":
		limit = "repositories"
	case "timeout":
		limit = "time"
	case "bytes":
		limit = "bytes"
	case "unavailable":
		limit = "unavailable"
	}
	label := "Incomplete · " + limit
	if value > 0 {
		label += " " + strconv.Itoa(value)
	}
	return label
}

func checkoutPath(repo review.Repository, wt review.Worktree) string {
	if wt.Path == "" || wt.Path == "." {
		return sanitizeGit(fallbackGit(repo.Name, wt.Path))
	}
	return sanitizeGit(wt.Path)
}

func checkoutMetadata(wt review.Worktree) string {
	switch {
	case wt.Branch != "":
		return "⎇ " + sanitizeGit(wt.Branch)
	case wt.Detached != "":
		return "⎇ detached @" + sanitizeGit(wt.Detached)
	}
	return ""
}

func sanitizeGit(s string) string {
	s = ansi.Strip(s)
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) {
			return ' '
		}
		return r
	}, s)
}

func (g *gitSection) summary(width int, hoverValues map[string]hoverValue) string {
	frame := g.spinnerFrame
	if g.loading() && frame == "" {
		frame = "⠋"
	}
	return g.summaryFrame(width, frame, hoverValues)
}

func (g *gitSection) loading() bool {
	return g.data.Loading || g.data.ContinuingDiscovery || len(g.data.Scanning) > 0 || g.summaryLoading
}

func (g *gitSection) repositoryCount() int {
	if g.data.Snapshot == nil {
		return 0
	}
	count := 0
	for _, repo := range g.data.Snapshot.Repositories {
		if repo.Kind != review.RepoNone {
			count++
		}
	}
	return count
}

// summaryFrame paints the header without rebuilding the tree. Brightness comes
// only from the shared hover ramp; keyboard focus marks the disclosure glyph.
func (g *gitSection) summaryFrame(width int, frame string, hoverState ...map[string]hoverValue) string {
	var hoverValues map[string]hoverValue
	if len(hoverState) > 0 {
		hoverValues = hoverState[0]
	}
	hover := gitHoverProgress(hoverValues, g.rowID(0))
	label := styles.MutedStyle.Render("Files - Git")
	if g.open() {
		// The selected mode keeps its semantic color; the rest stays muted.
		filesStyle, gitStyle := styles.MutedStyle, styles.MutedStyle
		if g.data.Mode == review.ModeFiles {
			filesStyle = styles.TabPrimaryStyle
		} else {
			gitStyle = styles.TabPrimaryStyle
		}
		label = filesStyle.Render("Files") + styles.MutedStyle.Render(" - ") + gitStyle.Render("Git")
	}
	if g.data.Snapshot != nil {
		label += styles.MutedStyle.Render(" · " + strconv.Itoa(g.repositoryCount()))
	}
	label = styles.HoverText(label, hover, styles.TextPrimary)
	if g.loading() && frame != "" {
		label += " " + styles.SpinnerDotsHighlightStyle.Render(frame)
	}
	glyph := "›"
	if g.open() {
		glyph = "⌄"
	}
	glyphStyle := styles.MutedStyle
	if g.focused && g.cursor == 0 {
		glyphStyle = styles.TabPrimaryStyle
	}
	width = max(0, width-1-ansi.StringWidth(glyph))
	label = ansi.Truncate(label, width, "…")
	return label + strings.Repeat(" ", max(1, width+1-ansi.StringWidth(label))) + glyphStyle.Render(glyph)
}

func (g *gitSection) modeAtX(x int) (review.Mode, bool) {
	switch {
	case x >= 0 && x < ansi.StringWidth("Files"):
		return review.ModeFiles, true
	case x >= ansi.StringWidth("Files - ") && x < ansi.StringWidth("Files - Git"):
		return review.ModeChanges, true
	default:
		return review.ModeChanges, false
	}
}

// lines renders the Files header and the active Git tree.
func (g *gitSection) lines(width int, hoverState ...map[string]hoverValue) []string {
	var hoverValues map[string]hoverValue
	if len(hoverState) > 0 {
		hoverValues = hoverState[0]
	}
	if !g.visible() {
		return nil
	}
	g.prepareDisplayRows(width)
	n := 1
	if g.open() {
		n = len(g.displayRows) + 1
	}
	out := make([]string, n)
	for i := range out {
		out[i] = g.line(width, i, hoverValues)
	}
	return out
}

func (g *gitSection) prepareDisplayRows(width int) {
	g.displayRows = g.displayRows[:0]
	g.displayToSource = g.displayToSource[:0]
	indent := min(2, max(0, width-1))
	for source, row := range g.rows {
		if row.kind != gitRowNote || strings.TrimSpace(ansi.Strip(row.label)) == "" {
			g.displayRows = append(g.displayRows, row)
			g.displayToSource = append(g.displayToSource, source)
			continue
		}
		inner := max(8, width-indent-ansi.StringWidth(row.guides))
		plain := strings.TrimSpace(ansi.Strip(row.label))
		wrapped := strings.Split(ansi.Wrap(plain, inner, ""), "\n")
		if row.key == "discovery-limit" || row.key == "discovery-note" {
			wrapped = []string{ansi.Truncate(plain, inner, "…")}
		}
		for i, text := range wrapped {
			displayRow := row
			style := styles.MutedStyle
			if row.warning {
				style = styles.WarningStyle
			}
			displayRow.label = style.Render(text)
			if i > 0 {
				displayRow.key = row.key + ":line:" + strconv.Itoa(i)
			}
			g.displayRows = append(g.displayRows, displayRow)
			g.displayToSource = append(g.displayToSource, source)
		}
	}
	g.cursor = max(0, min(g.cursor, len(g.displayRows)))
}

// line renders one section line (0 = header, rows from 1) for partial refreshes.
func (g *gitSection) line(width, line int, hoverState ...map[string]hoverValue) string {
	var hoverValues map[string]hoverValue
	if len(hoverState) > 0 {
		hoverValues = hoverState[0]
	}
	switch {
	case line == 0:
		return g.summary(width, hoverValues)
	case line < 1 || line > len(g.displayRows):
		return ""
	}
	indent := min(2, max(0, width-1))
	r := g.displayRows[line-1]
	focused := g.focused && g.cursor == line
	hover := gitHoverProgress(hoverValues, g.rowID(line))
	selected := r.kind == gitRowFile && g.data.Open.WorktreeID == r.worktree && g.data.Open.Path == r.file.Path && g.data.Open.Group == r.group
	selected = selected || r.kind == gitRowBrowse && !r.branch && g.data.Open.Mode == review.ModeFiles && g.data.Open.WorktreeID == r.worktree && g.data.Open.Path == r.path
	resting := g.data.Open.Path != "" && !selected && !focused
	fileRow := r.kind == gitRowFile || r.kind == gitRowBrowse && !r.branch
	if resting && fileRow && styles.ReviewRestFg() != nil {
		// A theme that authors review_rest_fg mutes only the filename; status and stats keep full strength.
		return strings.Repeat(" ", indent) + g.renderRowRest(r, max(1, width-indent), focused, hover, true)
	}
	painted := strings.Repeat(" ", indent) + g.renderRow(r, max(1, width-indent), focused, hover)
	if resting {
		// Resting de-emphasis is static; pointer brightness comes only from the shared hover ramp in renderRow.
		return styles.FadeLine(painted, gitOtherFileRestAlpha)
	}
	return painted
}

// gitOtherFileRestAlpha is the legacy resting strength of rows other than the open file.
const gitOtherFileRestAlpha = .55

// gitHoverProgress reads the shared hover ramp only; a missing entry is rest.
func gitHoverProgress(values map[string]hoverValue, key string) float64 {
	return values["git:"+key].value
}

func (g *gitSection) lineForHoverKey(key string) int {
	key, _ = strings.CutPrefix(key, "git:")
	for line := range len(g.displayRows) + 1 {
		if g.rowID(line) == key {
			return line
		}
	}
	return -1
}

func (g *gitSection) renderRow(r gitRow, width int, focused bool, hover float64) string {
	return g.renderRowRest(r, width, focused, hover, false)
}

// renderRowRest renders a row; mutedName paints a resting file row's filename in the theme's review_rest_fg.
func (g *gitSection) renderRowRest(r gitRow, width int, focused bool, hover float64, mutedName bool) string {
	guides := styles.MutedStyle.Render(r.guides)
	label := r.label
	inlineStats := false
	if r.kind == gitRowFile || r.kind == gitRowBrowse && !r.branch {
		statusText, filePath, stats := r.file.Status, r.file.Path, ""
		if r.kind == gitRowBrowse {
			statusText, filePath = r.status, r.path
			stats = fileStats(r.file, r.counts)
			inlineStats = stats != ""
		} else {
			stats = fileStats(r.file, r.counts)
			inlineStats = stats != ""
		}
		status := gitStatusStyle(statusText).Render(sanitizeGit(fallbackGit(statusText, "·")))
		name := sanitizeGit(filePath)
		selected := r.kind == gitRowBrowse && g.data.Open.Mode == review.ModeFiles && g.data.Open.WorktreeID == r.worktree && g.data.Open.Path == r.path
		if r.kind == gitRowBrowse {
			name = path.Base(filePath)
			if selected {
				name = "▸ " + name
			}
		}
		statsWidth := ansi.StringWidth(stats)
		statGap := 0
		if stats != "" {
			statGap = 1
		}
		avail := max(1, width-ansi.StringWidth(r.guides)-ansi.StringWidth(status)-2-statGap-statsWidth)
		name = truncateMiddleGit(name, avail)
		nameStyle := styles.BaseStyle
		if focused || selected {
			nameStyle = styles.HighlightWhiteStyle
		} else if mutedName && styles.ReviewRestFg() != nil {
			nameStyle = styles.BaseStyle.Foreground(styles.ReviewRestFg())
		}
		label = styles.HoverText(status+" "+nameStyle.Render(name), hover, styles.TextPrimary)
		if stats != "" {
			label += " " + stats
		}
		r.tail = ""
	}
	if r.kind != gitRowFile && (r.kind != gitRowBrowse || r.branch) {
		label = g.styleLabel(label, r, focused, hover)
	}
	tail := r.tail
	chevron := "›"
	if r.open {
		chevron = "⌄"
	}
	if r.branch {
		if tail != "" {
			tail += " "
		}
		tail += styles.MutedStyle.Render(chevron)
	}
	if (r.kind == gitRowWorktree || r.kind == gitRowGroup || r.kind == gitRowBrowse) && ansi.StringWidth(guides+label)+ansi.StringWidth(tail)+1 > width {
		plain := strings.Fields(ansi.Strip(r.tail))
		if len(plain) > 1 {
			count := plain[0]
			tail = styles.MutedStyle.Render(count)
			if r.branch {
				tail += " " + styles.MutedStyle.Render(chevron)
			}
		}
	}
	leftWidth := width
	if tail != "" {
		leftWidth -= ansi.StringWidth(tail) + 1
	}
	left := guides + label
	if !inlineStats {
		left = ansi.Truncate(left, max(0, leftWidth), "…")
	}
	if tail == "" {
		if inlineStats {
			return left + strings.Repeat(" ", max(0, width-ansi.StringWidth(left)))
		}
		return left
	}
	return left + strings.Repeat(" ", max(1, width-ansi.StringWidth(left)-ansi.StringWidth(tail))) + tail
}

func (g *gitSection) styleLabel(label string, row gitRow, focused bool, hover float64) string {
	switch {
	case focused:
		label = styles.HighlightWhiteStyle.Render(ansi.Strip(label))
	case row.kind == gitRowContinue && !g.data.ContinuingDiscovery:
		label = styles.InfoStyle.Render(ansi.Strip(label))
	case row.kind == gitRowWorktree && g.data.Scanning[row.worktree]:
		label = styles.BaseStyle.Background(styles.CardBg).Foreground(styles.Info).Render(ansi.Strip(label))
	}
	if row.kind != gitRowNote {
		return styles.HoverText(label, hover, styles.TextPrimary)
	}
	return label
}

func gitStatusStyle(status string) lipgloss.Style {
	switch status {
	case "M":
		return styles.WarningStyle
	case "A":
		return styles.SuccessStyle
	case "D", "U":
		return styles.ErrorStyle
	case "?":
		return styles.MutedStyle
	}
	return styles.InfoStyle
}

func fallbackGit(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func truncateMiddleGit(p string, width int) string {
	if ansi.StringWidth(p) <= width {
		return p
	}
	base := path.Base(p)
	bw := ansi.StringWidth(base)
	if bw+2 > width {
		return ansi.Truncate(p, width, "…")
	}
	return ansi.Truncate(strings.TrimSuffix(p, base), width-bw-1, "") + "…" + base
}

// activate handles a click or Enter on line i of the section (0 = summary).
func (g *gitSection) interactive(i int) bool {
	if i == 0 {
		return true
	}
	if i < 1 || i-1 >= len(g.displayRows) {
		return false
	}
	switch g.displayRows[i-1].kind {
	case gitRowRepo, gitRowWorktree, gitRowGroup, gitRowFile, gitRowContinue, gitRowBrowse:
		return true
	default:
		return false
	}
}

func (g *gitSection) activate(i int) tea.Cmd {
	if i == 0 {
		return g.toggleSection()
	}
	if i < 1 || i-1 >= len(g.displayRows) {
		return nil
	}
	display := g.displayRows[i-1]
	r := display
	if i-1 < len(g.displayToSource) && g.displayToSource[i-1] < len(g.rows) {
		r = g.rows[g.displayToSource[i-1]]
	}
	if display.kind == gitRowContinue {
		return core.CmdHandler(GitRequestMsg{Owner: g.data.Owner, Action: GitContinueDiscovery})
	}
	switch r.kind {
	case gitRowFile:
		return core.CmdHandler(GitRequestMsg{Owner: g.data.Owner, Action: GitOpen, WorktreeID: r.worktree, Path: r.file.Path, Group: r.group})
	case gitRowBrowse:
		if r.branch {
			return g.toggle(r)
		}
		return core.CmdHandler(GitRequestMsg{Owner: g.data.Owner, Action: GitOpenFile, WorktreeID: r.worktree, Path: r.path})
	case gitRowRepo, gitRowWorktree, gitRowGroup:
		return g.toggle(r)
	}
	return nil
}

func (g *gitSection) toggleSection() tea.Cmd {
	if g.expanded == nil {
		g.expanded = map[string]bool{}
	}
	g.expanded[gitSectionKey] = !g.open()
	g.rebuild()
	var cmds []tea.Cmd
	if g.open() && g.data.Snapshot == nil && !g.data.Loading {
		// Files browses one folder without a repository scan; Changes needs discovery.
		action := GitLoad
		if g.data.Mode == review.ModeFiles {
			action = GitBrowse
		}
		cmds = append(cmds, core.CmdHandler(GitRequestMsg{Owner: g.data.Owner, Action: action}))
	}
	if g.open() && g.data.Mode == review.ModeFiles {
		cmds = append(cmds, g.requestCurrentScopeDirectories())
	}
	cmds = append(cmds, core.CmdHandler(g.watchState()))
	return tea.Batch(cmds...)
}

func (g *gitSection) watchState() GitWatchMsg {
	state := GitWatchMsg{Owner: g.data.Owner, Open: g.open(), Mode: g.data.Mode}
	if !state.Open {
		return state
	}
	if state.Mode == review.ModeFiles {
		for _, wt := range g.currentScopeWorktrees() {
			state.ExpandedWorktrees = append(state.ExpandedWorktrees, wt.ID)
		}
	}
	for _, row := range g.rows {
		if row.kind == gitRowWorktree && row.open {
			state.ExpandedWorktrees = append(state.ExpandedWorktrees, row.worktree)
		}
	}
	return state
}

func (g *gitSection) toggle(r gitRow) tea.Cmd {
	if g.data.Mode == review.ModeFiles && r.branch && r.open {
		var dirPath string
		switch r.kind {
		case gitRowWorktree:
			if dir, ok := g.directory(r.worktree, ""); ok && dir.State == review.StateError {
				return g.requestDirectory(r.worktree, "", true)
			}
		case gitRowBrowse:
			dirPath = r.path
			if dir, ok := g.directory(r.worktree, dirPath); ok && dir.State == review.StateError {
				return g.requestDirectory(r.worktree, dirPath, true)
			}
		}
	}
	if g.expanded == nil {
		g.expanded = map[string]bool{}
	}
	g.expanded[r.key] = !r.open
	g.rebuild()
	var cmds []tea.Cmd
	if r.kind == gitRowWorktree && !r.open {
		if g.data.Mode == review.ModeFiles {
			cmds = append(cmds, g.requestDirectory(r.worktree, ""))
		} else if wt, ok := g.worktreeByID(r.worktree); ok && wt.State == review.StateUnscanned {
			cmds = append(cmds, core.CmdHandler(GitRequestMsg{Owner: g.data.Owner, Action: GitLoadWorktree, WorktreeID: wt.ID}))
		}
	}
	if r.kind == gitRowBrowse && r.branch && !r.open {
		cmds = append(cmds, g.requestDirectory(r.worktree, r.path))
	}
	cmds = append(cmds, core.CmdHandler(g.watchState()))
	return tea.Batch(cmds...)
}

func (g *gitSection) worktreeByID(id string) (review.Worktree, bool) {
	if g.data.Snapshot == nil {
		return review.Worktree{}, false
	}
	for _, repo := range g.data.Snapshot.Repositories {
		for _, wt := range repo.Worktrees {
			if wt.ID == id {
				return wt, true
			}
		}
	}
	return review.Worktree{}, false
}

// key handles keyboard input while the section has focus.
func (g *gitSection) key(k string) (bool, tea.Cmd) {
	n := len(g.displayRows)
	if !g.open() {
		n = 0
	}
	switch k {
	case "tab":
		mode := review.ModeFiles
		if g.data.Mode == review.ModeFiles {
			mode = review.ModeChanges
		}
		return true, g.setMode(mode)
	case "1":
		return true, g.setMode(review.ModeChanges)
	case "2", "b":
		return true, g.setMode(review.ModeFiles)
	case "up", "k":
		g.cursor = max(0, g.cursor-1)
	case "down", "j":
		g.cursor = min(n, g.cursor+1)
	case "home", "g":
		g.cursor = 0
	case "end", "G":
		g.cursor = n
	case "enter", "space":
		return true, g.activate(g.cursor)
	case "right", "l":
		if g.cursor == 0 {
			if !g.open() {
				return true, g.toggleSection()
			}
			g.cursor = min(n, 1)
			return true, nil
		}
		r := g.displayRows[g.cursor-1]
		if r.kind == gitRowContinue {
			return true, g.activate(g.cursor)
		}
		if r.branch && !r.open {
			return true, g.toggle(r)
		}
		if r.kind == gitRowFile || r.kind == gitRowBrowse && !r.branch {
			return true, g.activate(g.cursor)
		}
		g.cursor = min(n, g.cursor+1)
	case "left", "h":
		if g.cursor == 0 {
			if g.open() {
				return true, g.toggleSection()
			}
			return true, nil
		}
		r := g.displayRows[g.cursor-1]
		if r.branch && r.open {
			return true, g.toggle(r)
		}
		g.cursor = g.parentOf(g.cursor)
	case "r":
		clear(g.directoryPending)
		return true, core.CmdHandler(GitRequestMsg{Owner: g.data.Owner, Action: GitRefresh})
	default:
		return false, nil
	}
	return true, nil
}

// parentOf returns the line of the nearest row with shallower guides.
func (g *gitSection) parentOf(line int) int {
	if line <= 0 || line-1 >= len(g.displayRows) {
		return 0
	}
	depth := len(g.displayRows[line-1].guides)
	for i := line - 1; i >= 1; i-- {
		if g.displayRows[i-1].branch && len(g.displayRows[i-1].guides) < depth {
			return i
		}
	}
	return 0
}

// rowID is a stable placement identity for a section line.
func (g *gitSection) setHovered(line int) bool {
	if line < -1 || line >= len(g.displayRows)+1 {
		line = -1
	}
	if g.hovered == line {
		return false
	}
	g.previousHovered, g.hovered = g.hovered, line
	g.revision++
	return true
}

func (g *gitSection) rowID(line int) string {
	switch {
	case line == 0:
		return "git:summary"
	case line >= 1 && line <= len(g.displayRows) && g.displayRows[line-1].key != "":
		return "git:" + g.displayRows[line-1].key
	}
	return "git:line:" + strconv.Itoa(line)
}

// GitSidebar is the Git section surface the chat page drives.
type GitSidebar interface {
	SetGitChanges(changes GitChanges) tea.Cmd
	SetGitMode(mode review.Mode) tea.Cmd
	GitWatchState() GitWatchMsg
	ActivateGitLine(line string) tea.Cmd
	RevealGit() tea.Cmd
	GitFocused() bool
	SetGitFocused(focused bool) tea.Cmd
	GitKey(msg tea.KeyPressMsg) (bool, tea.Cmd)
}

func (m *model) GitWatchState() GitWatchMsg { return m.git.watchState() }

func (m *model) SetGitChanges(c GitChanges) tea.Cmd {
	m.git.set(c)
	var cmds []tea.Cmd
	if m.git.data.Mode == review.ModeFiles && m.git.open() && !m.git.data.Loading && m.git.data.Snapshot != nil {
		cmds = append(cmds, m.git.requestCurrentScopeDirectories())
		for _, row := range m.git.rows {
			if row.kind == gitRowWorktree && row.open {
				cmds = append(cmds, m.git.requestDirectory(row.worktree, ""))
			} else if row.kind == gitRowBrowse && row.branch && row.open {
				cmds = append(cmds, m.git.requestDirectory(row.worktree, row.path))
			}
		}
	}
	loading := m.git.loading()
	if loading != m.gitLoading {
		m.gitLoading = loading
		if loading {
			cmds = append(cmds, m.startSpinner())
		} else {
			m.stopSpinner()
		}
	}
	if loading {
		m.git.spinnerFrame = m.spinner.RawFrame()
	} else {
		m.git.spinnerFrame = ""
	}
	m.invalidateCache()
	cmds = append(cmds, m.ReconcileLayout())
	return tea.Batch(cmds...)
}

// ActivateGitLine handles a section line (header=0, body rows from 1).
func (m *model) ActivateGitLine(line string) tea.Cmd {
	if line == "mode:changes" || line == "mode:browse" {
		mode := review.ModeChanges
		if line == "mode:browse" {
			mode = review.ModeFiles
		}
		var cmd tea.Cmd
		if m.git.data.Mode == mode {
			cmd = m.git.toggleSection()
		} else {
			if m.git.expanded == nil {
				m.git.expanded = map[string]bool{}
			}
			m.git.expanded[gitSectionKey] = true
			cmd = m.git.setMode(mode)
		}
		return m.finishGitMode(cmd)
	}
	n, err := strconv.Atoi(line)
	if err != nil || n < 0 {
		return nil
	}
	m.git.cursor = n
	cmd := m.git.activate(n)
	m.invalidateCache()
	return tea.Batch(cmd, m.ReconcileLayout())
}

// SetGitMode applies the host-selected Changes/Browse presentation.
func (m *model) SetGitMode(mode review.Mode) tea.Cmd {
	if m.git.data.Mode == mode {
		return nil
	}
	cmd := m.git.setMode(mode)
	return m.finishGitMode(cmd)
}

func (m *model) finishGitMode(cmd tea.Cmd) tea.Cmd {
	m.invalidateCache()
	return tea.Batch(cmd, m.ReconcileLayout())
}

// RevealGit opens the section (not its repositories) and focuses its summary.
func (m *model) RevealGit() tea.Cmd {
	var cmd tea.Cmd
	if !m.git.open() {
		cmd = m.git.toggleSection()
	}
	m.git.cursor, m.git.focused = 0, true
	m.invalidateCache()
	return tea.Batch(cmd, m.ReconcileLayout())
}

func (m *model) GitFocused() bool { return m.git.focused }

func (m *model) SetGitFocused(focused bool) tea.Cmd {
	if m.git.focused == focused {
		return nil
	}
	m.git.focused = focused
	m.invalidateCache()
	return m.ReconcileLayout()
}

func (m *model) GitKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	handled, cmd := m.git.key(msg.String())
	if !handled {
		return false, nil
	}
	m.invalidateCache()
	reconcile := m.ReconcileLayout()
	m.ensureGitCursorVisible()
	return true, tea.Batch(cmd, reconcile)
}

func (m *model) ensureGitCursorVisible() {
	if m.gitStart < 0 {
		return
	}
	line := m.gitStart + m.git.cursor
	m.scrollview.EnsureLineVisible(line)
}
