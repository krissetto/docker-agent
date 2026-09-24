package sessionbrowser

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

func browser(t *testing.T) *Model {
	t.Helper()
	m := New()
	m.SetSize(36, 18)
	m.Focus()
	m.SetWorkspaces([]WorkspaceItem{{ID: "one", Name: "First", Active: true}, {ID: "two", Name: "Second"}})
	m.SetRows([]Row{{SessionID: "a", Title: "First session", ProjectID: "/repo", WorkingDir: "/repo/main"}, {SessionID: "b", Title: "Second session", ProjectID: "/repo", WorkingDir: "/repo/main"}}, false)
	return m
}
func key(code rune, mods ...tea.KeyMod) tea.KeyPressMsg {
	var mod tea.KeyMod
	for _, m := range mods {
		mod |= m
	}
	return tea.KeyPressMsg{Code: code, Mod: mod}
}
func commandMessages(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var result []tea.Msg
		for _, child := range batch {
			result = append(result, commandMessages(child)...)
		}
		return result
	}
	return []tea.Msg{msg}
}
func TestKeyboardOpenMessages(t *testing.T) {
	m := browser(t)
	for _, tt := range []struct {
		key  tea.KeyPressMsg
		mode string
	}{{key(tea.KeyEnter), "open"}, {key('s', tea.ModCtrl), "split"}, {key('o', tea.ModCtrl), "other"}} {
		require.Equal(t, []tea.Msg{Open{SessionID: "a", Mode: tt.mode}}, commandMessages(m.Update(tt.key)))
	}
	m.Blur()
	assert.Nil(t, m.Update(key(tea.KeyEnter)))
}
func TestSearchIsCallerOwned(t *testing.T) {
	m := browser(t)
	m.Update(key('/'))
	msgs := commandMessages(m.Update(tea.PasteMsg{Content: "elsewhere/東京"}))
	assert.Contains(t, msgs, QueryChanged{Query: "elsewhere/東京"})
	assert.Len(t, m.rows, 2, "query must not silently search only loaded metadata")
	m.Update(key(tea.KeyEscape))
	assert.Equal(t, "a", m.SelectedSessionID())
}
func TestActiveFirstAndStableSelection(t *testing.T) {
	m := browser(t)
	rows := []Row{
		{SessionID: "z", Title: "Old", ProjectID: "/alpha", WorkingDir: "/alpha"},
		{SessionID: "b", Title: "Inactive", ProjectID: "/zulu", WorkingDir: "/zulu/other"},
		{SessionID: "a", Title: "Selected", ProjectID: "/zulu", WorkingDir: "/zulu/main"},
		{SessionID: "active", Title: "Running", ProjectID: "/zulu", WorkingDir: "/zulu/main", Active: true},
	}
	m.SetRows(rows, true)
	require.Equal(t, "project:/zulu", m.entries[0].key)
	require.Equal(t, "/zulu/main", m.entries[1].label)
	assert.Equal(t, "active", m.entries[2].row.SessionID)
	assert.Equal(t, "a", m.SelectedSessionID())
	rows[3].Active = false
	rows[0].Active = true
	m.SetRows(rows, false)
	assert.Equal(t, "a", m.SelectedSessionID())
	assert.Equal(t, "project:/alpha", m.entries[0].key)
}
func TestCollapseHierarchyAndRefresh(t *testing.T) {
	m := browser(t)
	m.Update(key(tea.KeyLeft))
	require.Nil(t, m.selectedEntry().row)
	m.Update(key(tea.KeyLeft))
	require.Len(t, m.entries, 2)
	m.SetRows(m.rows, false)
	require.Len(t, m.entries, 2, "refresh must preserve collapsed groups")
	m.Update(key(tea.KeyLeft))
	assert.Equal(t, "project:/repo", m.selected)
	m.Update(key(tea.KeyLeft))
	assert.Len(t, m.entries, 1)
	m.Update(key(tea.KeyRight))
	assert.Len(t, m.entries, 2)
	m.Update(key(tea.KeyRight))
	m.Update(key(tea.KeyRight))
	assert.Len(t, m.entries, 4)
	m.Update(key(tea.KeyEnd))
	assert.Equal(t, "b", m.SelectedSessionID())
	m.Update(key(tea.KeyHome))
	assert.Equal(t, "project:/repo", m.selected)
}
func TestWorkspaceMessagesAndRename(t *testing.T) {
	m := browser(t)
	assert.Equal(t, []tea.Msg{CreateWorkspace{}}, commandMessages(m.Update(key('n', tea.ModCtrl))))
	assert.Equal(t, []tea.Msg{CloseWorkspace{ID: "one"}}, commandMessages(m.Update(key('w', tea.ModCtrl))))
	m.Update(key(tea.KeyTab))
	m.Update(key(tea.KeyTab))
	m.Update(key(tea.KeyRight))
	assert.Equal(t, []tea.Msg{SelectWorkspace{ID: "two"}}, commandMessages(m.Update(key(tea.KeyEnter))))
	m.Update(key(tea.KeyF2))
	m.rename.SetValue("   ")
	assert.Nil(t, m.Update(key(tea.KeyEnter)))
	assert.NotEmpty(t, m.renameError)
	assert.Equal(t, "two", m.renaming)
	m.rename.SetValue(" Renamed 東京 ")
	assert.Equal(t, []tea.Msg{RenameWorkspace{ID: "two", Name: "Renamed 東京"}}, commandMessages(m.Update(key(tea.KeyEnter))))
	assert.Empty(t, m.renaming)
	m.Update(key(tea.KeyF2))
	m.rename.SetValue("Discard")
	m.Update(key(tea.KeyEscape))
	assert.Empty(t, m.renaming)
	assert.Equal(t, "Second", m.selectedWorkspace().Name, "rename is not optimistically persisted")
	m.SetWorkspaces([]WorkspaceItem{{ID: "two", Name: "Second"}, {ID: "one", Name: "First", Active: true}})
	assert.Equal(t, "two", m.workspaceID)
}
func TestRenameRejectsControlsAndCancelsWhenRemoved(t *testing.T) {
	m := browser(t)
	m.Update(key(tea.KeyF2))
	m.rename.SetValue("valid")
	m.SetWorkspaces(nil)
	assert.Empty(t, m.renaming)
	assert.Nil(t, m.Update(key(tea.KeyF2)))
}
func TestLoadMoreErrorAndMissing(t *testing.T) {
	m := browser(t)
	assert.Nil(t, m.Update(key('l', tea.ModCtrl)))
	m.SetRows(m.rows, true)
	m.Update(key(tea.KeyEnd))
	assert.Equal(t, []tea.Msg{LoadMore{}}, commandMessages(m.Update(key(tea.KeyEnter))))
	m.SetLoading(true)
	assert.Contains(t, ansi.Strip(m.View()), "Loading sessions")
	assert.Nil(t, m.Update(key('l', tea.ModCtrl)))
	m.SetLoading(false)
	m.SetRows(nil, false)
	m.SetError("database unavailable")
	assert.Contains(t, ansi.Strip(m.View()), "Error: database unavailable")
	assert.Equal(t, []tea.Msg{LoadMore{}}, commandMessages(m.Update(key('l', tea.ModCtrl))))
	m.SetError("")
	assert.Contains(t, ansi.Strip(m.View()), "No sessions yet")
	m.SetRows([]Row{{SessionID: "missing", UnavailableReason: "directory removed"}}, false)
	assert.Contains(t, ansi.Strip(m.View()), "unavailable")
	assert.Nil(t, m.Update(key(tea.KeyEnter)))
	assert.Equal(t, []tea.Msg{RetryMissing{SessionID: "missing"}}, commandMessages(m.Update(key('r', tea.ModCtrl))))
	assert.Equal(t, []tea.Msg{RemoveMissing{SessionID: "missing"}}, commandMessages(m.Update(key(tea.KeyDelete))))
}
func TestWrappedUnicodeMouseAndBounds(t *testing.T) {
	m := browser(t)
	m.SetPosition(7, 11)
	m.SetRows([]Row{{SessionID: "unicode", Title: "\x1b[31m東京🦊é\x1b[0m " + strings.Repeat("世界", 25), ProjectID: "/repo", WorkingDir: "/repo/main"}}, false)
	m.selected = "session:unicode"
	m.prepare()
	i := m.selectedIndex()
	require.Greater(t, m.ends[i], m.starts[i])
	for _, line := range []int{m.starts[i], m.starts[i] + 1} {
		click := tea.MouseClickMsg{X: 8, Y: 11 + m.bodyY + line, Button: tea.MouseLeft}
		assert.Equal(t, []tea.Msg{Open{SessionID: "unicode", Mode: "open"}}, commandMessages(m.Update(click)))
	}
	for _, point := range [][2]int{{6, 11 + m.bodyY}, {7 + m.width, 11 + m.bodyY}, {8, 10}, {8, 11 + m.height}, {7 + m.scroll.ContentWidth(), 11 + m.bodyY + m.starts[i]}} {
		assert.Nil(t, m.Update(tea.MouseClickMsg{X: point[0], Y: point[1], Button: tea.MouseLeft}))
	}
	assert.NotContains(t, strings.Join(m.header, "\n"), "\x1b[31m")
}
func TestPagingWheelAndPinnedControls(t *testing.T) {
	m := browser(t)
	var rows []Row
	for i := range 60 {
		rows = append(rows, Row{SessionID: fmt.Sprint(i), Title: fmt.Sprintf("Session %d", i), WorkingDir: "/repo"})
	}
	m.SetRows(rows, true)
	header := append([]string(nil), m.header...)
	m.Update(key(tea.KeyPgDown))
	assert.Greater(t, m.selectedIndex(), 2)
	m.Update(key(tea.KeyEnd))
	assert.Equal(t, "more", m.selected)
	assert.Greater(t, m.scroll.ScrollOffset(), 0)
	assert.Equal(t, header, m.header)
	before := m.scroll.ScrollOffset()
	m.Update(tea.MouseWheelMsg{X: 2, Y: m.bodyY + 1, Button: tea.MouseWheelUp})
	assert.Less(t, m.scroll.ScrollOffset(), before)
	before = m.scroll.ScrollOffset()
	m.Update(tea.MouseWheelMsg{X: 2, Y: 1, Button: tea.MouseWheelUp})
	assert.Equal(t, before, m.scroll.ScrollOffset(), "header wheel must not move hidden list")
	m.Update(key(tea.KeyHome))
	assert.Zero(t, m.scroll.ScrollOffset())
}
func TestAllSizesAndResizingSelection(t *testing.T) {
	m := browser(t)
	m.SetRows([]Row{{SessionID: "a", Title: strings.Repeat("東京🦊", 20), WorkingDir: "/long/path"}}, true)
	for width := 0; width < 45; width++ {
		for height := 0; height < 22; height++ {
			m.SetSize(width, height)
			view := m.View()
			if width == 0 || height == 0 {
				require.Empty(t, view)
				continue
			}
			lines := strings.Split(view, "\n")
			require.Len(t, lines, height, "size %dx%d", width, height)
			for _, line := range lines {
				require.Equal(t, width, ansi.StringWidth(line), "size %dx%d", width, height)
			}
			assert.Equal(t, "a", m.SelectedSessionID())
		}
	}
}
func TestCoalescedWheelBounds(t *testing.T) {
	m := browser(t)
	m.SetSize(20, 9)
	m.SetPosition(3, 4)
	m.Update(messages.WheelCoalescedMsg{X: 4, Y: 4 + m.bodyY, Delta: 2})
	require.Positive(t, m.scroll.ScrollOffset())
	before := m.scroll.ScrollOffset()
	for _, point := range [][2]int{{2, 4 + m.bodyY}, {4, 4}, {4, 4 + m.height - 1}} {
		m.Update(messages.WheelCoalescedMsg{X: point[0], Y: point[1], Delta: -1})
		assert.Equal(t, before, m.scroll.ScrollOffset())
	}
}

func TestWorkspaceMouseUsesCellBounds(t *testing.T) {
	m := browser(t)
	m.SetWorkspaces([]WorkspaceItem{{ID: "one", Name: "東京🦊", Active: true}, {ID: "two", Name: "Other"}})
	var next hit
	for _, h := range m.hits {
		if h.action == "next-workspace" {
			next = h
		}
	}
	require.Positive(t, next.width)
	m.Update(tea.MouseClickMsg{X: next.x, Y: next.y, Button: tea.MouseLeft})
	assert.Equal(t, "two", m.workspaceID)
	assert.Nil(t, m.Update(tea.MouseClickMsg{X: 35, Y: 2, Button: tea.MouseLeft}), "blank padding is not an action")
}
