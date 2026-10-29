package review

import (
	"path"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/components/tab"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	modeChangesLabel = "Changes"
	modeFilesLabel   = "Files"
	modeGap          = 2
)

func itoa(n int) string { return strconv.Itoa(n) }

func (m *Model) modeLabel() string {
	if m.mode == ModeFiles {
		return modeFilesLabel
	}
	return modeChangesLabel
}

func (m *Model) draftLabel() string {
	n := len(m.draft.Comments)
	if strings.TrimSpace(m.draft.General) != "" {
		n++
	}
	return "Draft " + itoa(n)
}

// draftLabelAt reports whether a navigator heading column hits the Draft tab.
func (m *Model) draftLabelAt(x int) bool {
	start := ansi.StringWidth(m.modeLabel()) + modeGap
	return x >= start && x < start+ansi.StringWidth(m.draftLabel())
}

func fit(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s = ansi.Truncate(s, width, "")
	return s + strings.Repeat(" ", max(0, width-ansi.StringWidth(s)))
}

// View renders exactly width×height cells.
func (m *Model) View() string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}
	if m.height < minRenderableHeight {
		lines := make([]string, m.height)
		lines[0] = fit(styles.MutedStyle.Render("Enlarge terminal to review changes"), m.width)
		for i := 1; i < m.height; i++ {
			lines[i] = strings.Repeat(" ", m.width)
		}
		return strings.Join(lines, "\n")
	}
	g := m.geometry()
	var nav, reader []string
	if g.showNav {
		nav = m.navigatorView(g)
	}
	if g.showRead {
		reader = m.readerView(g)
	}
	sep := styles.MutedStyle.Render("│")
	lines := make([]string, 0, m.height)
	for y := range m.panelHeight() {
		switch {
		case g.twoColumn && m.navLeft:
			lines = append(lines, nav[y]+sep+reader[y])
		case g.twoColumn:
			lines = append(lines, reader[y]+sep+nav[y])
		case g.showNav:
			lines = append(lines, nav[y])
		default:
			lines = append(lines, reader[y])
		}
	}
	return strings.Join(lines, "\n")
}

func (m *Model) navFocused() bool    { return m.focused && m.area == areaNavigator }
func (m *Model) readerFocused() bool { return m.focused && m.area == areaReader }

func titleStyle(focused bool) lipgloss.Style {
	if focused {
		return styles.TabTitleStyle.Bold(true)
	}
	return styles.MutedStyle
}

// heading reuses the sidebar section heading (tab component) and puts short
// status text at the end of its rule instead of in extra rows.
func heading(title, status string, width int) string {
	line := strings.SplitN(tab.Render(title, "", max(1, width)), "\n", 2)[0]
	if status == "" {
		return fit(line, width)
	}
	sw := ansi.StringWidth(status)
	cut := max(ansi.StringWidth(title)+2, width-sw-1)
	return fit(ansi.Cut(line, 0, cut)+" "+status, width)
}

func (m *Model) panelHint(focused bool) string {
	if focused && m.hint != "" {
		return styles.WarningStyle.Render(m.hint)
	}
	return ""
}

