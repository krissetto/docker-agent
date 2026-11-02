package messages

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// vprlCorrectnessSizes are the sizes that compare full frames against a fresh
// load; 6000 is compared only in the opt-in heavy mode.
func vprlCorrectnessSizes() []int {
	if os.Getenv("VPRL_HEAVY") != "" {
		return vprlSizes
	}
	return []int{100, 1000}
}

// vprlBoundSizes are the sizes whose render counts are asserted.
func vprlBoundSizes() []int { return vprlCorrectnessSizes() }

// vprlTheme returns a theme variant that changes every role the transcript reads.
func vprlTheme(original *styles.Theme) *styles.Theme {
	theme := *original
	theme.Colors.Background, theme.Colors.TextPrimary, theme.Colors.TextMuted = "#ffffff", "#111111", "#445566"
	theme.Colors.BackgroundAlt = "#dddddd"
	return &theme
}

// TestViewportRelayoutWidthMatchesFreshLoad is a behavioural guard: a sequence
// of width changes followed by a return to the start width must render the same
// frames, at the top and at the bottom, as a model loaded fresh at that width.
func TestViewportRelayoutWidthMatchesFreshLoad(t *testing.T) {
	for _, n := range vprlCorrectnessSizes() {
		t.Run(fmt.Sprintf("messages=%d", n), func(t *testing.T) {
			sess := vprlRealSession(t, n)
			got := vprlLoad(t, 120, sess)
			t.Cleanup(got.StopAnimations)
			for _, width := range []int{90, 60, 100, 120} {
				got.SetSize(width, 40)
				_ = got.View()
			}
			want := vprlLoad(t, 120, sess)
			t.Cleanup(want.StopAnimations)
			// Raw estimated totals may differ until items are measured. The exact
			// comparison runs after an explicit full demand in
			// TestViewportRelayoutTotalsConvergeAfterFullDemand.
			require.GreaterOrEqual(t, got.RenderedContentHeight(), 40, "estimated total must cover the viewport")
			require.GreaterOrEqual(t, want.RenderedContentHeight(), 40, "estimated total must cover the viewport")

			got.scrollToTop()
			want.scrollToTop()
			require.Equal(t, want.View(), got.View(), "top frame after width round trip")

			_ = got.scrollToBottom()
			_ = want.scrollToBottom()
			require.Equal(t, want.View(), got.View(), "bottom frame after width round trip")
		})
	}
}

// TestViewportRelayoutThemeFirstViewMatchesFreshRender checks that the first
// frame after a theme change equals a model rendered fresh under that theme.
func TestViewportRelayoutThemeFirstViewMatchesFreshRender(t *testing.T) {
	for _, n := range vprlCorrectnessSizes() {
		t.Run(fmt.Sprintf("messages=%d", n), func(t *testing.T) {
			original := styles.CurrentTheme()
			t.Cleanup(func() { styles.ApplyTheme(original) })
			sess := vprlRealSession(t, n)
			got := vprlLoad(t, 120, sess)
			t.Cleanup(got.StopAnimations)
			before := got.View()

			styles.ApplyTheme(vprlTheme(original))
			after := got.View()
			require.NotEqual(t, before, after, "the variant must change the transcript")

			want := vprlLoad(t, 120, sess)
			t.Cleanup(want.StopAnimations)
			require.Equal(t, want.View(), after, "first themed frame equals a fresh themed render")
		})
	}
}

