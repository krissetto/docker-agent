package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const paneMinWidth, paneMinHeight = 24, 6

// All identities are supervisor routing IDs, never persisted session IDs.
// The tree owns presentation only; the maps remain the sole component owners.
func (m *appModel) paneFocus() string {
	if m.supervisor == nil {
		return ""
	}
	return m.supervisor.ActiveID()
}

func (m *appModel) paneLayout() splitLayout {
	if len(m.panes.Sessions()) == 0 {
		return newSplitLayout(m.paneFocus())
	}
	return m.panes
}

func (m *appModel) panesEnabled() bool {
	return !m.leanMode && len(m.panes.Sessions()) > 1
}

func presentationRect(r splitRect) chat.PresentationRect {
	return chat.PresentationRect{X: r.X, Y: r.Y, Width: r.W, Height: r.H}
}

func paneRect(r chat.PresentationRect) splitRect {
	return splitRect{X: r.X, Y: r.Y, W: r.Width, H: r.Height}
}

func inPane(r splitRect, x, y int) bool {
	return x >= r.X && y >= r.Y && x < r.X+r.W && y < r.Y+r.H
}

func (m *appModel) measurePanes() (chat.SplitShellGeometry, splitRect, bool) {
	p, ok := m.chatPage.(chat.SplitPresentation)
	if !ok || m.leanMode {
		return chat.SplitShellGeometry{}, splitRect{}, false
	}
	shell := p.MeasureSplitShell(m.width, m.contentHeight)
	return shell, paneRect(shell.TranscriptArea), true
}

func (m *appModel) paneVisible(id string) bool {
	if !m.panesEnabled() {
		return id == m.paneFocus()
	}
	_, ok := m.paneGeometry.Panes[id]
	return ok
}

func setPaneVisible(page chat.Page, visible bool) tea.Cmd {
	if p, ok := page.(chat.PresentationVisibility); ok {
		return p.SetPresentationVisible(visible)
	}
	return nil
}

// Validate every optional seam before touching any page. Unsupported pages
// leave the old layout intact and give actionable feedback at the entry point.
func (m *appModel) supportsPanes(layout splitLayout) bool {
	if m.leanMode {
		return false
	}
	for _, id := range layout.Sessions() {
		if _, ok := m.chatPages[id].(chat.SplitPresentation); !ok {
			return false
		}
	}
	return true
}

func (m *appModel) resizePanes() tea.Cmd {
	var cmds []tea.Cmd
	if !m.panesEnabled() {
		m.paneGeometry = splitGeometry{}
		for id, page := range m.chatPages {
			if id == m.paneFocus() {
				if p, ok := page.(chat.SplitPresentation); ok {
					cmds = append(cmds, p.SetSplitPresentation(nil))
				}
			}
			if m.paneFocus() != "" {
				cmds = append(cmds, setPaneVisible(page, id == m.paneFocus()))
			}
		}
		return tea.Batch(append(cmds, m.chatPage.SetSize(m.width, m.contentHeight))...)
	}
	shell, bounds, ok := m.measurePanes()
	if !ok || !m.supportsPanes(m.panes) {
		return nil
	}
	geometry := m.panes.Compute(bounds, m.paneFocus(), paneMinWidth, paneMinHeight)
	if m.paneGesture != nil && (m.paneBounds != bounds || !samePaneGeometry(m.paneGeometry, geometry)) {
		m.cancelPaneGesture()
	}
	m.paneBounds, m.paneShell, m.paneGeometry = bounds, shell, geometry
	for id, page := range m.chatPages {
		r, visible := geometry.Panes[id]
		cmds = append(cmds, setPaneVisible(page, visible), chat.SetSidebarPresentationActive(page, visible && id == m.paneFocus()))
		if !visible {
			continue
		}
		transcript := splitRect{X: r.X, Y: r.Y + 1, W: r.W, H: max(0, r.H-1)}
		cmds = append(cmds, page.SetSize(m.width, m.contentHeight), page.(chat.SplitPresentation).SetSplitPresentation(&chat.SplitPresentationGeometry{
			Transcript: presentationRect(transcript), Shell: shell, ShowSidebar: id == m.paneFocus(),
		}))
	}
	return tea.Batch(cmds...)
}

