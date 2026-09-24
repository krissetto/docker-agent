package sessionbrowser

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

type group struct {
	key, label string
	active     bool
	rows       []Row
	children   []*group
}

func (m *Model) rebuild() {
	previous := m.selectedEntry()
	var parents []string
	if previous != nil {
		for e := previous; e != nil && e.parent != ""; {
			parents = append(parents, e.parent)
			parent := e.parent
			e = nil
			for i := range m.entries {
				if m.entries[i].key == parent {
					e = &m.entries[i]
					break
				}
			}
		}
	}
	projects := make([]*group, 0)
	projectByID := make(map[string]*group)
	dirByID := make(map[string]*group)
	for _, row := range m.rows {
		if row.SessionID == "" {
			continue
		}
		projectID := row.ProjectID
		if projectID == "" {
			projectID = row.WorkingDir
		}
		projectKey := "project:" + projectID
		project := projectByID[projectKey]
		if project == nil {
			label := singleLine(row.ProjectName)
			if label == "" {
				label = singleLine(filepath.Base(projectID))
			}
			if label == "" || label == "." {
				label = "Unknown project"
			}
			project = &group{key: projectKey, label: label}
			projectByID[projectKey] = project
			projects = append(projects, project)
		}
		project.active = project.active || row.Active
		dirKey := projectKey + "\x00dir:" + row.WorkingDir
		dir := dirByID[dirKey]
		if dir == nil {
			label := singleLine(row.WorkingDir)
			if label == "" {
				label = "Unknown directory"
			}
			dir = &group{key: dirKey, label: label}
			dirByID[dirKey] = dir
			project.children = append(project.children, dir)
		}
		dir.active = dir.active || row.Active
		dir.rows = append(dir.rows, row)
	}
	sortGroups := func(groups []*group) {
		slices.SortStableFunc(groups, func(a, b *group) int {
			if a.active != b.active {
				if a.active {
					return -1
				}
				return 1
			}
			return cmp.Compare(strings.ToLower(a.label), strings.ToLower(b.label))
		})
	}
	sortGroups(projects)
	m.entries = nil
	for _, project := range projects {
		m.entries = append(m.entries, entry{key: project.key, label: project.label})
		if m.collapsed[project.key] {
			continue
		}
		sortGroups(project.children)
		for _, dir := range project.children {
			m.entries = append(m.entries, entry{key: dir.key, parent: project.key, label: dir.label, depth: 1})
			if m.collapsed[dir.key] {
				continue
			}
			slices.SortStableFunc(dir.rows, func(a, b Row) int {
				if a.Active == b.Active {
					return 0
				}
				if a.Active {
					return -1
				}
				return 1
			})
			for _, row := range dir.rows {
				title := singleLine(row.Title)
				if title == "" {
					title = "Session " + ansi.Truncate(singleLine(row.SessionID), 8, "")
				}
				m.entries = append(m.entries, entry{key: "session:" + row.SessionID, parent: dir.key, label: title, row: &row, depth: 2})
			}
		}
	}
	if m.hasMore {
		m.entries = append(m.entries, entry{key: "more", label: "Load more · Ctrl+L", more: true})
	}
	if m.selectedEntry() == nil {
		m.selected = ""
		for _, parent := range parents {
			for _, e := range m.entries {
				if e.key == parent {
					m.selected = parent
					break
				}
			}
			if m.selected != "" {
				break
			}
		}
		if m.selected == "" && len(m.entries) > 0 {
			m.selected = m.entries[0].key
			for _, e := range m.entries {
				if e.row != nil {
					m.selected = e.key
					break
				}
			}
		}
	}
}

