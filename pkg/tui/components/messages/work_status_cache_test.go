package messages

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// buildWorkStatusHistory builds turns of finalized markdown history followed by
// one tool exchange, so compact Work has a block to report status on. Building
// it may leave a reply handoff in flight.
// statusCacheFinalTurn owns the final exchange's settled timing in these fixtures.
const statusCacheFinalTurn = "status-cache-final"

func buildWorkStatusHistory(tb testing.TB, workEnabled bool, turns int) *model {
	tb.Helper()
	m := NewScrollableView(animation.NewRuntime(), 80, 24, &service.SessionState{}).(*model)
	tb.Cleanup(m.StopAnimations)
	m.SetCompactWorkLog(workEnabled)
	for i := range turns {
		m.addMessage(types.User(fmt.Sprintf("request %d", i)))
		m.addMessage(types.Agent(types.MessageTypeAssistant, "root", fmt.Sprintf("answer %d\n\n```go\nfunc answer%d() {}\n```", i, i)))
	}
	call := tools.ToolCall{ID: "status-cache-call", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"true"}`}}
	final := types.User("final request")
	final.TurnID = statusCacheFinalTurn
	m.addMessage(final)
	m.addMessage(types.ToolCallMessage("root", call, tools.Tool{Name: "shell"}, types.ToolStatusCompleted))
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "final answer"))
	_ = m.View()
	return m
}

// quietHandoff stops the reply fade left by building history, so counters and
// geometry measure only the calls under test.
func quietHandoff(m *model) {
	if m.replyHandoff.Active() {
		m.replyHandoff.Stop()
		m.replyHandoffFrameRows = nil
		m.replyHandoffSerial++
	}
	_ = m.View()
}

func newWorkStatusCacheHistory(tb testing.TB, workEnabled bool, turns int) *model {
	tb.Helper()
	m := buildWorkStatusHistory(tb, workEnabled, turns)
	quietHandoff(m)
	return m
}

// transcriptPlainText returns every virtual row, so history stays checkable
// even when it is scrolled out of the viewport.
func transcriptPlainText(m *model) string {
	// The explicit full demand makes the loop run over an exact, settled total.
	m.ensureAllItemsRendered()
	var b strings.Builder
	for i := range m.displayTotalHeight() {
		b.WriteString(ansi.Strip(m.renderedLine(i)))
		b.WriteByte('\n')
	}
	return b.String()
}

// messageLineOf returns the first virtual row containing marker.
func messageLineOf(t *testing.T, m *model, marker string) int {
	t.Helper()
	for i := range m.displayTotalHeight() {
		if strings.Contains(ansi.Strip(m.renderedLine(i)), marker) {
			return i
		}
	}
	t.Fatalf("marker %q is not in the transcript", marker)
	return -1
}

// frameWithoutWork drops Work header rows, which are the only rows status changes.
func frameWithoutWork(m *model) string {
	var kept []string
	for line := range strings.SplitSeq(ansi.Strip(m.View()), "\n") {
		if !strings.Contains(line, "Work") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// Toggling status must not re-render finalized history. Expected misses are the
// last row (dropped like invalidateHistoryStep) once per call, plus any Work
// block that is re-rendered; the bound is 4 plus blocks. Before the fix each
// toggle re-rendered all ~400 history assistants.
func TestSetWorkStatusDoesNotRerenderHistory(t *testing.T) {
	for _, workEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("workEnabled=%t", workEnabled), func(t *testing.T) {
			m := newWorkStatusCacheHistory(t, workEnabled, 200)
			_, _, before := m.ResizeCacheStats()

			m.SetWorkStatus(true, 0)
			_ = m.View()
			m.SetWorkStatus(false, 0)
			m.SetTurnTiming(statusCacheFinalTurn, settledTiming(statusCacheFinalTurn, 2*time.Second, "completed"))
			_ = m.View()

			_, _, after := m.ResizeCacheStats()
			blocks := uint64(len(m.workBlocks))
			require.LessOrEqual(t, after-before, 4+blocks, "status toggles re-rendered history")

			plain := transcriptPlainText(m)
			require.Contains(t, plain, "answer 0")
			require.Contains(t, plain, "answer 199")
			require.Contains(t, plain, "final answer")

			if !workEnabled {
				require.Nil(t, firstWorkBlock(m))
				return
			}
			block := firstWorkBlock(m)
			require.NotNil(t, block, "the tool exchange projects into a Work block")
			require.False(t, block.Status().Active)
			header := ansi.Strip(block.View())
			require.Contains(t, header, "Work")
			require.NotContains(t, header, "Working")
			require.Contains(t, header, "· 2s")

			m.SetWorkStatus(true, 0)
			_ = m.View()
			require.True(t, block.Status().Active)
			require.Contains(t, ansi.Strip(block.View()), "Working")
		})
	}
}

// A live reply handoff keeps its frame geometry and sources through status
// changes, headers repaint, and history is not re-rendered. The fade is not
// stopped, unlike the counter test above.
func TestSetWorkStatusDuringLiveHandoffRepaintsWithoutRerender(t *testing.T) {
	m := buildWorkStatusHistory(t, true, 200)
	require.True(t, m.replyHandoff.Active(), "the final reply starts a reply handoff that is still in flight")
	before := slices.Clone(m.replyHandoffFrameRows)
	require.NotEmpty(t, before)
	// Scroll to the legal bottom so the Work header is in the viewport.
	_, _ = m.Update(m.ScrollToBottom()())
	_ = m.View()
	offset := m.scrollOffset
	_, _, renders := m.ResizeCacheStats()

	m.SetWorkStatus(true, 3*time.Second)
	require.True(t, m.replyHandoff.Active(), "a status change must not finish the fade")
	require.Equal(t, offset, m.scrollOffset, "status repaint must not move the viewport")
	requireHandoffGeometry(t, before, m.replyHandoffFrameRows)
	require.Contains(t, ansi.Strip(m.View()), "Working", "the header repaints mid-handoff")
	require.True(t, m.replyHandoff.Active())

	m.SetWorkStatus(false, 0)
	m.SetTurnTiming(statusCacheFinalTurn, settledTiming(statusCacheFinalTurn, 2*time.Second, "completed"))
	require.True(t, m.replyHandoff.Active(), "settling must not finish the fade")
	requireHandoffGeometry(t, before, m.replyHandoffFrameRows)
	frame := ansi.Strip(m.View())
	require.Contains(t, frame, "· 2s", "the settled header repaints mid-handoff")
	require.NotContains(t, frame, "Working")
	require.Equal(t, offset, m.scrollOffset)

	_, _, after := m.ResizeCacheStats()
	require.LessOrEqual(t, after-renders, 4+uint64(len(m.workBlocks)), "status toggles re-rendered history during the handoff")
}

// requireHandoffGeometry mirrors work_status_handoff_paint_only_test.go: frame
// rows keep their geometry and sources; only composite header text may change.
func requireHandoffGeometry(t *testing.T, before, after []replyHandoffFrameRow) {
	t.Helper()
	require.Len(t, after, len(before))
	for i, row := range after {
		require.InDelta(t, before[i].Y, row.Y, 0, "row %d geometry", i)
		require.InDelta(t, before[i].Height, row.Height, 0, "row %d geometry", i)
		require.Equal(t, before[i].LocalRow, row.LocalRow, "row %d local row", i)
		require.Equal(t, before[i].Source, row.Source, "row %d source", i)
		if row.Composite == nil {
			require.Equal(t, before[i].Text, row.Text, "row %d source text", i)
		}
	}
}

// With a sticky bottom and no manual scroll, every status toggle leaves the view
// at the legal bottom. Before the fix updateScrollState saw totalHeight 0 and
// forced a bottom-follow; now it sees the real delta, which is zero unless the
// Work header or body height changes, and the clamp keeps the bottom either way.
func TestSetWorkStatusKeepsStickyBottom(t *testing.T) {
	for _, workEnabled := range []bool{false, true} {
		for _, expanded := range []bool{false, true} {
			if expanded && !workEnabled {
				continue
			}
			t.Run(fmt.Sprintf("workEnabled=%t/expanded=%t", workEnabled, expanded), func(t *testing.T) {
				m := newWorkStatusCacheHistory(t, workEnabled, 60)
				if expanded {
					block := firstWorkBlock(m)
					require.NotNil(t, block)
					block.SetExpandedImmediately(true)
					m.invalidateAllItems()
					_ = m.View()
				}
				// Quieting the handoff can shrink the transcript; re-anchor at the bottom first.
				_, _ = m.Update(m.ScrollToBottom()())
				_ = m.View()
				require.False(t, m.userHasScrolled)
				require.True(t, m.isAtBottom(), "fixture starts at the bottom")

				for _, step := range []struct {
					active  bool
					elapsed time.Duration
				}{{true, 0}, {false, 2 * time.Second}, {true, 0}, {false, 5 * time.Second}} {
					m.SetWorkStatus(step.active, step.elapsed)
					_ = m.View()
					require.False(t, m.userHasScrolled, "status must not mark a manual scroll")
					require.Equal(t, max(0, m.totalScrollableHeight()-m.height), m.scrollOffset, "bottom after SetWorkStatus(%t)", step.active)
					require.True(t, m.isAtBottom())
				}
			})
		}
	}
}

// After a manual scroll up, status toggles keep scrollOffset, userHasScrolled and
// visible content. Base code also kept them: updateScrollState only clamps when
// userHasScrolled is set, so the forced totalHeight 0 never moved the reader.
func TestSetWorkStatusPreservesManualScrollPosition(t *testing.T) {
	for _, workEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("workEnabled=%t", workEnabled), func(t *testing.T) {
			m := newWorkStatusCacheHistory(t, workEnabled, 60)
			// Set the manual scroll directly: wheel routing depends on pointer geometry
			// that this fixture does not pin down.
			m.setScrollOffset(messageLineOf(t, m, "answer 30"))
			m.userHasScrolled = true
			_ = m.View()
			require.True(t, m.userHasScrolled)
			require.Less(t, m.scrollOffset, max(0, m.totalScrollableHeight()-m.height))
			offset := m.scrollOffset
			frame := frameWithoutWork(m)

			m.SetWorkStatus(true, 0)
			require.Equal(t, frame, frameWithoutWork(m), "active status changed visible transcript rows")
			require.True(t, m.userHasScrolled)
			require.Equal(t, offset, m.scrollOffset)

			m.SetWorkStatus(false, 2*time.Second)
			require.Equal(t, frame, frameWithoutWork(m), "settled status changed visible transcript rows")
			require.True(t, m.userHasScrolled)
			require.Equal(t, offset, m.scrollOffset)
		})
	}
}

// A hovered message keeps its hover index and its global-line mapping across a
// status toggle. The hovered message is above the Work block, so its rows do not
// move when the Work header changes.
func TestSetWorkStatusKeepsHoverAndLineMapping(t *testing.T) {
	for _, workEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("workEnabled=%t", workEnabled), func(t *testing.T) {
			m := newWorkStatusCacheHistory(t, workEnabled, 60)
			target := -1
			for i, msg := range m.messages {
				if strings.HasPrefix(msg.Content, "answer 30\n") {
					target = i
				}
			}
			require.GreaterOrEqual(t, target, 0)

			line := messageLineOf(t, m, "answer 30")
			m.setScrollOffset(line)
			m.userHasScrolled = true
			_ = m.View()
			row := rowOf(strings.Split(ansi.Strip(m.View()), "\n"), "answer 30")
			require.GreaterOrEqual(t, row, 0, "hover target is visible")
			_, _ = m.handleMouseMotion(tea.MouseMotionMsg{X: 5, Y: row})
			_ = m.View()
			require.Equal(t, target, m.hoveredMessageIndex)

			for _, active := range []bool{true, false} {
				m.SetWorkStatus(active, 2*time.Second)
				_ = m.View()
				require.Equal(t, target, m.hoveredMessageIndex, "hover moved after SetWorkStatus(%t)", active)
				require.Equal(t, line, messageLineOf(t, m, "answer 30"), "hovered row moved")
				msgIdx, _ := m.globalLineToMessageLineCached(line)
				require.Equal(t, target, msgIdx, "line mapping changed after SetWorkStatus(%t)", active)
				require.Equal(t, row, rowOf(strings.Split(ansi.Strip(m.View()), "\n"), "answer 30"))
			}
		})
	}
}

// Reference hover is cancelled only when offsets actually move. With unchanged
// geometry the valid reference survives both the disabled early return and the
// enabled rebuild; a real offset change still cancels it.
func TestSetWorkStatusCancelsReferenceHoverOnlyWhenOffsetsMove(t *testing.T) {
	m := referenceFixture(t, "header", 80)
	hoverLastReferenceCell(t, m)
	require.NotEmpty(t, m.referenceHoverValues)

	toggle := func(label string) {
		t.Helper()
		hoverLastReferenceCell(t, m)
		total, target := m.totalHeight, m.referenceHoverTarget
		require.NotNil(t, target.message, label)
		m.SetWorkStatus(true, 0)
		_ = m.View()
		require.Equal(t, total, m.totalHeight, "%s: active status moved offsets", label)
		require.Equal(t, target, m.referenceHoverTarget, "%s: active status cancelled hover", label)
		require.NotEmpty(t, m.referenceHoverValues, label)
		m.SetWorkStatus(false, 2*time.Second)
		_ = m.View()
		require.Equal(t, total, m.totalHeight, "%s: settled status moved offsets", label)
		require.Equal(t, target, m.referenceHoverTarget, "%s: settled status cancelled hover", label)
		require.NotEmpty(t, m.referenceHoverValues, label)
	}
	toggle("work off")

	m.SetCompactWorkLog(true)
	_ = m.View()
	toggle("work on")

	// Hover is keyed on the hovered message's identity and cancels only when that
	// message's own offset moves. Appending below it changes the total but not its
	// offset, so the hover stays.
	target := m.referenceHoverTarget
	m.addMessage(types.User("grow below the hovered item"))
	_ = m.View()
	require.Equal(t, target, m.referenceHoverTarget, "appending below must keep the hover")
	require.NotEmpty(t, m.referenceHoverValues)

	// Growth above the hovered reference through the production async result path.
	// The tool card above is expanded by a real click before any hover exists, the
	// reference is then hovered by real motion, and only the result arrival moves
	// the card. No click follows the hover.
	m.SetCompactWorkLog(false)
	call := tools.ToolCall{ID: "hover-above-tool", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"true"}`}}
	m.AddOrUpdateToolCall("root", call, tools.Tool{Name: "shell"}, types.ToolStatusPending)
	input := session.UserMessage("reference below the tool card")
	input.InputOrigin, input.SenderID, input.SenderName = session.InputOriginAgent, "child-session", "worker"
	m.AddInputMessage(input, 0)
	_ = m.View()
	frame := strings.Split(ansi.Strip(m.View()), "\n")
	row := rowOf(frame, "shell")
	require.GreaterOrEqual(t, row, 0, "the tool card above is visible")
	col := ansi.StringWidth(frame[row][:strings.LastIndex(frame[row], ">")]) + 1
	m.Update(tea.MouseClickMsg{X: m.xPos + col, Y: m.yPos + row, Button: tea.MouseLeft})
	_ = m.View()
	hoverLastReferenceCell(t, m)
	require.NotEmpty(t, m.referenceHoverValues)
	hovered := m.referenceHoverSlot
	require.GreaterOrEqual(t, hovered, 0)
	offset := m.slotOffset(hovered)

	m.AddToolResult(&runtime.ToolCallResponseEvent{
		ToolCallID:     "hover-above-tool",
		ToolDefinition: tools.Tool{Name: "shell"},
		Response:       strings.Repeat("tool output line\n", 30),
	}, types.ToolStatusCompleted)
	_ = m.View()
	require.NotEqual(t, offset, m.slotOffset(hovered), "the tool result above moved the hovered item")
	require.Empty(t, m.referenceHoverValues, "the hover is cancelled when its item's offset moves")
}

