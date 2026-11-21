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
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	tuimessages "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// Recent-first replay leaves older Work anchors unmeasured. Reading their
// responses from below must not require visiting those anchors first.
func TestStickyWorkScrollBothDirectionsThroughUnmeasuredHistory(t *testing.T) {
	for _, height := range []int{6, 10} {
		for _, input := range []string{"wheel", "key"} {
			t.Run(fmt.Sprintf("%s/height=%d", input, height), func(t *testing.T) {
				ar := animation.NewRuntime()
				m := NewScrollableView(ar, 64, height, &service.SessionState{}).(*model)
				t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
				m.SetCompactWorkLog(true)
				sess := session.New(session.WithID("sticky-direction"))
				for turn := range 12 {
					sess.AddMessage(session.UserMessage(fmt.Sprintf("request %d", turn)))
					sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: strings.Repeat("thinking\n\n", 12), Content: strings.Repeat("answer\n\n", 20)}))
				}
				BeginReplay(m, PrepareReplay(sess), nil)
				drainRetainedReplay(t, m)
				finishHistoryReplay(t, m)
				m.View()
				anchor := m.workProjection.exchanges[0].work[0]
				require.False(t, m.slotKnown[anchor], "fixture must leave older Work unmeasured")
				for _, delta := range []int{-1, 1} {
					checked := 0
					for steps := 0; steps < 2000; steps++ {
						source := m.slotAt(m.scrollOffset)
						if source < len(m.messages) && m.messages[source].Type == types.MessageTypeAssistant {
							exchange := m.workProjection.exchanges[m.workProjection.owner[source]]
							last := exchange.response[len(exchange.response)-1]
							// These rows are well inside a measured response, away from both
							// the natural header and the section-end suppression budget.
							if m.slotKnown[source] && m.slotKnown[last] && m.scrollOffset >= m.slotOffset(source)+3 && m.slotOffset(last)+m.slotBase[last]-m.scrollOffset >= 3 {
								expected := exchange.work[0]
								beforeVisits, beforeBlockViews := m.stats.itemVisits, m.stats.blockViews
								beforeTotal, beforeOffset := m.layoutTotal(), m.scrollOffset
								got, _, ok := m.stickyWork()
								require.Equal(t, beforeVisits, m.stats.itemVisits, "overlay must not render offscreen slots")
								require.Equal(t, beforeBlockViews, m.stats.blockViews)
								require.Equal(t, beforeTotal, m.layoutTotal())
								require.Equal(t, beforeOffset, m.scrollOffset)
								require.True(t, ok, "delta=%d offset=%d anchorKnown=%t", delta, m.scrollOffset, m.slotKnown[expected])
								require.Equal(t, expected, got)
								frame := strings.Split(ansi.Strip(m.View()), "\n")
								require.Contains(t, frame[0], "Work [+]")
								require.Len(t, frame, height)
								checked++
							}
						}
						before := m.scrollOffset
						if input == "wheel" {
							m.Update(tuimessages.WheelCoalescedMsg{Delta: delta, X: 3, Y: height - 1})
						} else {
							key := tea.KeyUp
							if delta > 0 {
								key = tea.KeyDown
							}
							m.Update(tea.KeyPressMsg{Code: key})
						}
						m.View()
						if before == m.scrollOffset {
							break
						}
					}
					require.Greater(t, checked, 12, "both directions must traverse response interiors")
				}
			})
		}
	}
}

func TestStickyWorkDoesNotOccludeCollapsedPreview(t *testing.T) {
	ar := animation.NewRuntime()
	m := NewScrollableView(ar, 64, 6, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	m.SetCompactWorkLog(true)
	m.suppressReplyHandoff = true
	m.AddUserMessage("request")
	m.SetWorkStatus(true, 0)
	m.AppendReasoning("root", "preview one\n\npreview two\n\npreview three")
	m.syncWorkProjection()
	anchor := m.workProjection.exchanges[0].work[0]
	block := m.workHeaderBySource[anchor]
	require.False(t, block.Expanded())
	// A response keeps the section long enough to qualify for an overlay while
	// live reasoning still owns its collapsed preview during the handoff.
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", strings.Repeat("answer\n\n", 20)))
	m.views[anchor].(*reasoningblock.Model).SetReasoningActive(true)
	m.invalidateItem(anchor)
	m.ensureAllItemsRendered()
	block.StopAnimation()
	m.invalidateItem(anchor)
	m.measureSlot(anchor)
	require.Equal(t, 4, m.slotBase[anchor], "header plus all three preview rows")
	m.userHasScrolled = true
	m.setScrollOffset(m.slotOffset(anchor) + 1)
	_, _, ok := m.stickyWork()
	require.True(t, ok, "sticky header must be additional to the collapsed preview")
	require.Contains(t, strings.Split(ansi.Strip(m.View()), "\n")[0], "Working [+]")
	frame := ansi.Strip(m.View())
	require.Contains(t, frame, "preview one")
	require.Contains(t, frame, "preview two")
	require.Contains(t, frame, "preview three")
}

func BenchmarkStickyWorkScrollView(b *testing.B) {
	ar := animation.NewRuntime()
	m := NewScrollableView(ar, 64, 10, &service.SessionState{}).(*model)
	b.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	m.SetCompactWorkLog(true)
	m.suppressReplyHandoff = true
	for turn := range 50 {
		m.addMessage(types.User(fmt.Sprintf("request %d", turn)))
		m.addMessage(types.Agent(types.MessageTypeAssistantReasoningBlock, "root", "thinking"))
		m.addMessage(types.Agent(types.MessageTypeAssistant, "root", strings.Repeat("answer\n\n", 20)))
	}
	m.View()
	delta := -1
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		before := m.scrollOffset
		m.Update(tuimessages.WheelCoalescedMsg{Delta: delta, X: 3, Y: 9})
		m.View()
		if before == m.scrollOffset {
			delta = -delta
		}
	}
}
