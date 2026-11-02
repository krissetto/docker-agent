package messages

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	builtinshell "github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// vprlAnchorTopTokens returns the tokens on the top row of the current frame.
func vprlAnchorTopTokens(m *model) []string {
	return vprlRowTokens(m.View())
}

// TestViewportAnchorRoundTripKeepsContent shrinks and grows the width through
// 120, 60, 100, 60, 120. The anchored content must stay on the top row at
// every step, and the walk back to 120 must not drift.
func TestViewportAnchorRoundTripKeepsContent(t *testing.T) {
	for _, n := range vprlCorrectnessSizes() {
		t.Run(fmt.Sprintf("history=%d", n), func(t *testing.T) {
			m := vprlLoad(t, 120, vprlAnchorSession(n/2))
			t.Cleanup(m.StopAnimations)
			anchor := vprlFindAnchor(t, m)
			unproven := viewportAnchorStats.unproven.Load()
			for _, width := range []int{60, 100, 60, 120} {
				m.SetSize(width, 40)
				assert.Contains(t, vprlAnchorTopTokens(m), anchor, "width %d", width)
			}
			assert.Equal(t, unproven, viewportAnchorStats.unproven.Load(), "no textual fallback to row 0")
		})
	}
}

// TestViewportAnchorHoldsWhenSlotsAboveAreCorrected measures slots above the
// reader after a width epoch. Their estimates change, so offsets above the
// anchor move. The anchored content must still stay on the top row.
func TestViewportAnchorHoldsWhenSlotsAboveAreCorrected(t *testing.T) {
	for _, n := range vprlCorrectnessSizes() {
		t.Run(fmt.Sprintf("history=%d", n), func(t *testing.T) {
			m := vprlLoad(t, 120, vprlAnchorSession(n/2))
			t.Cleanup(m.StopAnimations)
			anchor := vprlFindAnchor(t, m)
			m.SetSize(80, 40)
			require.Contains(t, vprlAnchorTopTokens(m), anchor)

			slot := m.slotAt(m.scrollOffset)
			require.Positive(t, slot, "the anchor must have slots above it")
			above := slot - 1
			require.False(t, m.materialized(above), "the slot above is still an estimate after the epoch")
			// The estimate of the slot above is wrong by three rows. Until it is
			// measured, the reader's offset is computed from that estimate.
			m.setSlotBase(above, m.slotBase[above]+3)
			require.Contains(t, vprlAnchorTopTokens(m), anchor, "a wrong estimate above keeps the anchored row on top")
			// Measuring the slot replaces the estimate. Offsets below it move back.
			before := m.slotOffset(slot)
			_ = m.renderedLineAt(m.slotOffset(above))
			require.NotEqual(t, before, m.slotOffset(slot), "the measurement must move the anchor's offset")
			assert.Contains(t, vprlAnchorTopTokens(m), anchor, "the anchored row must not move on screen")
		})
	}
}

// TestViewportAnchorPinnedBottomFollowStaysPinned checks that a reader at the
// bottom is not anchored to a top row across a width change.
func TestViewportAnchorPinnedBottomFollowStaysPinned(t *testing.T) {
	m := vprlLoad(t, 120, vprlAnchorSession(50))
	t.Cleanup(m.StopAnimations)
	_ = m.scrollToBottom()
	_ = m.View()
	require.True(t, m.isAtBottom())
	m.SetSize(70, 40)
	_ = m.View()
	assert.False(t, m.layoutAnchor.pending, "a pinned reader has no epoch anchor")
	assert.False(t, m.userHasScrolled)
	assert.True(t, m.isAtBottom(), "pinned bottom-follow stays at the bottom")
}

// TestViewportAnchorRealFixturesNeverFallBack runs both real transcript
// compositions through shrink and grow. Textual positions must always be
// proven, so the row-0 fallback counter must not move.
func TestViewportAnchorRealFixturesNeverFallBack(t *testing.T) {
	for _, fx := range vprlFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			m := vprlLoad(t, 120, fx.build(t, 100))
			t.Cleanup(m.StopAnimations)
			m.userHasScrolled = true
			m.setScrollOffset(m.RenderedContentHeight() / 2)
			_ = m.View()
			before := viewportAnchorStats.unproven.Load()
			sourceBefore := viewportAnchorStats.source.Load()
			for _, width := range []int{80, 140, 60, 120} {
				m.SetSize(width, 40)
				_ = m.View()
			}
			assert.Equal(t, before, viewportAnchorStats.unproven.Load(), "textual MapUnproven must not happen on %s", fx.name)
			assert.Greater(t, viewportAnchorStats.source.Load(), sourceBefore, "the source map must place the anchor")
		})
	}
}

