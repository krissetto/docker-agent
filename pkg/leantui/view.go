package leantui

import "time"

// buildLines produces the entire frame and cursor position.
func (m *model) buildLines() (lines []string, cursorLine, cursorCol int) {
	m.screen.Status = m.status
	m.screen.TransientNotice = ""
	m.majorNoticeVisible = false
	// Warnings and errors remain in the transcript; an armed cancellation or
	// tool decision keeps the scarce composer footer instead of a summary.
	if !time.Now().Before(m.priorityNoticeUntil) && !m.interruptPending && !m.cancelMarkerPending && m.screen.Confirm == nil && m.majorEvents != nil && m.app != nil && m.app.Session() != nil {
		if notice, active := m.majorEvents.Current(m.app.Session().ID, time.Now()); active {
			m.screen.TransientNotice = notice.Text
			m.majorNoticeVisible = true
		}
	}
	return m.screen.Frame(m.width, m.height, m.spinnerFrame, m.busy(), m.sessionState, m.pendingUsers)
}