func samePaneGeometry(a, b splitGeometry) bool {
	if a.Compact != b.Compact || len(a.Panes) != len(b.Panes) {
		return false
	}
	for id, r := range a.Panes {
		if b.Panes[id] != r {
			return false
		}
	}
	return true
}

func (m *appModel) splitPane(source, target string, edge splitEdge) tea.Cmd {
	layout, ok := m.paneLayout().Insert(source, target, edge)
	if !ok {
		return notification.ErrorCmd("Choose a different session and a pane edge")
	}
	if !m.supportsPaneCandidate(layout, source) {
		return notification.ErrorCmd("Panes unavailable: this session does not support split presentation")
	}
	_, bounds, ok := m.measurePanes()
	if !ok || layout.Compute(bounds, source, paneMinWidth, paneMinHeight).Compact {
		return notification.ErrorCmd("Not enough room: panes require at least 24 columns × 6 rows each")
	}
	if m.chatPages[source] == nil || m.pendingRestores[source] != "" {
		return m.beginPaneHydration(source, target, edge, layout, bounds)
	}
	m.cancelPaneGesture()
	m.panes = layout
	m.viewCacheValid = false
	_, cmd := m.handleSwitchTab(source)
	return cmd
}

func (m *appModel) removePane(id string) tea.Cmd {
	layout, ok := m.paneLayout().Remove(id)
	if !ok {
		return nil
	}
	m.cancelPaneGesture()
	m.panes = layout
	m.viewCacheValid = false
	if id == m.paneFocus() {
		_, cmd := m.handleSwitchTab(layout.Sessions()[0])
		return cmd
	}
	return m.resizeAll()
}

func (m *appModel) singlePane() tea.Cmd {
	m.cancelPaneGesture()
	m.panes = newSplitLayout(m.paneFocus())
	m.viewCacheValid = false
	return m.resizeAll()
}

// Switching a hidden tab substitutes the focused leaf, preserving the rest of
// the immutable tree and retaining the outgoing canonical page as a warm tab.
func (m *appModel) selectPaneTab(old, next string) {
	if !m.panesEnabled() {
		m.panes = newSplitLayout(next)
		return
	}
	if m.panes.Contains(next) {
		return
	}
	if !m.panes.Contains(old) {
		old = m.panes.Sessions()[0]
	}
	layout, ok := m.panes.Insert(next, old, splitRight)
	if ok {
		m.panes, _ = layout.Remove(old)
	}
}

func (m *appModel) paneAt(x, y int) string {
	for _, id := range m.panes.Sessions() {
		if inPane(m.paneGeometry.Panes[id], x, y) {
			return id
		}
	}
	return ""
}

// Pointer coordinates remain global: scrollview and message positions already
// use absolute terminal cells. Wheels never change the shared composer owner.
func (m *appModel) forwardPanePointer(msg tea.Msg, x, y int, focus bool) (tea.Model, tea.Cmd) {
	id := m.paneAt(x, y)
	if id == "" {
		if inPane(paneRect(m.paneShell.Sidebar), x, y) || inPane(paneRect(m.paneShell.SidebarHandle), x, y) || m.chatPage.IsSelecting() {
			return m.forwardChat(msg)
		}
		return m, nil
	}
	var cmds []tea.Cmd
	if focus && id != m.paneFocus() {
		_, cmd := m.handleSwitchTab(id)
		cmds = append(cmds, cmd)
	}
	if y == m.paneGeometry.Panes[id].Y {
		return m, tea.Batch(cmds...)
	}
	if id == m.paneFocus() {
		return m, tea.Batch(append(cmds, m.updateChatCmd(msg))...)
	}
	page, cmd := m.chatPages[id].Update(msg)
	m.chatPages[id] = page.(chat.Page)
	return m, tea.Batch(append(cmds, m.routePaneCmd(id, cmd))...)
}