// TestViewportAnchorPunctuationOnlyIsNotOpaque records a punctuation-only code
// block. It is text, so its anchor must use the source map, never proportional
// placement.
func TestViewportAnchorPunctuationOnlyIsNotOpaque(t *testing.T) {
	sess := session.New(session.WithID("vprl-punct"))
	sess.AddMessage(session.UserMessage("question about punctuation"))
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "```\n;;; ()) {} !!! ;;;\n;;; ()) {} !!! ;;;\n```\n"}))
	m := vprlLoad(t, 120, sess)
	t.Cleanup(m.StopAnimations)
	slot := len(m.views) - 1
	require.False(t, m.opaqueSlot(slot))
	sp := m.spotAt(slot, 0, 0, spotTop)
	assert.Equal(t, anchorSource, sp.mode)
	assert.True(t, sp.hasSrc)
}

// TestViewportAnchorPlaceSpotRoundTripSameWidth checks that at an unchanged
// width each spot placement returns the exact row and column it captured.
func TestViewportAnchorPlaceSpotRoundTripSameWidth(t *testing.T) {
	m := vprlLoad(t, 100, vprlAnchorSession(10))
	t.Cleanup(m.StopAnimations)
	slot := len(m.views) - 1
	lines := m.slotLines(slot)
	require.NotEmpty(t, lines)
	sm := BuildSourceMap(m.slotSourceText(slot), lines)
	for row := range min(len(lines), 12) {
		for _, col := range []int{0, 3} {
			sp := m.spotAt(slot, row, col, spotStart)
			if sp.mode != anchorSource {
				continue
			}
			_, mr, _, ok := sm.StartAt(row, col)
			require.True(t, ok)
			r, c := m.placeSpot(sp)
			assert.Equal(t, mr, r, "start row %d col %d", row, col)
			assert.Equal(t, col, c, "start col %d row %d", col, row)
		}
	}
}

// TestSelectionRebaseRoundTripAcrossWidths selects five rows from the anchored
// line and returns through 60 and 100 to 120. The copied content between the
// same tokens must not change.
func TestSelectionRebaseRoundTripAcrossWidths(t *testing.T) {
	for _, n := range vprlCorrectnessSizes() {
		t.Run(fmt.Sprintf("history=%d", n), func(t *testing.T) {
			m := vprlLoad(t, 120, vprlAnchorSession(n/2))
			t.Cleanup(m.StopAnimations)
			anchor := vprlFindAnchor(t, m)
			top := m.scrollOffset
			m.selection.active = true
			m.selection.startLine, m.selection.startCol = top, 0
			m.selection.endLine, m.selection.endCol = top+5, 500
			before := vprlTokenSeq(m.extractSelectedText())
			require.NotEmpty(t, before)
			end := before[len(before)-1]
			for _, width := range []int{60, 100, 120} {
				m.SetSize(width, 40)
				_ = m.View()
			}
			after := vprlTokenSeq(m.extractSelectedText())
			assert.Equal(t, vprlTrimTokens(before, anchor, end), vprlTrimTokens(after, anchor, end))
		})
	}
}

// TestSelectionOffscreenEndpointsDoNotMaterializeOnView selects rows far below
// the viewport, changes width, and renders. The offscreen endpoint slot must
// not be materialized by the View; copy then places it.
func TestSelectionOffscreenEndpointsDoNotMaterializeOnView(t *testing.T) {
	m := vprlLoad(t, 120, vprlAnchorSession(60))
	t.Cleanup(m.StopAnimations)
	m.scrollToTop()
	_ = m.View()
	const questionSlot = 40 // a short user question far below the top
	far := m.slotOffset(questionSlot)
	farSlot := m.slotAt(far)
	require.Equal(t, questionSlot, farSlot)
	m.selection.active = true
	m.selection.startLine, m.selection.startCol = far, 0
	m.selection.endLine, m.selection.endCol = far+1, 500
	m.SetSize(90, 40)
	_ = m.View()
	assert.False(t, m.materialized(farSlot), "a View must not materialize an offscreen endpoint")
	assert.NotNil(t, m.selection.refs[endpointStart], "the offscreen endpoint stays pending")

	copied := m.extractSelectedText()
	assert.Contains(t, copied, "question 40 about the transcript relayout")
	assert.Nil(t, m.selection.refs[endpointStart], "copy places the endpoint")
}

