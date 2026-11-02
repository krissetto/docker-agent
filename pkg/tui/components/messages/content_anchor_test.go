package messages

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/markdown"
)

// csToken matches the unique tokens every fixture below is built from.
var csToken = regexp.MustCompile(`t\d{4}`)

// csRender renders markdown at width through the production renderer and
// returns the rows as the message view would hold them (ANSI kept).
func csRender(t *testing.T, src string, width int) []string {
	t.Helper()
	out, err := markdown.NewRenderer(width).Render(src)
	require.NoError(t, err)
	return strings.Split(strings.TrimRight(out, "\n"), "\n")
}

// csSourceOffsets maps each token in src to its byte offset. Every token must
// occur exactly once, so ground truth is unambiguous.
func csSourceOffsets(t *testing.T, src string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, loc := range csToken.FindAllStringIndex(src, -1) {
		tok := src[loc[0]:loc[1]]
		require.NotContains(t, out, tok, "fixture token %s must be unique", tok)
		out[tok] = loc[0]
	}
	return out
}

type csPos struct{ row, col int }

// csRenderedPositions maps each token to its rendered (row, display column)
// by scanning the plain rows. Tokens are read from the rendered output, not
// from the source, so the expectation does not depend on the mapper.
func csRenderedPositions(t *testing.T, rows []string) map[string]csPos {
	t.Helper()
	out := map[string]csPos{}
	for r, row := range rows {
		plain := ansi.Strip(row)
		for _, loc := range csToken.FindAllStringIndex(plain, -1) {
			tok := plain[loc[0]:loc[1]]
			require.NotContains(t, out, tok, "rendered token %s must be unique", tok)
			out[tok] = csPos{row: r, col: ansi.StringWidth(plain[:loc[0]])}
		}
	}
	return out
}

// csCellFrom returns the plain text of row starting at display column col.
func csCellFrom(row string, col int) string {
	plain := ansi.Strip(row)
	at := 0
	g := uniseg.NewGraphemes(plain)
	for g.Next() {
		if at >= col {
			start, _ := g.Positions()
			return plain[start:]
		}
		at += ansi.StringWidth(g.Str())
	}
	return ""
}

// csTokensByOffset names the token that starts at each source offset.
func csTokensByOffset(offsets map[string]int) map[int]string {
	out := map[int]string{}
	for tok, off := range offsets {
		out[off] = tok
	}
	return out
}

// csFixture is real markdown: headings, prose with emphasis, code and a link,
// bullets, a fenced code block and a table. Every token is unique.
func csFixture() string {
	var b strings.Builder
	n := 0
	word := func() string { n++; return fmt.Sprintf("t%04d", n) }
	fmt.Fprintf(&b, "## %s %s\n\n", word(), word())
	for p := range 6 {
		for range 30 {
			b.WriteString(word())
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "**%s** and `%s` plus [%s](https://example.com/p%d) end.\n\n", word(), word(), word(), p)
		fmt.Fprintf(&b, "- %s item %s\n- %s item %s\n\n", word(), word(), word(), word())
		fmt.Fprintf(&b, "```go\nfunc %s() int {\n\treturn %s\n}\n```\n\n", word(), word())
		fmt.Fprintf(&b, "| %s | %s |\n| --- | --- |\n| %s | %s |\n\n", word(), word(), word(), word())
	}
	return b.String()
}

func TestContentAnchorPlainWrapIsExact(t *testing.T) {
	t.Parallel()
	src := "alpha beta gamma\ndelta epsilon"
	rows := []string{"alpha beta", "gamma delta", "epsilon"}
	m := BuildSourceMap(src, rows)

	require.True(t, m.Mapped())
	require.Equal(t, 3, m.Rows())
	start, end, ok := m.RowSpan(1)
	require.True(t, ok)
	assert.Equal(t, strings.Index(src, "gamma"), start)
	assert.Equal(t, strings.Index(src, "delta")+len("delta"), end)
	for _, tc := range []struct {
		word     string
		row, col int
	}{{"alpha", 0, 0}, {"beta", 0, 6}, {"gamma", 1, 0}, {"delta", 1, 6}, {"epsilon", 2, 0}} {
		row, col, ok := m.ColForSource(strings.Index(src, tc.word))
		require.True(t, ok, tc.word)
		assert.Equal(t, csPos{tc.row, tc.col}, csPos{row, col}, tc.word)
	}
	assert.Equal(t, 2, m.SourceLine(strings.Index(src, "delta")))
	assert.Equal(t, 1, m.SourceLine(0))
}

