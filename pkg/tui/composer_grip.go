package tui

import (
	"github.com/docker/docker-agent/pkg/tui/components/grip"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// composerGripHandleAt distinguishes direct hover on the compact line segment
// from the larger separator row that remains available for pointer resizing.
func (m *appModel) composerGripHandleAt(x, y int) bool {
	if !m.composerResizeHit(x, y) {
		return false
	}
	innerWidth := m.composerColumn().W - appPaddingHorizontal
	return grip.HorizontalHandleAt(innerWidth, x-m.composerColumn().X-styles.AppPadding)
}

func (m *appModel) setComposerGripHover(edge, handle bool) {
	before, highlight := m.composerGrip.Visibility(), m.composerGrip.HandleHighlight()
	m.composerGrip.SetHovered(edge)
	m.composerGrip.SetHandleHovered(handle)
	if m.composerGrip.Visibility() != before || m.composerGrip.HandleHighlight() != highlight {
		m.viewCacheValid = false
	}
}

func (m *appModel) setComposerGripDragging(dragging bool) {
	if m.composerGrip.Dragging() == dragging {
		return
	}
	m.composerGrip.SetDragging(dragging)
	m.viewCacheValid = false
}

func (m *appModel) resetComposerGrip() {
	if m.composerGrip.Visibility() == 0 && !m.composerGrip.Hovered() && !m.composerGrip.HandleHovered() && !m.composerGrip.Dragging() {
		return
	}
	m.composerGrip.Reset()
	m.viewCacheValid = false
}
