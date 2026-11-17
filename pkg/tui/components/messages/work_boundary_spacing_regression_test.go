package messages

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/workblock"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestWorkBoundarySpacingUsesCompositeRatherThanAnchorType(t *testing.T) {
	for _, kind := range []types.MessageType{types.MessageTypeToolCall, types.MessageTypeRuntimeNotice, types.MessageTypeAssistant, types.MessageTypeAssistantReasoningBlock} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			ar := animation.NewRuntime()
			t.Cleanup(ar.Stop)
			m := NewScrollableView(ar, 84, 20, &service.SessionState{}).(*model)
			t.Cleanup(m.StopAnimations)
			m.SetCompactWorkLog(true)
			before := types.Agent(types.MessageTypeRuntimeNotice, "root", "preceding compact content")
			anchor := types.Agent(kind, "root", "work source")
			hidden := types.Agent(types.MessageTypeToolCall, "root", "hidden work source")
			after := types.Agent(types.MessageTypeRuntimeNotice, "root", "following compact content")
			m.messages = []*types.Message{before, anchor, hidden, after}
			block := workblock.New(ar)
			t.Cleanup(block.StopAnimation)
			m.setWorkHeaders(map[int]*workblock.Model{1: block})
			m.workSourceToHeader = map[int]int{1: 1, 2: 1}
			require.True(t, m.displayNeedsSeparator(0), "Work always has a gap above, even for compact or assistant anchors")
			require.True(t, m.displayNeedsSeparator(1), "Work has a gap before adjacent compact content")
			require.False(t, m.displayNeedsSeparator(2), "hidden children cannot add separators")
		})
	}
}

// An existing Work header can change anchors across retained prose when an old
// reply joins Work. Header paint and hits must not sweep through that prose.
func TestWorkRelocatedHeaderStaysAtDestinationDuringHandoff(t *testing.T) {
	m := newFourthReplyFixture(t)
	call := tools.ToolCall{ID: "relocated-header", Function: tools.FunctionCall{Name: "check"}}
	m.AddOrUpdateToolCall("root", call, tools.Tool{Name: "check"}, types.ToolStatusCompleted)
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", ""))
	m.View()
	x, y, _ := fourthReplyTextCoord(t, m, "seed reply 1 prose")
	m.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	m.AppendToLastMessage("root", "fourth reply prose")
	m.handleMouseRelease(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	for range 100 {
		if !m.replyHandoff.Active() {
			break
		}
		m.View()
		if m.replyHandoff.Phase() == replyHandoffReflow {
			for _, row := range m.replyHandoffFrameRows {
				if row.Composite == nil || row.Source != nil {
					continue
				}
				for _, target := range m.replyHandoff.targetLayout {
					if target.Composite == row.Composite && target.CompositeLine == row.CompositeLine {
						require.Equal(t, target.Y, row.Y, "header uses destination geometry throughout reflow")
					}
				}
				first, last := replyHandoffTerminalSpan(row.Y, row.Height)
				for line := first; line < last; line++ {
					mapped, ok := m.replyHandoffRowAt(line)
					require.True(t, ok)
					require.Same(t, row.Composite, mapped.Composite, "paint and header hits use the same row")
				}
			}
			for _, row := range m.replyHandoffFrameRows {
				if row.Source == nil || row.Spacer || !row.Interactive {
					continue
				}
				first, _ := replyHandoffTerminalSpan(row.Y, row.Height)
				mapped, ok := m.replyHandoffRowAt(first)
				require.True(t, ok)
				require.Same(t, row.Source, mapped.Source, "Work header never covers retained prose")
			}
		}
		acceptAndUpdateHandoffTick(t, m.ar, m)
	}
	require.False(t, m.replyHandoff.Active())
}

func TestWorkBoundaryReplayRendersGapAndSettledPreview(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	m := NewScrollableView(ar, 84, 40, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	m.SetCompactWorkLog(true)
	sess := session.New()
	sess.AddMessage(session.UserMessage("preceding request"))
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "answer", ReasoningContent: "replayed reasoning"}))
	BeginReplay(m, PrepareReplay(sess), nil)
	finishPreparedReplay(t, m)
	rows := strings.Split(ansi.Strip(m.View()), "\n")
	found := false
	for i, row := range rows {
		if strings.Contains(row, "Work [+]") {
			require.Greater(t, i, 0)
			require.Empty(t, strings.TrimSpace(rows[i-1]), "replayed Work keeps a gap above its header")
			found = true
		}
	}
	require.True(t, found)
	require.Zero(t, ar.ActiveCount(), "replay never animates historical previews")
}