func TestContentAnchorMarkdownAcrossShrinkGrowShrink(t *testing.T) {
	t.Parallel()
	src := csFixture()
	offsets := csSourceOffsets(t, src)
	byOffset := csTokensByOffset(offsets)
	// Each step is the next width in a shrink, grow, shrink cycle.
	widths := []int{120, 60, 100, 60, 120}

	rows := csRender(t, src, widths[0])
	m := BuildSourceMap(src, rows)
	require.True(t, m.Mapped())
	pos := csRenderedPositions(t, rows)
	anchor := offsets["t0100"]
	require.Contains(t, pos, "t0100")

	for _, width := range widths {
		rows = csRender(t, src, width)
		m = BuildSourceMap(src, rows)
		require.True(t, m.Mapped(), "width %d must map", width)

		// Every rendered token must map to its own source position.
		pos = csRenderedPositions(t, rows)
		for tok, p := range pos {
			row, col, ok := m.ColForSource(offsets[tok])
			require.True(t, ok, "token %s at width %d", tok, width)
			assert.Equal(t, csPos{row, col}, p, "token %s at width %d", tok, width)
		}

		// The anchored content stays on the same content line through every step.
		row, col, ok := m.ColForSource(anchor)
		require.True(t, ok)
		assert.True(t, strings.HasPrefix(csCellFrom(rows[row], col), byOffset[anchor]),
			"width %d: anchor cell %q must start with its token", width, csCellFrom(rows[row], col))
		// A top row anchors to the first content on it, which is at or before the anchored token.
		top, ok := m.TopAnchor(row)
		require.True(t, ok)
		assert.LessOrEqual(t, top, anchor, "width %d: top row starts at or before the anchor", width)
		topRow, ok := m.RowForSource(top)
		require.True(t, ok)
		assert.Equal(t, row, topRow, "width %d: the top anchor maps back to the same row", width)
	}
}

func TestContentAnchorSelectionEndpointsFollowContent(t *testing.T) {
	t.Parallel()
	src := csFixture()
	offsets := csSourceOffsets(t, src)
	byOffset := csTokensByOffset(offsets)

	rows := csRender(t, src, 120)
	m := BuildSourceMap(src, rows)
	// A selection from the start of one token to the start of a token a few
	// words later, drawn at width 120.
	startTok, endTok := "t0040", "t0045"
	startRow, startCol, ok := m.ColForSource(offsets[startTok])
	require.True(t, ok)
	endRow, endCol, ok := m.ColForSource(offsets[endTok])
	require.True(t, ok)
	require.GreaterOrEqual(t, endRow, startRow)
	selStart, ok := m.SourceAt(startRow, startCol)
	require.True(t, ok)
	selEnd, ok := m.SourceAt(endRow, endCol)
	require.True(t, ok)
	require.Equal(t, offsets[startTok], selStart)
	require.Equal(t, offsets[endTok], selEnd)

	for _, width := range []int{60, 100, 60, 120} {
		rows = csRender(t, src, width)
		m = BuildSourceMap(src, rows)
		r0, c0, ok := m.ColForSource(selStart)
		require.True(t, ok)
		r1, c1, ok := m.ColForSource(selEnd)
		require.True(t, ok)
		assert.True(t, strings.HasPrefix(csCellFrom(rows[r0], c0), byOffset[selStart]), "width %d: start endpoint", width)
		assert.True(t, strings.HasPrefix(csCellFrom(rows[r1], c1), byOffset[selEnd]), "width %d: end endpoint", width)
		back0, ok := m.SourceAt(r0, c0)
		require.True(t, ok)
		back1, ok := m.SourceAt(r1, c1)
		require.True(t, ok)
		assert.Equal(t, selStart, back0, "width %d", width)
		assert.Equal(t, selEnd, back1, "width %d", width)
	}
}

