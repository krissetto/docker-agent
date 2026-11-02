package messages

import (
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// Invariant I5 is about the SAME OBSERVED mutation sequence. Authority values
// (totals, rows, placed slots) must not depend on when a View, copy or mouse
// event happened within that sequence. Reader state can depend on what a View
// observed: a content shrink that a View observed creates bottom slack, which is
// base parity, so a run with no View between the grow and the shrink settles to
// different slack. The scenarios below therefore compare only runs that observe
// the same growth and shrink events. Epoch and content events in the same observed
// order must be invariant: totals, frames, offsets and slack agree.

// ordItems is the persisted transcript size of every order scenario.
const ordItems = 200

// ordState is everything an order-independent layout must agree on.
type ordState struct {
	total  int      // layoutTotal: the authority transcript height
	scroll int      // scrollOffset
	slack  int      // bottomSlack
	frame  string   // View after the last step
	copied []string // text copied by copy steps, in step order
}

// ordStep is one event or observation applied to the model under test.
type ordStep func(t *testing.T, m *model, copied *[]string)

// ordLoad loads the mixed transcript at width 120 through the production replay
// path and pins the reader to the bottom, the state every order starts from.
func ordLoad(t *testing.T) *model {
	t.Helper()
	m := vprlLoad(t, 120, vprlRealSession(t, ordItems))
	t.Cleanup(m.StopAnimations)
	_ = m.scrollToBottom()
	_ = m.View()
	return m
}

// ordRun loads the transcript, applies steps in order and returns the state
// after one final View. The theme is restored afterwards, so every run starts
// from the same global style state.
func ordRun(t *testing.T, steps ...ordStep) ordState {
	t.Helper()
	original := styles.CurrentTheme()
	styles.ApplyTheme(original)
	t.Cleanup(func() { styles.ApplyTheme(original) })
	m := ordLoad(t)
	var copied []string
	for _, step := range steps {
		step(t, m, &copied)
	}
	frame := m.View()
	return ordState{total: m.layoutTotal(), scroll: m.scrollOffset, slack: m.bottomSlack, frame: frame, copied: copied}
}

// ordRequireSame asserts that two orders reached the same authoritative state.
func ordRequireSame(t *testing.T, want, got ordState) {
	t.Helper()
	require.Equal(t, want.total, got.total, "layoutTotal must not depend on call order")
	require.Equal(t, want.scroll, got.scroll, "scrollOffset must not depend on call order")
	require.Equal(t, want.slack, got.slack, "bottomSlack must not depend on call order")
	require.Equal(t, want.frame, got.frame, "View must not depend on call order")
	require.Equal(t, want.copied, got.copied, "copied text must not depend on call order")
}

func ordView() ordStep {
	return func(_ *testing.T, m *model, _ *[]string) { _ = m.View() }
}

func ordSize(w, h int) ordStep {
	return func(_ *testing.T, m *model, _ *[]string) { m.SetSize(w, h) }
}

// ordTheme applies a style set and delivers the theme-changed message the way
// the program does, so the layout sees a theme epoch.
func ordTheme(theme *styles.Theme) ordStep {
	return func(_ *testing.T, m *model, _ *[]string) {
		styles.ApplyTheme(theme)
		_, _ = m.Update(messages.ThemeChangedMsg{})
	}
}

// ordWheelToMiddle scrolls up with real wheel messages to the middle of the
// transcript, which sets userHasScrolled exactly as a user does.
func ordWheelToMiddle() ordStep {
	return func(t *testing.T, m *model, _ *[]string) {
		t.Helper()
		vtstWheelUpTo(t, m, m.totalScrollableHeight()/2)
	}
}

// ordMotion and ordClick deliver real mouse messages through Update. A click is
// a press and a release at the same cell, with no drag.
func ordMotion(x, y int) ordStep {
	return func(_ *testing.T, m *model, _ *[]string) {
		_, _ = m.Update(tea.MouseMotionMsg{X: x, Y: y})
	}
}

func ordClick(x, y int) ordStep {
	return func(_ *testing.T, m *model, _ *[]string) {
		_, _ = m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
		_, _ = m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	}
}

// ordCopy selects the content span from prompt view k to the next prompt and
// copies it. Both endpoints resolve by source text at the current width, and
// the span lies offscreen when the reader is elsewhere.
func ordCopy(k int) ordStep {
	return func(t *testing.T, m *model, copied *[]string) {
		t.Helper()
		require.True(t, m.selectSourceSpan(k, 0, k+2, len(vprlSpanEnd(k))), "span from view %d must match rendered cells", k)
		*copied = append(*copied, m.extractSelectedText())
	}
}

// ordChunk appends one streamed chunk to the live (last) assistant message.
func ordChunk(text string) ordStep {
	return func(_ *testing.T, m *model, _ *[]string) { _ = m.AppendToLastMessage("root", text) }
}

// ordSpinnerAdd appends a trailing spinner: a content growth that is not an
// epoch. ordSpinnerRemove takes it away again through the production path.
func ordSpinnerAdd() ordStep {
	return func(_ *testing.T, m *model, _ *[]string) { _ = m.AddAssistantMessage("root", "") }
}

func ordSpinnerRemove() ordStep {
	return func(_ *testing.T, m *model, _ *[]string) { m.RemoveSpinner() }
}

// ordSpinnerRemoveChecked removes the trailing spinner between two Views and
// checks the content-shrink contract: the shrink adds slack up to the cap, and
// a shrink the slack absorbs does not move the pinned reader.
func ordSpinnerRemoveChecked() ordStep {
	return func(t *testing.T, m *model, _ *[]string) {
		t.Helper()
		_ = m.View()
		prevTotal, prevSlack, prevScroll := m.followTotal, m.bottomSlack, m.scrollOffset
		m.RemoveSpinner()
		_ = m.View()
		delta := m.layoutTotal() - prevTotal
		require.Negative(t, delta, "removing the spinner must shrink the content")
		if prevSlack-delta <= m.maxBottomSlack() {
			require.Equal(t, prevSlack-delta, m.bottomSlack, "a content shrink of %d rows must add that much slack", -delta)
			require.Equal(t, prevScroll, m.scrollOffset, "a shrink absorbed by slack must not move the pinned reader")
		}
		require.False(t, m.userHasScrolled)
	}
}

// ordFullDemand measures every slot and re-pins the reader to the bottom. It is
// the explicit demand after which the authority must equal a fresh layout.
func ordFullDemand() ordStep {
	return func(_ *testing.T, m *model, _ *[]string) {
		m.ensureAllItemsRendered()
		_ = m.scrollToBottom()
	}
}

// ordAssertPinned checks the pinned-follow contract after a View: a reader that
// has not scrolled sits at the bottom of the slack-inclusive scrollable height.
func ordAssertPinned() ordStep {
	return func(t *testing.T, m *model, _ *[]string) {
		t.Helper()
		_ = m.View()
		require.False(t, m.userHasScrolled, "a pinned reader has not scrolled")
		want := max(0, m.displayTotalHeight()+m.bottomSlack-m.height)
		require.Equal(t, want, m.scrollOffset, "pinned reader must sit at the bottom (total=%d slack=%d)", m.layoutTotal(), m.bottomSlack)
	}
}

// ordFreshFinal is the reference for the epoch scenario: a model that loads
// under the final theme, is sized to the final width and height, and is fully
// demanded. Its state is the authority any order must converge to.
func ordFreshFinal(t *testing.T, theme *styles.Theme, w, h int) ordState {
	t.Helper()
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	styles.ApplyTheme(theme)
	m := vprlLoad(t, 120, vprlRealSession(t, ordItems))
	t.Cleanup(m.StopAnimations)
	m.SetSize(w, h)
	m.ensureAllItemsRendered()
	_ = m.scrollToBottom()
	frame := m.View()
	return ordState{total: m.layoutTotal(), scroll: m.scrollOffset, slack: m.bottomSlack, frame: frame}
}

// ordWheelUntilToken scrolls up one real wheel event at a time from the bottom
// until the reader is scrolled (not pinned) and the frame shows an anchor token,
// so the top rows carry content the test can follow across a width epoch.
func ordWheelUntilToken(t *testing.T, m *model) {
	t.Helper()
	_ = m.scrollToBottom()
	_ = m.View()
	for range 100_000 {
		_, _ = m.Update(messages.WheelCoalescedMsg{X: 5, Y: 5, Delta: -1})
		if _, row := ordAnchorToken(m.View()); row >= 0 && m.userHasScrolled && !m.isAtBottom() {
			return
		}
	}
	require.FailNow(t, "no anchor token became visible while scrolling up")
}

// ordAnchorToken returns the first anchor token of the first frame row that has
// one, and that row's index. The anchor fixture's tokens are unique per message.
func ordAnchorToken(frame string) (string, int) {
	for y, line := range splitRendered(ansi.Strip(frame)) {
		if toks := vprlTokenSeq(line); len(toks) > 0 {
			return toks[0], y
		}
	}
	return "", -1
}

// TestLayoutOrderPackageSwitchesStayAtDefaults pins the two behaviour switches
// the order scenarios depend on: both stay false until the product decides.
func TestLayoutOrderPackageSwitchesStayAtDefaults(t *testing.T) {
	require.False(t, anchorFollowsContentAbove, "anchorFollowsContentAbove is a pending decision and must stay false")
	require.False(t, pinnedShrinkClearsSlack, "pinnedShrinkClearsSlack is a pending decision and must stay false")
}

// TestLayoutOrderMouseBeforeViewMatchesViewFirst: a real motion and click through
// Update, delivered after a width epoch and before any View, must reach the same
// state as the View that runs first.
func TestLayoutOrderMouseBeforeViewMatchesViewFirst(t *testing.T) {
	viewFirst := ordRun(t, ordWheelToMiddle(), ordSize(90, 40), ordView(), ordMotion(5, 10), ordClick(5, 10))
	mouseFirst := ordRun(t, ordWheelToMiddle(), ordSize(90, 40), ordMotion(5, 10), ordClick(5, 10), ordView())
	ordRequireSame(t, viewFirst, mouseFirst)
}

// TestLayoutOrderCopyMaterializationBeforeViewMatchesViewFirst: copying an
// offscreen span materializes its slots through the copy path. Doing that before
// the View that follows a width epoch must give the same state and copied text.
func TestLayoutOrderCopyMaterializationBeforeViewMatchesViewFirst(t *testing.T) {
	const k = 20 // prompt view above the middle reader: offscreen at the copy
	copyFirst := ordRun(t, ordWheelToMiddle(), ordSize(90, 40), ordCopy(k))
	viewFirst := ordRun(t, ordWheelToMiddle(), ordSize(90, 40), ordView(), ordCopy(k))
	require.Len(t, copyFirst.copied, 1)
	require.Contains(t, copyFirst.copied[0], fmt.Sprintf("question %d about the transcript relayout", k))
	ordRequireSame(t, viewFirst, copyFirst)
}

// TestLayoutOrderEpochsAnyOrderConvergeToFreshLayout runs the same height, width
// and theme epochs in three orders, with Views placed differently. After one
// explicit full demand, every order must equal a fresh model that loaded under
// the final theme and was sized to the final width and height.
func TestLayoutOrderEpochsAnyOrderConvergeToFreshLayout(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	variant := vprlTheme(original)
	const w, h = 100, 30
	want := ordFreshFinal(t, variant, w, h)

	orders := []struct {
		name  string
		steps []ordStep
	}{
		{"views-between", []ordStep{ordSize(70, 40), ordView(), ordTheme(variant), ordView(), ordSize(w, h), ordView(), ordFullDemand()}},
		{"no-intermediate-views", []ordStep{ordTheme(variant), ordSize(70, 40), ordSize(w, h), ordFullDemand()}},
		{"overshoot", []ordStep{ordSize(w, h), ordTheme(variant), ordSize(70, 40), ordSize(w, h), ordFullDemand()}},
	}
	for _, order := range orders {
		t.Run(order.name, func(t *testing.T) {
			got := ordRun(t, order.steps...)
			require.Equal(t, want.total, got.total, "full demand total must equal the fresh layout")
			require.Equal(t, want.frame, got.frame, "frame must equal the fresh layout")
			require.Equal(t, want.scroll, got.scroll, "pinned reader must sit where the fresh layout does")
		})
	}
}

// TestLayoutOrderPinnedStreamViewPerChunkMatchesViewAtEnd streams chunks into
// a pinned reader. A View after every chunk and Views only at the end must give
// identical totals, frames, offsets and slack; the per-chunk run also keeps the
// reader at the bottom after every View.
func TestLayoutOrderPinnedStreamViewPerChunkMatchesViewAtEnd(t *testing.T) {
	var perChunk, atEnd []ordStep
	for k := range 20 {
		chunk := ordChunk(fmt.Sprintf("streamed reply chunk %d with enough words to wrap across several rows of the transcript\n", k))
		perChunk = append(perChunk, chunk, ordView(), ordAssertPinned())
		atEnd = append(atEnd, chunk)
	}
	viewPerChunk := ordRun(t, perChunk...)
	viewAtEnd := ordRun(t, atEnd...)
	ordRequireSame(t, viewAtEnd, viewPerChunk)
}

// TestLayoutOrderStreamWithEpochsAndShrink streams chunks through width epochs
// (grow and shrink) while the reader is pinned. The run with a View after each
// event keeps the reader at the bottom at every View; the run with Views only at
// the end must reach the same totals, frames, offsets and slack. Only events
// both runs observe identically are compared: a spinner that one run observes
// and the other does not is a different reader history (see the shrink test).
func TestLayoutOrderStreamWithEpochsAndShrink(t *testing.T) {
	chunk := func(k int) ordStep {
		return ordChunk(fmt.Sprintf("live chunk %d streams while the terminal changes width and the reader stays pinned\n", k))
	}
	perView := []ordStep{
		chunk(0), ordView(), ordAssertPinned(),
		ordSize(90, 40), chunk(1), ordView(), ordAssertPinned(),
		chunk(2), ordView(), ordAssertPinned(),
		ordSize(160, 40), chunk(3), ordView(), ordAssertPinned(),
		ordSize(60, 40), chunk(4), ordView(), ordAssertPinned(),
		chunk(5),
	}
	viewAtEnd := []ordStep{
		chunk(0), ordSize(90, 40), chunk(1), chunk(2),
		ordSize(160, 40), chunk(3), ordSize(60, 40), chunk(4), chunk(5),
	}
	a := ordRun(t, perView...)
	b := ordRun(t, viewAtEnd...)
	ordRequireSame(t, b, a)
}

// TestLayoutOrderContentShrinkAfterEpochKeepsSlackAndReader: a content shrink
// that is not an epoch (a spinner removed) right after a width epoch. The reader
// stays pinned at every View, and the shrink adds slack up to the cap without
// moving the reader while the slack absorbs it.
func TestLayoutOrderContentShrinkAfterEpochKeepsSlackAndReader(t *testing.T) {
	steps := []ordStep{
		ordChunk("live reply before the epoch\n"), ordView(), ordAssertPinned(),
		ordSize(90, 40), ordView(), ordAssertPinned(),
		ordSpinnerAdd(), ordView(), ordAssertPinned(),
		ordSize(160, 40), ordView(), ordAssertPinned(),
		ordSpinnerAdd(), ordView(), ordAssertPinned(),
		ordSpinnerRemoveChecked(), ordAssertPinned(),
		ordChunk("live reply after the shrink\n"), ordView(), ordAssertPinned(),
	}
	got := ordRun(t, steps...)
	require.Positive(t, got.slack, "the spinner shrink must leave slack in the final state")
}

// TestLayoutOrderScrolledReaderKeepsTopContentAcrossWidth: a reader scrolled with
// real wheel messages keeps the same top anchor token on the same frame row
// across a width epoch, whether or not a View ran before the epoch.
func TestLayoutOrderScrolledReaderKeepsTopContentAcrossWidth(t *testing.T) {
	run := func(viewBeforeEpoch bool) (frame, tok string) {
		m := vprlLoad(t, 120, vprlAnchorSession(200))
		t.Cleanup(m.StopAnimations)
		ordWheelUntilToken(t, m)
		if viewBeforeEpoch {
			_ = m.View()
		}
		tok, row := ordAnchorToken(m.View())
		require.GreaterOrEqual(t, row, 0, "the scrolled frame must show an anchor token")
		m.SetSize(70, 40)
		frame = m.View()
		require.Equal(t, row, vprlRowOfToken(frame, tok), "the top anchor token %q must keep its frame row across a width epoch", tok)
		return frame, tok
	}
	frameA, tokA := run(true)
	frameB, tokB := run(false)
	require.Equal(t, tokA, tokB, "the top token must not depend on a View before the epoch")
	require.Equal(t, frameA, frameB, "the frame must not depend on whether a View ran before the epoch")
}

// TestLayoutOrderSlackStableAcrossViews: after a content shrink, repeated Views
// with no event in between keep bottomSlack, the reader's offset and the frame
// exactly. Oscillation between Views would show up as a changing value.
func TestLayoutOrderSlackStableAcrossViews(t *testing.T) {
	for _, pinned := range []bool{true, false} {
		t.Run(fmt.Sprintf("pinned=%t", pinned), func(t *testing.T) {
			m := ordLoad(t)
			if !pinned {
				vtstWheelUpTo(t, m, m.totalScrollableHeight()/2)
			}
			ordSpinnerAdd()(t, m, nil)
			_ = m.View()
			ordSpinnerRemove()(t, m, nil)
			_ = m.View()
			if pinned {
				require.Positive(t, m.bottomSlack, "a content shrink must leave slack to keep stable")
			} else {
				require.Zero(t, m.bottomSlack, "a scrolled reader has no slack")
			}
			slack, scroll := m.bottomSlack, m.scrollOffset
			frame := m.View()
			for i := range 10 {
				require.Equal(t, slack, m.bottomSlack, "View %d changed bottomSlack", i)
				require.Equal(t, scroll, m.scrollOffset, "View %d changed scrollOffset", i)
				require.Equal(t, frame, m.View(), "View %d changed the frame", i)
			}
		})
	}
}
