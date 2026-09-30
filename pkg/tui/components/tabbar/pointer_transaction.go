package tabbar

import tea "charm.land/bubbletea/v2"

// TabBodyAt uses the same clipped, animated terminal-cell geometry as View.
// Close buttons, overflow arrows and the plus button are deliberately excluded.
func (t *TabBar) TabBodyAt(x, y int) (string, bool) {
	if y != 0 || t.Height() == 0 {
		return "", false
	}
	t.installGeometry()
	for _, z := range t.zones {
		if !z.isClose && z.tabIdx >= 0 && z.tabIdx < len(t.tabs) && x >= z.startX && x < z.endX {
			return t.tabs[z.tabIdx].SessionID, true
		}
	}
	return "", false
}

// BeginPointerPreview does not activate a tab or schedule a hold timer. The
// root owns the threshold and transaction while this component owns reflow.
func (t *TabBar) BeginPointerPreview(x int) tea.Cmd {
	t.installGeometry()
	for _, z := range t.zones {
		if z.isClose || z.tabIdx < 0 || z.tabIdx >= len(t.tabs) || x < z.startX || x >= z.endX {
			continue
		}
		t.dragSeq++
		t.drag = dragState{pending: true, dragIdx: z.tabIdx, dropIdx: noTab, startX: x, cursorX: x, seq: t.dragSeq}
		return nil
	}
	return nil
}

func (t *TabBar) PreviewPointer(x int) tea.Cmd {
	var activate tea.Cmd
	if t.drag.pending {
		activate = t.Update(DragHoldMsg{seq: t.drag.seq})
	}
	return tea.Batch(activate, t.Update(tea.MouseMotionMsg{X: x, Y: 0, Button: tea.MouseLeft}))
}

func (t *TabBar) CommitPointer(x int) tea.Cmd {
	return t.Update(tea.MouseReleaseMsg{X: x, Y: 0, Button: tea.MouseLeft})
}

// UpdateHover preserves the background-dialog tab affordance without advancing
// a pending drag or creating an overlay behind the dialog.
func (t *TabBar) UpdateHover(x, y int) tea.Cmd {
	if t.Height() == 0 {
		return nil
	}
	t.installGeometry()
	cmd := t.setPlusHovered(t.plusAt(x, y))
	t.recordVisualState()
	return cmd
}

// CancelPointer rolls back preview offsets, never synthesizing a click/reorder.
func (t *TabBar) CancelPointer() {
	t.dragSeq++
	t.drag = dragState{dropIdx: noTab}
	t.dragAnim.Cancel()
	t.dragOffsetFrom, t.dragOffsetTo = nil, nil
	t.viewDirty, t.visualDirty = true, true
	t.visualGeneration++
	t.recordVisualState()
}

// DragTabView uses the same source rendering as the in-bar floating layer.
// Root-owned pane drags must not substitute a different label or width budget.
func (t *TabBar) DragTabView(sessionID string) string {
	for _, info := range t.tabs {
		if info.SessionID == sessionID {
			return renderTab(info, t.maxTitleLen, dragRoleSource, t.ar.Now()).View()
		}
	}
	return ""
}

// DragGrabOffset measures the pointer against the same clipped bounds used to
// activate an in-bar drag, including a partially visible source tab.
func (t *TabBar) DragGrabOffset(sessionID string, x int) int {
	t.installGeometry()
	for _, bound := range t.dragBounds {
		if bound.sessionID == sessionID {
			return max(0, x-bound.start)
		}
	}
	return 0
}

// ResumePointerPreview retains source identity and its original grab point when
// a root-owned drag reenters after the tab strip has scrolled or reflowed.
func (t *TabBar) ResumePointerPreview(sessionID string, grabOffset, cursorX int) tea.Cmd {
	for index, tab := range t.tabs {
		if tab.SessionID != sessionID {
			continue
		}
		t.dragSeq++
		t.drag = dragState{active: true, dragIdx: index, dropIdx: noTab, cursorX: cursorX, startX: cursorX, grabOffset: max(0, grabOffset), seq: t.dragSeq}
		t.viewDirty, t.visualDirty = true, true
		t.visualGeneration++
		t.recordVisualState()
		return tea.Batch(t.updateDragReflow(), t.syncIndicatorSub())
	}
	return nil
}