// buildWorkStatusSessionHistory loads turns persisted history messages with
// LoadFromSession, the production path, so a 6000-row fixture costs one replay
// instead of one snapshot per addMessage. The tool exchange is appended live
// afterwards and then quieted, so the benchmark starts from a settled transcript.
func buildWorkStatusSessionHistory(tb testing.TB, workEnabled bool, turns int) *model {
	tb.Helper()
	m := NewScrollableView(animation.NewRuntime(), 120, 40, &service.SessionState{}).(*model)
	tb.Cleanup(m.StopAnimations)
	sess := session.New()
	// Alternate user and assistant rows, as real history does: assistant rows without
	// user boundaries project into one Work exchange and skip the history render path.
	for i := range turns / 2 {
		sess.AddMessage(session.UserMessage(fmt.Sprintf("request %d", i)))
		sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "**history**\n\n```go\nfmt.Println(123)\n```\n\n"}))
	}
	m.LoadFromSession(sess, nil)
	m.SetCompactWorkLog(workEnabled)
	call := tools.ToolCall{ID: "status-cache-call", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"true"}`}}
	final := types.User("final request")
	final.TurnID = statusCacheFinalTurn
	m.addMessage(final)
	m.addMessage(types.ToolCallMessage("root", call, tools.Tool{Name: "shell"}, types.ToolStatusCompleted))
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "final answer"))
	_ = m.View()
	quietHandoff(m)
	return m
}

// benchmarkSetWorkStatusHistory measures one stream start (SetWorkStatus(true)+View),
// one settle (SetWorkStatus(false)+View) and, outside the timer, one reference
// full re-render (InvalidateRenderCaches+View). The start/settle counters
// exclude the reference phase.
func benchmarkSetWorkStatusHistory(b *testing.B, workEnabled bool, turns int) {
	b.Helper()
	m := buildWorkStatusSessionHistory(b, workEnabled, turns)
	const settledFor = 2 * time.Second
	var startTotal, settleTotal, fullTotal time.Duration
	var rebuilds, renders, fullRenders uint64
	b.ResetTimer()
	for range b.N {
		rebuilds0, _, renders0 := m.ResizeCacheStats()
		began := time.Now()
		m.SetWorkStatus(true, 0)
		_ = m.View()
		startTotal += time.Since(began)
		began = time.Now()
		m.SetWorkStatus(false, settledFor)
		_ = m.View()
		settleTotal += time.Since(began)
		rebuilds1, _, renders1 := m.ResizeCacheStats()
		rebuilds += rebuilds1 - rebuilds0
		renders += renders1 - renders0

		b.StopTimer()
		_, _, fullBefore := m.ResizeCacheStats()
		began = time.Now()
		m.InvalidateRenderCaches()
		_ = m.View()
		fullTotal += time.Since(began)
		_, _, fullAfter := m.ResizeCacheStats()
		fullRenders += fullAfter - fullBefore
		b.StartTimer()
	}
	b.StopTimer()
	ops := float64(b.N)
	b.ReportMetric(float64(rebuilds)/ops, "rebuilds/op")
	b.ReportMetric(float64(renders)/ops, "renderedMessages/op")
	b.ReportMetric(float64(startTotal.Nanoseconds())/ops, "ns/start")
	b.ReportMetric(float64(settleTotal.Nanoseconds())/ops, "ns/settle")
	b.ReportMetric(float64(fullTotal.Nanoseconds())/ops, "ns/full-rerender")
	b.ReportMetric(float64(fullRenders)/ops, "renderedMessages/full-rerender")
}

// BenchmarkSetWorkStatusHistory6000 is opt-in (-bench) and never runs in unit tests.
// It uses 6000 persisted history messages (3000 user/assistant pairs), the scale at which the full re-render
// removed by SetWorkStatus was measured.
func BenchmarkSetWorkStatusHistory6000(b *testing.B) {
	for _, workEnabled := range []bool{false, true} {
		b.Run(fmt.Sprintf("workEnabled=%t", workEnabled), func(b *testing.B) {
			benchmarkSetWorkStatusHistory(b, workEnabled, 6000)
		})
	}
}
