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
	lines := []string{styles.MutedStyle.Render("Queue:")}
	m.queueRows = append(m.queueRows, queueRow{id: "queue-header"})
	for _, msg := range m.queuedMessages {
		room := max(1, width-4)
		wrapped := strings.Split(ansi.Hardwrap(msg.Text, room, false), "\n")
		if len(wrapped) > 2 {
			wrapped = wrapped[:2]
			wrapped[1] = ansi.Truncate(wrapped[1], max(0, room-1), "") + "…"
		}
		for i, line := range wrapped {
			prefix := "  "
			if i == 0 {
				prefix = "- "
			}
			body := m.hoverText(styles.TabPrimaryStyle.Render(prefix+line), "queue:"+msg.ID)
			body = padRight(ansi.Truncate(body, max(0, width-2), ""), max(0, width-2))
			remove := " "
			if i == 0 && (m.hoverTarget == "queue:"+msg.ID || m.hoverTarget == "queue-remove:"+msg.ID) {
				style := styles.MutedStyle
				if m.hoverTarget == "queue-remove:"+msg.ID {
					style = styles.ErrorStyle
				}
				remove = style.Render("×")
			}
			lines = append(lines, ansi.Truncate(body+" "+remove, width, ""))
			m.queueRows = append(m.queueRows, queueRow{id: fmt.Sprintf("queue:%s:%d", msg.ID, i), turnID: msg.ID, first: i == 0})
		}
	}
	return strings.Join(lines, "\n")
}
