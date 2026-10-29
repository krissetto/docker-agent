package messages

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// Hovering an assistant body while its item is dirty must repaint the action
// glyph on the next View, not return the cached frame.
func TestHoverRetargetOnDirtyAssistantRepaintsActionGlyph(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 120, 40, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations() })
	m.SetPosition(4, 2)
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "2 + 2 equals 4."))
	m.View()

	var x, y int
	for row, line := range strings.Split(ansi.Strip(m.View()), "\n") {
		if before, _, ok := strings.Cut(line, "2 + 2 equals 4."); ok {
			x, y = m.xPos+ansi.StringWidth(before)+1, m.yPos+row
			break
		}
	}
	require.NotZero(t, y, "assistant body is visible")
	require.NotContains(t, ansi.Strip(m.View()), types.MessageCopyLabel, "precondition: no action row before hover")

	m.renderDirty = true
	_, _ = m.Update(tea.MouseMotionMsg{X: x, Y: y})
	require.Contains(t, ansi.Strip(m.View()), types.MessageCopyLabel)
}
