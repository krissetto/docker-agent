package tui

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

type panePointerTransaction struct {
	source          string
	startX, startY  int
	layout          splitLayout
	order           []string
	bounds          splitRect
	geometry        splitGeometry
	active, reorder bool
	target          string
	edge            splitEdge
	preview         splitRect
	divider         *splitDivider
	position        int
	keyboard        bool
}

func (m *appModel) paneOrder() []string {
	var ids []string
	if m.supervisor != nil {
		tabs, _ := m.supervisor.GetTabs()
		for _, tab := range tabs {
			ids = append(ids, tab.SessionID)
		}
	}
	return ids
}

func (m *appModel) cancelPaneGesture() {
	if m.paneHydration != nil {
		m.paneHydration.cancel()
		m.paneHydration = nil
		m.viewCacheValid = false
	}
	if m.paneGesture == nil {
		return
	}
	// Neither layout nor supervisor order changes during previews.
	m.paneGesture = nil
	if m.tabBar != nil {
		m.tabBar.CancelPointer()
	}
	m.viewCacheValid = false
}

func (m *appModel) validPaneGesture() bool {
	g := m.paneGesture
	if g == nil {
		return false
	}
	if m.dialogMgr.Open() || m.leanMode || !slices.Equal(g.order, m.paneOrder()) {
		return false
	}
	if g.source != "" && m.supervisor.GetRunner(g.source) == nil {
		return false
	}
	_, bounds, ok := m.measurePanes()
	return ok && bounds == g.bounds && m.paneLayout().root == g.layout.root
}

func (m *appModel) beginPaneGesture(msg tea.MouseClickMsg) bool {
	if m.leanMode || m.dialogMgr.Open() || msg.Button != tea.MouseLeft {
		return false
	}
	shell, bounds, supported := m.measurePanes()
	_ = shell
	if !supported {
		return false
	}
	if m.hitTestRegion(msg.Y) == regionTabBar {
		source, ok := m.tabBar.TabBodyAt(msg.X-tabFrameOrigin(), msg.Y-m.contentHeight-1)
		if !ok || m.supervisor == nil || m.supervisor.GetRunner(source) == nil {
			return false
		}
		// Materialize the initial leaf so geometry/topology tokens are stable.
		m.panes = m.paneLayout()
		m.paneGesture = &panePointerTransaction{source: source, startX: msg.X, startY: msg.Y, layout: m.panes, order: m.paneOrder(), bounds: bounds, geometry: m.panes.Compute(bounds, m.paneFocus(), paneMinWidth, paneMinHeight)}
		return true
	}
	if m.panesEnabled() && !m.paneGeometry.Compact {
		for _, d := range m.paneGeometry.Dividers {
			if inPane(d.Rect, msg.X, msg.Y) {
				m.paneGesture = &panePointerTransaction{layout: m.panes, order: m.paneOrder(), bounds: bounds, geometry: m.paneGeometry, divider: &d, active: true, position: dividerPosition(d)}
				return true
			}
		}
	}
	return false
}

func dividerPosition(d splitDivider) int {
	if d.Axis == splitColumns {
		return d.Rect.X
	}
	return d.Rect.Y
}

func cellDistance(a, b int) int {
	if a > b {
		return a - b
	}
	return b - a
}

// Ratios rather than raw cell distance make the edge zones spatially stable
// in wide/short panes. Exact corner ties choose left, right, top, bottom.
func nearestPaneEdge(r splitRect, x, y int) (splitEdge, bool) {
	if !inPane(r, x, y) || r.W <= 0 || r.H <= 0 {
		return 0, false
	}
	d := []float64{float64(x-r.X) / float64(r.W), float64(r.X+r.W-1-x) / float64(r.W), float64(y-r.Y) / float64(r.H), float64(r.Y+r.H-1-y) / float64(r.H)}
	edge := 0
	for i := 1; i < len(d); i++ {
		if d[i] < d[edge] {
			edge = i
		}
	}
	return splitEdge(edge), d[edge] <= 0.25
}

func (m *appModel) paneDrop(x, y int) (string, splitEdge, splitRect, bool) {
	g := m.paneGesture
	if g == nil || !inPane(g.bounds, x, y) {
		return "", 0, splitRect{}, false
	}
	for _, id := range g.layout.Sessions() {
		r, visible := g.geometry.Panes[id]
		if !visible || id == g.source {
			continue
		}
		edge, ok := nearestPaneEdge(r, x, y)
		if !ok {
			continue
		}
		next, ok := g.layout.Insert(g.source, id, edge)
		if !ok || !m.supportsPaneCandidate(next, g.source) {
			return "", 0, splitRect{}, false
		}
		geometry := next.Compute(g.bounds, g.source, paneMinWidth, paneMinHeight)
		if geometry.Compact {
			return "", 0, splitRect{}, false
		}
		return id, edge, geometry.Panes[g.source], true
	}
	return "", 0, splitRect{}, false
}

