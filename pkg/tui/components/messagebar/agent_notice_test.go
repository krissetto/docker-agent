package messagebar

import (
	"image/color"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestAgentNoticeIdentityAndSeverityColors(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	for _, ref := range []string{"default", "default-light", "custom"} {
		t.Run(ref, func(t *testing.T) {
			applyNoticeTheme(t, ref)
			for _, tc := range []struct {
				severity Severity
				category Category
				color    color.Color
			}{
				{Info, Background, styles.TextSecondary},
				{Info, Notice, styles.TextSecondary},
				{Info, Hint, styles.TextSecondary},
				{Warning, Notice, styles.Warning},
				{Error, Notice, styles.Error},
				{Success, Cancellation, styles.Success},
				{Info, Cancellation, styles.Info},
			} {
				m := New()
				m.SetSize(80, 1)
				m.SetMessage(Message{AgentName: "parcel-worker", AgentNodeID: "a1b2c", Text: "Done", Severity: tc.severity, Category: tc.category})
				view := m.View()
				assert.Equal(t, "parcel-worker (a1b2c) · Done", strings.TrimSpace(ansi.Strip(view)))
				assert.Equal(t, rgba(styles.AgentIdentityStyle("parcel-worker", false).GetForeground()), rgba(foregroundAt(t, view, "p")))
				assert.Equal(t, rgba(styles.TextSecondary), rgba(foregroundAt(t, view, "(")))
				assert.Equal(t, rgba(tc.color), rgba(foregroundAt(t, view, "D")))
			}
		})
	}
}

func TestAgentNoticeNarrowActionsAndSanitization(t *testing.T) {
	for _, width := range []int{0, 1, 2, 8, 20, 32, 80} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			m := New()
			m.SetSize(width, 1)
			msg := notice()
			msg.AgentName, msg.AgentNodeID = "\x1b[31m界 worker\x1b[0m\n", "a1b2c\t"
			m.SetMessage(msg)
			assert.Equal(t, "界 worker", m.message.AgentName)
			assert.Equal(t, "a1b2c", m.message.AgentNodeID)
			assert.Equal(t, width, ansi.StringWidth(m.View()))
			assert.NotContains(t, m.View(), "\n")
			assert.NotContains(t, m.View(), "\t")
			for _, bounds := range m.bounds {
				assert.Equal(t, " "+m.message.Actions[bounds.index].Label+" ", ansi.Cut(ansi.Strip(m.View()), bounds.start, bounds.end))
				cmd := m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: bounds.start})
				require.NotNil(t, cmd)
				assert.Equal(t, m.message.Actions[bounds.index].Command(), cmd())
			}
		})
	}
	m := New()
	m.SetSize(80, 1)
	m.SetMessage(Message{AgentName: "worker", Owner: "a1b2c-session-uuid", Text: "Done"})
	assert.Equal(t, "worker · Done", strings.TrimSpace(ansi.Strip(m.View())), "owner identity must never fabricate a display ID")
}

func TestAgentNoticeRosterRefreshKeepsViewPure(t *testing.T) {
	t.Cleanup(func() { styles.SetAgentOrder(nil) })
	styles.SetAgentOrder([]string{"other", "parcel-worker"})
	m := New()
	m.SetSize(80, 1)
	m.SetMessage(Message{AgentName: "parcel-worker", AgentNodeID: "a1b2c", Text: "Done"})
	cached, generation := m.cachedView, m.agentColors
	styles.SetAgentOrder([]string{"parcel-worker", "other"})
	view := m.View()
	assert.Equal(t, rgba(styles.AgentIdentityStyle("parcel-worker", false).GetForeground()), rgba(foregroundAt(t, view, "p")))
	assert.Equal(t, cached, m.cachedView)
	assert.Equal(t, generation, m.agentColors)
	m.Update(struct{}{})
	assert.Equal(t, view, m.cachedView)
	assert.Equal(t, styles.AgentColorGeneration(), m.agentColors)
	assert.Zero(t, testing.AllocsPerRun(20, func() { _ = m.View() }))
}

