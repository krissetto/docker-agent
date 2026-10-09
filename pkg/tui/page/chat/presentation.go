package chat

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// PresentationRect is a terminal-cell rectangle. Empty rectangles own no input.
type PresentationRect struct {
	X, Y, Width, Height int
}

func (r PresentationRect) contains(x, y int) bool {
	return x >= r.X && y >= r.Y && x < r.X+r.Width && y < r.Y+r.Height
}

// SplitShellGeometry describes the shared shell in content-local coordinates
// when measured, and screen coordinates when supplied to SetSplitPresentation.
// Sidebar excludes SidebarHandle; TranscriptArea excludes application padding.
type SplitShellGeometry struct {
	TranscriptArea PresentationRect
	Sidebar        PresentationRect
	SidebarHandle  PresentationRect
}

// SplitPresentationGeometry projects this page's existing transcript into one
// screen rectangle. Only the focused page presents the shared sidebar.
type SplitPresentationGeometry struct {
	Transcript  PresentationRect
	Shell       SplitShellGeometry
	ShowSidebar bool
}

// SplitPresentation is optional: single-page and lean shells need only Page.
type SplitPresentation interface {
	MeasureSplitShell(width, height int) SplitShellGeometry
	SetSplitPresentation(geometry *SplitPresentationGeometry) tea.Cmd
	TranscriptView() string
	SidebarView() string
	SidebarHandleView() string
	SidebarVisualGeneration() uint64
}

// PresentationVisibility distinguishes a visible, unfocused transcript from a
// hidden page. Visibility never changes session ownership or scroll position.
type PresentationVisibility interface {
	SetPresentationVisible(visible bool) tea.Cmd
}

// PresentationSelection lets the shell clear a previous pane's selection without
// changing its focus, reader position, inline draft, or execution state.
type PresentationSelection interface {
	ClearPresentationSelection()
}

// IsMessagesScrollbarDragging exposes only the existing transcript capture;
// the root retains its page owner when the pointer crosses section boundaries.
func (p *chatPage) IsMessagesScrollbarDragging() bool {
	return p.messages.IsScrollbarDragging()
}

// CancelMessagesScrollbarDrag is a capture fence, not a mouse click/release
// routed through other components. It never clears text selection.
func (p *chatPage) CancelMessagesScrollbarDrag() {
	if owner, ok := p.messages.(interface{ CancelScrollbarDrag() }); ok {
		owner.CancelScrollbarDrag()
	}
}

func (p *chatPage) ClearPresentationSelection() {
	if owner, ok := p.messages.(interface{ ClearPresentationSelection() }); ok {
		owner.ClearPresentationSelection()
	}
}

var (
	_ PresentationSelection  = (*chatPage)(nil)
	_ SplitPresentation      = (*chatPage)(nil)
	_ PresentationVisibility = (*chatPage)(nil)
)

// MeasureSplitShell uses the same width clamps and collapsed-band rules as the
// single-page layout without resizing either child or changing presentation.
func (p *chatPage) MeasureSplitShell(width, height int) SplitShellGeometry {
	sl := p.computeSidebarLayoutForSize(width, height)
	transcriptY := 0
	if sl.mode != sidebarVertical && !sl.bandAtBottom {
		transcriptY = sl.sidebarHeight
	}
	g := SplitShellGeometry{
		TranscriptArea: PresentationRect{styles.AppPadding + sl.chatStartX, transcriptY, sl.chatWidth, sl.chatHeight},
	}
	if !p.sidebarHidden() {
		switch sl.mode {
		case sidebarVertical:
			g.Sidebar = PresentationRect{styles.AppPadding + sl.sidebarStartX, 0, sl.sidebarWidth - toggleColumnWidth, sl.sidebarHeight}
			g.SidebarHandle = PresentationRect{styles.AppPadding + sl.handleX, 0, toggleColumnWidth, sl.chatHeight}
		default:
			g.Sidebar = PresentationRect{styles.AppPadding, sl.bandY(), sl.sidebarWidth, sl.sidebarHeight}
			if sl.showToggle() {
				g.SidebarHandle = PresentationRect{styles.AppPadding + sl.innerWidth - toggleColumnWidth, sl.bandY(), toggleColumnWidth, 1}
			}
		}
	}
	// The single-page layout reserves at least one transcript row. A split
	// shell must additionally remain within a possibly empty terminal area.
	g.TranscriptArea = clipPresentationRect(g.TranscriptArea, width, height)
	g.Sidebar = clipPresentationRect(g.Sidebar, width, height)
	g.SidebarHandle = clipPresentationRect(g.SidebarHandle, width, height)
	return g
}

func clipPresentationRect(r PresentationRect, width, height int) PresentationRect {
	r.X = min(max(0, r.X), max(0, width))
	r.Y = min(max(0, r.Y), max(0, height))
	r.Width = min(max(0, r.Width), max(0, width-r.X))
	r.Height = min(max(0, r.Height), max(0, height-r.Y))
	return r
}

