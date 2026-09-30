package sidebar

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

type queueRow struct {
	id, turnID string
	first      bool
}

func (m *model) queueSection(width int) string {
	m.queueRows = nil
	if len(m.queuedMessages) == 0 {
		return ""
	}
	lines := []string{ansi.Truncate(styles.MutedStyle.Render("Queue:"), width, "")}
	indent, actions := rowActions(width, false)
	m.queueRows = append(m.queueRows, queueRow{id: "queue-header"})
	for _, msg := range m.queuedMessages {
		room := actions.TextWidth
		wrapped := strings.Split(ansi.Hardwrap(ansi.Strip(strings.ReplaceAll(msg.Text, "\r", "")), room, false), "\n")
		if len(wrapped) > 2 {
			wrapped = wrapped[:2]
			wrapped[1] = ansi.Truncate(wrapped[1], max(0, room-1), "") + "…"
		}
		for i, line := range wrapped {
			prefix := strings.Repeat(" ", indent)
			if i == 0 && indent > 0 {
				prefix = "-" + strings.Repeat(" ", indent-1)
			}
			body := actions.Render(styles.TabPrimaryStyle.Render(line), "", i == 0)
			lines = append(lines, prefix+body)
			m.queueRows = append(m.queueRows, queueRow{id: fmt.Sprintf("queue:%s:%d", msg.ID, i), turnID: msg.ID, first: i == 0})
		}
	}
	return strings.Join(lines, "\n")
}

// ConfirmQueuedRemoval arms the visible control before allowing a destructive action.
func (m *model) ConfirmQueuedRemoval(id string) bool {
	found := false
	for _, msg := range m.queuedMessages {
		if msg.ID == id {
			found = true
			break
		}
	}
	confirmed := found && m.queueRemoveArmed == id
	m.queueRemoveArmed = ""
	if found && !confirmed {
		m.queueRemoveArmed = id
	}
	m.todoRemoveArmed = ""
	m.invalidateHover()
	return confirmed
}
