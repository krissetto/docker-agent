package tui

import (
	"image/color"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/agentidentity"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/subagentview"
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

func (m *appModel) panePresentationEnabled() bool {
	_, supported := m.chatPage.(chat.SplitPresentation)
	return !m.leanMode && supported
}

// Pane bars belong only to an actually visible split, not its saved tree.
func (m *appModel) paneHeaderHeight() int {
	if len(m.paneGeometry.Panes) > 1 {
		return 1
	}
	return 0
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
	if m.width > 0 && m.contentHeight > 0 && shell.TranscriptArea.Width == 0 {
		// Degenerate terminals cannot afford app padding or a sidebar. Keep
		// the focused leaf visible in the cells that actually exist,
		// without changing the immutable layout or saved sidebar preferences.
		shell = chat.SplitShellGeometry{TranscriptArea: chat.PresentationRect{Width: m.width, Height: m.contentHeight}}
	}
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

func (m *appModel) capturePaneSidebarSettings() {
	if !m.panesEnabled() && m.paneSidebarSettings == nil {
		settings := m.chatPage.GetSidebarSettings()
		m.paneSidebarSettings = &settings
	}
}

// The tiled sidebar is layout-wide presentation state, not a tab preference.
func (m *appModel) syncPaneSidebarSettings() {
	if !m.panesEnabled() {
		return
	}
	if owner, ok := m.chatPage.(chat.SplitSidebarPresentation); ok {
		if settings, active := owner.SplitSidebarSettings(); active {
			m.paneSidebarSettings = &settings
		}
	}
}

func (m *appModel) applyPaneSidebarSettings() tea.Cmd {
	if m.panesEnabled() {
		if m.paneSidebarSettings == nil {
			settings := m.chatPage.GetSidebarSettings()
			m.paneSidebarSettings = &settings
		}
	} else {
		m.paneSidebarSettings = nil
	}
	var cmds []tea.Cmd
	for _, page := range m.chatPages {
		if owner, ok := page.(chat.SplitSidebarPresentation); ok {
			cmds = append(cmds, owner.SetSplitSidebarSettings(m.paneSidebarSettings))
		}
	}
	return tea.Batch(cmds...)
}

func (m *appModel) resizePanes() tea.Cmd {
	cmds := []tea.Cmd{m.applyPaneSidebarSettings()}
	if !m.panePresentationEnabled() {
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
	layout := m.paneLayout()
	shell, bounds, ok := m.measurePanes()
	if !ok || !m.supportsPanes(layout) {
		return tea.Batch(cmds...)
	}
	geometry := layout.Compute(bounds, m.paneFocus(), paneMinWidth, paneMinHeight)
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
		header := m.paneHeaderHeight()
		transcript := splitRect{X: r.X, Y: r.Y, W: r.W, H: max(0, r.H-header)}
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

// paneSplitCandidate is pure presentation planning. A self-edge drop leaves
// the next hidden tab in the vacated leaf; already-visible leaves are never
// duplicated or displaced. No page initialization or store access occurs here.
func (m *appModel) paneSplitCandidate(current splitLayout, source, target string, edge splitEdge) (splitLayout, string, bool) {
	load := source
	if source == target {
		order := m.paneOrder()
		start := -1
		for i, id := range order {
			if id == source {
				start = i
				break
			}
		}
		if start < 0 || !current.Contains(source) {
			return current, "", false
		}
		replacement := ""
		for offset := 1; offset < len(order); offset++ {
			id := order[(start+offset)%len(order)]
			if !current.Contains(id) {
				replacement = id
				break
			}
		}
		if replacement == "" {
			return current, "", false
		}
		// Replacing one leaf before inserting preserves every ancestor ratio.
		var ok bool
		current, ok = current.Replace(source, replacement)
		if !ok {
			return current, "", false
		}
		target, load = replacement, replacement
	}
	next, ok := current.Insert(source, target, edge)
	return next, load, ok
}

func (m *appModel) splitPane(source, target string, edge splitEdge) tea.Cmd {
	layout, load, ok := m.paneSplitCandidate(m.paneLayout(), source, target, edge)
	if !ok {
		return notification.ErrorCmd("Choose a different session and a pane edge")
	}
	if !m.supportsPaneCandidate(layout, load) {
		return notification.ErrorCmd("Panes unavailable: this session does not support split presentation")
	}
	_, bounds, ok := m.measurePanes()
	if !ok || layout.Compute(bounds, source, paneMinWidth, paneMinHeight).Compact {
		return notification.ErrorCmd("Not enough room: panes require at least 24 columns × 6 rows each")
	}
	if m.chatPages[load] == nil || m.pendingRestores[load] != "" {
		return m.beginPaneHydration(load, source, target, edge, layout, bounds)
	}
	m.cancelPaneGesture()
	m.capturePaneSidebarSettings()
	m.commitPaneWorkspace(layout)
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
	m.commitPaneWorkspace(layout)
	m.viewCacheValid = false
	if id == m.paneFocus() {
		_, cmd := m.handleSwitchTab(layout.Sessions()[0])
		return cmd
	}
	return m.resizeAll()
}

func (m *appModel) singlePane() tea.Cmd {
	m.cancelPaneGesture()
	m.commitPaneWorkspace(newSplitLayout(m.paneFocus()))
	m.viewCacheValid = false
	return m.resizeAll()
}

// Tabs select their assigned workspace rather than replacing a focused leaf.
func (m *appModel) selectPaneTab(old, next string) {
	m.switchPaneWorkspace(old, next)
}

func (m *appModel) paneAt(x, y int) string {
	for _, id := range m.paneLayout().Sessions() {
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
		if _, motion := msg.(tea.MouseMotionMsg); motion {
			return m.forwardChat(msg)
		}
		if inPane(paneRect(m.paneShell.Sidebar), x, y) || inPane(paneRect(m.paneShell.SidebarHandle), x, y) || m.chatPage.IsSelecting() {
			return m.forwardChat(msg)
		}
		return m, nil
	}
	var cmds []tea.Cmd
	if _, motion := msg.(tea.MouseMotionMsg); motion {
		cmds = append(cmds, chat.ClearSidebarHover(m.chatPage))
	}
	if focus && id != m.paneFocus() {
		// Dispatch against the pane that was actually painted. Switching focus
		// can replace composer chrome and resize this transcript before the
		// press reaches it, turning a thumb press into an unrelated track hit.
		if m.paneHeaderHeight() == 0 || y != m.paneGeometry.Panes[id].Y+m.paneGeometry.Panes[id].H-1 {
			updated, cmd := m.chatPages[id].Update(msg)
			page := updated.(chat.Page)
			m.chatPages[id] = page
			cmds = append(cmds, m.routePaneCmd(id, cmd))
			m.capturePaneMessagesScrollbar(id, page)
		}
		_, cmd := m.handleSwitchTab(id)
		return m, tea.Batch(append(cmds, cmd)...)
	}
	if m.paneHeaderHeight() > 0 && y == m.paneGeometry.Panes[id].Y+m.paneGeometry.Panes[id].H-1 {
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
	name, nodeID := m.tabAgentIdentity(messages.TabInfo{SessionID: id})
	if name == "" {
		name = "agent"
	}
	ref := lifecycle.InputReference{Kind: lifecycle.InputReferenceNode, ID: nodeID, Name: name, Agent: name, DisplayID: paneDisplayNodeID(nodeID)}
	title := "New Session"
	if state := m.sessionStates[id]; state != nil && state.SessionTitle() != "" {
		title = state.SessionTitle()
	}
	focused := id == m.paneFocus()
	prefix := " "
	background := styles.CardBg
	if focused {
		background = styles.EditorBg
	}
	activity := m.paneActivity(id)
	count := ""
	if children := m.paneSubagentCount(id, subagent.NodeID(nodeID)); children > 0 {
		count = " (" + strconv.Itoa(children) + ")"
	}
	budget := max(0, width-ansi.StringWidth(prefix)-ansi.StringWidth(activity)-ansi.StringWidth(count)-5)
	identity := agentidentity.Label(ref, budget) + styles.MutedStyle.Render(count)
	label := prefix + identity + " " + activity + styles.MutedStyle.Render(" · "+title)
	if width <= 3 {
		label = "●"
	}
	surface := lipgloss.NewStyle().Foreground(styles.TextPrimary).Background(background)
	heading := styles.RenderComposite(surface, paneClipped(label, width, 1))
	if m.dimInactivePanes && m.panesEnabled() && !focused {
		context := styles.NewFadeContext()
		heading = styles.FadeLineCtx(heading, inactivePaneContrast, &context)
	}
	return heading
}

// Count direct subagents in every state, not open views or recursive descendants.
func (m *appModel) paneSubagentCount(id string, nodeID subagent.NodeID) int {
	if m.supervisor == nil {
		return 0
	}
	runner := m.supervisor.GetRunner(id)
	if runner == nil || runner.App == nil {
		return 0
	}
	if provider, ok := runner.App.Runtime().(interface{ SubagentTree() *subagent.Tree }); ok && provider.SubagentTree() != nil {
		if node, found := subagentview.Find(provider.SubagentTree().Snapshot().Nodes, nodeID); found {
			return len(node.Children)
		}
	}
	// An initial live tree may not yet contain a restored session's root.
	if sess := runner.App.Session(); sess != nil {
		if snapshot := sess.GetSubagentTree(); snapshot != nil {
			if node, found := subagentview.Find(snapshot.Nodes, nodeID); found {
				return len(node.Children)
			}
		}
	}
	return 0
}

func (m *appModel) hasRunningPane() bool {
	for _, tab := range m.tabInfos {
		if tab.Activity == messages.TabActivityRunning && m.paneVisible(tab.SessionID) {
			return true
		}
	}
	return false
}

// paneActivity reads canonical per-session state; headers never acquire leases.
func (m *appModel) paneActivity(id string) string {
	if status, exists := m.paneRenderStatus(id); exists && status.Dormant {
		return styles.WarningStyle.Render("Restored · paused · /resume")
	}
	if state := m.sessionStates[id]; state != nil {
		switch state.PauseState() {
		case service.PausePaused:
			return styles.WarningStyle.Render("⏸ paused")
		case service.PausePausing:
			return styles.WarningStyle.Render("⏸ pausing")
		}
	}
	for _, tab := range m.tabInfos {
		if tab.SessionID != id {
			continue
		}
		if tab.NeedsAttention {
			return styles.WarningStyle.Render("! attention")
		}
		switch tab.Activity {
		case messages.TabActivityRunning:
			return styles.SpinnerDotsHighlightStyle.Render(animation.Card.FrameAt(m.ar.Now()) + " running")
		case messages.TabActivityPending:
			return styles.MutedStyle.Render("· pending")
		case messages.TabActivityDescendantRunning:
			return styles.MutedStyle.Render("◇")
		}
	}
	if runner := m.supervisor.GetRunner(id); runner != nil && runner.App != nil && runner.App.Session() != nil {
		switch m.paneOutcomes[runner.App.Session().ID] {
		case runtime.TurnCompleted:
			return styles.SuccessStyle.Render("✓ done")
		case runtime.TurnCanceled:
			return styles.MutedStyle.Render("· canceled")
		case runtime.TurnFailed:
			return styles.ErrorStyle.Render("! failed")
		}
	}
	return styles.MutedStyle.Render("· idle")
}

// Keep 62% of each semantic color's distance from the active theme background.
// This visibly subordinates inactive content without replacing its hues or
// relying on terminal faint support, on both dark and light themes.
const inactivePaneContrast = 0.62

type paneDimEntry struct {
	content, rendered string
	theme             uint64
}

func (m *appModel) paneTranscript(id, content string) string {
	if !m.dimInactivePanes || !m.panesEnabled() || id == m.paneFocus() {
		return content
	}
	generation := styles.ThemeGeneration()
	if entry, ok := m.paneDimCache[id]; ok && entry.theme == generation && entry.content == content {
		return entry.rendered
	}
	if m.paneDimCache == nil {
		m.paneDimCache = make(map[string]paneDimEntry)
	}
	// Cache only the current bounded viewport, never transcript histories.
	fc := styles.NewFadeContext()
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		lines[i] = styles.FadeLineCtx(line, inactivePaneContrast, &fc)
	}
	rendered := strings.Join(lines, "\n")
	m.paneDimCache[id] = paneDimEntry{content: content, rendered: rendered, theme: generation}
	for cached := range m.paneDimCache {
		if !m.paneVisible(cached) {
			delete(m.paneDimCache, cached)
		}
	}
	return rendered
}

func (m *appModel) composePanes() string {
	if m.paneGestureBaseValid() {
		return m.paneGesture.base
	}
	// Pane rectangles are nonoverlapping. Assemble cell-bounded row spans
	// directly instead of allocating a full terminal cell buffer per layer.
	rows := make([][]paneRowSpan, max(0, m.contentHeight))
	add := func(content string, r splitRect) {
		if r.W <= 0 || r.H <= 0 {
			return
		}
		lines := strings.Split(content, "\n")
		for offset := 0; offset < r.H && offset < len(lines); offset++ {
			y := r.Y + offset
			if y < 0 || y >= len(rows) || r.X < 0 || r.X >= m.width {
				continue
			}
			width := min(r.W, m.width-r.X)
			line := ansi.Truncate(lines[offset], width, "")
			// Later shared sidebar handles overlay the rightmost band cell.
			// Cut the existing span at cell boundaries before adding the handle.
			for i := range rows[y] {
				previous := &rows[y][i]
				if previous.x < r.X && previous.x+previous.width > r.X {
					previous.width = r.X - previous.x
					previous.content = ansi.Truncate(previous.content, previous.width, "")
				}
			}
			rows[y] = append(rows[y], paneRowSpan{x: r.X, width: width, content: line})
		}
	}
	for _, id := range m.paneLayout().Sessions() {
		r, visible := m.paneGeometry.Panes[id]
		if !visible {
			continue
		}
		p, ok := m.chatPages[id].(chat.SplitPresentation)
		if !ok {
			continue
		}
		header := m.paneHeaderHeight()
		if header > 0 {
			add(m.paneTitle(id, r.W), splitRect{X: r.X, Y: r.Y + r.H - header, W: r.W, H: header})
		}
		add(m.paneTranscript(id, p.TranscriptView()), splitRect{X: r.X, Y: r.Y, W: r.W, H: r.H - header})
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
	view := renderPaneRows(rows, m.width)
	m.cachePaneGestureBase(view)
	return view
}

type paneRowSpan struct {
	x, width int
	content  string
}

func renderPaneRows(rows [][]paneRowSpan, width int) string {
	var out strings.Builder
	out.Grow(max(0, width+1) * len(rows))
	for y, spans := range rows {
		if y > 0 {
			out.WriteByte('\n')
		}
		// Each row has only a handful of spans. Insertion sort avoids a
		// reflection/slice allocation on every transcript row.
		for i := 1; i < len(spans); i++ {
			for j := i; j > 0 && spans[j].x < spans[j-1].x; j-- {
				spans[j], spans[j-1] = spans[j-1], spans[j]
			}
		}
		column := 0
		for _, span := range spans {
			if span.x < column {
				// Geometry must remain unique; overlapping same-origin spans
				// are rejected rather than emitting excess terminal cells.
				continue
			}
			out.WriteString(strings.Repeat(" ", span.x-column))
			out.WriteString(span.content)
			out.WriteString(strings.Repeat(" ", max(0, span.width-ansi.StringWidth(span.content))))
			if strings.Contains(span.content, "\x1b") {
				out.WriteString("\x1b[m")
				if strings.Contains(span.content, "\x1b]8;") {
					out.WriteString(ansi.ResetHyperlink())
				}
			}
			column = span.x + span.width
		}
		out.WriteString(strings.Repeat(" ", max(0, width-column)))
	}
	// Match the terminal compositor's serialization: trailing default cells
	// are implicit, but styled backgrounds and hyperlink boundaries remain.
	lines := strings.Split(out.String(), "\n")
	for i := range lines {
		lines[i] = trimPaneDefaultPadding(lines[i])
	}
	return strings.Join(lines, "\n")
}

// trimPaneDefaultPadding drops only trailing default-style space cells. Escape
// resets and OSC link boundaries are retained; colored or decorated padding is
// meaningful terminal content and must remain, even when its glyph is a space.
func trimPaneDefaultPadding(line string) string {
	if !strings.Contains(line, "\x1b") {
		return strings.TrimRight(line, " ")
	}
	parser := ansi.GetParser()
	defer ansi.PutParser(parser)
	attributes := make(map[int]bool)
	linked := false
	var state byte
	keep, offset := 0, 0
	for offset < len(line) {
		seq, width, consumed, next := ansi.DecodeSequence(line[offset:], state, parser)
		if consumed == 0 {
			return line
		}
		state = next
		payload, hyperlink := strings.CutPrefix(seq, "\x1b]8;")
		switch {
		case ansi.HasCsiPrefix(seq) && parser.Command() == 'm':
			params := parser.Params()
			if len(params) == 0 {
				clear(attributes)
			}
			for i := 0; i < len(params); i++ {
				value := params[i].Param(0)
				switch {
				case value == 0:
					clear(attributes)
				case value == 38 || value == 48 || value == 58:
					var c color.Color
					if n := ansi.ReadStyleColor(params[i:], &c); n > 0 {
						attributes[value] = true
						i += n - 1
					} else {
						// Malformed extended colors are opaque: channel values
						// must not become reset/underline/blink attributes. Keep
						// uncertain padding until a later explicit SGR reset.
						attributes[value] = true
						if params[i].HasMore() {
							for i+1 < len(params) && params[i].HasMore() {
								i++
							}
						} else {
							i = len(params)
						}
					}
				case value == 39 || value == 49 || value == 59:
					delete(attributes, value-1)
				case value >= 30 && value <= 37 || value >= 90 && value <= 97:
					attributes[38] = true
				case value >= 40 && value <= 47 || value >= 100 && value <= 107:
					attributes[48] = true
				case value == 22:
					delete(attributes, 1)
					delete(attributes, 2)
				case value >= 23 && value <= 29:
					delete(attributes, value-20)
					if value == 25 {
						delete(attributes, 6)
					}
				case value == 10:
					delete(attributes, 10)
				case value >= 11 && value <= 19:
					attributes[10] = true
				default:
					attributes[value] = true
				}
			}
		case hyperlink:
			_, url, _ := strings.Cut(payload, ";")
			url = strings.TrimSuffix(strings.TrimSuffix(url, "\x1b\\"), "\a")
			linked = url != ""
		case width > 0 && (strings.Trim(seq, " ") != "" || len(attributes) > 0 || linked):
			keep = offset + consumed
		}
		offset += consumed
	}
	if keep == len(line) {
		return line
	}
	// The suffix contains only default spaces and zero-width escape controls.
	// Remove the spaces without discarding the controls' terminal semantics.
	var out strings.Builder
	out.Grow(len(line))
	out.WriteString(line[:keep])
	state = 0
	for rest := line[keep:]; rest != ""; {
		seq, width, consumed, next := ansi.DecodeSequence(rest, state, parser)
		if consumed == 0 {
			out.WriteString(rest)
			break
		}
		if width == 0 {
			out.WriteString(seq)
		}
		state, rest = next, rest[consumed:]
	}
	return out.String()
}

// Only real five-hex tree identities are human-short IDs. Synthetic root:UUID
// identifiers and arbitrary session IDs are never truncated into fake IDs.
func paneDisplayNodeID(id string) string {
	if len(id) != 5 {
		return ""
	}
	for _, ch := range id {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') && (ch < 'A' || ch > 'F') {
			return ""
		}
	}
	return id
}

// Header semantics belong to the canonical projection, not the transcript's
// visual generation. Observe the immutable status head without snapshots or IO
// so status-only changes cannot reuse stale complete views or drag canvases.
func (m *appModel) visiblePaneStatuses() map[string]runtime.SessionStatus {
	statuses := make(map[string]runtime.SessionStatus)
	if m.supervisor == nil {
		return statuses
	}
	for _, id := range m.paneLayout().Sessions() {
		if !m.paneVisible(id) {
			continue
		}
		if runner := m.supervisor.GetRunner(id); runner != nil && runner.App != nil {
			if projection := runner.App.Presentation(); projection != nil {
				statuses[id] = projection.Status
			}
		}
	}
	return statuses
}

func (m *appModel) paneStatusesMatch(cached map[string]runtime.SessionStatus) bool {
	if m.supervisor == nil {
		return len(cached) == 0
	}
	count := 0
	for _, id := range m.paneLayout().Sessions() {
		if !m.paneVisible(id) {
			continue
		}
		if runner := m.supervisor.GetRunner(id); runner != nil && runner.App != nil {
			if projection := runner.App.Presentation(); projection != nil {
				status, exists := cached[id]
				if !exists || status != projection.Status {
					return false
				}
				count++
			}
		}
	}
	return count == len(cached)
}

func (m *appModel) paneRenderStatus(id string) (runtime.SessionStatus, bool) {
	if m.composingPaneStatuses != nil {
		status, exists := m.composingPaneStatuses[id]
		return status, exists
	}
	if m.supervisor != nil {
		if runner := m.supervisor.GetRunner(id); runner != nil && runner.App != nil {
			if projection := runner.App.Presentation(); projection != nil {
				return projection.Status, true
			}
		}
	}
	return runtime.SessionStatus{}, false
}