// SetSplitPresentation copies shell-owned geometry; subsequent caller mutation
// cannot move hit targets. Passing nil restores the existing single layout.
func (p *chatPage) SetSplitPresentation(g *SplitPresentationGeometry) tea.Cmd {
	if g == nil && p.splitPresentation == nil {
		return nil
	}
	if g != nil && p.splitPresentation != nil && *g == *p.splitPresentation {
		return nil
	}
	wasSidebarInteractive := p.sidebarInteractive()
	if g == nil {
		p.splitPresentation = nil
	} else {
		geometry := *g
		geometry.Transcript.Width = max(0, geometry.Transcript.Width)
		geometry.Transcript.Height = max(0, geometry.Transcript.Height)
		p.splitPresentation = &geometry
	}
	p.shellVisualGeneration++
	if wasSidebarInteractive && !p.sidebarInteractive() {
		p.releaseSidebarInput()
	}
	return tea.Batch(p.SetSize(p.width, p.height), SetSidebarPresentationActive(p, !p.presentationHidden))
}

func (p *chatPage) applySplitPresentation() tea.Cmd {
	g := p.splitPresentation
	sl := p.computeSidebarLayout()
	if sl != p.appliedLayout {
		p.shellVisualGeneration++
	}
	p.appliedLayout = sl
	cmds := []tea.Cmd{
		p.messages.SetPosition(g.Transcript.X, g.Transcript.Y),
		p.messages.SetSize(g.Transcript.Width, g.Transcript.Height),
	}
	if p.sidebarInteractive() {
		mode := sidebar.ModeCollapsed
		if sl.mode == sidebarVertical {
			mode = sidebar.ModeVertical
		}
		p.sidebar.SetMode(mode)
		p.sidebar.SetMirroredPadding(sl.mode == sidebarVertical && sl.sidebarOnLeft)
		r := g.Shell.Sidebar
		cmds = append(cmds, p.sidebar.SetPosition(r.X, r.Y), p.sidebar.SetSize(max(0, r.Width), max(0, r.Height)), p.reconcileSidebarLayout())
	}
	return tea.Batch(cmds...)
}

// TranscriptView contains no sidebar, handle, or application padding. Clipping
// uses terminal cells rather than byte offsets, preserving ANSI and wide glyphs.
func (p *chatPage) TranscriptView() string {
	sl := p.computeSidebarLayout()
	if g := p.splitPresentation; g != nil {
		sl.chatWidth, sl.chatHeight = g.Transcript.Width, g.Transcript.Height
	}
	// Always obtain the current frame: streaming, selection and media can
	// change raw output independently of the shell's visual generation.
	raw := p.messagesView(sl)
	return p.transcriptPresentation.render(raw, sl.chatWidth, sl.chatHeight)
}

// presentationCache holds only the last viewport, never a transcript history.
// Byte equality deliberately replaces generation-based invalidation here.
type presentationCache struct {
	raw, rendered   string
	width, height   int
	themeGeneration uint64
	valid           bool
}

func (c *presentationCache) render(raw string, width, height int) string {
	generation := styles.ThemeGeneration()
	if c.valid && c.raw == raw && c.width == width && c.height == height && c.themeGeneration == generation {
		return c.rendered
	}
	rendered := presentationView(raw, width, height)
	*c = presentationCache{raw: raw, rendered: rendered, width: width, height: height, themeGeneration: generation, valid: true}
	return rendered
}

// SidebarView is content-only, matching Shell.Sidebar (not its handle).
// The root chooses which sidebar to compose; explicit reads remain available
// while hidden or unfocused without acquiring presentation subscriptions.
func (p *chatPage) SidebarView() string {
	if p.sidebarHidden() {
		return ""
	}
	if g := p.splitPresentation; g != nil {
		return presentationView(p.sidebar.View(), g.Shell.Sidebar.Width, g.Shell.Sidebar.Height)
	}
	return p.sidebar.View()
}

func (p *chatPage) SidebarHandleView() string {
	if !p.sidebarInteractive() {
		return ""
	}
	g := p.splitPresentation
	if g == nil {
		return ""
	}
	r := g.Shell.SidebarHandle
	return presentationView(p.renderSidebarHandle(r.Height), r.Width, r.Height)
}

func presentationView(view string, width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	lines := strings.Split(view, "\n")
	lines = lines[:min(height, len(lines))]
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], width, "")
	}
	return lipgloss.NewStyle().Width(width).Height(height).Render(strings.Join(lines, "\n"))
}

func (p *chatPage) sidebarInteractive() bool {
	return !p.presentationHidden && !p.sidebarHidden() && (p.splitPresentation == nil || p.splitPresentation.ShowSidebar)
}

func (p *chatPage) releaseSidebarInput() {
	p.lastSidebarClick = sidebarClick{}
	p.isDraggingSidebar = false
	// End capture at an outside coordinate without committing title/queue edits.
	p.sidebar.Update(tea.MouseReleaseMsg{X: -1, Y: -1, Button: tea.MouseLeft})
}

