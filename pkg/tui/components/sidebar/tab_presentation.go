package sidebar

import (
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// PresentationSnapshot is inert painted state, never a session or input owner.
// Only the visible content shell is captured; footer and scroll state stay local.
type PresentationSnapshot struct {
	rows []placedRow
}

func (m *model) CapturePresentation() PresentationSnapshot {
	if m.mode != ModeVertical || m.placement == nil {
		return PresentationSnapshot{}
	}
	var snapshot PresentationSnapshot
	offset := m.scrollview.ScrollOffset()
	painted := m.paintedRows()
	for y := range m.viewportHeight() {
		row, ok := painted[offset+y]
		if !ok {
			continue
		}
		row.text = m.placementText(row, m.placement.width)
		row.y -= float64(offset)
		row.targetY = row.y
		row.action, row.payload, row.controls = ClickNone, "", nil
		row.target, row.queueControls = false, false
		snapshot.rows = append(snapshot.rows, row)
	}
	return snapshot
}

// TransitionFrom retargets the existing shared placement animation after sizing.
// It cannot restore canonical data, hover, collapse preferences or old actions.
func (m *model) TransitionFrom(snapshot PresentationSnapshot) tea.Cmd {
	if !m.presentationActive || m.mode != ModeVertical || len(snapshot.rows) == 0 || m.viewportHeight() == 0 {
		return nil
	}
	rows := slices.Clone(snapshot.rows)
	offset := float64(m.scrollview.ScrollOffset())
	width := m.contentWidth(m.cachedNeedsScrollbar)
	for i := range rows {
		rows[i].text = ansi.Truncate(rows[i].text, width, "")
		rows[i].y += offset
		rows[i].targetY = rows[i].y
	}
	m.placement = &placementState{rows: rows, width: width, crossSession: true}
	m.reconcileDirty = true
	return m.ReconcileLayout()
}

// Distinct changed-content identities use the same exit path as removed rows.
// Presentation-only glyph/hover refreshes never enter this semantic path.
func (m *model) preserveChangedRows(target []placedRow) {
	current := m.placement
	if current == nil {
		return
	}
	texts := make(map[string]string, len(target))
	for _, row := range target {
		texts[row.id] = row.text
	}
	for i := range current.rows {
		row := &current.rows[i]
		if text, ok := texts[row.id]; ok && ansi.Strip(text) != ansi.Strip(row.text) {
			// Recap glyph/count interpolation already has its own shared channels.
			if row.target && (row.id == "tree-summary" || row.id == "todo-summary") {
				continue
			}
			row.id = fmt.Sprintf("exit:%d:%d:%s", m.visualGeneration, i, row.id)
			row.action, row.payload, row.controls = ClickNone, "", nil
		}
	}
}

func (m *model) placementText(row placedRow, width int) string {
	text := row.text
	if !row.target {
		switch {
		case strings.HasPrefix(row.id, "todo:") && row.payload != "":
			return m.todoHoverText(row)
		case strings.HasPrefix(row.id, "queue:") && row.payload != "":
			return m.actionRowText(row.text, "queue:"+row.payload+":", row.queueControls, false, false)
		}
		return text
	}
	switch {
	case strings.HasPrefix(row.id, "todo:") && row.payload != "":
		text = m.todoHoverText(row)
	case row.action == ClickQueuedMessage:
		text = m.actionRowText(row.text, "queue:"+row.payload+":", row.queueControls, false, false)
	case row.id == "active-agent":
		text = m.agentIdentityView(width)
	case row.action == ClickModel:
		text = m.modelRowView(width, modelPlacementRow(row.id))
	case row.id == "tree-summary":
		text = m.hoverText(m.treeSummary(width), "tree-summary")
	case row.id == "todo-summary":
		text = m.todoSummary(width)
	case strings.HasPrefix(row.id, "exit:"):
		return row.text
	}
	return text
}