func TestContentAnchorRepeatsKeepOccurrenceOrder(t *testing.T) {
	t.Parallel()
	// "rep" occurs many times and nowhere else as a substring, so occurrences
	// in the rendered rows and in the source pair up in order.
	src := "rep rep rep. rep, rep! rep? (rep) rep-rep rep\n\nrep rep rep rep rep rep rep rep rep rep"
	var srcOff []int
	for i := 0; ; {
		j := strings.Index(src[i:], "rep")
		if j < 0 {
			break
		}
		srcOff = append(srcOff, i+j)
		i += j + len("rep")
	}
	for _, width := range []int{80, 12, 30} {
		rows := csRender(t, src, width)
		var got []csPos
		for r, row := range rows {
			plain := ansi.Strip(row)
			for i := 0; ; {
				j := strings.Index(plain[i:], "rep")
				if j < 0 {
					break
				}
				got = append(got, csPos{r, ansi.StringWidth(plain[:i+j])})
				i += j + len("rep")
			}
		}
		require.Len(t, got, len(srcOff), "width %d: every repeat rendered", width)
		m := BuildSourceMap(src, rows)
		for k, off := range srcOff {
			row, col, ok := m.ColForSource(off)
			require.True(t, ok)
			assert.Equal(t, csPos{row, col}, got[k], "width %d occurrence %d", width, k)
		}
	}
}

func TestContentAnchorUnicodeUsesDisplayColumns(t *testing.T) {
	t.Parallel()
	src := "naïve café 日本語 🐳 👩‍💻 e\u0301x t0100 and 日本語 t0101 done"
	offsets := map[string]int{"t0100": strings.Index(src, "t0100"), "t0101": strings.Index(src, "t0101")}
	for _, width := range []int{80, 16, 9} {
		rows := csRender(t, src, width)
		pos := csRenderedPositions(t, rows)
		require.Contains(t, pos, "t0100")
		require.Contains(t, pos, "t0101")
		m := BuildSourceMap(src, rows)
		for tok, off := range offsets {
			row, col, ok := m.ColForSource(off)
			require.True(t, ok)
			assert.Equal(t, csPos{row, col}, pos[tok], "width %d token %s", width, tok)
			assert.True(t, strings.HasPrefix(csCellFrom(rows[row], col), tok), "width %d token %s", width, tok)
		}
	}
}

func TestContentAnchorHardBrokenWordStaysMapped(t *testing.T) {
	t.Parallel()
	long := "t9000" + strings.Repeat("x", 150) + "t9001"
	src := "pre " + long + " post t9002"
	const width = 40
	rows := csRender(t, src, width)
	pos := csRenderedPositions(t, rows)
	// The long word is split across rows, so its first token is the only one
	// rendered whole. The rest of the run is x, which has no unique token.
	require.Contains(t, pos, "t9000")
	require.Contains(t, pos, "t9002")
	m := BuildSourceMap(src, rows)

	row, col, ok := m.ColForSource(strings.Index(src, "t9000"))
	require.True(t, ok)
	assert.Equal(t, csPos{row, col}, pos["t9000"])

	// The fragments of the long word must each map back inside it.
	longStart := strings.Index(src, "t9000")
	longEnd := longStart + len(long)
	fragments := 0
	for r := range rows {
		if off, ok := m.SourceAt(r, 0); ok && off >= longStart && off < longEnd {
			fragments++
		}
	}
	assert.GreaterOrEqual(t, fragments, 3, "the long word must span several rows")
	row, col, ok = m.ColForSource(strings.Index(src, "t9002"))
	require.True(t, ok)
	assert.Equal(t, csPos{row, col}, pos["t9002"])
}