func (m *appModel) tickVisiblePanes(msg animation.TickMsg) tea.Cmd {
	cmds := []tea.Cmd{m.updateChatCmd(msg)}
	for _, id := range m.panes.Sessions() {
		if id == m.paneFocus() || !m.paneVisible(id) {
			continue
		}
		page := m.chatPages[id]
		if page == nil {
			continue
		}
		updated, cmd := page.Update(msg)
		m.chatPages[id] = updated.(chat.Page)
		cmds = append(cmds, m.routePaneCmd(id, cmd))
	}
	return tea.Batch(cmds...)
}

func (m *appModel) visiblePaneCacheValid() bool {
	if !m.panesEnabled() {
		return true
	}
	if len(m.viewPaneGenerations) != len(m.paneGeometry.Panes) {
		return false
	}
	for id := range m.paneGeometry.Panes {
		if m.viewPaneGenerations[id] != chatVisualGeneration(m.chatPages[id]) {
			return false
		}
	}
	return true
}

func (m *appModel) cachePaneGenerations() {
	if !m.panesEnabled() {
		m.viewPaneGenerations = nil
		return
	}
	m.viewPaneGenerations = make(map[string]uint64, len(m.paneGeometry.Panes))
	for id := range m.paneGeometry.Panes {
		m.viewPaneGenerations[id] = chatVisualGeneration(m.chatPages[id])
	}
}

func paneClipped(content string, width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	lines := strings.Split(content, "\n")
	out := make([]string, height)
	for i := range out {
		if i < len(lines) {
			out[i] = ansi.Truncate(lines[i], width, "")
		}
		out[i] += strings.Repeat(" ", max(0, width-ansi.StringWidth(out[i])))
	}
	return strings.Join(out, "\n")
}

func (m *appModel) paneTitle(id string, width int) string {
	title := id
	if state := m.sessionStates[id]; state != nil && state.SessionTitle() != "" {
		title = state.SessionTitle()
	}
	prefix := "  "
	if id == m.paneFocus() {
		prefix = "● Composer: "
	}
	return styles.MutedStyle.Render(paneClipped(prefix+title, width, 1))
}

func (m *appModel) composePanes() string {
	layers := []*lipgloss.Layer{lipgloss.NewLayer(paneClipped("", m.width, m.contentHeight))}
	add := func(content string, r splitRect) {
		if r.W > 0 && r.H > 0 {
			layers = append(layers, lipgloss.NewLayer(paneClipped(content, r.W, r.H)).X(r.X).Y(r.Y))
		}
	}
	for _, id := range m.panes.Sessions() {
		r, visible := m.paneGeometry.Panes[id]
		if !visible {
			continue
		}
		p, ok := m.chatPages[id].(chat.SplitPresentation)
		if !ok {
			continue
		}
		add(m.paneTitle(id, r.W), splitRect{X: r.X, Y: r.Y, W: r.W, H: 1})
		add(p.TranscriptView(), splitRect{X: r.X, Y: r.Y + 1, W: r.W, H: r.H - 1})
	}
	for _, d := range m.paneGeometry.Dividers {
		glyph := strings.Repeat("─", d.Rect.W)
		if d.Axis == splitColumns {
			glyph = strings.Repeat("│\n", d.Rect.H)
		}
		add(styles.MutedStyle.Render(glyph), d.Rect)
	}
	if p, ok := m.chatPage.(chat.SplitPresentation); ok {
		add(p.SidebarView(), paneRect(m.paneShell.Sidebar))
		add(p.SidebarHandleView(), paneRect(m.paneShell.SidebarHandle))
	}
	return lipgloss.NewCompositor(layers...).Render()
}