func (m *Model) navigatorView(g geometry) []string {
	width := g.navW
	focused := m.navFocused()
	active, inactive := titleStyle(focused), styles.MutedStyle
	tree, draft := active, inactive
	if m.draftOpen {
		tree, draft = inactive, active
	}
	if len(m.draft.Comments) > 0 && !m.draftOpen {
		draft = styles.InfoStyle
	}
	title := tree.Render(m.modeLabel()) + strings.Repeat(" ", modeGap) + draft.Render(m.draftLabel())
	var status []string
	if s := m.panelHint(focused); s != "" {
		status = append(status, s)
	}
	if m.snapshot.MaybeOutdated {
		status = append(status, styles.WarningStyle.Render("may be outdated"))
	}
	if m.snapshot.Scope.Remote {
		status = append(status, styles.WarningStyle.Render("remote"))
	}
	pad := strings.Repeat(" ", navPadding)
	out := make([]string, 0, m.height)
	out = append(out, pad+heading(title, strings.Join(status, " "), width-navPadding))
	if g.navBodyY > 1 {
		out = append(out, pad+fit(m.filterLine(), width-navPadding))
	}
	var visible []string
	if m.draftOpen {
		visible = m.draftRows(m.listWidth())
	} else {
		rows := m.currentRows()
		for i := m.navTop; i < m.navTop+g.navBodyH && i < len(rows); i++ {
			visible = append(visible, m.renderRow(rows[i], m.listWidth(), i == m.cursor, focused))
		}
	}
	for line := range strings.SplitSeq(m.navSV.ViewWithLines(visible), "\n") {
		out = append(out, pad+line)
	}
	return padRows(out, width, m.panelHeight())
}

func (m *Model) filterLine() string {
	if m.filterActive {
		return m.filter.View()
	}
	return styles.InfoStyle.Render("/ "+sanitize(m.filter.Value())) + styles.MutedStyle.Render(" · "+m.filterScope())
}

func padRows(rows []string, width, height int) []string {
	for len(rows) < height {
		rows = append(rows, "")
	}
	rows = rows[:height]
	for i := range rows {
		rows[i] = fit(rows[i], width)
	}
	return rows
}

func chevron(r row) string {
	if !r.expandable {
		return ""
	}
	if r.expanded {
		return styles.MutedStyle.Render("⌄")
	}
	return styles.MutedStyle.Render("›")
}

func worktreeLabel(wt Worktree) string {
	switch {
	case wt.Branch != "":
		return sanitize(wt.Branch)
	case wt.Detached != "":
		return "detached @" + sanitize(wt.Detached)
	}
	return sanitize(wt.Path)
}

func statusStyle(status string) lipgloss.Style {
	switch status {
	case "M":
		return styles.WarningStyle
	case "A":
		return styles.SuccessStyle
	case "D":
		return styles.ErrorStyle
	case "U":
		return styles.ErrorStyle.Bold(true)
	case "?":
		return styles.MutedStyle
	}
	return styles.InfoStyle
}

func (m *Model) renderRow(r row, width int, selected, focused bool) string {
	mark := " "
	if selected && r.selectable() {
		if focused {
			mark = styles.HighlightWhiteStyle.Render("▌")
		} else {
			mark = styles.MutedStyle.Render("▌")
		}
	}
	indent := strings.Repeat(" ", min(r.depth*2, max(0, width/3)))
	label := styles.BaseStyle
	if selected && focused {
		label = styles.HighlightWhiteStyle
	}
	var main, tail string
	repo, wt, _ := m.worktree(r)
	switch r.kind {
	case rowNote:
		style := styles.MutedStyle.Italic(true)
		if r.tone == toneError {
			style = styles.ErrorStyle
		}
		main = style.Render(r.label)
	case rowRepo:
		main = label.Bold(true).Render(sanitize(repo.Name)) + repoBadge(repo.Kind)
		tail = chevron(r)
	case rowWorktree:
		main, tail = m.worktreeCells(r, repo, wt, label), m.worktreeTail(r, wt)
	case rowHidden:
		main = styles.MutedStyle.Render("⊘ " + r.label)
	case rowGroup:
		style := label
		if r.group == GroupConflicts {
			style = styles.ErrorStyle
		}
		main = style.Render(r.group.String()) + " " + styles.MutedStyle.Render(r.label)
		tail = chevron(r)
	case rowFile:
		f := r.file
		status := statusStyle(f.Status).Render(fallback(sanitize(f.Status), "·"))
		tail = fileStats(f)
		avail := width - 1 - len(indent) - 2 - ansi.StringWidth(tail) - 1
		name := sanitize(f.Path)
		if f.OldPath != "" {
			name = sanitize(f.OldPath) + " → " + name
		}
		main = status + " " + label.Render(truncateMiddle(name, max(1, avail)))
	case rowDir:
		main = label.Render(r.label + "/")
		tail = chevron(r)
	case rowEntry:
		main = label.Render(r.label)
	}
	left := mark + indent + main
	tw := ansi.StringWidth(tail)
	left = ansi.Truncate(left, max(0, width-tw-1), "…")
	if tail == "" {
		return left
	}
	return left + strings.Repeat(" ", max(1, width-ansi.StringWidth(left)-tw)) + tail
}

