package messages

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// Every word of the long mixed message is a unique token pNNNwMM, so a visible
// row names the exact content it shows. Words 0-39 are prose, 40-49 list items,
// 50-55 code, and 56-57 table cells.
var vprlTokenRe = regexp.MustCompile(`p\d{3}w\d{2}`)

// vprlMixedMessage returns a long markdown answer with prose, lists, code
// blocks and tables. Every token in it is unique.
func vprlMixedMessage() string {
	var b strings.Builder
	b.WriteString("## Mixed long answer\n\n")
	for p := range 14 {
		for w := range 40 {
			fmt.Fprintf(&b, "p%03dw%02d ", p, w)
		}
		b.WriteString("\n\n")
		if p%3 == 2 {
			for item := range 3 {
				fmt.Fprintf(&b, "- p%03dw%02d p%03dw%02d list item text for the row %d\n", p, 40+item*3, p, 41+item*3, item)
			}
			b.WriteString("\n")
		}
		if p%4 == 3 {
			fmt.Fprintf(&b, "```go\nfunc p%03dw50() {\n\treturn p%03dw51 + p%03dw52\n}\n```\n\n", p, p, p)
		}
		if p%5 == 4 {
			fmt.Fprintf(&b, "| p%03dw56 | p%03dw57 |\n| --- | --- |\n| cell | value |\n\n", p, p)
		}
	}
	return b.String()
}

// vprlAnchorSession returns before short items followed by the mixed message,
// which is the last item.
func vprlAnchorSession(before int) *session.Session {
	sess := session.New(session.WithID("vprl-anchor"))
	short := func(i int) {
		if i%2 == 0 {
			sess.AddMessage(session.UserMessage(fmt.Sprintf("question %d about the transcript relayout", i)))
			return
		}
		sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: fmt.Sprintf("## Answer %d\n\nShort reply.\n", i)}))
	}
	for i := range before {
		short(i)
	}
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: vprlMixedMessage()}))
	return sess
}

// vprlRowTokens returns the tokens shown on the top row of a frame.
func vprlRowTokens(frame string) []string {
	lines := strings.Split(ansi.Strip(frame), "\n")
	if len(lines) == 0 {
		return nil
	}
	return vprlTokenRe.FindAllString(lines[0], -1)
}

// vprlSlotRows returns the global rows [top, end) of view slot i. It uses only
// displayEnd, which both trees keep exact, so no slot outside i is measured.
func vprlSlotRows(m *model, i int) (top, end int) {
	if i > 0 {
		top = m.displayEnd(i - 1)
	}
	return top, m.displayEnd(i)
}

// vprlFindAnchor returns the first prose token, paragraph 4 to 11, on the top row
// of the mixed answer. The mixed answer is the last view of vprlAnchorSession, so
// the search walks only that slot's rows. It never iterates the estimated total,
// which is frozen when a range starts and grows as rows are measured. The slot
// bounds are re-read on every step, because measuring a row can settle the slot.
func vprlFindAnchor(t *testing.T, m *model) string {
	t.Helper()
	m.userHasScrolled = true
	slot := len(m.views) - 1
	top, _ := vprlSlotRows(m, slot)
	steps := 0
	for off := top; ; off++ {
		_, end := vprlSlotRows(m, slot)
		if off >= end {
			break
		}
		steps++
		require.LessOrEqual(t, steps, 4000, "the mixed answer must yield a prose anchor within its rows")
		m.setScrollOffset(off)
		frame := m.View()
		toks := vprlRowTokens(frame)
		if len(toks) == 0 {
			continue
		}
		para, _ := strconv.Atoi(toks[0][1:4])
		word, _ := strconv.Atoi(toks[0][5:])
		if para >= 4 && para <= 11 && word > 0 && word < 40 {
			m.setScrollOffset(off)
			_ = m.View()
			return toks[0]
		}
	}
	require.FailNow(t, "no prose anchor found inside the mixed message")
	return ""
}

// vprlTrimTokens keeps the tokens from the first from to the last to.
func vprlTrimTokens(seq []string, from, to string) []string {
	start, end := -1, -1
	for i, tok := range seq {
		if tok == from && start < 0 {
			start = i
		}
		if tok == to {
			end = i
		}
	}
	if start < 0 || end < start {
		return nil
	}
	return seq[start : end+1]
}

