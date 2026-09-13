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
