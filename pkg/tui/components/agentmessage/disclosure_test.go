package agentmessage

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/tui/animation"
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
	require.Equal(t, "header", d.Render("header", body))
	d.Toggle()
	require.EqualValues(t, 1, ar.ActiveCount())
	require.Equal(t, "v", d.Chevron())
	require.Equal(t, "header", d.Render("header", body), "first frame retains compact height")
	advance(t, ar, &d)
	partial := d.Render("header", body)
	require.Positive(t, strings.Count(partial, "\n"))
	require.Less(t, strings.Count(partial, "\n"), 20)
	d.Toggle()
	require.Equal(t, partial, d.Render("header", body), "reversal starts at current visible height")
	require.EqualValues(t, 1, ar.ActiveCount(), "reversal reuses the transition lease")
	for d.NeedsTick() {
		advance(t, ar, &d)
	}
	require.Equal(t, "header", d.Render("header", body))
	require.Zero(t, ar.ActiveCount())
	d.Toggle()
	for d.NeedsTick() {
		advance(t, ar, &d)
	}
	require.Equal(t, "header\n"+body, d.Render("header", body))
	require.LessOrEqual(t, ar.Now(), 350*time.Millisecond)
	d.Toggle()
	d.Settle()
	require.Equal(t, "header", d.Render("header", body))
	require.Zero(t, ar.ActiveCount())
	d.Settle()
	require.Zero(t, ar.ActiveCount())
}

func TestSingleBorderHeaderKeepsIdentityAndControlAcrossMotionAndWrap(t *testing.T) {
	ref := lifecycle.InputReference{Kind: lifecycle.InputReferenceNode, ID: "abcde-full-node", Name: "director 界", Agent: "director", DisplayID: "abcde"}
	ar := animation.NewRuntimeWithScheduler(&clock{now: time.Unix(1, 0)})
	d := New(ar)
	for _, width := range []int{80, 28, 8, 4, 80} {
		header := Header(ref, d.Chevron(), "✓ turn finished", width)
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
		if width >= 28 {
			require.Contains(t, ansi.Strip(header), "(abcde) >")
		}
	}
	body := Body("unchanged <system_info>literal report</system_info>", 80, false, "")
	d.Toggle()
	expandedHeader := Header(ref, d.Chevron(), "✓ turn finished", 80)
	for d.NeedsTick() {
		advance(t, ar, &d)
		output := ansi.Strip(d.Render(expandedHeader, body))
		require.Equal(t, 1, strings.Count(output, ref.Label()), "intermediate frames have one identity header")
		require.True(t, strings.HasPrefix(output, ansi.Strip(expandedHeader)), "header remains stable during reveal")
	}
	require.Contains(t, ansi.Strip(d.Render(expandedHeader, body)), "unchanged <system_info>literal report</system_info>")
	d.Toggle()
	collapsedHeader := Header(ref, d.Chevron(), "✓ turn finished", 80)
	for d.NeedsTick() {
		advance(t, ar, &d)
		require.Equal(t, 1, strings.Count(ansi.Strip(d.Render(collapsedHeader, body)), ref.Label()))
	}
}
