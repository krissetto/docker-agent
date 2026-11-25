package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/widgets/cursor"
)

func (m *appModel) cursorOccluded() bool {
	return m.err != nil || m.blurred || m.paneHidden || m.opening != nil || m.paneGesture != nil || m.paneHydration != nil || m.paneSource != nil
}

func (m *appModel) activeInputCursor() (cursor.Input, func()) {
	if m.cursorOccluded() {
		return nil, nil
	}
	if m.dialogMgr != nil && m.dialogMgr.Open() {
		return cursor.Of(m.dialogMgr.TopDialog()), m.dialogMgr.InvalidateCursorView
	}
	if m.chatPage != nil && (m.chatPage.IsInlineEditing() || m.chatPage.IsTitleEditing()) {
		if owner, ok := m.chatPage.(interface{ InvalidateCursorView() }); ok {
			return cursor.Of(m.chatPage), owner.InvalidateCursorView
		}
	}
	if review := m.reviewModel(); review != nil && review.Focused() {
		return cursor.Of(review), nil
	}
	if m.focusedPanel == PanelEditor {
		return cursor.Of(m.editor), nil
	}
	return nil, nil
}

// syncInputCursor changes only presentation, leaving covered input drafts and
// logical focus in place. It runs after handlers, including dialog fade ticks.
func (m *appModel) syncInputCursor() tea.Cmd {
	current, repaint := m.activeInputCursor()
	if current != nil && !current.Focused() {
		current, repaint = nil, nil
	}
	if m.exposedInput == current {
		return nil
	}
	if m.exposedInput != nil {
		_, changed := m.exposedInput.SetCursorExposed(false)
		if changed && m.repaintInput != nil {
			m.repaintInput()
		}
	}
	m.exposedInput, m.repaintInput = current, repaint
	m.viewCacheValid = false
	if current == nil {
		return nil
	}
	cmd, changed := current.SetCursorExposed(true)
	if changed && repaint != nil {
		repaint()
	}
	return cmd
}

// Cursor ticks bypass normal input and animation routing. Only the exposed
// widget may consume its current token; stale ticks must not repaint anything.
func (m *appModel) advanceInputCursor(tick cursor.BlinkMsg) tea.Cmd {
	if m.exposedInput == nil {
		return nil
	}
	cmd, changed := m.exposedInput.ApplyCursorBlink(tick)
	if changed {
		if m.repaintInput != nil {
			m.repaintInput()
		}
		m.viewCacheValid = false
	}
	return cmd
}

func (m *appModel) nativeInputCursor() *tea.Cursor {
	if m.cursorOccluded() || (m.dialogMgr != nil && m.dialogMgr.Open()) {
		return nil
	}
	if review := m.reviewModel(); review != nil && review.CommentInputActive() {
		return review.Cursor()
	}
	return nil
}
