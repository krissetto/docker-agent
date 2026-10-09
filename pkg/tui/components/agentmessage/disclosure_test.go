package agentmessage

import (
	"image/color"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/agentidentity"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }
func (c *clock) Tick(delay time.Duration, create func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg { c.now = c.now.Add(delay); return create(c.now) }
}

func advance(t *testing.T, ar *animation.Runtime, d *Disclosure) {
	t.Helper()
	cmd := ar.Continue()
	require.NotNil(t, cmd)
	tick, ok := ar.Accept(cmd().(animation.TickMsg))
	require.True(t, ok)
	d.Tick(tick)
	require.True(t, tick.Dirty())
}

func TestDisclosureShortReversibleTransitionAndCleanup(t *testing.T) {
	ar := animation.NewRuntimeWithScheduler(&clock{now: time.Unix(1, 0)})
	d := New(ar)
	body := strings.Repeat("body\n", 19) + "end"
	require.Equal(t, "header", d.Render("header", body, 6, false))
	d.Toggle()
	require.EqualValues(t, 1, ar.ActiveCount())
	require.Equal(t, "v", d.Chevron())
	require.Equal(t, "header", d.Render("header", body, 6, false), "first frame retains compact height")
	advance(t, ar, &d)
	partial := d.Render("header", body, 6, false)
	require.Positive(t, strings.Count(partial, "\n"))
	require.Less(t, strings.Count(partial, "\n"), 20)
	d.Toggle()
	require.Equal(t, partial, d.Render("header", body, 6, false), "reversal starts at current visible height")
	require.EqualValues(t, 1, ar.ActiveCount(), "reversal reuses the transition lease")
	for d.NeedsTick() {
		advance(t, ar, &d)
	}
	require.Equal(t, "header", d.Render("header", body, 6, false))
	require.Zero(t, ar.ActiveCount())
	d.Toggle()
	for d.NeedsTick() {
		advance(t, ar, &d)
	}
	require.Equal(t, "header\n"+body, ansi.Strip(d.Render("header", body, 6, false)))
	require.LessOrEqual(t, ar.Now(), 350*time.Millisecond)
	d.Toggle()
	d.Settle()
	require.Equal(t, "header", d.Render("header", body, 6, false))
	require.Zero(t, ar.ActiveCount())
	d.Settle()
	require.Zero(t, ar.ActiveCount())
}

func TestStableHeaderKeepsIdentityAndControlAcrossMotionAndWrap(t *testing.T) {
	ref := lifecycle.InputReference{Kind: lifecycle.InputReferenceNode, ID: "abcde-full-node", Name: "director 界", Agent: "director", DisplayID: "abcde"}
	ar := animation.NewRuntimeWithScheduler(&clock{now: time.Unix(1, 0)})
	d := New(ar)
	for _, width := range []int{80, 28, 8, 4, 80} {
		header := d.Header("✓ ", ref, " turn finished", width)
		hits := 0
		for y, line := range strings.Split(header, "\n") {
			require.LessOrEqual(t, ansi.StringWidth(line), width)
			for x := range width {
				if ToggleAt(header, y, x) {
					hits++
					require.Equal(t, ">", ansi.Strip(ansi.Cut(line, x, x+1)))
				}
			}
		}
		require.Equal(t, 1, hits)
		if width == 80 {
			require.Contains(t, ansi.Strip(header), "✓ director 界 (abcde) turn finished >")
		}
	}
	body := Body("unchanged <system_info>literal report</system_info>", 80, false, "")
	d.Toggle()
	expandedHeader := d.Header("✓ ", ref, " turn finished", 80)
	for d.NeedsTick() {
		advance(t, ar, &d)
		output := ansi.Strip(d.Render(expandedHeader, body, 80, false))
		require.Equal(t, 1, strings.Count(output, ref.Label()), "intermediate frames have one identity header")
		require.True(t, strings.HasPrefix(output, ansi.Strip(expandedHeader)), "header remains stable during reveal")
	}
	require.Contains(t, ansi.Strip(d.Render(expandedHeader, body, 80, false)), "unchanged <system_info>literal report</system_info>")
	d.Toggle()
	collapsedHeader := d.Header("✓ ", ref, " turn finished", 80)
	for d.NeedsTick() {
		advance(t, ar, &d)
		require.Equal(t, 1, strings.Count(ansi.Strip(d.Render(collapsedHeader, body, 80, false)), ref.Label()))
	}
}

func TestQuietCompactHeaderBeforeExpansionAndAfterCollapse(t *testing.T) {
	ref := lifecycle.InputReference{Kind: lifecycle.InputReferenceNode, ID: "74399-full", Name: "director", Agent: "director", DisplayID: "74399"}
	for _, status := range []string{"has replied", "sent a message", "· turn finished", "Messaged"} {
		ar := animation.NewRuntimeWithScheduler(&clock{now: time.Unix(1, 0)})
		d := New(ar)
		render := func(width int) string {
			return d.Header("✓ ", ref, " "+status, width)
		}
		compact := ansi.Strip(render(80))
		require.Equal(t, "✓ director (74399) "+status+" >", compact)
		for _, width := range []int{80, 28, 8, 4, 80} {
			header := render(width)
			require.NotContains(t, ansi.Strip(header), "━")
			hits := 0
			for y, line := range strings.Split(header, "\n") {
				require.LessOrEqual(t, ansi.StringWidth(line), width)
				for x := range width {
					if ToggleAt(header, y, x) {
						hits++
						require.Equal(t, ">", ansi.Strip(ansi.Cut(line, x, x+1)))
					}
				}
			}
			require.Equal(t, 1, hits)
		}
		body := Body("unchanged literal report", 80, false, "")
		d.Toggle()
		require.NotContains(t, ansi.Strip(render(80)), "━", "no card before first visible body frame")
		advance(t, ar, &d)
		expanded := ansi.Strip(render(80))
		require.Equal(t, strings.TrimSuffix(compact, ">")+"v", expanded)
		require.Contains(t, expanded, strings.TrimSuffix(compact, ">")+"v", "expansion preserves the icon, wording and order")
		require.Equal(t, 1, strings.Count(ansi.Strip(d.Render(render(80), body, 80, false)), ref.Label()))
		d.Toggle()
		require.Equal(t, compact, ansi.Strip(render(80)), "collapse retains the same header while rows remain visible")
		d.Toggle()
		for d.NeedsTick() {
			advance(t, ar, &d)
			require.Equal(t, 1, strings.Count(ansi.Strip(d.Render(render(80), body, 80, false)), ref.Label()))
			require.Equal(t, strings.TrimSuffix(compact, ">")+"v", ansi.Strip(render(80)), "every reveal frame stays aligned")
		}
		d.Toggle()
		for d.NeedsTick() {
			advance(t, ar, &d)
		}
		require.Equal(t, compact, ansi.Strip(render(80)))
		require.Zero(t, ar.ActiveCount())
		d.Toggle()
		d.Settle()
		d.Toggle()
		d.Settle()
		require.Equal(t, compact, ansi.Strip(render(80)), "hidden collapse settles to quiet header")
	}
}

func TestExpandedHeaderSharesSurfaceWithoutMovingWrappedCells(t *testing.T) {
	ref := lifecycle.InputReference{Kind: lifecycle.InputReferenceNode, ID: "abcde-full-node", Name: "Cafe\u0301 界 worker", Agent: "worker", DisplayID: "abcde"}
	for _, width := range []int{1, 2, 4, 8, 28, 80} {
		for _, selected := range []bool{false, true} {
			ar := animation.NewRuntimeWithScheduler(&clock{now: time.Unix(1, 0)})
			d := New(ar)
			body := Body(strings.Repeat("literal body\n", 20), width, selected, "")
			d.Toggle()
			for d.NeedsTick() {
				advance(t, ar, &d)
				header := d.Header(styles.ToolCompletedIcon.Render("✓")+" ", ref, " sent a message", width)
				output := strings.Split(d.Render(header, body, width, selected), "\n")
				for y, compact := range strings.Split(header, "\n") {
					line := output[y]
					expected := ansi.Strip(compact)
					if strings.HasPrefix(compact, " ") {
						edge := styles.UserMessageStyle.GetBorderStyle().Left
						if y == 0 {
							edge = styles.UserMessageStyle.GetBorderStyle().TopLeft
						}
						expected = edge + strings.TrimPrefix(expected, " ")
					}
					require.Equal(t, expected, ansi.Strip(ansi.Cut(line, 0, ansi.StringWidth(compact))), "no icon, name, neutral ID, status or control cell moves: width=%d row=%d", width, y)
					require.Equal(t, max(width, ansi.StringWidth(compact)), ansi.StringWidth(line), "surface preserves existing hard-wrap at width=%d row=%d header=%q painted=%q", width, y, ansi.Strip(compact), ansi.Strip(line))
					hover := agentidentity.HoverProgress(line, 0, ansi.StringWidth(line), ref, 1)
					require.Equal(t, ansi.Strip(line), ansi.Strip(hover))
					for x := range width {
						require.Equal(t, backgroundAt(line, x), backgroundAt(hover, x), "name-only highlight cannot interrupt the card surface")
						require.Equal(t, color.NRGBAModel.Convert(styles.UserMessageStyle.GetBackground()), backgroundAt(line, x), "header is inside USER surface: width=%d row=%d col=%d", width, y, x)
					}
				}
				if width == 80 {
					require.Contains(t, ansi.Strip(output[0]), " v ━", "header completes the card's top rail")
				}
			}
			d.Toggle()
			d.Settle()
			compact := d.Header(styles.ToolCompletedIcon.Render("✓")+" ", ref, " sent a message", width)
			require.Equal(t, compact, d.Render(compact, body, width, selected), "settled collapse is quiet and unpainted")
			require.Zero(t, ar.ActiveCount())
		}
	}
}

func backgroundAt(line string, col int) color.Color {
	parser := ansi.GetParser()
	defer ansi.PutParser(parser)
	var state byte
	var bg color.Color
	for x := 0; line != ""; {
		seq, width, n, next := ansi.DecodeSequence(line, state, parser)
		if n == 0 {
			break
		}
		if ansi.HasCsiPrefix(seq) && parser.Command() == 'm' {
			params := parser.Params()
			if len(params) == 0 {
				bg = nil
			}
			for i := 0; i < len(params); i++ {
				switch params[i].Param(0) {
				case 0, 49:
					bg = nil
				case 38, 48, 58:
					var c color.Color
					read := ansi.ReadStyleColor(params[i:], &c)
					if params[i].Param(0) == 48 {
						bg = c
					}
					i += max(0, read-1)
				}
			}
		}
		if width > 0 && x+width > col {
			if bg == nil {
				return nil
			}
			return color.NRGBAModel.Convert(bg)
		}
		x += width
		state, line = next, line[n:]
	}
	return nil
}