func TestContentAnchorDroppedRunAndRepeatedWords(t *testing.T) {
	t.Parallel()
	// Rendered rows drop the URL and add a bullet, as the renderer can. The
	// repeated "dup" words must still pair in order after the drop.
	src := "see https://a.example/path/z9 then dup and dup again t0007"
	rows := []string{"• see then dup and", "dup again t0007"}
	m := BuildSourceMap(src, rows)
	require.True(t, m.Mapped())

	first := strings.Index(src, "dup")
	second := strings.Index(src[first+1:], "dup") + first + 1
	row, col, ok := m.ColForSource(first)
	require.True(t, ok)
	assert.Equal(t, csPos{0, 11}, csPos{row, col})
	row, col, ok = m.ColForSource(second)
	require.True(t, ok)
	assert.Equal(t, csPos{1, 0}, csPos{row, col})
	row, col, ok = m.ColForSource(strings.Index(src, "t0007"))
	require.True(t, ok)
	assert.Equal(t, csPos{1, 10}, csPos{row, col})
}

func TestContentAnchorOpaqueRowsFallBackProportionally(t *testing.T) {
	t.Parallel()
	// An image or widget has no text in the rows, so nothing maps. Callers
	// must then use ProportionalRow, which keeps the same fraction of the content.
	cases := []struct {
		name, src string
		rows      []string
	}{
		{"image", "![cover art](https://example.com/cover.png)", []string{"▓▓▓▓▓▓▓▓", "▓▓▓▓▓▓▓▓", "▓▓▓▓▓▓▓▓"}},
		{"empty source", "", []string{"row one", "row two"}},
		{"unrelated rows", "hello world", []string{"▓▓▓▓", "░░░░"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := BuildSourceMap(tc.src, tc.rows)
			assert.False(t, m.Mapped())
		})
	}

	// Proportional mapping is monotone, keeps the top, and stays in bounds.
	assert.Equal(t, 0, ProportionalRow(0, 10, 25))
	assert.Equal(t, 13, ProportionalRow(5, 10, 25))
	assert.Equal(t, 24, ProportionalRow(10, 10, 25))
	assert.Equal(t, 0, ProportionalRow(3, 1, 25))
	assert.Equal(t, 0, ProportionalRow(3, 10, 0))
	prev := 0
	for row := range 100 {
		got := ProportionalRow(row, 100, 37)
		assert.GreaterOrEqual(t, got, prev)
		assert.Less(t, got, 37)
		prev = got
	}
}

func TestContentAnchorHeavyAnswerMapsEveryWidth(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../markdown/testdata/streaming_benchmark.md")
	require.NoError(t, err)
	src := string(data)
	for _, width := range []int{120, 60, 100, 60} {
		rows := csRender(t, src, width)
		m := BuildSourceMap(src, rows)
		require.True(t, m.Mapped(), "heavy answer must map at width %d", width)
		lastRow, lastStart := -1, -1
		for r := range rows {
			start, _, ok := m.RowSpan(r)
			if !ok {
				continue
			}
			assert.GreaterOrEqual(t, start, lastStart, "row spans are in source order (row %d)", r)
			lastStart = start
			lastRow = r
			row, _, ok := m.ColForSource(start)
			require.True(t, ok)
			assert.Equal(t, r, row, "a row's first content maps back to that row (width %d)", width)
		}
		assert.GreaterOrEqual(t, lastRow, len(rows)/2, "matched content reaches the end")
	}
}

