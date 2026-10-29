package tui

import (
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

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

// mainContentInset leaves the full TUI's transcript area clear at the top.
// Lean mode keeps its compact, bottom-aligned layout unchanged.
func (m *appModel) mainContentInset() int {
	if m.leanMode || m.contentHeight <= 0 {
		return 0
	}
	return 1
}

func (m *appModel) mainContentHeight() int {
	return max(0, m.contentHeight-m.mainContentInset())
}

// Current chrome avoids depending on the previous resize's contentHeight.
func (m *appModel) dockContentHeight() int {
	_, editorHeight := m.editor.GetSize()
	chrome := m.separatorHeight + m.tabsHeight + m.contextHeight + m.messageBarHeight() + m.editor.BannerHeight() + editorHeight
	return max(1, m.height-chrome-1)
}

// composerColumn is the shared transcript/composer column only when the
// opt-in setting is active for a visible vertical sidebar. Other layouts keep
// the historical full-width composer.
func (m *appModel) composerColumn() splitRect {
	if m.leanMode || !m.layoutSettings.AlignInputWithMessages {
		return splitRect{W: m.width}
	}
	page, ok := m.chatPage.(chat.SplitPresentation)
	if !ok {
		return splitRect{W: m.width}
	}
	pageWidth := m.width
	if m.reviewDocked() {
		pageWidth = m.reviewDockChatWidth()
	}
	shell := page.MeasureSplitShell(pageWidth, m.dockContentHeight())
	if shell.SidebarHandle.Height <= 1 {
		return splitRect{W: m.width}
	}
	r := shell.TranscriptArea
	return splitRect{X: r.X, W: r.Width}
}

// Shared bounds prevent rendering and pointer routing from drifting apart.
type composerDock struct {
	column splitRect
	rows   composerLayout
}

func (m *appModel) composerDock() composerDock {
	return composerDock{column: m.composerColumn(), rows: m.composerLayout()}
}

func (d composerDock) height() int { return d.rows.contextBottom - d.rows.bannerTop }

func (d composerDock) tabOrigin() int { return d.column.X + tabFrameOrigin() }

func (d composerDock) tabWidth() int { return tabFrameWidth(d.column.W) }

// Compose locally so the root positions the whole dock only once.
func (m *appModel) composerDockView(tabBarView, editorView string) string {
	d := m.composerDock()
	w, g := d.column.W, d.rows
	var rows []string
	appendRows := func(content string, height int) {
		if height > 0 {
			rows = append(rows, paneClipped(content, w, height))
		}
	}
	appendRows(m.editor.BannerView(max(1, w)), g.separatorTop-g.bannerTop)
	if g.separatorTop < g.tabsTop {
		appendRows(m.renderResizeHandle(w), g.tabsTop-g.separatorTop)
	}
	tabsHeight := g.editorTop - g.tabsTop
	if tabsHeight > 0 {
		framed := m.tabFrameCache.render(tabBarView, w, tabsHeight, func() string {
			clipped := paneClipped(tabBarView, tabFrameWidth(w), tabsHeight)
			return paneClipped(lipgloss.NewStyle().Padding(0, styles.EditorStyle.GetMarginRight(), 0, tabFrameOrigin()).Render(clipped), w, tabsHeight)
		})
		appendRows(framed, tabsHeight)
	}
	appendRows(editorView, g.editorBottom-g.editorTop)
	if m.contextBar != nil && g.contextBottom > g.editorBottom {
		raw := m.contextBar.View()
		contextHeight := g.contextBottom - g.editorBottom
		framed := m.contextFrameCache.render(raw, w, contextHeight, func() string {
			return paneClipped(lipgloss.NewStyle().Padding(0, styles.EditorHMargin).Render(raw), w, contextHeight)
		})
		appendRows(framed, contextHeight)
	}
	return strings.Join(rows, "\n")
}

func (m *appModel) footerTop() int { return max(0, m.height-m.messageBarHeight()) }

func (m *appModel) tabOrigin() int { return m.composerDock().tabOrigin() }

func (m *appModel) tabWidth() int { return m.composerDock().tabWidth() }
