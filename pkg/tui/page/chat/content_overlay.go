package chat

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// ContentOverlay replaces the transcript area (never the sidebar) with
// another view, such as the Git diff inspector. The page sizes it with the
// transcript and routes pointer events inside its rectangle to it.
type ContentOverlay interface {
	View() string
	SetSize(width, height int)
	SetPosition(x, y int)
	HandleMouse(msg tea.MouseMsg) (bool, tea.Cmd)
}

// SetContentOverlay shows overlay in place of the transcript; nil restores it.
func (p *chatPage) SetContentOverlay(overlay ContentOverlay) {
	p.contentOverlay = overlay
	p.shellVisualGeneration++
	p.layoutOverlay()
}

func (p *chatPage) ContentOverlayActive() bool { return p.contentOverlay != nil }

// ContentOverlayBounds reports the overlay's transcript-only terminal rectangle.
func (p *chatPage) ContentOverlayBounds() (x, y, width, height int) {
	r := p.overlayRect()
	return r.X, r.Y, r.Width, r.Height
}

// layoutOverlay mirrors the transcript's own geometry in every layout.
func (p *chatPage) layoutOverlay() {
	if p.contentOverlay == nil {
		return
	}
	if g := p.splitPresentation; g != nil {
		p.contentOverlay.SetSize(g.Transcript.Width, g.Transcript.Height)
		p.contentOverlay.SetPosition(g.Transcript.X, g.Transcript.Y)
		return
	}
	sl := p.computeSidebarLayout()
	x, y := styles.AppPadding+sl.chatStartX, 0
	if sl.mode != sidebarVertical && !sl.bandAtBottom && !p.hideSidebar && !p.leanMode {
		y = sl.sidebarHeight
	}
	p.contentOverlay.SetSize(sl.chatWidth, sl.chatHeight)
	p.contentOverlay.SetPosition(x, y)
}

// overlayMouse gives the overlay pointer events inside its rectangle.
func (p *chatPage) overlayMouse(msg tea.Msg) (bool, tea.Cmd) {
	if p.contentOverlay == nil {
		return false, nil
	}
	switch m := msg.(type) {
	case tea.MouseMsg:
		mouse := m.Mouse()
		if !p.overlayContains(mouse.X, mouse.Y) {
			return false, nil
		}
		return p.contentOverlay.HandleMouse(m)
	case msgtypes.WheelCoalescedMsg:
		if m.Delta == 0 || !p.overlayContains(m.X, m.Y) {
			return false, nil
		}
		button := tea.MouseWheelDown
		if m.Delta < 0 {
			button = tea.MouseWheelUp
		}
		return p.contentOverlay.HandleMouse(tea.MouseWheelMsg{X: m.X, Y: m.Y, Button: button})
	}
	return false, nil
}

func (p *chatPage) overlayContains(x, y int) bool {
	return p.contentOverlay != nil && p.overlayRect().contains(x, y)
}

func (p *chatPage) overlayRect() PresentationRect {
	if g := p.splitPresentation; g != nil {
		return g.Transcript
	}
	sl := p.computeSidebarLayout()
	ox, oy := styles.AppPadding+sl.chatStartX, 0
	if sl.mode != sidebarVertical && !sl.bandAtBottom && !p.hideSidebar && !p.leanMode {
		oy = sl.sidebarHeight
	}
	return PresentationRect{X: ox, Y: oy, Width: sl.chatWidth, Height: sl.chatHeight}
}

func (p *chatPage) gitSidebar() (sidebar.GitSidebar, bool) {
	g, ok := p.sidebar.(sidebar.GitSidebar)
	return g, ok
}

// SetGitChanges updates the sidebar's Git section.
func (p *chatPage) GitWatchState() sidebar.GitWatchMsg {
	if g, ok := p.gitSidebar(); ok {
		return g.GitWatchState()
	}
	return sidebar.GitWatchMsg{}
}

func (p *chatPage) SetGitChanges(c sidebar.GitChanges) tea.Cmd {
	if g, ok := p.gitSidebar(); ok {
		return g.SetGitChanges(c)
	}
	return nil
}

func (p *chatPage) RevealGit() tea.Cmd {
	if g, ok := p.gitSidebar(); ok {
		return g.RevealGit()
	}
	return nil
}

func (p *chatPage) GitFocused() bool {
	g, ok := p.gitSidebar()
	return ok && g.GitFocused()
}

func (p *chatPage) SetGitFocused(focused bool) tea.Cmd {
	if g, ok := p.gitSidebar(); ok {
		return g.SetGitFocused(focused)
	}
	return nil
}

func (p *chatPage) GitKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	if g, ok := p.gitSidebar(); ok {
		return g.GitKey(msg)
	}
	return false, nil
}