// TestViewportRelayoutRenderBounds asserts the viewport-bounded render contract
// with the existing renderedMessages counter. Each step may render at most the
// visible item count plus vprlAnchorSlack. Parent code is expected to fail the
// resize and theme assertions because it re-renders the whole transcript.
func TestViewportRelayoutRenderBounds(t *testing.T) {
	for _, n := range vprlBoundSizes() {
		t.Run(fmt.Sprintf("messages=%d", n), func(t *testing.T) {
			original := styles.CurrentTheme()
			t.Cleanup(func() { styles.ApplyTheme(original) })
			sess := vprlRealSession(t, n)
			m := vprlLoad(t, 120, sess)
			t.Cleanup(m.StopAnimations)

			check := func(name string, step func()) {
				t.Helper()
				before := vprlRenders(m)
				step()
				_ = m.View()
				rendered := vprlRenders(m) - before
				bound := uint64(vprlVisibleItems(m) + vprlAnchorSlack)
				t.Logf("%s messages=%d renders=%d bound=%d", name, n, rendered, bound)
				assert.LessOrEqual(t, rendered, bound, "%s must render only the viewport", name)
			}

			check("width_shrink", func() { m.SetSize(90, 40) })
			check("width_grow", func() { m.SetSize(120, 40) })
			check("theme_first_view", func() { styles.ApplyTheme(vprlTheme(original)) })

			m.SetSize(100, 40)
			_ = m.View()
			m.scrollToTop()
			_ = m.View()
			const steps = 20
			for step := range steps {
				check(fmt.Sprintf("scroll_after_resize_step_%02d", step), func() {
					_, _ = m.Update(messages.WheelCoalescedMsg{Delta: 1, X: 5, Y: 5})
				})
			}
		})
	}
}

// vprlSpanStart returns the even item index of the first prompt of the copied
// span. Items alternate prompt and answer, so view k is a prompt and k+2 is the
// next prompt, with one answer between them.
func vprlSpanStart(n int) int { return (n / 2) &^ 1 }

// vprlSpanEnd is the exact source text of the span's last prompt view.
func vprlSpanEnd(k int) string {
	return fmt.Sprintf("question %d about the transcript relayout and viewport", k+2)
}

// vprlSelectSpan selects by content: from the start of prompt view k to the end
// of prompt view k+2. Cells are resolved at the model's current width.
func vprlSelectSpan(t *testing.T, m *model, k int) {
	t.Helper()
	require.True(t, m.selectSourceSpan(k, 0, k+2, len(vprlSpanEnd(k))),
		"source span from view %d to view %d must match rendered cells", k, k+2)
}

// vprlWords returns the copied text as words, so line wrapping does not matter.
func vprlWords(text string) []string { return strings.Fields(ansi.Strip(text)) }

// TestViewportRelayoutOffscreenSelectionCopy copies a content-selected span
// that lies outside the viewport, across two items and one answer. The copied
// words must be the same at the original width, after a width change, and in a
// fresh model at the new width.
func TestViewportRelayoutOffscreenSelectionCopy(t *testing.T) {
	for _, n := range vprlCorrectnessSizes() {
		t.Run(fmt.Sprintf("messages=%d", n), func(t *testing.T) {
			k := vprlSpanStart(n)
			sess := vprlRealSession(t, n)
			got := vprlLoad(t, 120, sess)
			t.Cleanup(got.StopAnimations)
			require.Len(t, got.views, n)
			got.scrollToTop()
			_ = got.View()
			vprlSelectSpan(t, got, k)
			require.GreaterOrEqual(t, got.selection.startLine, got.scrollOffset+got.height,
				"the selection must start below the viewport")

			before := vprlWords(got.extractSelectedText())
			require.Greater(t, len(before), 3)
			require.Equal(t, []string{"question", strconv.Itoa(k), "about"}, before[:3])
			require.Contains(t, before, "Answer", "the span must cover the answer between the two prompts")
			require.Equal(t, "viewport", before[len(before)-1])

			got.SetSize(90, 40)
			_ = got.View()
			after := vprlWords(got.extractSelectedText())

			fresh := vprlLoad(t, 90, sess)
			t.Cleanup(fresh.StopAnimations)
			fresh.scrollToTop()
			_ = fresh.View()
			vprlSelectSpan(t, fresh, k)
			require.Equal(t, vprlWords(fresh.extractSelectedText()), after,
				"copy after width change equals a fresh copy of the same source span")
			require.Equal(t, before, after, "copied words do not depend on width")
		})
	}
}

// vprlTokenKey orders unique tokens by their source position: para*100 + word.
func vprlTokenKey(tok string) int {
	para, _ := strconv.Atoi(tok[1:4])
	word, _ := strconv.Atoi(tok[5:])
	return para*100 + word
}

// vprlFirstVisibleToken returns the first content token of a frame in row-major
// order. It returns "" when no row carries a token.
func vprlFirstVisibleToken(frame string) string {
	for line := range strings.SplitSeq(ansi.Strip(frame), "\n") {
		if tok := vprlTokenRe.FindString(line); tok != "" {
			return tok
		}
	}
	return ""
}