// TestSelectionDirectWriteWinsOverPendingRef writes the selection globals
// after an epoch. The direct values must be used and the pending ref dropped.
func TestSelectionDirectWriteWinsOverPendingRef(t *testing.T) {
	m := vprlLoad(t, 120, vprlAnchorSession(40))
	t.Cleanup(m.StopAnimations)
	m.scrollToTop()
	_ = m.View()
	m.selection.active = true
	m.selection.startLine, m.selection.startCol = 2, 0
	m.selection.endLine, m.selection.endCol = 4, 500
	m.SetSize(90, 40)
	require.NotNil(t, m.selection.refs[endpointStart])
	m.selection.startLine, m.selection.endLine = 1, 3
	_ = m.View()
	m.settleSelection()
	assert.Equal(t, 1, m.selection.startLine)
	assert.Equal(t, 3, m.selection.endLine)
	assert.Nil(t, m.selection.refs[endpointStart])
}

// TestSelectionFollowsThemeChange keeps a selection across a theme change. The
// copied text must be unchanged, since a theme alters colors, not content.
func TestSelectionFollowsThemeChange(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	m := vprlLoad(t, 120, vprlAnchorSession(40))
	t.Cleanup(m.StopAnimations)
	anchor := vprlFindAnchor(t, m)
	top := m.scrollOffset
	m.selection.active = true
	m.selection.startLine, m.selection.startCol = top, 0
	m.selection.endLine, m.selection.endCol = top+3, 500
	before := vprlTokenSeq(m.extractSelectedText())
	require.NotEmpty(t, before)
	styles.ApplyTheme(vprlTheme(original))
	_ = m.View()
	after := vprlTokenSeq(m.extractSelectedText())
	assert.Equal(t, before, after)
	assert.Equal(t, anchor, after[0])
}

// TestContentAnchorCellLookups checks the cell-level lookups on a wrapped
// paragraph: start and end cells are exact at the width they were taken.
func TestContentAnchorCellLookups(t *testing.T) {
	source := "alpha beta gamma delta epsilon zeta eta theta"
	rows := []string{"alpha beta gamma", "delta epsilon", "zeta eta theta"}
	sm := BuildSourceMap(source, rows)
	require.Equal(t, MapExact, sm.Status())

	src, mr, mc, ok := sm.StartAt(1, 0)
	require.True(t, ok)
	assert.Equal(t, 1, mr)
	assert.Equal(t, 0, mc)
	assert.Equal(t, strings.Index(source, "delta"), src)

	end, er, ec, ok := sm.EndBefore(1, 8)
	require.True(t, ok)
	assert.Equal(t, 1, er)
	assert.Equal(t, 8, ec, "the last cluster before column 8 is the p of epsilon at column 7")
	assert.Equal(t, strings.Index(source, "epsilon")+2, end, "the exclusive end is after that p")

	row, col, ok := sm.CellAfterSource(end)
	require.True(t, ok)
	assert.Equal(t, 1, row)
	assert.Equal(t, 8, col)
}

// anchorCardModel returns a model whose first item is a shell tool card with
// forty output lines, followed by messages user messages. The card starts
// collapsed. It uses the disclosure scheduler from shell_disclosure_test.go.
func anchorCardModel(t *testing.T, messages int) (*model, *animation.Runtime, *shellDisclosureScheduler) {
	t.Helper()
	scheduler := &shellDisclosureScheduler{now: time.Unix(1, 0)}
	ar := animation.NewRuntimeWithScheduler(scheduler)
	t.Cleanup(ar.Stop)
	m := NewScrollableView(ar, 100, 40, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	call := tools.ToolCall{ID: "anchor-card", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: `{"cmd":"seq 1 40"}`}}
	def := tools.Tool{Name: builtinshell.ToolNameShell}
	m.AddOrUpdateToolCall("root", call, def, types.ToolStatusRunning)
	var out strings.Builder
	for i := range 40 {
		fmt.Fprintf(&out, "card output line %02d\n", i)
	}
	m.AppendToolOutput(&runtime.ToolCallOutputEvent{ToolCallID: call.ID, Output: out.String()})
	for i := range messages {
		m.AddUserMessage(fmt.Sprintf("question %03d about the card above", i))
	}
	_ = m.View()
	return m, ar, scheduler
}

// toggleCardAbove toggles the card the way its header click does (the view
// changes its own disclosure, then the item is invalidated), then lets the
// disclosure animation finish and measures the card at its new height.
func toggleCardAbove(t *testing.T, m *model, ar *animation.Runtime, sched *shellDisclosureScheduler) {
	t.Helper()
	m.views[0].(toggleableView).Toggle()
	m.invalidateItem(0)
	advanceShellDisclosure(t, ar, sched, m)
	_ = m.renderedLineAt(m.slotOffset(0))
	_ = m.View()
}