func (p *chatPage) SetPresentationVisible(visible bool) tea.Cmd {
	if p.presentationHidden == !visible {
		return nil
	}
	p.presentationHidden = !visible
	p.shellVisualGeneration++
	if !visible {
		p.releaseSidebarInput()
		p.stopHiddenPresentation()
		return SetSidebarPresentationActive(p, false)
	}
	var resumeCmd tea.Cmd
	if owner, ok := p.messages.(interface{ ResumeAnimations() tea.Cmd }); ok {
		resumeCmd = owner.ResumeAnimations()
	}
	var layoutCmd tea.Cmd
	if p.splitPresentation != nil {
		layoutCmd = p.applySplitPresentation()
	}
	return tea.Batch(resumeCmd, layoutCmd, SetSidebarPresentationActive(p, !p.presentationHidden))
}

func (p *chatPage) stopHiddenPresentation() {
	if p.presentationHidden {
		p.messages.StopAnimations()
	}
}

func (p *chatPage) routeSplitMouseEvent(msg tea.Msg) tea.Cmd {
	var x, y int
	switch m := msg.(type) {
	case tea.MouseClickMsg:
		x, y = m.X, m.Y
	case tea.MouseMotionMsg:
		x, y = m.X, m.Y
	case tea.MouseReleaseMsg:
		x, y = m.X, m.Y
	}
	g := p.splitPresentation
	if p.sidebarInteractive() && g.Shell.Sidebar.contains(x, y) {
		p.messages.ClearReferenceHover()
		model, cmd := p.sidebar.Update(msg)
		p.sidebar = model.(sidebar.Model)
		return cmd
	}
	var hoverCmd tea.Cmd
	if _, motion := msg.(tea.MouseMotionMsg); motion {
		hoverCmd = p.sidebar.ClearSubagentHover()
	}
	if !p.PointerTargetsMessages(x, y) {
		p.messages.ClearReferenceHover()
		return hoverCmd
	}
	model, cmd := p.messages.Update(msg)
	p.messages = model.(messages.Model)
	return tea.Batch(hoverCmd, cmd)
}

// SidebarCacheStats reports work counters without rendering or acquiring leases.
func (p *chatPage) SidebarCacheStats() (invalidations, renders uint64) {
	if owner, ok := p.sidebar.(interface{ CacheStats() (uint64, uint64) }); ok {
		return owner.CacheStats()
	}
	return 0, 0
}

// SplitSidebarPresentation keeps shell geometry independent of each session's
// saved sidebar preferences. The root owns the lifetime of the copied override.
type SplitSidebarPresentation interface {
	SetSplitSidebarSettings(settings *SidebarSettings) tea.Cmd
	SplitSidebarSettings() (SidebarSettings, bool)
}

func (p *chatPage) SetSplitSidebarSettings(settings *SidebarSettings) tea.Cmd {
	if settings == nil {
		if p.splitSidebarSettings == nil {
			return nil
		}
		p.splitSidebarSettings = nil
	} else {
		if p.splitSidebarSettings != nil && *p.splitSidebarSettings == *settings {
			return nil
		}
		copied := *settings
		p.splitSidebarSettings = &copied
	}
	p.shellVisualGeneration++
	return p.SetSize(p.width, p.height)
}

func (p *chatPage) SplitSidebarSettings() (SidebarSettings, bool) {
	if p.splitSidebarSettings == nil {
		return SidebarSettings{}, false
	}
	return *p.splitSidebarSettings, true
}

func (p *chatPage) presentationSidebarSettings() SidebarSettings {
	if settings, ok := p.SplitSidebarSettings(); ok {
		return settings
	}
	return p.GetSidebarSettings()
}

func (p *chatPage) setPresentationSidebarSettings(settings SidebarSettings) {
	if p.splitSidebarSettings != nil {
		*p.splitSidebarSettings = settings
		return
	}
	p.SetSidebarSettings(settings)
}

func (p *chatPage) togglePresentationSidebar() {
	settings := p.presentationSidebarSettings()
	settings.Collapsed = !settings.Collapsed
	if !settings.Collapsed && settings.PreferredWidth < sidebar.MinWidth {
		settings.PreferredWidth = sidebar.DefaultWidth
	}
	p.setPresentationSidebarSettings(settings)
}

// CaptureSidebarPresentation copies only the currently painted, bounded sidebar.
func CaptureSidebarPresentation(page Page) sidebar.PresentationSnapshot {
	if p, ok := page.(*chatPage); ok && p.sidebarInteractive() {
		if owner, ok := p.sidebar.(interface {
			CapturePresentation() sidebar.PresentationSnapshot
		}); ok {
			return owner.CapturePresentation()
		}
	}
	return sidebar.PresentationSnapshot{}
}

// TransitionSidebarFrom is called after the destination shell has been sized.
func TransitionSidebarFrom(page Page, snapshot sidebar.PresentationSnapshot) tea.Cmd {
	if p, ok := page.(*chatPage); ok && p.sidebarInteractive() {
		if owner, ok := p.sidebar.(interface {
			TransitionFrom(snapshot sidebar.PresentationSnapshot) tea.Cmd
		}); ok {
			return owner.TransitionFrom(snapshot)
		}
	}
	return nil
}
