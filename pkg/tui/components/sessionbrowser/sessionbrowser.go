// Package sessionbrowser presents caller-owned session metadata and workspace controls.
package sessionbrowser

import (
	"strings"
	"unicode"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type Row struct {
	SessionID, Title, ProjectID, ProjectName, WorkingDir, Activity, UnavailableReason string
	NeedsAttention, Active                                                            bool
}

type WorkspaceItem struct {
	ID, Name string
	Active   bool
}

type QueryChanged struct{ Query string }
type Open struct{ SessionID, Mode string }
type LoadMore struct{}
type RenameWorkspace struct{ ID, Name string }
type SelectWorkspace struct{ ID string }
type CreateWorkspace struct{}
type CloseWorkspace struct{ ID string }
type RetryMissing struct{ SessionID string }
type RemoveMissing struct{ SessionID string }

type focusArea int

const (
	treeArea focusArea = iota
	searchArea
	workspaceArea
)

type entry struct {
	key, parent, label string
	row                *Row
	depth              int
	more               bool
}

type hit struct {
	x, y, width int
	action      string
}

type Model struct {
	width, height, x, y   int
	focused               bool
	area                  focusArea
	search, rename        textinput.Model
	renaming, renameError string
	rows                  []Row
	workspaces            []WorkspaceItem
	workspaceID           string
	hasMore, loading      bool
	err                   string
	collapsed             map[string]bool
	entries               []entry
	selected              string
	scroll                *scrollview.Model
	header                []string
	hits                  []hit
	lineEntries           []int
	starts, ends          []int
	bodyY, bodyHeight     int
}

func New() *Model {
	search := textinput.New()
	search.Prompt = "/ "
	search.Placeholder = "Search titles and paths"
	search.CharLimit = 256
	rename := textinput.New()
	rename.Prompt = "Name: "
	rename.CharLimit = 80
	return &Model{search: search, rename: rename, collapsed: make(map[string]bool), scroll: scrollview.New(scrollview.WithReserveScrollbarSpace(true), scrollview.WithKeyMap(nil), scrollview.WithFadeEffectDisabled())}
}

func (m *Model) Init() tea.Cmd { return nil }
func (m *Model) Focused() bool { return m.focused }
func (m *Model) Query() string { return m.search.Value() }
func (m *Model) SelectedSessionID() string {
	if e := m.selectedEntry(); e != nil && e.row != nil {
		return e.row.SessionID
	}
	return ""
}
func (m *Model) Focus() tea.Cmd {
	m.focused = true
	return m.focusInput()
}
func (m *Model) Blur() {
	m.focused = false
	m.search.Blur()
	m.rename.Blur()
	m.prepare()
}
func (m *Model) SetSize(width, height int) tea.Cmd {
	m.width, m.height = max(0, width), max(0, height)
	m.search.SetWidth(max(1, m.width-2))
	m.rename.SetWidth(max(1, m.width-6))
	m.prepare()
	m.ensureSelectedVisible()
	return nil
}

// SetPosition sets the absolute origin used by mouse messages.
func (m *Model) SetPosition(x, y int) { m.x, m.y = x, y; m.prepare() }

// SetRows accepts a complete, already queried page accumulation; it never filters locally.
func (m *Model) SetRows(rows []Row, hasMore bool) {
	m.rows = append([]Row(nil), rows...)
	m.hasMore = hasMore
	m.rebuild()
	m.prepare()
	m.ensureSelectedVisible()
}
func (m *Model) SetWorkspaces(items []WorkspaceItem) {
	m.workspaces = append([]WorkspaceItem(nil), items...)
	found := false
	for _, item := range items {
		found = found || item.ID == m.workspaceID
	}
	if !found {
		m.workspaceID = ""
		for _, item := range items {
			if m.workspaceID == "" || item.Active {
				m.workspaceID = item.ID
			}
			if item.Active {
				break
			}
		}
	}
	if m.renaming != "" {
		found = false
		for _, item := range items {
			found = found || item.ID == m.renaming
		}
		if !found {
			m.cancelRename()
		}
	}
	m.prepare()
}
func (m *Model) SetLoading(loading bool) { m.loading = loading; m.prepare() }
func (m *Model) SetError(message string) { m.err = singleLine(message); m.prepare() }

func (m *Model) selectedEntry() *entry {
	for i := range m.entries {
		if m.entries[i].key == m.selected {
			return &m.entries[i]
		}
	}
	return nil
}
func (m *Model) selectedIndex() int {
	for i := range m.entries {
		if m.entries[i].key == m.selected {
			return i
		}
	}
	return -1
}
func (m *Model) ensureSelectedVisible() {
	if i := m.selectedIndex(); i >= 0 && i < len(m.starts) && m.bodyHeight > 0 {
		m.scroll.EnsureRangeVisible(m.starts[i], m.ends[i])
	}
}
func (m *Model) focusInput() tea.Cmd {
	m.search.Blur()
	m.rename.Blur()
	var cmd tea.Cmd
	if m.focused {
		if m.renaming != "" {
			cmd = m.rename.Focus()
		} else if m.area == searchArea {
			cmd = m.search.Focus()
		}
	}
	m.prepare()
	return cmd
}
func singleLine(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, ansi.Strip(value))
	return strings.Join(strings.Fields(value), " ")
}
func emit(msg tea.Msg) tea.Cmd { return func() tea.Msg { return msg } }
func (m *Model) selectedWorkspace() *WorkspaceItem {
	for i := range m.workspaces {
		if m.workspaces[i].ID == m.workspaceID {
			return &m.workspaces[i]
		}
	}
	return nil
}
func (m *Model) beginRename() tea.Cmd {
	item := m.selectedWorkspace()
	if item == nil {
		return nil
	}
	m.renaming = item.ID
	m.renameError = ""
	m.rename.SetValue(singleLine(item.Name))
	m.rename.CursorEnd()
	return m.focusInput()
}
func (m *Model) cancelRename() {
	m.renaming = ""
	m.renameError = ""
	m.rename.Blur()
}
func (m *Model) commitRename() tea.Cmd {
	name := strings.TrimSpace(m.rename.Value())
	if name == "" || singleLine(name) != name {
		m.renameError = "Use a nonempty, single-line name"
		return nil
	}
	id := m.renaming
	m.cancelRename()
	m.focusInput()
	return emit(RenameWorkspace{ID: id, Name: name})
}
func (m *Model) loadMore() tea.Cmd {
	if m.loading || (!m.hasMore && m.err == "") {
		return nil
	}
	return emit(LoadMore{})
}
func (m *Model) open(mode string) tea.Cmd {
	e := m.selectedEntry()
	if e == nil {
		return nil
	}
	if e.more {
		return m.loadMore()
	}
	if e.row == nil {
		m.collapsed[e.key] = !m.collapsed[e.key]
		m.rebuild()
		return nil
	}
	if e.row.UnavailableReason != "" {
		return nil
	}
	return emit(Open{SessionID: e.row.SessionID, Mode: mode})
}

func (m *Model) prepare() {
	m.search.SetStyles(styles.DialogInputStyle)
	m.rename.SetStyles(styles.DialogInputStyle)
	m.prepareHeader()
	m.bodyY = min(len(m.header), m.height)
	footer := 0
	if m.height > m.bodyY {
		footer = 1
	}
	m.bodyHeight = max(0, m.height-m.bodyY-footer)
	m.scroll.SetSize(m.width, m.bodyHeight)
	m.scroll.SetPosition(m.x, m.y+m.bodyY)
	m.prepareLines()
}
