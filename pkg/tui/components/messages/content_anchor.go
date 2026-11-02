package messages

import (
	"sort"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"
)

// Source mapping for rendered message rows.
//
// The markdown renderer produces ANSI text and keeps no record of where each
// output cell came from. BuildSourceMap recovers that record by aligning the
// rendered rows against the message's raw source text. Both sides are reduced
// to a stream of non-space grapheme clusters. The renderer may drop markup
// (emphasis markers, fences, link targets), add decoration (bullets, table
// rules, labels), collapse whitespace, and break words at the width. The
// alignment absorbs all of these as insertions or deletions, so it never needs
// to know which kind of text it is looking at.
//
// Matching is by exact cluster equality, in order. Between matches it uses a
// bounded local longest-common-subsequence alignment, so the renderer's
// insertions and deletions are absorbed without any letters-only key. Where
// the source and rows repeat the same text, the result is one optimal local
// alignment, not a proof of the author's intent. The fidelity tests in
// content_anchor_test.go check the mapping on real markdown.
//
// Source offsets are byte offsets into the raw message content. They do not
// depend on width, so an anchor expressed in source offsets survives any
// number of rewraps and theme changes.

const (
	// sourceResyncWindow bounds the local alignment used to resynchronise after
	// a mismatch. A run the renderer drops (a link target, for example) stays
	// mappable only while it is shorter than this. It must stay below 256,
	// because the local table stores lengths in a byte.
	sourceResyncWindow = 64
	// sourceLocalRun is how many equal clusters in a row a local resync needs
	// before its first match is trusted.
	sourceLocalRun = 4
)

// sourceCluster is one non-space grapheme of the raw source.
type sourceCluster struct {
	text       string
	start, end int // byte offsets into the source
}

// renderedCluster is one non-space grapheme of a rendered row.
type renderedCluster struct {
	text string
	row  int
	col  int // display column within the row
	src  int // index into the source clusters, or -1 when unmatched
}

// sourceMatch is one matched grapheme, in source order.
type sourceMatch struct {
	srcStart, srcEnd int
	row, col, width  int
}

// rowSummary records which matches belong to a row.
type rowSummary struct {
	first, end int // matches[first:end] are this row's matched clusters
	unmatched  int // non-space clusters on this row with no source match
}

// SourceMap relates the rendered rows of one message to its raw source.
// The zero value maps nothing. A SourceMap is immutable after BuildSourceMap.
type SourceMap struct {
	rows           []rowSummary
	matches        []sourceMatch
	renderedGlyphs int   // rendered non-space clusters
	matchedGlyphs  int   // rendered non-space clusters with a source match
	wordClusters   int   // rendered word-like clusters
	matchedWords   int   // rendered word-like clusters with a source match
	sourceGlyphs   int   // source non-space clusters
	newlines       []int // byte offsets of '\n' in the source, ascending
}

// BuildSourceMap aligns rendered rows (ANSI allowed) against the raw source
// text they were rendered from. Rows may come from any width. Cost is linear
// in the number of clusters, plus a bounded local search per mismatch.
func BuildSourceMap(source string, rows []string) SourceMap {
	src := sourceClusters(source)
	var rendered []renderedCluster
	rowCount := len(rows)
	rowStart := make([]int, rowCount+1)
	for r, row := range rows {
		rowStart[r] = len(rendered)
		rendered = appendRenderedClusters(rendered, row, r)
	}
	rowStart[rowCount] = len(rendered)

	alignClusters(rendered, src)

	m := SourceMap{rows: make([]rowSummary, rowCount), sourceGlyphs: len(src), newlines: newlineOffsets(source)}
	m.renderedGlyphs = len(rendered)
	for _, c := range rendered {
		matched := c.src >= 0
		if matched {
			m.matchedGlyphs++
		}
		if isWordCluster(c.text) {
			m.wordClusters++
			if matched {
				m.matchedWords++
			}
		}
	}
	for r := range rowCount {
		s := rowSummary{first: len(m.matches), end: len(m.matches)}
		for k := rowStart[r]; k < rowStart[r+1]; k++ {
			c := rendered[k]
			if c.src < 0 {
				s.unmatched++
				continue
			}
			sc := src[c.src]
			m.matches = append(m.matches, sourceMatch{srcStart: sc.start, srcEnd: sc.end, row: r, col: c.col, width: ansi.StringWidth(c.text)})
			s.end = len(m.matches)
		}
		m.rows[r] = s
	}
	return m
}

