package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/rendering/retained"
)

// Preserve the previous assembly-before-retention implementation as an oracle
// and bounded allocation benchmark, independent of the new early-hit path.
type priorPaneRows struct {
	slots []retained.Slot[string]
}

func (c *priorPaneRows) render(rows [][]paneRowSpan, width int) string {
	if len(c.slots) != len(rows) {
		c.slots = make([]retained.Slot[string], len(rows))
	}
	lines := make([]string, len(rows))
	for y, spans := range rows {
		lines[y] = c.slots[y].Render(assemblePaneRow(spans, width), trimPaneDefaultPadding)
	}
	return strings.Join(lines, "\n")
}

func clonePaneRows(rows [][]paneRowSpan) [][]paneRowSpan {
	out := make([][]paneRowSpan, len(rows))
	for y := range rows {
		out[y] = slices.Clone(rows[y])
	}
	return out
}

func TestPaneRowsRetainCompleteOwnedInput(t *testing.T) {
	var cache paneRenderCache
	rows := [][]paneRowSpan{{preparedPaneSpan("static", 12)}, {preparedPaneSpan("spinner ⠋", 12)}}
	check := func(width int) {
		t.Helper()
		var prior priorPaneRows
		want := prior.render(clonePaneRows(rows), width)
		require.Equal(t, want, cache.render(rows, width))
	}
	check(30)
	check(30)
	require.EqualValues(t, 1, cache.rows[0].builds)
	require.EqualValues(t, 1, cache.rows[1].builds)

	// Incoming metadata belongs to the caller and may change in place.
	rows[1][0] = preparedPaneSpan("spinner ⠙", 12)
	check(30)
	require.EqualValues(t, 1, cache.rows[0].builds, "unrelated row is not assembled")
	require.EqualValues(t, 2, cache.rows[1].builds)
	for _, change := range []struct {
		name string
		run  func(*paneRowSpan)
	}{
		{"position", func(s *paneRowSpan) { s.x = 3 }},
		{"span width", func(s *paneRowSpan) { s.width = 14 }},
		{"content", func(s *paneRowSpan) { s.content = "new authoritative bytes" }},
		{"serialized paint", func(s *paneRowSpan) { s.serialized = "\x1b[31mred\x1b[m" }},
		{"preparation", func(s *paneRowSpan) { s.prepared = false }},
	} {
		t.Run(change.name, func(t *testing.T) {
			before := cache.rows[1].builds
			change.run(&rows[1][0])
			check(30)
			require.Equal(t, before+1, cache.rows[1].builds)
		})
	}
	before := cache.rows[0].builds
	check(31)
	require.Equal(t, before+1, cache.rows[0].builds, "viewport width is part of the input")

	rows[0] = append(rows[0], paneRowSpan{x: 18, width: 4, content: "tail"})
	check(31)
	rows[0] = rows[0][:1]
	check(31)
	require.Len(t, cache.rows[0].spans, 1)
	require.Zero(t, cache.rows[0].spans[:cap(cache.rows[0].spans)][1], "old strings are released on shrink")
	rows = rows[:1]
	check(31)
	require.Len(t, cache.rows, 1)
	rows = append(rows, []paneRowSpan{{width: 12, content: "replacement"}})
	check(31)
	require.Len(t, cache.rows, 2)
	rows = nil
	check(31)
	require.Empty(t, cache.rows)
}

func TestPaneRowsEarlyRetentionMatchesPriorTerminalBytes(t *testing.T) {
	var cache paneRenderCache
	for _, raw := range []string{
		"", "λ界 👩‍💻 é", "\x1b[31mred\x1b[m  ",
		"\x1b]8;;https://example.test\x1b\\link\x1b]8;;\x1b\\",
		"\x1b[48;2;10;20;30mcolored padding   \x1b[m",
		"\x1b_cagent-image;123;4;2;0\x1b\\    ",
		"\x1b_Ga=T,c=4,r=2;YWJj\x1b\\    ",
	} {
		for _, width := range []int{0, 1, 12, 40} {
			rows := [][]paneRowSpan{{{x: 18, width: 4, content: "tail"}, preparedPaneSpan(raw, 12)}, nil}
			var prior priorPaneRows
			want := prior.render(clonePaneRows(rows), width)
			require.Equal(t, want, cache.render(clonePaneRows(rows), width))
			before := cache.rows[0].builds
			require.Equal(t, want, cache.render(clonePaneRows(rows), width))
			require.Equal(t, before, cache.rows[0].builds, "unsorted source order is retained exactly")
		}
	}
}

func TestPaneRowsReusedUnsortedInputStabilizes(t *testing.T) {
	var cache paneRenderCache
	rows := [][]paneRowSpan{{{x: 8, width: 4, content: "tail"}, {width: 4, content: "head"}}}
	var prior priorPaneRows
	want := prior.render(clonePaneRows(rows), 12)
	require.Equal(t, want, cache.render(rows, 12))
	// Assembly historically sorts the caller's input. Unlike fresh composition
	// slices, reusing that sorted slice changes the key once, then stabilizes.
	require.Equal(t, 0, rows[0][0].x)
	require.Equal(t, want, cache.render(rows, 12))
	require.EqualValues(t, 2, cache.rows[0].builds)
	for range 3 {
		require.Equal(t, want, cache.render(rows, 12))
	}
	require.EqualValues(t, 2, cache.rows[0].builds)
}

func paneRowAllocationFixture() [][]paneRowSpan {
	rows := make([][]paneRowSpan, 48)
	for y := range rows {
		left := preparedPaneSpan("\x1b[31mstatic λ界\x1b[m", 60)
		right := preparedPaneSpan("\x1b]8;;https://example.test\x1b\\link\x1b]8;;\x1b\\", 59)
		right.x = 61
		rows[y] = []paneRowSpan{left, {x: 60, width: 1, content: "│"}, right}
	}
	return rows
}

func TestPaneRowsEarlyHitAllocations(t *testing.T) {
	rows := paneRowAllocationFixture()
	var cache paneRenderCache
	var prior priorPaneRows
	require.Equal(t, prior.render(rows, 120), cache.render(rows, 120))
	currentAllocs := testing.AllocsPerRun(100, func() { cache.render(rows, 120) })
	priorAllocs := testing.AllocsPerRun(100, func() { prior.render(rows, 120) })
	require.Less(t, currentAllocs, priorAllocs)
	require.LessOrEqual(t, currentAllocs, float64(2), "only line table and joined frame allocate on a row hit")
}

func BenchmarkPaneRowsRetained(b *testing.B) {
	rows := paneRowAllocationFixture()
	for _, prior := range []bool{true, false} {
		name := "early"
		var cache paneRenderCache
		render := cache.render
		if prior {
			name = "prior"
			var old priorPaneRows
			render = old.render
		}
		b.Run(name, func(b *testing.B) {
			render(rows, 120)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				render(rows, 120)
			}
		})
	}
}