func TestAgentNoticeExpiryDedupAndCancellationPriority(t *testing.T) {
	now := time.Unix(10, 0)
	var aggregator Aggregator
	snapshot, ok := aggregator.Add(canonical("owner", "turn", CompletedTurn, 1), now)
	require.True(t, ok)
	m := New()
	m.SetSize(80, 1)
	info := Message{Owner: "owner", AgentName: "worker", AgentNodeID: "a1b2c", Text: snapshot.Text, Category: Background}
	token, _, ok := m.SetNotice(info, snapshot.Deadline)
	require.True(t, ok)
	_, ok = aggregator.Add(canonical("owner", "turn", CompletedTurn, 2), now.Add(time.Second))
	require.False(t, ok)
	current, ok := aggregator.Current("owner", now.Add(time.Second))
	require.True(t, ok)
	require.Equal(t, now.Add(3*time.Second), current.Deadline)
	m.Expire(token, now.Add(3*time.Second-time.Nanosecond))
	require.Contains(t, ansi.Strip(m.View()), "worker (a1b2c)")
	m.Expire(token, now.Add(3*time.Second))
	require.Empty(t, strings.TrimSpace(ansi.Strip(m.View())))
	cancel, _, ok := m.SetNotice(Message{Text: "Press Esc again", Severity: Warning, Category: Cancellation}, time.Time{})
	require.True(t, ok)
	for _, incoming := range []Message{info, {Text: "hint", Category: Hint}, {Text: "error", Severity: Error}} {
		_, _, accepted := m.SetNotice(incoming, now.Add(10*time.Second))
		require.False(t, accepted)
	}
	m.Expire(token, now.Add(10*time.Second))
	m.Expire(cancel, now.Add(10*time.Second))
	require.Equal(t, "Press Esc again", strings.TrimSpace(ansi.Strip(m.View())))
	require.Equal(t, rgba(styles.Warning), rgba(foregroundAt(t, m.View(), "P")))
}

func applyNoticeTheme(t *testing.T, ref string) {
	t.Helper()
	if ref == "custom" {
		theme := *styles.DefaultTheme()
		theme.Colors.Background = "#102030"
		theme.Colors.Accent = "#d4a0e8"
		theme.Colors.TextSecondary = "#90a0b0"
		theme.Colors.Info = "#00ccff"
		theme.Colors.Warning = "#eebb55"
		theme.Colors.Error = "#ff7788"
		styles.ApplyTheme(&theme)
		return
	}
	theme, err := styles.LoadTheme(ref)
	require.NoError(t, err)
	styles.ApplyTheme(theme)
}

// These authored ANSI expectations specify exact cells and colors, not an
// encoder's equivalent SGR ordering or resets. They are not captured output.
func TestAgentNoticeANSIVisualFixtures(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	for _, ref := range []string{"default", "default-light", "custom"} {
		t.Run(ref, func(t *testing.T) {
			applyNoticeTheme(t, ref)
			lines := strings.Split(strings.TrimSuffix(map[string]string{"default": agentNoticeDefaultANSI, "default-light": agentNoticeLightANSI, "custom": agentNoticeCustomANSI}[ref], "\n"), "\n")
			require.Len(t, lines, 5)
			for i, tc := range []struct {
				width   int
				message Message
			}{
				{64, Message{AgentName: "parcel-worker", AgentNodeID: "a1b2c", Text: "1 turn completed", Category: Background}},
				{64, Message{AgentName: "parcel-worker", Text: "1 subagent spawned", Category: Background}},
				{64, Message{AgentName: "parcel-worker", AgentNodeID: "a1b2c", Text: "Warning", Severity: Warning}},
				{64, Message{Text: "Press Esc again to cancel the response.", Severity: Warning, Category: Cancellation}},
				{18, Message{AgentName: "parcel-worker", AgentNodeID: "a1b2c", Text: "1 turn completed", Category: Background}},
			} {
				m := New()
				m.SetSize(tc.width, 1)
				m.SetMessage(tc.message)
				view := m.View()
				t.Logf("actual-messagebar theme=%s row=%d width=%d rawANSI=%q", ref, i, tc.width, view)
				assert.Equal(t, noticeCells(t, lines[i]), noticeCells(t, view), "row %d", i)
			}
		})
	}
}

type noticeCell struct {
	glyph string
	fg    color.RGBA64
	bg    color.RGBA64
}