// vprlRowOfToken returns the row of a frame that carries tok as a whole token,
// or -1 when tok is not visible.
func vprlRowOfToken(frame, tok string) int {
	for y, line := range strings.Split(ansi.Strip(frame), "\n") {
		if slices.Contains(vprlTokenRe.FindAllString(line, -1), tok) {
			return y
		}
	}
	return -1
}

// TestViewportRelayoutAnchorSurvivesWidthChange anchors the reader inside the
// long mixed message and changes width. The anchored content token must stay
// visible in the frame, and the first visible source line must not start below
// it. Both are checks on content, not on message markers or fixed rows.
func TestViewportRelayoutAnchorSurvivesWidthChange(t *testing.T) {
	for _, n := range vprlCorrectnessSizes() {
		t.Run(fmt.Sprintf("messages=%d", n), func(t *testing.T) {
			m := vprlLoad(t, 120, vprlAnchorSession(n/2))
			t.Cleanup(m.StopAnimations)
			_ = m.scrollToBottom()
			_ = m.View()
			// Scroll up through the real wheel path, so the reader is marked as scrolled.
			_, _ = m.Update(messages.WheelCoalescedMsg{Delta: -5, X: 5, Y: 5})
			require.True(t, m.userHasScrolled, "a wheel step up must mark the reader as scrolled")
			assert.NotContains(t, ansi.Strip(m.View()), "p013w39", "the reader must be scrolled above the transcript end")
			before := m.View()
			anchor := vprlFirstVisibleToken(before)
			require.NotEmpty(t, anchor, "the reader must sit inside the mixed message")

			m.SetSize(90, 40)
			after := m.View()
			first := vprlFirstVisibleToken(after)
			require.NotEmpty(t, first, "a token must stay visible after the width change")
			row := vprlRowOfToken(after, anchor)
			require.GreaterOrEqual(t, row, 0, "anchor %s must stay visible after the width change", anchor)
			require.Less(t, row, m.height, "anchor %s must stay inside the frame", anchor)
			assert.LessOrEqual(t, vprlTokenKey(first), vprlTokenKey(anchor),
				"the first visible source line %s must not start below the anchor %s", first, anchor)
		})
	}
}

// TestViewportRelayoutTotalsConvergeAfterFullDemand checks exact totals only
// after an explicit full demand on both models. Before that, totals are
// estimates. The demand is deliberate: ensureAllItemsRendered measures every
// item, which the viewport-bounded path never does on its own.
func TestViewportRelayoutTotalsConvergeAfterFullDemand(t *testing.T) {
	for _, n := range vprlCorrectnessSizes() {
		t.Run(fmt.Sprintf("messages=%d", n), func(t *testing.T) {
			sess := vprlRealSession(t, n)
			got := vprlLoad(t, 120, sess)
			t.Cleanup(got.StopAnimations)
			for _, width := range []int{90, 60, 100, 120} {
				got.SetSize(width, 40)
				_ = got.View()
			}
			want := vprlLoad(t, 120, sess)
			t.Cleanup(want.StopAnimations)
			got.ensureAllItemsRendered()
			want.ensureAllItemsRendered()
			require.Equal(t, want.RenderedContentHeight(), got.RenderedContentHeight(), "exact total after full demand")
		})
	}
}

// vprlViewportSlots returns the slot indices whose rows intersect the viewport.
// It uses the exact displayEnd bounds that vprlVisibleItems uses, so both sides
// read the same geometry.
func vprlViewportSlots(m *model) []int {
	n := len(m.views)
	top, bottom := m.scrollOffset, m.scrollOffset+m.height
	if n == 0 || m.height <= 0 {
		return nil
	}
	start := func(i int) int {
		if i == 0 {
			return 0
		}
		return m.displayEnd(i - 1)
	}
	first := sort.Search(n, func(i int) bool { return m.displayEnd(i) > top })
	past := sort.Search(n, func(i int) bool { return start(i) >= bottom })
	var out []int
	for i := first; i < past; i++ {
		if m.displayEnd(i) > start(i) {
			out = append(out, i)
		}
	}
	return out
}