func (m *appModel) movePaneGesture(msg tea.MouseMotionMsg) tea.Cmd {
	if !m.validPaneGesture() {
		m.cancelPaneGesture()
		return nil
	}
	g := m.paneGesture
	if g.divider != nil {
		position := msg.X
		if g.divider.Axis == splitRows {
			position = msg.Y
		}
		m.previewPaneDivider(position)
		return nil
	}
	if !g.active && (cellDistance(msg.X, g.startX) >= 3 || cellDistance(msg.Y, g.startY) >= 2) {
		g.active = true
	}
	if !g.active {
		return nil
	}
	m.viewCacheValid = false
	if m.hitTestRegion(msg.Y) == regionTabBar && msg.Y == g.startY {
		if !g.reorder {
			m.tabBar.BeginPointerPreview(g.startX - tabFrameOrigin())
			g.reorder = true
		}
		g.target, g.preview = "", splitRect{}
		return m.tabBar.PreviewPointer(msg.X - tabFrameOrigin())
	}
	if g.reorder {
		m.tabBar.CancelPointer()
		g.reorder = false
	}
	g.target, g.edge, g.preview, _ = m.paneDrop(msg.X, msg.Y)
	return nil
}

func (m *appModel) previewPaneDivider(position int) {
	g := m.paneGesture
	if position == dividerPosition(*g.divider) {
		g.preview, g.position = g.divider.Rect, position
		m.viewCacheValid = false
		return
	}
	next, ok := g.layout.Resize(g.divider.ID, position, g.bounds, paneMinWidth, paneMinHeight)
	if !ok {
		return
	}
	for _, d := range next.Compute(g.bounds, m.paneFocus(), paneMinWidth, paneMinHeight).Dividers {
		if d.ID == g.divider.ID {
			g.preview, g.position = d.Rect, dividerPosition(d)
		}
	}
	m.viewCacheValid = false
}

func (m *appModel) releasePaneGesture(msg tea.MouseReleaseMsg) tea.Cmd {
	if !m.validPaneGesture() {
		m.cancelPaneGesture()
		return nil
	}
	g := m.paneGesture
	if msg.Button != tea.MouseLeft {
		m.cancelPaneGesture()
		return nil
	}
	if g.divider != nil {
		if !inPane(g.bounds, msg.X, msg.Y) {
			m.cancelPaneGesture()
			return nil
		}
		position := msg.X
		if g.divider.Axis == splitRows {
			position = msg.Y
		}
		return m.commitPaneDivider(position)
	}
	if !g.active {
		source, hit := m.tabBar.TabBodyAt(msg.X-tabFrameOrigin(), msg.Y-m.contentHeight-1)
		m.cancelPaneGesture()
		if hit && source == g.source {
			_, cmd := m.handleSwitchTab(source)
			return cmd
		}
		return nil
	}
	if g.reorder && m.hitTestRegion(msg.Y) == regionTabBar && msg.X >= tabFrameOrigin() && msg.X < tabFrameOrigin()+tabFrameWidth(m.width) {
		// Capture the command before ending the root transaction. The tabbar
		// emits precisely one reorder message and never mutates supervisor order.
		cmd := m.tabBar.CommitPointer(msg.X - tabFrameOrigin())
		m.paneGesture = nil
		return cmd
	}
	target, edge, _, ok := m.paneDrop(msg.X, msg.Y)
	m.cancelPaneGesture()
	if ok {
		return m.splitPane(g.source, target, edge)
	}
	return nil
}

func (m *appModel) commitPaneDivider(position int) tea.Cmd {
	g := m.paneGesture
	next, ok := g.layout.Resize(g.divider.ID, position, g.bounds, paneMinWidth, paneMinHeight)
	m.cancelPaneGesture()
	if !ok {
		return nil
	}
	m.panes = next
	return m.resizeAll()
}

func (m *appModel) paneGestureLayer() *lipgloss.Layer {
	if m.dialogMgr.Open() || m.paneGesture == nil {
		return nil
	}
	r := m.paneGesture.preview
	if r.W <= 0 || r.H <= 0 {
		return nil
	}
	// Static source rectangle: no page SetSize/View and no history reflow.
	text := strings.Repeat("░", r.W)
	if m.paneGesture.divider != nil {
		text = strings.Repeat("┃", r.W)
	}
	rows := make([]string, r.H)
	for i := range rows {
		rows[i] = text
	}
	return lipgloss.NewLayer(styles.MutedStyle.Render(strings.Join(rows, "\n"))).X(r.X).Y(r.Y)
}

func (m *appModel) handlePaneGestureKey(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	if m.paneHydration != nil && msg.String() == "esc" {
		m.cancelPaneGesture()
		return nil, true
	}
	g := m.paneGesture
	if g == nil {
		return nil, false
	}
	if msg.String() == "esc" {
		m.cancelPaneGesture()
		return nil, true
	}
	if !g.keyboard {
		return nil, false
	}
	switch msg.String() {
	case "enter":
		return m.commitPaneDivider(g.position), true
	case "left", "up":
		m.previewPaneDivider(g.position - 1)
	case "right", "down":
		m.previewPaneDivider(g.position + 1)
	}
	return nil, true
}