// TestViewportRelayoutAnchorContentLineAcrossResizes anchors a reader in the
// middle of a long mixed message and checks that the same content line stays
// on the top row through width shrink, grow and shrink, height changes, and a
// theme change. The check is on content tokens, not on message index or row.
func TestViewportRelayoutAnchorContentLineAcrossResizes(t *testing.T) {
	for _, n := range vprlCorrectnessSizes() {
		t.Run(fmt.Sprintf("history=%d", n), func(t *testing.T) {
			original := styles.CurrentTheme()
			t.Cleanup(func() { styles.ApplyTheme(original) })
			m := vprlLoad(t, 120, vprlAnchorSession(n/2))
			t.Cleanup(m.StopAnimations)
			anchor := vprlFindAnchor(t, m)
			t.Logf("anchor=%s at width=120 height=40", anchor)

			steps := []struct {
				name  string
				apply func()
			}{
				{"width_shrink_80", func() { m.SetSize(80, 40) }},
				{"width_grow_140", func() { m.SetSize(140, 40) }},
				{"width_shrink_90", func() { m.SetSize(90, 40) }},
				{"height_shrink_25", func() { m.SetSize(90, 25) }},
				{"height_grow_40", func() { m.SetSize(90, 40) }},
				{"theme_change", func() { styles.ApplyTheme(vprlTheme(original)) }},
			}
			for _, step := range steps {
				step.apply()
				frame := m.View()
				top := vprlRowTokens(frame)
				t.Logf("%s top_row_tokens=%v", step.name, top)
				assert.Contains(t, top, anchor, "%s: the anchored content line must stay on the top row", step.name)
			}
		})
	}
}

// TestViewportRelayoutSelectionEndpointsFollowContentAcrossWidth selects five
// rows from the anchored content line, changes width, and requires the copied
// text to cover the same content between the same two tokens.
func TestViewportRelayoutSelectionEndpointsFollowContentAcrossWidth(t *testing.T) {
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
			require.Equal(t, anchor, before[0], "the selection starts on the anchored line")
			end := before[len(before)-1]

			m.SetSize(90, 40)
			_ = m.View()
			assert.True(t, m.selection.active, "a width change keeps the selection active")
			after := vprlTokenSeq(m.extractSelectedText())
			assert.Equal(t,
				vprlTrimTokens(before, anchor, end),
				vprlTrimTokens(after, anchor, end),
				"copied content between the same endpoints must not change with width")
		})
	}
}

func vprlTokenSeq(text string) []string {
	return vprlTokenRe.FindAllString(text, -1)
}

// TestViewportRelayoutBottomFollowWhileStreaming keeps the reader at the bottom
// of a mixed message that is still streaming. The newest chunk must stay on the
// bottom rows through a mid-stream width change.
func TestViewportRelayoutBottomFollowWhileStreaming(t *testing.T) {
	for _, n := range vprlCorrectnessSizes() {
		t.Run(fmt.Sprintf("history=%d", n), func(t *testing.T) {
			m := vprlLoad(t, 120, vprlAnchorSession(n/2))
			t.Cleanup(m.StopAnimations)
			_ = m.scrollToBottom()
			_ = m.View()
			for i := range 40 {
				if i == 20 {
					m.SetSize(90, 40)
				}
				newest := fmt.Sprintf("stream%03d", i)
				_ = m.AppendToLastMessage("root", newest+" tail words for the streaming answer\n\n")
				frame := m.View()
				lines := strings.Split(ansi.Strip(frame), "\n")
				bottom := strings.Join(lines[max(0, len(lines)-3):], "\n")
				assert.Contains(t, bottom, newest, "chunk %d must stay on the bottom rows", i)
				assert.False(t, m.userHasScrolled, "bottom-follow must not mark the reader as scrolled at chunk %d", i)
			}
		})
	}
}

// TestViewportRelayoutAnchorShrinkGrowShrink is the explicit shrink, grow,
// shrink sequence. After each step the top row must still contain the anchored
// token, and that row must come from the same source paragraph. The token may
// sit at any column of the wrapped row, so this is containment of the source
// offset, not a first-column match.
func TestViewportRelayoutAnchorShrinkGrowShrink(t *testing.T) {
	for _, n := range vprlCorrectnessSizes() {
		t.Run(fmt.Sprintf("history=%d", n), func(t *testing.T) {
			m := vprlLoad(t, 120, vprlAnchorSession(n/2))
			t.Cleanup(m.StopAnimations)
			anchor := vprlFindAnchor(t, m)
			paragraph := anchor[:4]
			for _, width := range []int{80, 140, 90} {
				m.SetSize(width, 40)
				top := vprlRowTokens(m.View())
				assert.Contains(t, top, anchor, "width %d: the anchored token must stay on the top row", width)
				sameParagraph := false
				for _, tok := range top {
					sameParagraph = sameParagraph || strings.HasPrefix(tok, paragraph)
				}
				assert.True(t, sameParagraph, "width %d: the top row must come from source paragraph %s", width, paragraph)
			}
		})
	}
}
