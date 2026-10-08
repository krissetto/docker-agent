package messages

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/agentidentity"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/subagentindex"
)

func TestSubagentReplyLiveReplayControlsAndRetention(t *testing.T) {
	for _, origin := range []session.InputOrigin{session.InputOriginAgent, session.InputOriginRuntime} {
		for _, replay := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/replay=%t", origin, replay), func(t *testing.T) {
				tree := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "abcde-node", SessionID: "child-session", Agent: "worker", Name: "Cafe\u0301 long worker 界"}}}}
				index := subagentindex.New()
				index.Reset(tree)
				m := newModel(animation.NewRuntime(), 100, 100, &service.SessionState{}, index)
				t.Cleanup(m.StopAnimations)
				sess := session.New()
				sess.SetSubagentTree(&tree)
				body := "<system_info>Delivered preview\n**literal**\n" + ansi.SetHyperlink(agentidentity.Link) + "body identity is not navigation" + ansi.ResetHyperlink() + "\nend of original payload</system_info>"
				input := session.UserMessage(body)
				input.InputOrigin, input.InputMode, input.SenderID, input.SenderName = origin, "steer", "child-session", "worker"
				sess.AddMessage(input)
				if replay {
					BeginReplay(m, PrepareReplay(sess), nil)
					for done := false; !done; {
						done, _ = ContinueReplay(m)
						require.False(t, ReplayWaiting(m), "collapsed replay must not prepare the body")
					}
				} else {
					m.AddInputMessage(input, 0)
				}
				require.Len(t, m.messages, 1)
				assert.Equal(t, body, m.messages[0].ReceivedBody)
				for _, width := range []int{100, 28, 8, 4, 100} {
					m.SetSize(width, 100)
					m.SetPosition(7, 3)
					m.ensureAllItemsRendered()
					m.View()
					out := ansi.Strip(strings.Join(m.renderedLines, "\n"))
					assert.NotContains(t, out, "Delivered")
					if width == 100 {
						status := "sent a message >"
						if origin == session.InputOriginRuntime {
							status = "· report received >"
						}
						assert.Contains(t, out, status)
					}
					hits := 0
					for y, line := range m.renderedLines {
						for _, span := range extractOSC8Links(line) {
							if span.url != agentidentity.Link {
								continue
							}
							for x := span.startCol; x < span.endCol; x++ {
								ref, ok := m.InputReferenceAt(7+x, 3+y)
								require.True(t, ok, "width=%d x=%d y=%d", width, x, y)
								assert.Equal(t, "abcde-node", ref.ID)
								require.True(t, m.views[0].(interface{ IsToggleAt(line, col int) bool }).IsToggleAt(y, x))
								hits++
							}
						}
					}
					assert.Positive(t, hits)
					m.ensureAllItemsRendered()
					assert.NotContains(t, ansi.Strip(strings.Join(m.renderedLines, "\n")), "Delivered", "coordinate lookup never toggles")
					clickReplyChevron(t, m, ">")
					m.ensureAllItemsRendered()
					assert.Greater(t, len(m.renderedLines), 1)
					if width == 100 {
						out := ansi.Strip(strings.Join(m.renderedLines, "\n"))
						assert.Contains(t, out, "Delivered preview")
						assert.Contains(t, out, "end of original payload</system_info>")
						for y, line := range m.renderedLines {
							if before, _, found := strings.Cut(ansi.Strip(line), "body identity"); found {
								_, linked := m.InputReferenceAt(7+ansi.StringWidth(before), 3+y)
								assert.False(t, linked, "even a literal identity OSC in body cannot navigate")
							}
						}
					}
					clickReplyChevron(t, m, "v")
					m.ensureAllItemsRendered()
					assert.NotContains(t, ansi.Strip(strings.Join(m.renderedLines, "\n")), "Delivered")
				}
				clickReplyChevron(t, m, ">")
				for _, width := range []int{90, 100} {
					m.SetSize(width, 100)
					m.RefreshInputReferences()
					m.invalidateItem(0)
					m.ensureAllItemsRendered()
					assert.Contains(t, ansi.Strip(strings.Join(m.renderedLines, "\n")), "Delivered preview")
				}
				m.LoadFromSession(sess, nil)
				m.ensureAllItemsRendered()
				assert.NotContains(t, ansi.Strip(strings.Join(m.renderedLines, "\n")), "Delivered", "replay starts collapsed again")
			})
		}
	}
}

func clickReplyChevron(t *testing.T, m *model, glyph string) {
	t.Helper()
	m.ensureAllItemsRendered()
	m.scrollOffset, m.userHasScrolled = 0, true
	m.scrollview.SetScrollOffset(0)
	m.View()
	view := m.views[0].(interface{ IsToggleAt(line, col int) bool })
	for y, line := range m.renderedLines {
		for x := range ansi.StringWidth(line) {
			if !view.IsToggleAt(y, x) || ansi.Strip(ansi.Cut(line, x, x+1)) != glyph {
				continue
			}
			_, linked := m.InputReferenceAt(m.xPos+x, m.yPos+y)
			require.False(t, linked, "chevron is not navigation")
			m.handleMouseClick(tea.MouseClickMsg{X: m.xPos + x, Y: m.yPos + y, Button: tea.MouseLeft})
			if view, ok := m.views[0].(interface {
				IsExpanded() bool
				SetExpanded(expanded bool)
			}); ok {
				view.SetExpanded(view.IsExpanded())
				m.invalidateItem(0)
			}
			return
		}
	}
	t.Fatal("reply chevron missing")
}