// csLongDroppedFixture puts a link target far longer than the local resync
// window before each of several repeated paragraphs. The renderer drops the
// target, so the mapper must jump over it and keep mapping what follows.
func csLongDroppedFixture() string {
	var b strings.Builder
	n := 0
	word := func() string { n++; return fmt.Sprintf("t%04d", n) }
	target := strings.Repeat("segment/", 40) // about 320 source clusters
	for p := range 3 {
		fmt.Fprintf(&b, "Shared context alpha beta gamma delta %s closes paragraph %d.\n\n", word(), p)
		fmt.Fprintf(&b, "See [%s](https://example.com/%s%d) for details.\n\n", word(), target, p)
	}
	fmt.Fprintf(&b, "Anchor paragraph %s shared context alpha beta gamma delta %s.\n\n", word(), word())
	fmt.Fprintf(&b, "```go\nfunc %s() {}\n```\n\n", word())
	fmt.Fprintf(&b, "Final prose %s after the code block with more context alpha %s.\n", word(), word())
	return b.String()
}

// csOccurrences returns the byte offsets of every occurrence of word in src.
func csOccurrences(src, word string) []int {
	var out []int
	for i := 0; ; {
		j := strings.Index(src[i:], word)
		if j < 0 {
			return out
		}
		out = append(out, i+j)
		i += j + len(word)
	}
}

func TestContentAnchorMapsAcrossLongDroppedRunsAndRepeatedContext(t *testing.T) {
	t.Parallel()
	src := csLongDroppedFixture()
	offsets := csSourceOffsets(t, src)
	require.Greater(t, len(offsets), 10)
	for _, width := range []int{120, 50} {
		rows := csRender(t, src, width)
		for _, row := range rows {
			require.NotContains(t, ansi.Strip(row), "segment/", "the link target must be dropped by the renderer")
		}
		m := BuildSourceMap(src, rows)
		require.True(t, m.Mapped(), "width %d: normal text must stay mapped", width)

		pos := csRenderedPositions(t, rows)
		for tok, off := range offsets {
			require.Contains(t, pos, tok, "width %d: token %s must render", width, tok)
			row, col, ok := m.ColForSource(off)
			require.True(t, ok, "width %d: token %s must map", width, tok)
			assert.Equal(t, csPos{row, col}, pos[tok], "width %d: token %s after a dropped run", width, tok)
		}

		// Repeated context: the k-th rendered "alpha" pairs with the k-th source "alpha".
		var got []csPos
		for r, row := range rows {
			plain := ansi.Strip(row)
			for i := 0; ; {
				j := strings.Index(plain[i:], "alpha")
				if j < 0 {
					break
				}
				got = append(got, csPos{r, ansi.StringWidth(plain[:i+j])})
				i += j + len("alpha")
			}
		}
		want := csOccurrences(src, "alpha")
		require.Len(t, got, len(want), "width %d: every alpha renders", width)
		for k, off := range want {
			row, col, ok := m.ColForSource(off)
			require.True(t, ok)
			assert.Equal(t, csPos{row, col}, got[k], "width %d: alpha occurrence %d", width, k)
		}
	}
}

func TestContentAnchorStatusClassesFromTextOnly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		src    string
		rows   []string
		status MapStatus
		mapped bool
	}{
		{"blank source and rows", "", []string{"", ""}, MapBlank, false},
		{"text rendered to nothing", "hello world t0001", []string{"", ""}, MapUnproven, false},
		{"image glyphs without source match", "![cover](https://example.com/c.png)", []string{"▓▓▓▓▓▓▓▓"}, MapUnproven, false},
		{"punctuation-only code is content", "`{}[]();`", []string{"{}[]();"}, MapExact, true},
		{"prose with exact words", "alpha beta gamma", []string{"alpha beta", "gamma"}, MapExact, true},
		{"prose with a changed word", "alpha beta gamma", []string{"alpha xxxx gamma"}, MapPartial, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := BuildSourceMap(tc.src, tc.rows)
			assert.Equal(t, tc.status, m.Status())
			assert.Equal(t, tc.mapped, m.Mapped())
		})
	}
	assert.False(t, BuildSourceMap("", nil).HasContentSource())
	assert.True(t, BuildSourceMap("`{}`", nil).HasContentSource(), "punctuation-only source is content")
}