// anchorCardExpanded reports whether the card at view 0 is expanded.
func anchorCardExpanded(m *model) bool {
	view, ok := m.views[0].(interface{ IsExpanded() bool })
	return ok && view.IsExpanded()
}

// readerTop returns the text of the row at the top of the viewport.
func readerTop(m *model) string {
	return ansi.Strip(m.renderedLine(m.scrollOffset))
}

// anchorReaderAt scrolls a scrolled-up reader to the first row of slot that
// carries its question text, and returns that row's text.
func anchorReaderAt(t *testing.T, m *model, slot int) string {
	t.Helper()
	m.userHasScrolled = true
	for g := m.slotOffset(slot); g < m.slotOffset(slot+1); g++ {
		if strings.Contains(ansi.Strip(m.renderedLine(g)), "question") {
			m.setScrollOffset(g)
			_ = m.View()
			return readerTop(m)
		}
	}
	require.FailNow(t, "slot has no question row", "slot %d", slot)
	return ""
}

// TestViewportAnchorToolCardAboveKeepsReaderRow expands and collapses a real
// tool card above a scrolled-up reader. With anchorFollowsContentAbove set, the
// row at the top of the viewport must stay on its content at each step.
func TestViewportAnchorToolCardAboveKeepsReaderRow(t *testing.T) {
	prev := anchorFollowsContentAbove
	anchorFollowsContentAbove = true
	t.Cleanup(func() { anchorFollowsContentAbove = prev })
	m, ar, sched := anchorCardModel(t, 80)
	m.scrollToTop()
	_ = m.View()
	toggle := m.views[0].(interface{ IsToggleAt(line, col int) bool })
	require.True(t, toggle.IsToggleAt(0, 0))
	m.handleMouseClick(tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft})
	advanceShellDisclosure(t, ar, sched, m)
	_ = m.View()
	require.True(t, anchorCardExpanded(m))

	top := anchorReaderAt(t, m, 20)
	require.Contains(t, top, "question 019")

	m.views[0].(toggleableView).Toggle()
	m.invalidateItem(0)
	advanceShellDisclosure(t, ar, sched, m)
	_ = m.renderedLineAt(m.slotOffset(0))
	_ = m.View()
	assert.False(t, anchorCardExpanded(m), "the card collapsed above the reader")
	assert.Equal(t, top, readerTop(m), "collapsing the card above keeps the reader's row")

	m.views[0].(toggleableView).Toggle()
	m.invalidateItem(0)
	advanceShellDisclosure(t, ar, sched, m)
	_ = m.renderedLineAt(m.slotOffset(0))
	_ = m.View()
	assert.True(t, anchorCardExpanded(m))
	assert.Equal(t, top, readerTop(m), "re-expanding the card above keeps the reader's row")
	assert.True(t, m.userHasScrolled, "the reader stays scrolled up")
}

// anchorBottomRows returns the last three rows of the current frame.
func anchorBottomRows(m *model) string {
	lines := strings.Split(ansi.Strip(m.View()), "\n")
	return strings.Join(lines[max(0, len(lines)-3):], "\n")
}

// toggleCardInView toggles the card the way a header click does and lets the
// disclosure finish. Only View measures the card, so an out-of-band measure
// cannot hide the height change from the pinned logic.
func toggleCardInView(t *testing.T, m *model, ar *animation.Runtime, sched *shellDisclosureScheduler) {
	t.Helper()
	m.views[0].(toggleableView).Toggle()
	m.invalidateItem(0)
	advanceShellDisclosure(t, ar, sched, m)
	_ = m.View()
}

