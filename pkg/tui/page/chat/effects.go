package chat

import (
	tea "charm.land/bubbletea/v2"

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
}

// Cmd returns the commands to dispatch for a visible or hidden page.
func (e Effects) Cmd(visible bool) tea.Cmd {
	if visible {
		return tea.Batch(e.Local, e.Visible)
	}
	return e.Local
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