func noticeCells(t *testing.T, text string) []noticeCell {
	t.Helper()
	parser := ansi.GetParser()
	defer ansi.PutParser(parser)
	var state byte
	var fg, bg color.RGBA64
	var cells []noticeCell
	for text != "" {
		seq, width, n, next := ansi.DecodeSequence(text, state, parser)
		require.Positive(t, n)
		if ansi.HasCsiPrefix(seq) && parser.Command() == 'm' {
			params := parser.Params()
			if len(params) == 0 {
				fg, bg = color.RGBA64{}, color.RGBA64{}
			}
			for i := 0; i < len(params); i++ {
				switch param := params[i].Param(0); param {
				case 0:
					fg, bg = color.RGBA64{}, color.RGBA64{}
				case 39:
					fg = color.RGBA64{}
				case 49:
					bg = color.RGBA64{}
				case 38, 48:
					var c color.Color
					if consumed := ansi.ReadStyleColor(params[i:], &c); consumed > 0 {
						if param == 38 {
							fg = rgba(c)
						} else {
							bg = rgba(c)
						}
						i += consumed - 1
					}
				}
			}
		}
		if width > 0 {
			cells = append(cells, noticeCell{glyph: seq, fg: fg, bg: bg})
		}
		state, text = next, text[n:]
	}
	return cells
}

// Built-in expectation colors come from HEAD theme Git objects; custom colors
// are the explicit test-only palette in applyNoticeTheme. No live assets read.
const (
	agentNoticeDefaultANSI = "\x1b[38;2;122;162;247mparcel-worker\x1b[0m\x1b[38;2;128;128;128m (a1b2c)\x1b[0m\x1b[38;2;128;128;128m · 1 turn completed\x1b[0m                        \x1b[0m\n" +
		"\x1b[38;2;122;162;247mparcel-worker\x1b[0m\x1b[38;2;128;128;128m · 1 subagent spawned\x1b[0m                              \x1b[0m\n" +
		"\x1b[38;2;122;162;247mparcel-worker\x1b[0m\x1b[38;2;128;128;128m (a1b2c)\x1b[0m\x1b[38;2;224;175;104m · Warning\x1b[0m                                 \x1b[0m\n" +
		"\x1b[38;2;224;175;104mPress Esc again to cancel the response.\x1b[0m                         \x1b[0m\n" +
		"\x1b[38;2;122;162;247mparcel-worker\x1b[0m\x1b[38;2;128;128;128m (a1…\x1b[0m\n"
	agentNoticeLightANSI = "\x1b[38;2;46;89;217mparcel-worker\x1b[0m\x1b[38;2;90;96;125m (a1b2c)\x1b[0m\x1b[38;2;90;96;125m · 1 turn completed\x1b[0m                        \x1b[0m\n" +
		"\x1b[38;2;46;89;217mparcel-worker\x1b[0m\x1b[38;2;90;96;125m · 1 subagent spawned\x1b[0m                              \x1b[0m\n" +
		"\x1b[38;2;46;89;217mparcel-worker\x1b[0m\x1b[38;2;90;96;125m (a1b2c)\x1b[0m\x1b[38;2;140;109;31m · Warning\x1b[0m                                 \x1b[0m\n" +
		"\x1b[38;2;140;109;31mPress Esc again to cancel the response.\x1b[0m                         \x1b[0m\n" +
		"\x1b[38;2;46;89;217mparcel-worker\x1b[0m\x1b[38;2;90;96;125m (a1…\x1b[0m\n"
	agentNoticeCustomANSI = "\x1b[38;2;212;160;232mparcel-worker\x1b[0m\x1b[38;2;144;160;176m (a1b2c)\x1b[0m\x1b[38;2;144;160;176m · 1 turn completed\x1b[0m                        \x1b[0m\n" +
		"\x1b[38;2;212;160;232mparcel-worker\x1b[0m\x1b[38;2;144;160;176m · 1 subagent spawned\x1b[0m                              \x1b[0m\n" +
		"\x1b[38;2;212;160;232mparcel-worker\x1b[0m\x1b[38;2;144;160;176m (a1b2c)\x1b[0m\x1b[38;2;238;187;85m · Warning\x1b[0m                                 \x1b[0m\n" +
		"\x1b[38;2;238;187;85mPress Esc again to cancel the response.\x1b[0m                         \x1b[0m\n" +
		"\x1b[38;2;212;160;232mparcel-worker\x1b[0m\x1b[38;2;144;160;176m (a1…\x1b[0m\n"
)