func (m *Model) prepareHeader() {
	m.header = nil
	m.hits = nil
	m.header = append(m.header, styles.TabTitleStyle.Bold(true).Render("Sessions"))
	m.header = append(m.header, m.search.View())
	m.hits = append(m.hits, hit{x: 0, y: 1, width: m.width, action: "search"})
	line := "‹ "
	m.hits = append(m.hits, hit{x: 0, y: 2, width: min(1, m.width), action: "previous-workspace"})
	name := "No workspace"
	if item := m.selectedWorkspace(); item != nil {
		name = singleLine(item.Name)
		if item.Active {
			name = "● " + name
		}
	}
	name = ansi.Truncate(name, max(0, m.width-4), "…")
	line += name + " ›"
	m.hits = append(m.hits, hit{x: 2, y: 2, width: ansi.StringWidth(name), action: "select-workspace"}, hit{x: 3 + ansi.StringWidth(name), y: 2, width: 1, action: "next-workspace"})
	if m.renaming != "" {
		line = m.rename.View()
		m.hits = m.hits[:1]
	}
	if m.area == workspaceArea && m.focused {
		line = styles.SelectionStyle.Render(line)
	}
	m.header = append(m.header, line)
	controls := []struct{ label, action string }{{"^N New", "create"}, {"F2 Rename", "rename"}, {"^W Close", "close"}}
	line = ""
	for _, control := range controls {
		if line != "" {
			line += " "
		}
		start := ansi.StringWidth(line)
		line += control.label
		// A clipped control is not a clickable action.
		if ansi.StringWidth(line) <= m.width {
			m.hits = append(m.hits, hit{x: start, y: 3, width: ansi.StringWidth(control.label), action: control.action})
		}
	}
	m.header = append(m.header, styles.MutedStyle.Render(line))
	status := fmt.Sprintf("%d sessions", len(m.rows))
	switch {
	case m.renameError != "":
		status = m.renameError
	case m.renaming != "":
		status = "Enter save · Esc cancel"
	case m.loading:
		status = "Loading sessions…"
	case m.err != "":
		status = "Error: " + m.err + " · Ctrl+L retry"
	case len(m.rows) == 0 && m.Query() != "":
		status = "No matching sessions"
	case len(m.rows) == 0:
		status = "No sessions yet"
	case m.hasMore:
		status += " · Ctrl+L more"
	}
	m.header = append(m.header, styles.MutedStyle.Render(status))
}

func (m *Model) prepareLines() {
	m.lineEntries = nil
	m.starts = nil
	m.ends = nil
	lines := make([]string, 0, len(m.entries))
	width := m.scroll.ContentWidth()
	for i, e := range m.entries {
		prefix := strings.Repeat("  ", e.depth)
		if e.row == nil && !e.more {
			if m.collapsed[e.key] {
				prefix += "▸ "
			} else {
				prefix += "▾ "
			}
		} else if e.row != nil {
			switch {
			case e.row.NeedsAttention:
				prefix += "! "
			case e.row.Active:
				prefix += "● "
			default:
				prefix += "· "
			}
		}
		label := e.label
		if e.row != nil {
			if e.row.Activity != "" {
				label += " · " + singleLine(e.row.Activity)
			}
			if e.row.UnavailableReason != "" {
				label += " · unavailable: " + singleLine(e.row.UnavailableReason)
			}
		}
		indent := min(ansi.StringWidth(prefix), max(0, width-2))
		prefix = ansi.Truncate(prefix, indent, "")
		wrapped := strings.Split(ansi.Hardwrap(ansi.Wrap(label, max(1, width-indent), ""), max(1, width-indent), false), "\n")
		m.starts = append(m.starts, len(lines))
		for j, text := range wrapped {
			leader := prefix
			if j > 0 {
				leader = strings.Repeat(" ", indent)
			}
			text = ansi.Truncate(leader+text, width, "")
			style := styles.BaseStyle
			if e.row == nil {
				style = styles.SecondaryStyle
			}
			if e.row != nil && e.row.UnavailableReason != "" {
				style = styles.MutedStyle
			}
			if e.key == m.selected && m.focused && m.area == treeArea {
				style = styles.SelectionStyle
			}
			lines = append(lines, style.Render(text))
			m.lineEntries = append(m.lineEntries, i)
		}
		m.ends = append(m.ends, len(lines)-1)
	}
	m.scroll.SetContent(lines, len(lines))
}

func (m *Model) View() string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}
	lines := append([]string(nil), m.header[:min(m.height, len(m.header))]...)
	if m.bodyHeight > 0 {
		lines = append(lines, strings.Split(m.scroll.View(), "\n")...)
	}
	if len(lines) < m.height {
		help := "↵ Open  ^S Split  ^O Other"
		if e := m.selectedEntry(); e != nil && e.row != nil && e.row.UnavailableReason != "" {
			help = "^R Retry · Delete remove missing"
		}
		if m.area == searchArea {
			help = "Search titles/paths · Esc tree"
		} else if m.area == workspaceArea {
			help = "←/→ Workspace · ↵ Select"
		}
		lines = append(lines, styles.MutedStyle.Render(help))
	}
	for len(lines) < m.height {
		lines = append(lines, "")
	}
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], m.width, "")
		lines[i] += strings.Repeat(" ", max(0, m.width-ansi.StringWidth(lines[i])))
	}
	return strings.Join(lines[:m.height], "\n")
}