// isWordCluster reports whether a cluster contains a letter or a digit.
func isWordCluster(text string) bool {
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// Rows returns the number of rendered rows the map was built from.
func (m SourceMap) Rows() int { return len(m.rows) }

// MapStatus classifies how rendered rows relate to their source. It is derived
// from the text alone. It never infers opaque content, because source text can
// be punctuation-only code. Images and widgets are opaque by message type, and
// the integration must say so explicitly.
type MapStatus int

const (
	// MapBlank: neither the rows nor the source hold any non-space text.
	MapBlank MapStatus = iota
	// MapExact: every word-like rendered cluster matched. With no word-like
	// text, every non-space rendered cluster matched.
	MapExact
	// MapPartial: some rendered text matched and some did not. Normal for
	// decoration (table rules, bullets) and rewritten text.
	MapPartial
	// MapUnproven: text on one side only, or no rendered cluster matched. It is
	// a textual block with no proof, never opaque, and must not be scaled.
	MapUnproven
)

// Status classifies the map. Text on one side with nothing on the other is
// MapUnproven: source text that rendered to nothing is a loss, not blank.
func (m SourceMap) Status() MapStatus {
	switch {
	case m.renderedGlyphs == 0 && m.sourceGlyphs == 0:
		return MapBlank
	case m.renderedGlyphs == 0 || m.matchedGlyphs == 0:
		return MapUnproven
	case m.matchedWords == m.wordClusters && (m.wordClusters > 0 || m.matchedGlyphs == m.renderedGlyphs):
		return MapExact
	default:
		return MapPartial
	}
}

// HasContentSource reports whether the raw source has any non-space text.
func (m SourceMap) HasContentSource() bool { return m.sourceGlyphs > 0 }

// MatchedWords returns how many rendered word-like clusters matched the source,
// and how many rendered word-like clusters there were. It is for diagnostics.
func (m SourceMap) MatchedWords() (matched, rendered int) {
	return m.matchedWords, m.wordClusters
}

// Mapped reports whether any rendered non-space cluster matched the source.
// It makes no claim about how much matched; use Status for that.
func (m SourceMap) Mapped() bool {
	return m.matchedGlyphs > 0
}

// Unmatched returns the number of non-space clusters on row that have no
// source counterpart: decoration, or text the renderer changed.
func (m SourceMap) Unmatched(row int) int {
	if row < 0 || row >= len(m.rows) {
		return 0
	}
	return m.rows[row].unmatched
}

// RowSpan returns the source byte range covered by the matched content on
// row. ok is false when the row has no matched content.
func (m SourceMap) RowSpan(row int) (start, end int, ok bool) {
	if row < 0 || row >= len(m.rows) || m.rows[row].first == m.rows[row].end {
		return 0, 0, false
	}
	s := m.rows[row]
	return m.matches[s.first].srcStart, m.matches[s.end-1].srcEnd, true
}

// TopAnchor returns the source byte offset of the first matched content at or
// after row. Rows with no matched content are skipped, so a blank or
// decoration row anchors to the text beneath it.
func (m SourceMap) TopAnchor(row int) (src int, ok bool) {
	for r := max(row, 0); r < len(m.rows); r++ {
		if s := m.rows[r]; s.first < s.end {
			return m.matches[s.first].srcStart, true
		}
	}
	return 0, false
}

// RowForSource returns the row whose content starts at the first matched
// cluster at or after the source offset src. When src lies after the last
// matched cluster, it returns the last row that has content.
func (m SourceMap) RowForSource(src int) (row int, ok bool) {
	i := m.firstMatchAtOrAfter(src)
	if i < 0 {
		return 0, false
	}
	return m.matches[i].row, true
}

// ColForSource returns the row and display column where the matched cluster
// for source offset src is rendered. It follows the same rule as RowForSource.
func (m SourceMap) ColForSource(src int) (row, col int, ok bool) {
	i := m.firstMatchAtOrAfter(src)
	if i < 0 {
		return 0, 0, false
	}
	return m.matches[i].row, m.matches[i].col, true
}

// SourceAt returns the source byte offset of the content at (row, col). When
// that cell is decoration, it returns the nearest matched content before it
// on the same row, and otherwise the nearest after it on that row. ok is
// false when the row has no matched content.
func (m SourceMap) SourceAt(row, col int) (src int, ok bool) {
	if row < 0 || row >= len(m.rows) {
		return 0, false
	}
	s := m.rows[row]
	if s.first == s.end {
		return 0, false
	}
	best := s.first
	for i := s.first; i < s.end; i++ {
		match := m.matches[i]
		if match.col > col {
			break
		}
		best = i
	}
	return m.matches[best].srcStart, true
}

// SourceLine returns the 1-based line of the raw source containing offset src.
// It is for diagnostics and for stating anchors in source-line terms.
func (m SourceMap) SourceLine(src int) int {
	// Count newlines strictly before src.
	lo, hi := 0, len(m.newlines)
	for lo < hi {
		mid := (lo + hi) / 2
		if m.newlines[mid] < src {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo + 1
}

// newlineOffsets returns the byte offset of every '\n' in source.
func newlineOffsets(source string) []int {
	var out []int
	for i := range len(source) {
		if source[i] == '\n' {
			out = append(out, i)
		}
	}
	return out
}

// firstMatchAtOrAfter returns the index of the first match whose source start
// is at or after src, or the last match when src is beyond every match.
// It returns -1 when there are no matches.
func (m SourceMap) firstMatchAtOrAfter(src int) int {
	if len(m.matches) == 0 {
		return -1
	}
	lo, hi := 0, len(m.matches)
	for lo < hi {
		mid := (lo + hi) / 2
		if m.matches[mid].srcStart < src {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == len(m.matches) {
		return len(m.matches) - 1
	}
	return lo
}

// ProportionalRow is the fallback for rows without source mapping. It keeps
// the same fraction of the content above the viewport, so the reader's place
// is approximate, not exact. Use it only when SourceMap.Mapped is false.
func ProportionalRow(oldRow, oldRows, newRows int) int {
	if newRows <= 0 {
		return 0
	}
	if oldRows <= 1 || oldRow <= 0 {
		return 0
	}
	row := (oldRow*newRows + oldRows/2) / oldRows
	return min(max(row, 0), newRows-1)
}

// sourceClusters returns the non-space grapheme clusters of source with their
// byte offsets.
func sourceClusters(source string) []sourceCluster {
	var out []sourceCluster
	g := uniseg.NewGraphemes(source)
	off := 0
	for g.Next() {
		text := g.Str()
		start := off
		off += len(text)
		if isSpaceCluster(text) {
			continue
		}
		out = append(out, sourceCluster{text: text, start: start, end: off})
	}
	return out
}

// appendRenderedClusters appends the non-space clusters of one rendered row,
// recording each display column. ANSI sequences are removed first.
func appendRenderedClusters(dst []renderedCluster, row string, r int) []renderedCluster {
	plain := ansi.Strip(row)
	col := 0
	g := uniseg.NewGraphemes(plain)
	for g.Next() {
		text := g.Str()
		w := ansi.StringWidth(text)
		if !isSpaceCluster(text) {
			dst = append(dst, renderedCluster{text: text, row: r, col: col, src: -1})
		}
		col += w
	}
	return dst
}

// isSpaceCluster reports whether a cluster is whitespace only.
func isSpaceCluster(text string) bool {
	return strings.TrimSpace(text) == ""
}

// alignClusters matches rendered clusters to source clusters in order and
// records the source index of each match in rendered[k].src.
//
// Where the next clusters differ it first tries a bounded local alignment of
// the next sourceResyncWindow clusters on each side. That absorbs short
// insertions and deletions (a link target, emphasis markers, table rules,
// bullets). When the renderer dropped a run longer than the window, no
// cluster is common within it, so it then looks for the next occurrence of
// the upcoming rendered run anywhere later in the source. Occurrences are
// searched in source order from the current position, so repeated text pairs
// with the occurrence that follows the previous match. Nothing is matched
// unless the clusters are equal.
func alignClusters(rendered []renderedCluster, src []sourceCluster) {
	var table []uint8
	var grams sourceGramIndex
	i, j := 0, 0
	for i < len(rendered) && j < len(src) {
		if rendered[i].text == src[j].text {
			rendered[i].src = j
			i++
			j++
			continue
		}
		var a, b int
		var ok bool
		a, b, table, ok = localResync(rendered, i, src, j, table)
		if ok {
			i += a
			j += b
			continue
		}
		if grams == nil {
			grams = buildSourceGramIndex(src)
		}
		if p, ok := grams.next(rendered, i, src, j); ok {
			// p > j because the cluster at j differs from rendered[i].
			j = p
			continue
		}
		// Neither a local nor a global resync: this rendered cluster is
		// decoration or changed text. Drop it and keep the source position.
		i++
	}
}

// sourceGramLen is the number of consecutive clusters that must agree for a
// global resync. It is long enough that a run of prose or code is unlikely to
// repeat by chance, and short enough that a rendered run does not need to be
// whole to find its place again.
const sourceGramLen = 6

// sourceGramIndex maps a hash of every sourceGramLen-cluster run of the source
// to the ascending source positions where it starts.
type sourceGramIndex map[uint64][]int

// buildSourceGramIndex indexes every run of sourceGramLen source clusters.
func buildSourceGramIndex(src []sourceCluster) sourceGramIndex {
	idx := sourceGramIndex{}
	for p := 0; p+sourceGramLen <= len(src); p++ {
		h := hashGram(func(k int) string { return src[p+k].text })
		idx[h] = append(idx[h], p)
	}
	return idx
}

// next returns the first source position at or after j where the rendered run
// starting at rendered[i] occurs in full. ok is false when there is no such run.
func (idx sourceGramIndex) next(rendered []renderedCluster, i int, src []sourceCluster, j int) (int, bool) {
	if i+sourceGramLen > len(rendered) {
		return 0, false
	}
	h := hashGram(func(k int) string { return rendered[i+k].text })
	positions := idx[h]
	for _, p := range positions[sort.SearchInts(positions, j):] {
		if gramEqual(rendered, i, src, p) {
			return p, true
		}
	}
	return 0, false
}

// gramEqual reports whether the rendered run at i equals the source run at p.
func gramEqual(rendered []renderedCluster, i int, src []sourceCluster, p int) bool {
	for k := range sourceGramLen {
		if rendered[i+k].text != src[p+k].text {
			return false
		}
	}
	return true
}

// hashGram is FNV-1a over the texts of a run, with a separator after each one.
// Equal hashes are verified with gramEqual before use.
func hashGram(text func(k int) string) uint64 {
	const prime = 1099511628211
	h := uint64(14695981039346656037)
	for k := range sourceGramLen {
		t := text(k)
		for x := range len(t) {
			h ^= uint64(t[x])
			h *= prime
		}
		h ^= 0xff
		h *= prime
	}
	return h
}

// localResync returns the skips (a rendered, b source) to the first match of
// the best local alignment of the next window clusters on each side. ok is
// false when the windows share no cluster. table is reused between calls.
func localResync(rendered []renderedCluster, i int, src []sourceCluster, j int, table []uint8) (a, b int, _ []uint8, ok bool) {
	wr := min(sourceResyncWindow, len(rendered)-i)
	ws := min(sourceResyncWindow, len(src)-j)
	stride := ws + 1
	need := (wr + 1) * stride
	if cap(table) < need {
		table = make([]uint8, need)
	}
	table = table[:need]
	// table[x*stride+y] is the LCS length of rendered[i+x:] and src[j+y:]
	// within the windows. Row wr and column ws are zero.
	for y := range stride {
		table[wr*stride+y] = 0
	}
	for x := wr - 1; x >= 0; x-- {
		table[x*stride+ws] = 0
		for y := ws - 1; y >= 0; y-- {
			if rendered[i+x].text == src[j+y].text {
				table[x*stride+y] = table[(x+1)*stride+y+1] + 1
			} else {
				table[x*stride+y] = max(table[(x+1)*stride+y], table[x*stride+y+1])
			}
		}
	}
	if table[0] == 0 {
		return 0, 0, table, false
	}
	// Walk the optimal path. A single coincidental cluster is weak evidence (any
	// long window shares some letters with another), so a match counts only
	// when it begins a run of sourceLocalRun equal clusters.
	x, y := 0, 0
	for x < wr && y < ws {
		if rendered[i+x].text == src[j+y].text && table[x*stride+y] == table[(x+1)*stride+y+1]+1 {
			if equalRun(rendered, i+x, src, j+y, min(wr-x, ws-y)) >= sourceLocalRun {
				return x, y, table, true
			}
			x++
			y++
			continue
		}
		if table[(x+1)*stride+y] >= table[x*stride+y+1] {
			x++
		} else {
			y++
		}
	}
	return 0, 0, table, false
}

// equalRun returns how many clusters starting at rendered[i] and src[j] are
// equal, looking at most limit clusters ahead.
func equalRun(rendered []renderedCluster, i int, src []sourceCluster, j, limit int) int {
	n := 0
	for n < limit && rendered[i+n].text == src[j+n].text {
		n++
	}
	return n
}

// matchIndexAtOrAfter returns the first matched cluster at or after the cell
// (row, col) in reading order, or len(matches) when there is none.
func (m SourceMap) matchIndexAtOrAfter(row, col int) int {
	return sort.Search(len(m.matches), func(i int) bool {
		mt := m.matches[i]
		return mt.row > row || (mt.row == row && mt.col >= col)
	})
}

// StartAt returns the source offset of the first matched cluster at or after
// the cell (row, col), and the cell where that cluster is rendered. ok is false
// when no matched cluster follows the cell.
func (m SourceMap) StartAt(row, col int) (src, matchRow, matchCol int, ok bool) {
	i := m.matchIndexAtOrAfter(row, col)
	if i >= len(m.matches) {
		return 0, 0, 0, false
	}
	mt := m.matches[i]
	return mt.srcStart, mt.row, mt.col, true
}

// EndBefore returns the exclusive source end of the last matched cluster that
// starts strictly before the cell (row, col), and the cell just after that
// cluster. A selection ending at (row, col) covers exactly those clusters.
func (m SourceMap) EndBefore(row, col int) (srcEnd, endRow, endCol int, ok bool) {
	i := m.matchIndexAtOrAfter(row, col) - 1
	if i < 0 {
		return 0, 0, 0, false
	}
	mt := m.matches[i]
	return mt.srcEnd, mt.row, mt.col + mt.width, true
}

// CellAfterSource returns the cell just after the matched cluster that ends at
// srcEnd, or after the last matched cluster ending before it. ok is false when
// no matched cluster ends at or before srcEnd.
func (m SourceMap) CellAfterSource(srcEnd int) (row, col int, ok bool) {
	i := sort.Search(len(m.matches), func(i int) bool { return m.matches[i].srcEnd > srcEnd }) - 1
	if i < 0 {
		return 0, 0, false
	}
	mt := m.matches[i]
	return mt.row, mt.col + mt.width, true
}
