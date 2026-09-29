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
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/agentidentity"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/subagentindex"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestInputIdentityCoordinatesCanonicalAcrossResizeScrollAndRestore(t *testing.T) {
	const nodeID = "a1b2c"
	const childSession = "99999999-child-session-not-node"
	snapshot := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: nodeID, SessionID: childSession, Agent: "worker", Name: "Worker 界"}}}}
	for _, origin := range []session.InputOrigin{session.InputOriginAgent, session.InputOriginRuntime} {
		for _, mode := range []string{"turn", "steer"} {
			for _, restored := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/restored=%t", origin, mode, restored), func(t *testing.T) {
					index := subagentindex.New()
					index.Reset(snapshot)
					m := newModel(animation.NewRuntime(), 80, 8, &service.SessionState{}, index)
					sess := session.New()
					sess.SetSubagentTree(&snapshot)
					for range 6 {
						sess.AddMessage(session.UserMessage("earlier wrapped history " + strings.Repeat("padding ", 8)))
					}
					input := session.UserMessage("body Worker 界 (a1b2c) must not be a link " + strings.Repeat("wrapped content ", 12))
					input.InputOrigin, input.InputMode, input.SenderID, input.SenderName = origin, mode, childSession, "worker"
					if restored {
						sess.AddMessage(input)
						m.LoadFromSession(sess, nil)
					} else {
						m.LoadFromSession(sess, nil)
						m.AddInputMessage(input, sess.ItemCount())
					}
					for _, width := range []int{80, 28, 53} {
						m.SetSize(width, 8)
						m.SetPosition(7, 3)
						m.ensureAllItemsRendered()
						idx := len(m.messages) - 1
						start := m.lineOffsets[idx]
						m.scrollOffset, m.userHasScrolled = start, true
						m.scrollview.SetScrollOffset(start)
						frame := m.View()
						plain := ansi.Strip(frame)
						require.Contains(t, plain, "Worker 界 (a1b2c)", "session provenance must resolve to sidebar identity")
						assert.NotContains(t, plain, "99999")
						assert.NotContains(t, plain, "Message from")
						lines := strings.Split(frame, "\n")
						labelLine, col := -1, -1
						for y, line := range lines {
							before, _, found := strings.Cut(ansi.Strip(line), "Worker 界 (a1b2c)")
							if found {
								labelLine, col = y, ansi.StringWidth(before)
								break
							}
						}
						require.GreaterOrEqual(t, labelLine, 0)
						if origin == session.InputOriginAgent && mode != "steer" {
							assert.Contains(t, ansi.Strip(lines[labelLine]), "━ Worker 界 (a1b2c) ━", "identity is integrated in the USER border")
						}
						for x := col; x < col+ansi.StringWidth("Worker 界 (a1b2c)"); x++ {
							id, ok := m.SubagentNodeAt(7+x, 3+labelLine)
							require.True(t, ok, "visible identity cell x=%d y=%d width=%d", x, labelLine, width)
							assert.Equal(t, subagent.NodeID(nodeID), id)
						}
						_, outside := m.SubagentNodeAt(7+col-1, 3+labelLine)
						assert.False(t, outside, "border/icon is not a target")
						_, outside = m.SubagentNodeAt(6, 3+labelLine)
						assert.False(t, outside, "off-viewport coordinates cannot clamp into a target")
						beforeHover := m.View()
						m.handleMouseMotion(tea.MouseMotionMsg{X: 7 + col, Y: 3 + labelLine})
						afterHover := m.View()
						assert.Equal(t, ansi.Strip(beforeHover), ansi.Strip(afterHover), "hover never moves hit coordinates")
						assert.NotEqual(t, beforeHover, afterHover, "identity brightens like sidebar")
						if origin == session.InputOriginAgent && mode != "steer" {
							rawHeader, rawBody := m.renderedLine(start), m.renderedLine(start+1)
							hoveredHeader := m.applyURLUnderline([]string{rawHeader}, start)[0]
							// Render an ordinary USER body at the same viewport edge to compare the actual fade.
							bodyLines := append([]string(nil), m.renderedLines...)
							bodyLines[start] = rawBody
							bodyViewport := scrollview.New(scrollview.WithReserveScrollbarSpace(true))
							bodyViewport.SetSize(width, 8)
							bodyViewport.SetContent(bodyLines, m.totalScrollableHeight())
							bodyViewport.SetScrollOffset(m.scrollOffset)
							fadedBody := strings.Split(bodyViewport.View(), "\n")[labelLine]
							for x := 1; x < m.contentWidth(); x++ {
								assert.Equal(t, backgroundAt(rawBody, 1), backgroundAt(rawHeader, x), "raw identity rule and label share USER background at x=%d", x)
								assert.Equal(t, backgroundAt(rawBody, 1), backgroundAt(hoveredHeader, x), "raw hovered identity preserves USER background at x=%d", x)
								assert.Equal(t, backgroundAt(fadedBody, 1), backgroundAt(lines[labelLine], x), "displayed identity uses the same USER background under edge fade at x=%d", x)
								assert.Equal(t, backgroundAt(lines[labelLine], x), backgroundAt(strings.Split(afterHover, "\n")[labelLine], x), "hover preserves USER background at x=%d", x)
							}
						}
						m.handleMouseMotion(tea.MouseMotionMsg{X: 7 + width - 2, Y: 3 + labelLine})
						if origin == session.InputOriginAgent && mode != "steer" {
							for y, line := range lines {
								if strings.Contains(ansi.Strip(line), "body") {
									_, linked := m.SubagentNodeAt(7+col, 3+y)
									assert.False(t, linked, "body identity text is not navigation")
								}
							}
						}
					}
				})
			}
		}
	}
}

