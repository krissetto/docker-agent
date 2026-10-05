package agentmessage

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

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