func repoBadge(kind RepoKind) string {
	switch kind {
	case RepoNested:
		return styles.MutedStyle.Render("  nested")
	case RepoSubmodule:
		return styles.MutedStyle.Render("  submodule")
	}
	return ""
}

func (m *Model) worktreeCells(r row, repo Repository, wt Worktree, label lipgloss.Style) string {
	branch := ""
	if repo.Kind != RepoNone && (wt.Branch != "" || wt.Detached != "") {
		branch = styles.SecondaryStyle.Render("⎇ " + worktreeLabel(wt))
	}
	path := sanitize(wt.Path)
	if wt.Path == "" || wt.Path == "." {
		path = sanitize(repo.Name)
	}
	if r.merged {
		s := label.Bold(true).Render(path)
		if branch != "" {
			s += "  " + branch
		}
		s += repoBadge(repo.Kind)
		if wt.Linked {
			s += "  " + styles.MutedStyle.Render("linked")
		}
		return s
	}
	dot := styles.MutedStyle.Render("○ ")
	if wt.Current {
		dot = styles.SuccessStyle.Render("● ")
	}
	s := dot + label.Render(path)
	var detail []string
	if branch != "" {
		detail = append(detail, "⎇ "+worktreeLabel(wt))
	}
	if wt.Linked {
		detail = append(detail, "linked")
	}
	if len(detail) > 0 {
		s += "  " + styles.MutedStyle.Render(strings.Join(detail, " · "))
	}
	return s
}

func (m *Model) worktreeTail(r row, wt Worktree) string {
	tail := chevron(r)
	if m.mode == ModeChanges && wt.State == StateReady && len(wt.Files) > 0 {
		tail = styles.MutedStyle.Render(itoa(len(wt.Files))) + " " + tail
	}
	if wt.State == StateError {
		tail = styles.ErrorStyle.Render("⚠") + " " + tail
	}
	return tail
}

func fileStats(f FileChange) string {
	if f.Binary {
		return styles.MutedStyle.Render("bin")
	}
	var parts []string
	if f.Added > 0 {
		parts = append(parts, styles.SuccessStyle.Render("+"+itoa(f.Added)))
	}
	if f.Removed > 0 {
		parts = append(parts, styles.ErrorStyle.Render("−"+itoa(f.Removed)))
	}
	return strings.Join(parts, " ")
}

// truncateMiddle keeps the file name visible and elides the directory middle.
func truncateMiddle(p string, width int) string {
	if ansi.StringWidth(p) <= width {
		return p
	}
	base := path.Base(p)
	bw := ansi.StringWidth(base)
	if bw+2 > width {
		return ansi.Truncate(p, width, "…")
	}
	head := strings.TrimSuffix(p, base)
	return ansi.Truncate(head, width-bw-1, "") + "…" + base
}