func TestInputIdentityWrappedNoticeCellsAndUnresolvedAreNotFabricated(t *testing.T) {
	index := subagentindex.New()
	snapshot := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "abcde-full-node-id", SessionID: "child-session", Agent: "worker", Name: "long worker name 界"}}}}
	index.Reset(snapshot)
	m := newModel(animation.NewRuntime(), 15, 20, &service.SessionState{}, index)
	input := session.UserMessage("hidden runtime envelope")
	input.InputOrigin, input.SenderID, input.SenderName = session.InputOriginRuntime, "child-session", "worker"
	m.AddInputMessage(input, 0)
	m.ensureAllItemsRendered()
	hits := 0
	for y, line := range m.renderedLines {
		for _, span := range extractOSC8Links(line) {
			if span.url != agentidentity.Link {
				continue
			}
			for x := span.startCol; x < span.endCol; x++ {
				id, ok := m.SubagentNodeAt(x, y)
				require.True(t, ok, "wrapped label cell (%d,%d)", x, y)
				assert.Equal(t, subagent.NodeID("abcde-full-node-id"), id)
				hits++
			}
		}
	}
	assert.Positive(t, hits)
	index.Clear()
	m.RefreshInputReferences()
	m.ensureAllItemsRendered()
	for y := range m.renderedLines {
		for x := range m.contentWidth() {
			_, ok := m.SubagentNodeAt(x, y)
			assert.False(t, ok, "unresolved sender never fabricates navigation")
		}
	}
	assert.Contains(t, ansi.Strip(strings.Join(m.renderedLines, "\n")), "ref")
}

func TestInputIdentityToolWrappedLabelMatchesCanonicalColorAndHitCells(t *testing.T) {
	index := subagentindex.New()
	index.Reset(subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "a1b2c", SessionID: "child-session", Agent: "worker", Name: "Visible worker"}}}})
	m := newModel(animation.NewRuntime(), 20, 30, &service.SessionState{}, index)
	msg := types.ToolCallMessage("root", tools.ToolCall{ID: "spawn", Function: tools.FunctionCall{Name: subagent.ToolSpawnSubagent, Arguments: `{}`}}, tools.Tool{}, types.ToolStatusCompleted)
	msg.Content = `Spawned subagent "Visible worker" (a1b2c). hidden tool body`
	m.messages = append(m.messages, msg)
	m.views = append(m.views, m.createToolCallView(msg))
	for _, width := range []int{20, 35, 80} {
		m.SetSize(width, 30)
		m.ensureAllItemsRendered()
		hits := 0
		for y, line := range m.renderedLines {
			assert.LessOrEqual(t, ansi.StringWidth(line), m.contentWidth(), "tool references wrap to actual view width")
			for x := range m.contentWidth() {
				expected := false
				for _, span := range extractOSC8Links(line) {
					if span.url == agentidentity.Link && x >= span.startCol && x < span.endCol {
						expected = true
					}
				}
				id, ok := m.SubagentNodeAt(x, y)
				assert.Equal(t, expected, ok, "only rendered label cells click at width=%d x=%d y=%d", width, x, y)
				if ok {
					hits++
					assert.Equal(t, subagent.NodeID("a1b2c"), id)
				}
			}
		}
		assert.Positive(t, hits)
		assert.NotContains(t, ansi.Strip(strings.Join(m.renderedLines, "\n")), "hidden tool body")
	}
}

func TestAgentBorderTinyResizeHitCellsExcludeRuleAndBody(t *testing.T) {
	index := subagentindex.New()
	index.Reset(subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "abcde-canonical-node", SessionID: "child-session", Agent: "worker", Name: "Cafe\u0301 界"}}}})
	m := newModel(animation.NewRuntime(), 80, 40, &service.SessionState{}, index)
	t.Cleanup(m.StopAnimations)
	input := session.UserMessage("Natural spaced prose cafe\u0301 界 **literal**")
	input.InputOrigin, input.SenderID, input.SenderName = session.InputOriginAgent, "child-session", "worker"
	m.AddInputMessage(input, 0)
	for _, width := range []int{80, 4, 8, 12, 28, 80} {
		m.SetSize(width, 40)
		m.SetPosition(7, 3)
		m.ensureAllItemsRendered()
		m.scrollOffset, m.userHasScrolled = 0, true
		m.scrollview.SetScrollOffset(0)
		m.View()
		for y, line := range m.renderedLines {
			for x := range m.contentWidth() {
				expected := false
				for _, span := range extractOSC8Links(line) {
					if span.url == agentidentity.Link && x >= span.startCol && x < span.endCol {
						expected = y == 0
					}
				}
				id, ok := m.SubagentNodeAt(7+x, 3+y)
				assert.Equal(t, expected, ok, "width=%d cell=(%d,%d)", width, x, y)
				if ok {
					assert.Equal(t, subagent.NodeID("abcde-canonical-node"), id)
				}
			}
		}
	}
	assert.Equal(t, input.Message.Content, m.messages[0].Content)
}