// TestViewportAnchorPinnedBottomStaysPinnedAcrossCardToggles toggles a card while
// the reader is pinned to the bottom, in both states of pinnedShrinkClearsSlack.
// The reader stays pinned. Base behaviour keeps the capped slack after a shrink;
// the flush behaviour follows the shorter content at once.
func TestViewportAnchorPinnedBottomStaysPinnedAcrossCardToggles(t *testing.T) {
	prev := pinnedShrinkClearsSlack
	t.Cleanup(func() { pinnedShrinkClearsSlack = prev })
	for _, flush := range []bool{false, true} {
		t.Run(fmt.Sprintf("pinnedShrinkClearsSlack=%t", flush), func(t *testing.T) {
			pinnedShrinkClearsSlack = flush

			t.Run("card above the pinned reader", func(t *testing.T) {
				m, ar, sched := anchorCardModel(t, 80)
				_ = m.scrollToBottom()
				_ = m.View()
				require.True(t, m.isAtBottom())
				want := anchorBottomRows(m)
				require.Contains(t, want, "question 079")
				for step := range 2 { // expand, then collapse
					toggleCardInView(t, m, ar, sched)
					assert.False(t, m.userHasScrolled, "toggle %d: the reader is not marked as scrolled", step)
					assert.True(t, m.isAtBottom(), "toggle %d: the reader stays at the bottom", step)
					assert.Equal(t, want, anchorBottomRows(m), "toggle %d: the bottom rows are unchanged", step)
				}
			})

			t.Run("card at the bottom", func(t *testing.T) {
				m, ar, sched := anchorCardModel(t, 0)
				_ = m.scrollToBottom()
				_ = m.View()
				require.True(t, m.isAtBottom())

				toggleCardInView(t, m, ar, sched) // expand: the card grows under the pinned reader
				assert.False(t, m.userHasScrolled)
				assert.True(t, m.isAtBottom(), "growing keeps the reader at the bottom")

				toggleCardInView(t, m, ar, sched) // collapse: the card shrinks under the pinned reader
				assert.False(t, m.userHasScrolled)
				assert.True(t, m.isAtBottom(), "shrinking keeps the reader at the bottom")
				if flush {
					assert.Zero(t, m.bottomSlack, "a pinned reader follows the shorter content at once")
				} else {
					assert.Positive(t, m.bottomSlack, "base behaviour keeps capped slack after a shrink")
					assert.LessOrEqual(t, m.bottomSlack, m.maxBottomSlack())
				}
			})
		})
	}
}

// TestViewportAnchorContentAboveSwitch checks both states of the switch. The
// numeric scroll is kept when it is off (base), and the reader's row is kept
// when it is on.
func TestViewportAnchorContentAboveSwitch(t *testing.T) {
	prev := anchorFollowsContentAbove
	t.Cleanup(func() { anchorFollowsContentAbove = prev })
	for _, follow := range []bool{false, true} {
		t.Run(fmt.Sprintf("anchorFollowsContentAbove=%t", follow), func(t *testing.T) {
			anchorFollowsContentAbove = follow
			m, ar, sched := anchorCardModel(t, 80)
			top := anchorReaderAt(t, m, 20)
			before := m.scrollOffset
			toggleCardAbove(t, m, ar, sched)
			if follow {
				assert.Equal(t, top, readerTop(m), "following: the reader's row stays on its content")
				assert.NotEqual(t, before, m.scrollOffset, "following: the scroll moves with the content above")
			} else {
				assert.NotEqual(t, top, readerTop(m), "base: content above moves on screen")
				assert.Equal(t, before, m.scrollOffset, "base: the numeric scroll is untouched")
			}
		})
	}
}

// TestSelectSourceSpanCopiesContentOfOneItem selects one question by content
// (view index and source offsets) and checks the copied text is that item.
func TestSelectSourceSpanCopiesContentOfOneItem(t *testing.T) {
	m := vprlLoad(t, 120, vprlAnchorSession(60))
	t.Cleanup(m.StopAnimations)
	const view = 40
	text := anchorSourceText(m.messages[view])
	require.True(t, m.selectSourceSpan(view, 0, view, len(text)))
	copied := m.extractSelectedText()
	assert.Contains(t, copied, "question 40 about the transcript relayout")
	assert.NotContains(t, copied, "question 41")
}

// TestViewportAnchorKeepsResidualRowsAcrossWidth checks that a reader whose top
// row is a blank row above the anchored token keeps the token on the same frame
// row across a width change. The blank row is the captured residual.
func TestViewportAnchorKeepsResidualRowsAcrossWidth(t *testing.T) {
	m := vprlLoad(t, 120, vprlAnchorSession(50))
	t.Cleanup(m.StopAnimations)
	_ = m.scrollToBottom()
	_ = m.View()
	m.setScrollOffset(m.scrollOffset - 20)
	m.userHasScrolled = true
	before := m.View()
	anchor := vprlFirstVisibleToken(before)
	require.NotEmpty(t, anchor)
	beforeRow := vprlRowOfToken(before, anchor)
	require.Positive(t, beforeRow, "the anchored token must sit below a blank top row")

	m.SetSize(90, 40)
	after := m.View()
	assert.Equal(t, beforeRow, vprlRowOfToken(after, anchor), "the residual row above the token is kept")
}