func (m *Model) readerView(g geometry) []string {
	width := g.readerW
	focused := m.readerFocused()
	out := make([]string, 0, m.height)
	if m.previewing() {
		out = append(out, m.previewTitle(width))
		lines := m.sendPreview.render(m.readerWidth(), g.readerBodyH, false, matcher{})
		out = append(out, strings.Split(m.readSV.ViewWithLines(lines), "\n")...)
		return padRows(out, width, m.panelHeight())
	}
	out = append(out, m.readerTitle(width, focused))
	if !m.previewing() && (m.searchOn || m.search.Value() != "") {
		out = append(out, fit(m.searchLine(), width))
	}
	out = append(out, "")
	var lines []string
	switch {
	case m.snapshot.Scope.Unavailable != "":
	case !m.hasDoc:
		lines = []string{styles.MutedStyle.Render(" Select a file")}
	case m.doc.State == StateLoading:
		lines = []string{styles.MutedStyle.Render(" Loading…")}
	case m.doc.State == StateError:
		lines = []string{styles.ErrorStyle.Render(" ⚠ " + sanitize(fallback(m.doc.Err, "cannot read file")))}
	case m.doc.Binary:
		lines = []string{styles.MutedStyle.Render(" Binary file" + noteSuffix(m.doc.Note))}
	case len(m.doc.Lines) == 0:
		if indicator := emptyFileIndicator(m.doc.Indicators); indicator != "" {
			lines = []string{indicator}
		} else {
			lines = []string{styles.MutedStyle.Render(" No textual changes" + noteSuffix(m.doc.Note))}
		}
	default:
		lines = m.reader.render(m.readerWidth(), g.readerBodyH, focused, newMatcher(m.search.Value()))
	}
	out = append(out, strings.Split(m.readSV.ViewWithLines(lines), "\n")...)
	return padRows(out, width, m.panelHeight())
}

func (m *Model) searchLine() string {
	if m.searchOn {
		return m.search.View()
	}
	cur, total := m.reader.matchPosition()
	return styles.InfoStyle.Render("/"+sanitize(m.search.Value())) + " " + styles.MutedStyle.Render(itoa(cur)+"/"+itoa(total))
}

func noteSuffix(note string) string {
	if note == "" {
		return ""
	}
	return " · " + sanitize(note)
}

func emptyFileIndicator(indicators []DocumentIndicator) string {
	for _, indicator := range indicators {
		if indicator.Line == 0 {
			return indicatorStyle(indicator.Kind).Render(" " + indicatorGlyph(indicator.Kind) + " " + map[ChangeKind]string{ChangeAdded: "new file", ChangeModified: "file changed", ChangeDeleted: "file deleted"}[indicator.Kind])
		}
	}
	return ""
}

func (m *Model) readerTitle(width int, focused bool) string {
	if !m.hasDoc {
		return heading(titleStyle(focused).Render("Reader"), m.panelHint(focused), width)
	}
	key := m.doc.Key
	var status []string
	if s := m.panelHint(focused); s != "" {
		status = append(status, s)
	}
	if key.Mode == ModeChanges {
		if key.Group != GroupNone {
			status = append(status, styles.MutedStyle.Render(key.Group.String()))
		}
		if cur, total := m.reader.hunkPosition(); total > 1 {
			status = append(status, styles.MutedStyle.Render("hunk "+itoa(cur)+"/"+itoa(total)))
		}
		if stats := fileStats(FileChange{Added: m.reader.added, Removed: m.reader.removed}); stats != "" {
			status = append(status, stats)
		}
	}
	if m.doc.Truncated {
		status = append(status, styles.WarningStyle.Render("truncated"))
	}
	if m.doc.State == StateReady && m.commentInput == nil {
		if key.Mode == ModeChanges {
			status = append(status, styles.MutedStyle.Render("[f File]"))
		} else {
			status = append(status, styles.MutedStyle.Render("[f Diff] [o Edit]"))
		}
	}
	right := strings.Join(status, styles.MutedStyle.Render(" · "))
	if right != "" {
		right = styles.MutedStyle.Render(" ") + right + styles.MutedStyle.Render(" ")
	}
	avail := max(4, width-ansi.StringWidth(right)-4)
	return heading(titleStyle(focused).Render(truncateMiddle(sanitize(key.Path), avail)), right, width)
}
