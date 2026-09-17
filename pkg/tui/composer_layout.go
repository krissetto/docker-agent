package tui

// composerLayout is the shared vertical geometry for rendering and pointer
// routing. Content is transcript-only; attachments precede the actual resize
// separator, not just the editor's decorative frame. Lean omits separator/tabs.
type composerLayout struct {
	bannerTop, separatorTop, tabsTop, editorTop int
	editorBottom, contextBottom                 int
}

func (m *appModel) composerLayout() composerLayout {
	g := composerLayout{bannerTop: m.contentHeight}
	g.separatorTop = g.bannerTop + m.editor.BannerHeight()
	g.tabsTop = g.separatorTop + m.separatorHeight
	g.editorTop = g.tabsTop + m.tabsHeight
	_, editorHeight := m.editor.GetSize()
	g.editorBottom = g.editorTop + editorHeight
	g.contextBottom = g.editorBottom + m.contextHeight
	return g
}

// editorTop returns the Y coordinate of the editor frame, below shell chrome.
func (m *appModel) editorTop() int { return m.composerLayout().editorTop }

// composerMaxTextHeight is the single automatic/manual ceiling. Decoration
// yields on tiny terminals; the textarea always retains at least one real row.
func (m *appModel) composerMaxTextHeight() int {
	chrome := m.separatorHeight + m.tabsHeight + m.contextHeight + m.messageBarHeight()
	available := max(1, m.height-chrome-m.editorFrame().GetVerticalFrameSize())
	return min(available, max(1, (m.height-6)/2-1))
}
