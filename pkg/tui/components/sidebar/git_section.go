package sidebar

import (
	"path"
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
	Scanning map[string]bool
	Err      string
	Snapshot *review.Snapshot
	// Open is the file shown in the inspector, highlighted in the tree.
	Open review.DocumentKey
}

type GitAction int

const (
	// GitLoad asks for the first bounded discovery (on first expand).
	GitLoad GitAction = iota
	GitRefresh
	GitLoadWorktree
	GitOpen
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
	ExpandedWorktrees []string
}

type gitRowKind int

const (
	gitRowNote gitRowKind = iota
	gitRowRepo
	gitRowWorktree
	gitRowGroup
	gitRowFile
)

type gitRow struct {
	kind     gitRowKind
	key      string
	guides   string
	label    string
	tail     string
	worktree string
	group    review.Group
	file     review.FileChange
	branch   bool
	open     bool
}

// gitSection renders like the subagent tree: a summary row, then indented
// rows with ├/│/└ guides. Everything starts collapsed and expansions are kept
// by stable identity across snapshots.
type gitSection struct {
	data     GitChanges
	expanded map[string]bool
	rows     []gitRow
	cursor   int
	focused  bool
	revision uint64
	hovered  int
}

const gitSectionKey = "git"

func (g *gitSection) set(data GitChanges) {
	selected := ""
	if g.cursor > 0 && g.cursor-1 < len(g.rows) {
		selected = g.rows[g.cursor-1].key
	}
	g.data = data
	g.rebuild()
	if selected != "" {
		for i, row := range g.rows {
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
	g.revision++
	g.rows = g.rows[:0]
	if !g.open() {
		g.clamp()
		return
	}
	snap := g.data.Snapshot
	switch {
	case g.data.Err != "" && snap == nil:
		g.rows = append(g.rows, gitRow{kind: gitRowNote, label: styles.ErrorStyle.Render("⚠ " + sanitizeGit(g.data.Err))})
	case snap == nil && g.data.Loading:
		g.rows = append(g.rows, gitRow{kind: gitRowNote, label: styles.MutedStyle.Render("scanning…")})
	case snap == nil:
		// An interrupted scan never leaves the section spinning.
		g.rows = append(g.rows, gitRow{kind: gitRowNote, label: styles.MutedStyle.Render("not loaded · r to refresh")})
	case snap.Scope.Unavailable != "":
		g.rows = append(g.rows, gitRow{kind: gitRowNote, label: styles.MutedStyle.Render(sanitizeGit(snap.Scope.Unavailable))})
	case len(snap.Repositories) == 0:
		g.rows = append(g.rows, gitRow{kind: gitRowNote, label: styles.MutedStyle.Render("no Git repositories")})
	default:
		for i, repo := range snap.Repositories {
			g.repository(repo, i == len(snap.Repositories)-1)
		}
		if snap.Scope.Note != "" {
			g.rows = append(g.rows, gitRow{kind: gitRowNote, label: styles.MutedStyle.Render(sanitizeGit(snap.Scope.Note))})
		}
	}
	g.clamp()
}

func (g *gitSection) clamp() {
	g.cursor = max(0, min(g.cursor, len(g.rows)))
}

func connector(prefix string, last bool) (string, string) {
	if last {
		return prefix + "└ ", prefix + "  "
	}
	return prefix + "├ ", prefix + "│ "
}

func (g *gitSection) repository(repo review.Repository, last bool) {
	if len(repo.Worktrees) == 1 {
		g.worktree(repo, repo.Worktrees[0], "", last, true)
		return
	}
	guides, child := connector("", last)
	key := "repo:" + repo.ID
	g.rows = append(g.rows, gitRow{kind: gitRowRepo, key: key, guides: guides, label: styles.TabPrimaryStyle.Render(sanitizeGit(repo.Name)), branch: true, open: g.expanded[key]})
	if !g.expanded[key] {
		return
	}
	for i, wt := range repo.Worktrees {
		g.worktree(repo, wt, child, i == len(repo.Worktrees)-1, false)
	}
	if repo.HiddenOutside > 0 {
		g.rows = append(g.rows, gitRow{kind: gitRowNote, guides: child + "  ", label: styles.MutedStyle.Render("⊘ +" + strconv.Itoa(repo.HiddenOutside) + " outside · not shown")})
	}
}

func (g *gitSection) worktree(repo review.Repository, wt review.Worktree, prefix string, last, merged bool) {
	guides, child := connector(prefix, last)
	key := "wt:" + wt.ID
	label := branchLabel(wt)
	if merged {
		label = styles.TabPrimaryStyle.Render(sanitizeGit(repo.Name))
		if b := branchLabel(wt); b != "" && repo.Kind != review.RepoNone {
			label += " " + styles.MutedStyle.Render("⎇ "+b)
		}
	} else if wt.Current {
		label = "● " + label
	}
	tail := ""
	if wt.State == review.StateReady && len(wt.Files) > 0 {
		tail = strconv.Itoa(len(wt.Files))
	}
	g.rows = append(g.rows, gitRow{kind: gitRowWorktree, key: key, guides: guides, label: label, tail: tail, worktree: wt.ID, branch: true, open: g.expanded[key]})
	if !g.expanded[key] {
		return
	}
	switch wt.State {
	case review.StateUnscanned, review.StateLoading:
		label := "not scanned · r to refresh"
		if g.data.Scanning[wt.ID] {
			label = "scanning…"
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
	g.rows = append(g.rows, gitRow{kind: gitRowGroup, key: key, guides: guides, label: grp.String(), tail: strconv.Itoa(len(files)), worktree: wt.ID, group: grp, branch: true, open: g.expanded[key]})
	if !g.expanded[key] {
		return
	}
	for i, f := range files {
		fg, _ := connector(child, i == len(files)-1)
		g.rows = append(g.rows, gitRow{kind: gitRowFile, key: "file:" + wt.ID + ":" + strconv.Itoa(int(grp)) + ":" + f.Path, guides: fg, worktree: wt.ID, group: grp, file: f})
	}
}

func branchLabel(wt review.Worktree) string {
	switch {
	case wt.Branch != "":
		return sanitizeGit(wt.Branch)
	case wt.Detached != "":
		return "detached @" + sanitizeGit(wt.Detached)
	}
	return sanitizeGit(wt.Path)
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

func (g *gitSection) summary(width int) string {
	glyph := "›"
	if g.open() {
		glyph = "⌄"
	}
	text := "Git changes"
	if snap := g.data.Snapshot; snap != nil {
		n, repos := 0, len(snap.Repositories)
		scanned := false
		for _, repo := range snap.Repositories {
			for _, wt := range repo.Worktrees {
				if wt.State == review.StateReady {
					scanned = true
					n += len(wt.Files)
				}
			}
		}
		if scanned {
			text = strconv.Itoa(n) + " changes"
		}
		if repos > 1 {
			text += " · " + strconv.Itoa(repos) + " repos"
		}
	}
	if g.data.Loading {
		text += " · scanning…"
	}
	label := styles.TabPrimaryStyle.Render(text)
	if g.focused && g.cursor == 0 {
		label = styles.HighlightWhiteStyle.Render(text)
	} else if g.hovered == 0 {
		label = styles.HoverText(label, 1, styles.TextPrimary)
	}
	label = ansi.Truncate(label, max(0, width-2), "…")
	return label + strings.Repeat(" ", max(1, width-ansi.StringWidth(label)-1)) + styles.MutedStyle.Render(glyph)
}

// lines renders the section: summary, then (when open) a blank row and the
// tree body indented like the subagent tree.
func (g *gitSection) lines(width int) []string {
	if !g.visible() {
		return nil
	}
	out := []string{g.summary(width)}
	if !g.open() {
		return out
	}
	out = append(out, "")
	indent := min(2, max(0, width-1))
	body := max(1, width-indent)
	for i, r := range g.rows {
		out = append(out, strings.Repeat(" ", indent)+g.renderRow(r, body, g.focused && g.cursor == i+1, g.hovered == i+1))
	}
	return out
}

func (g *gitSection) renderRow(r gitRow, width int, focused, hovered bool) string {
	guides := styles.MutedStyle.Render(r.guides)
	label := r.label
	if r.kind == gitRowFile {
		status := gitStatusStyle(r.file.Status).Render(sanitizeGit(fallbackGit(r.file.Status, "·")))
		stats := ""
		if r.file.Added > 0 {
			stats += styles.SuccessStyle.Render("+" + strconv.Itoa(r.file.Added))
		}
		if r.file.Removed > 0 {
			if stats != "" {
				stats += " "
			}
			stats += styles.ErrorStyle.Render("−" + strconv.Itoa(r.file.Removed))
		}
		avail := max(1, width-ansi.StringWidth(r.guides)-2-ansi.StringWidth(stats)-1)
		name := truncateMiddleGit(sanitizeGit(r.file.Path), avail)
		open := g.data.Open
		selected := open.WorktreeID == r.worktree && open.Path == r.file.Path && open.Group == r.group
		nameStyle := styles.BaseStyle
		selectedMark := " "
		switch {
		case selected:
			selectedMark = styles.InfoStyle.Render("▌")
			nameStyle = styles.SelectionStyle
			if hovered {
				nameStyle = styles.BaseStyle.Background(styles.TabHoverBg).Foreground(styles.TabHoverFg)
			}
		case focused:
			nameStyle = styles.HighlightWhiteStyle
		case hovered:
			nameStyle = styles.BaseStyle.Background(styles.TabHoverBg).Foreground(styles.TabHoverFg)
		}
		label = status + selectedMark + nameStyle.Render(name)
		r.tail = stats
	} else if focused {
		label = styles.HighlightWhiteStyle.Render(ansi.Strip(label))
	} else if r.kind == gitRowWorktree && g.data.Scanning[r.worktree] {
		label = styles.BaseStyle.Background(styles.CardBg).Foreground(styles.Info).Render(ansi.Strip(label))
	} else if hovered && r.kind != gitRowNote {
		label = styles.BaseStyle.Background(styles.TabHoverBg).Foreground(styles.TabHoverFg).Render(ansi.Strip(label))
	}
	tail := r.tail
	if tail != "" && r.kind != gitRowFile {
		tail = styles.MutedStyle.Render(tail)
	}
	if r.branch {
		chevron := "›"
		if r.open {
			chevron = "⌄"
		}
		if tail != "" {
			tail += " "
		}
		tail += styles.MutedStyle.Render(chevron)
	}
	left := ansi.Truncate(guides+label, max(0, width-ansi.StringWidth(tail)-1), "…")
	if tail == "" {
		return left
	}
	return left + strings.Repeat(" ", max(1, width-ansi.StringWidth(left)-ansi.StringWidth(tail))) + tail
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
	if i < 2 || i-2 >= len(g.rows) {
		return false
	}
	switch g.rows[i-2].kind {
	case gitRowRepo, gitRowWorktree, gitRowGroup, gitRowFile:
		return true
	default:
		return false
	}
}

func (g *gitSection) activate(i int) tea.Cmd {
	if i == 0 {
		return g.toggleSection()
	}
	if i-1 >= len(g.rows) {
		return nil
	}
	r := g.rows[i-1]
	switch r.kind {
	case gitRowFile:
		return core.CmdHandler(GitRequestMsg{Owner: g.data.Owner, Action: GitOpen, WorktreeID: r.worktree, Path: r.file.Path, Group: r.group})
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
		cmds = append(cmds, core.CmdHandler(GitRequestMsg{Owner: g.data.Owner, Action: GitLoad}))
	}
	cmds = append(cmds, core.CmdHandler(g.watchState()))
	return tea.Batch(cmds...)
}

func (g *gitSection) watchState() GitWatchMsg {
	state := GitWatchMsg{Owner: g.data.Owner, Open: g.open()}
	if !state.Open {
		return state
	}
	for _, row := range g.rows {
		if row.kind == gitRowWorktree && row.open {
			state.ExpandedWorktrees = append(state.ExpandedWorktrees, row.worktree)
		}
	}
	return state
}

func (g *gitSection) toggle(r gitRow) tea.Cmd {
	if g.expanded == nil {
		g.expanded = map[string]bool{}
	}
	g.expanded[r.key] = !r.open
	g.rebuild()
	var cmds []tea.Cmd
	if r.kind == gitRowWorktree && !r.open {
		if wt, ok := g.worktreeByID(r.worktree); ok && wt.State == review.StateUnscanned {
			cmds = append(cmds, core.CmdHandler(GitRequestMsg{Owner: g.data.Owner, Action: GitLoadWorktree, WorktreeID: wt.ID}))
		}
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
	n := len(g.rows)
	if !g.open() {
		n = 0
	}
	switch k {
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
		r := g.rows[g.cursor-1]
		if r.branch && !r.open {
			return true, g.toggle(r)
		}
		if r.kind == gitRowFile {
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
		r := g.rows[g.cursor-1]
		if r.branch && r.open {
			return true, g.toggle(r)
		}
		g.cursor = g.parentOf(g.cursor)
	case "r":
		return true, core.CmdHandler(GitRequestMsg{Owner: g.data.Owner, Action: GitRefresh})
	default:
		return false, nil
	}
	return true, nil
}

// parentOf returns the line of the nearest row with shallower guides.
func (g *gitSection) parentOf(line int) int {
	depth := len(g.rows[line-1].guides)
	for i := line - 1; i >= 1; i-- {
		if g.rows[i-1].branch && len(g.rows[i-1].guides) < depth {
			return i
		}
	}
	return 0
}

// rowID is a stable placement identity for a section line.
func (g *gitSection) setHovered(line int) bool {
	if line < -1 || line >= len(g.rows)+2 {
		line = -1
	}
	if g.hovered == line {
		return false
	}
	g.hovered = line
	g.revision++
	return true
}

func (g *gitSection) rowID(line int) string {
	switch {
	case line == 0:
		return "git:summary"
	case line == 1:
		return "gap:git"
	case line-2 < len(g.rows) && g.rows[line-2].key != "":
		return "git:" + g.rows[line-2].key
	}
	return "git:line:" + strconv.Itoa(line)
}

// GitSidebar is the Git section surface the chat page drives.
type GitSidebar interface {
	SetGitChanges(changes GitChanges) tea.Cmd
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
	m.invalidateCache()
	return m.ReconcileLayout()
}

// ActivateGitLine handles a click on a section line (summary=0, blank=1, rows from 2).
func (m *model) ActivateGitLine(line string) tea.Cmd {
	n, err := strconv.Atoi(line)
	if err != nil || n == 1 {
		return nil
	}
	i := n
	if n >= 2 {
		i = n - 1
	}
	m.git.cursor = i
	cmd := m.git.activate(i)
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
	if m.git.cursor > 0 {
		line++ // skip the blank row under the summary
	}
	m.scrollview.EnsureLineVisible(line)
}