func TestContentAnchorTabsInCodeMapExactly(t *testing.T) {
	t.Parallel()
	src := "Intro t0001 text.\n\n```go\nfunc t0002() {\n\tif t0003 {\n\t\treturn t0004\n\t}\n}\n```\n\nAfter t0005.\n"
	offsets := csSourceOffsets(t, src)
	for _, width := range []int{120, 40} {
		rows := csRender(t, src, width)
		for _, row := range rows {
			assert.NotContains(t, ansi.Strip(row), "\t", "tabs must be expanded by the renderer")
		}
		m := BuildSourceMap(src, rows)
		// The code block's language label and copy affordance are decoration with
		// no source counterpart, so partial is the correct status here.
		assert.Contains(t, []MapStatus{MapExact, MapPartial}, m.Status(), "width %d", width)
		assert.True(t, m.Mapped(), "width %d", width)
		pos := csRenderedPositions(t, rows)
		for tok, off := range offsets {
			require.Contains(t, pos, tok)
			row, col, ok := m.ColForSource(off)
			require.True(t, ok)
			assert.Equal(t, csPos{row, col}, pos[tok], "width %d token %s", width, tok)
		}
	}
}

func TestContentAnchorIdenticalParagraphsKeepTheirOwnContext(t *testing.T) {
	t.Parallel()
	// The same body text appears under two headings. Each copy must anchor to its
	// own rendered copy, not to the first one.
	body := "the same paragraph body repeats here with words alpha beta gamma"
	src := "## First context\n\n" + body + ".\n\n## Second context\n\n" + body + ".\n"
	for _, width := range []int{120, 30} {
		rows := csRender(t, src, width)
		m := BuildSourceMap(src, rows)
		assert.Equal(t, MapExact, m.Status(), "width %d", width)

		var renderedCopies []csPos
		for r, row := range rows {
			plain := ansi.Strip(row)
			if i := strings.Index(plain, "the same"); i >= 0 {
				renderedCopies = append(renderedCopies, csPos{r, ansi.StringWidth(plain[:i])})
			}
		}
		require.Len(t, renderedCopies, 2, "width %d: both copies render", width)
		sourceCopies := csOccurrences(src, "the same")
		require.Len(t, sourceCopies, 2)
		for k, off := range sourceCopies {
			row, col, ok := m.ColForSource(off)
			require.True(t, ok)
			assert.Equal(t, csPos{row, col}, renderedCopies[k], "width %d: copy %d", width, k)
		}
		// The second heading's anchor must land in the second copy.
		secondHeading := strings.Index(src, "## Second context")
		row, _, ok := m.ColForSource(secondHeading)
		require.True(t, ok)
		assert.Greater(t, row, renderedCopies[0].row, "width %d: second context is below the first copy", width)
	}
}

func TestContentAnchorRendererRewritesKeepTokensAligned(t *testing.T) {
	t.Parallel()
	// Escapes, entities, dashes, ellipses and inline HTML-like text change the
	// spelling of some source clusters. Every unique token around them must map
	// exactly, and the mapping must recover after each rewrite.
	src := "t0100 \\*literal\\* stars and &amp; ampersand t0101.\n\n" +
		"Dash -- and ellipsis ... then t0102 quote \"straight\" done.\n\n" +
		"Inline <span>html</span> and `code` t0103 and ~~strike~~ t0104.\n"
	offsets := csSourceOffsets(t, src)
	for _, width := range []int{120, 24} {
		rows := csRender(t, src, width)
		pos := csRenderedPositions(t, rows)
		m := BuildSourceMap(src, rows)
		for tok, off := range offsets {
			if _, ok := pos[tok]; !ok {
				continue // the renderer dropped this token; the test below checks which
			}
			row, col, ok := m.ColForSource(off)
			require.True(t, ok)
			assert.Equal(t, csPos{row, col}, pos[tok], "width %d token %s", width, tok)
		}
		for _, tok := range []string{"t0100", "t0101", "t0102", "t0103", "t0104"} {
			assert.Contains(t, pos, tok, "width %d: rewrites must not swallow %s", width, tok)
		}
	}
}