// TestViewportRelayoutLoadRendersViewportOnly loads a whole history through the
// real replay path and bounds every body built in any phase. The phases are
// BeginReplay, the recent-first drain (ContinueReplay), the first usable frame,
// the older-history paging (finishHistoryReplay), and the final frame. The first
// frame is captured straight after the drain, before older history is paged in.
// The counters are cumulative from construction and never reset, so the totals
// cover every phase. The bound is the union of the first and final viewport
// slots plus vprlAnchorSlack. vprlAnchorSlack is the anchor item plus one fill
// item, not a prefetch allowance. Each body category is bounded separately,
// because prepare and reasoning work are distinct item categories. Materialized
// slots must lie in the union or the reply handoff. The frame check only asserts
// that the first frame is non-blank; the exact first-frame and reference-frame
// tests cover content.
func TestViewportRelayoutLoadRendersViewportOnly(t *testing.T) {
	for _, fx := range vprlFixtures() {
		for _, n := range vprlCorrectnessSizes() {
			t.Run(fmt.Sprintf("fixture=%s/messages=%d", fx.name, n), func(t *testing.T) {
				sess := fx.build(t, n)
				m := NewScrollableView(animation.NewRuntime(), 120, 40, &service.SessionState{}).(*model)
				t.Cleanup(m.StopAnimations)
				BeginReplay(m, PrepareReplay(sess), nil)
				drainRetainedReplay(t, m)

				union := map[int]bool{}
				first := m.View()
				require.NotEmpty(t, strings.TrimSpace(ansi.Strip(first)), "the first usable frame must show transcript content")
				for _, i := range vprlViewportSlots(m) {
					union[i] = true
				}
				require.Positive(t, m.geom.Total(), "the layout total must be an estimate, not zero")

				finishHistoryReplay(t, m)
				final := m.View()
				require.NotEmpty(t, strings.TrimSpace(ansi.Strip(final)), "the final frame must show transcript content")
				for _, i := range vprlViewportSlots(m) {
					union[i] = true
				}

				s := m.layoutWorkStats()
				bound := uint64(len(union) + vprlAnchorSlack)
				bodies := s.syncRenders + s.compositeViews + s.prepareExecuted + s.reasoningExecuted
				t.Logf("fixture=%s messages=%d union=%d bound=%d bodies=%d sync=%d composite=%d prepare=%d/%d/%d reasoning=%d/%d/%d",
					fx.name, n, len(union), bound, bodies, s.syncRenders, s.compositeViews,
					s.prepareScheduled, s.prepareExecuted, s.prepareApplied,
					s.reasoningScheduled, s.reasoningExecuted, s.reasoningApplied)
				assert.LessOrEqual(t, bodies, bound, "load must build bodies only for the viewport union")
				assert.LessOrEqual(t, s.syncRenders, bound, "synchronous renders must stay in the viewport union")
				assert.LessOrEqual(t, s.compositeViews, bound, "composite bodies must stay in the viewport union")
				assert.LessOrEqual(t, s.prepareScheduled, bound, "no prepare may be scheduled outside the viewport union")
				assert.LessOrEqual(t, s.reasoningScheduled, bound, "no reasoning prepare may be scheduled outside the viewport union")
				assert.LessOrEqual(t, s.prepareExecuted, bound, "no prepare may execute outside the viewport union")
				assert.LessOrEqual(t, s.reasoningExecuted, bound, "no reasoning prepare may execute outside the viewport union")
				assert.GreaterOrEqual(t, s.prepareScheduled, s.prepareExecuted, "prepare cannot run unscheduled")
				assert.GreaterOrEqual(t, s.reasoningScheduled, s.reasoningExecuted, "reasoning cannot run unscheduled")
				assert.LessOrEqual(t, s.prepareApplied, s.prepareExecuted, "a prepare cannot apply before it executes")
				assert.LessOrEqual(t, s.reasoningApplied, s.reasoningExecuted, "a reasoning result cannot apply before it executes")

				keep := m.handoffRetained()
				for i := range m.renderedItems {
					if _, handoff := keep[i]; !union[i] && !handoff {
						assert.Fail(t, "materialized slot lies outside the viewport union", "slot %d", i)
					}
				}
				for i := range m.uncached {
					if _, handoff := keep[i]; !union[i] && !handoff {
						assert.Fail(t, "uncached slot lies outside the viewport union", "slot %d", i)
					}
				}
			})
		}
	}
}
