package chat

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
)

// Effects splits one page update into the work that keeps running while the
// page is hidden and the commands that drive the visible UI. Commands keep
// their native Bubble Tea composition; they are never wrapped.
type Effects struct {
	// Local is page-owned work (submissions and the session turns they start).
	// It runs even when the page is hidden, and its results route back to the page.
	Local tea.Cmd
	// Visible drives chrome, notifications and dialogs. Hidden pages drop it.
	Visible tea.Cmd
	// Global is application work owned by this page lifetime, such as exit after
	// the first response. It runs whether the page is visible or hidden.
	Global tea.Cmd
}

// Cmd returns the commands to dispatch for a visible or hidden page.
func (e Effects) Cmd(visible bool) tea.Cmd {
	if visible {
		return tea.Batch(e.Local, e.Visible, e.Global)
	}
	return tea.Batch(e.Local, e.Global)
}

// tabLocal classifies page work produced inside an update as Local. Outside an
// update it returns cmd unchanged so the caller dispatches it directly.
func (p *chatPage) tabLocal(cmd tea.Cmd) tea.Cmd {
	if p.effects == nil {
		return cmd
	}
	p.effects.Local = tea.Batch(p.effects.Local, cmd)
	return nil
}

// global classifies application work produced inside an update as Global.
// Outside an update it returns cmd unchanged.
//
//nolint:unused // Used by the exit-after-first-response effect once the runtime_events.go change lands.
func (p *chatPage) global(cmd tea.Cmd) tea.Cmd {
	if p.effects == nil {
		return cmd
	}
	p.effects.Global = tea.Batch(p.effects.Global, cmd)
	return nil
}

// GlobalMsg is an application effect owned by one page lifetime. The host applies
// Inner only while Origin is still the page in TabID and Application is still its app.
type GlobalMsg struct {
	TabID       string
	Origin      Page
	Application *app.App
	Inner       tea.Msg
}

// UpdateEffects is Update with the Local/Visible split exposed to routed
// delivery for hidden tabs. Update keeps its signature and dispatches both parts.
func (p *chatPage) UpdateEffects(msg tea.Msg) (layout.Model, Effects) {
	var effects Effects
	previous := p.effects
	p.effects = &effects
	defer func() { p.effects = previous }()

	model, visible := p.updateVisible(msg)
	effects.Visible = visible
	return model, effects
}
