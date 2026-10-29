package markdown

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const longInlineCodePath = "/Users/krissetto/dev/ai-dev/.tmp-public-api-unified-sdk/worktrees-generation-comparison/spec-trim"

// tableColumnTexts joins each column's cell text across physical lines, skipping
// separator and blank rows. Cell wrapping can split words, so spaces are dropped.
func tableColumnTexts(t *testing.T, out string, cols int) []string {
	t.Helper()
	texts := make([]string, cols)
	for line := range strings.SplitSeq(stripANSI(out), "\n") {
		if line == "" || strings.Contains(line, "─") {
			continue
		}
		parts := strings.Split(line, "│")
		require.Len(t, parts, cols, "line %q", line)
		for i, part := range parts {
			texts[i] += strings.TrimSpace(part)
		}
	}
	return texts
}

// separatorCells returns the display column of every │ on a rendered line.
func separatorCells(line string) []int {
	plain := stripANSI(line)
	var cols []int
	for idx := 0; idx < len(plain); {
		r, size := utf8.DecodeRuneInString(plain[idx:])
		if r == '│' {
			cols = append(cols, ansi.StringWidth(plain[:idx]))
		}
		idx += size
	}
	return cols
}

func assertLinesWithin(t *testing.T, out string, width int) {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		assert.LessOrEqual(t, ansiStringWidth(line), width, "line overflows viewport: %q", stripANSI(line))
	}
}

func TestTableLongInlineCodeFitsViewport(t *testing.T) {
	t.Parallel()
	input := "| what | directory | branch |\n| --- | --- | --- |\n| HEAD | `" + longInlineCodePath + "` | main |\n"

	for _, width := range []int{20, 40, 60, 100} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			t.Parallel()
			out, err := NewFastRenderer(width).Render(input)
			require.NoError(t, err)
			assertLinesWithin(t, out, width)

			texts := tableColumnTexts(t, out, 3)
			assert.Equal(t, "whatHEAD", texts[0])
			assert.Equal(t, "directory"+longInlineCodePath, texts[1])
			assert.Equal(t, "branchmain", texts[2])
		})
	}
}

func TestTableContinuationRowsKeepSeparatorColumns(t *testing.T) {
	t.Parallel()
	input := "| what | directory | branch |\n| --- | --- | --- |\n| HEAD | `" + longInlineCodePath + "` | main |\n| dev | `short` | feature/very-long-branch-name-here |\n"

	for _, width := range []int{40, 60, 100} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			t.Parallel()
			out, err := NewFastRenderer(width).Render(input)
			require.NoError(t, err)
			lines := strings.Split(out, "\n")
			var want []int
			for _, line := range lines {
				if cols := separatorCells(line); len(cols) > 0 {
					if want == nil {
						want = cols
					}
					assert.Equal(t, want, cols, "separators drifted on line %q", stripANSI(line))
					assert.Len(t, cols, 2)
				}
			}
			require.NotNil(t, want, "table rendered with separators")
		})
	}
}

func TestTableWideRuneCellsFitViewport(t *testing.T) {
	t.Parallel()
	input := "| 名前 | 説明 |\n| --- | --- |\n| 界界界界界界界界 | 文字列文字列 |\n"

	for _, width := range []int{12, 20, 30} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			t.Parallel()
			out, err := NewFastRenderer(width).Render(input)
			require.NoError(t, err)
			assertLinesWithin(t, out, width)
			texts := tableColumnTexts(t, out, 2)
			assert.Equal(t, "名前界界界界界界界界", texts[0])
			assert.Equal(t, "説明文字列文字列", texts[1])
		})
	}
}

func TestTableStackedFallbackKeepsAllContent(t *testing.T) {
	t.Parallel()
	input := "| 名前 | 説明 | 備考 |\n| --- | --- | --- |\n| 界界界界 | 文字列 | 注記 |\n"

	out, err := NewFastRenderer(5).Render(input)
	require.NoError(t, err)
	assertLinesWithin(t, out, 5)
	assert.NotContains(t, stripANSI(out), "│", "stacked fallback has no column separators")
	compact := strings.Join(strings.Fields(stripANSI(out)), "")
	for _, want := range []string{"名前", "説明", "備考", "界界界界", "文字列", "注記"} {
		assert.Contains(t, compact, want)
	}
}

func TestTableRenderIsDeterministicAcrossCachedRenders(t *testing.T) {
	t.Parallel()
	input := "| what | directory |\n| --- | --- |\n| HEAD | `" + longInlineCodePath + "` |\n"

	r := NewFastRenderer(60)
	first, err := r.Render(input)
	require.NoError(t, err)
	second, err := r.Render(input)
	require.NoError(t, err)
	fresh, err := NewFastRenderer(60).Render(input)
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.Equal(t, first, fresh)
}

func TestWrapTextKeepsStyleAcrossBrokenWord(t *testing.T) {
	t.Parallel()
	p := &parser{styles: getGlobalStyles()}
	const open = "\x1b[48;2;1;2;3m"
	wrapped := p.wrapText(open+"abcdefghijkl"+"\x1b[m", 5)

	lines := strings.Split(wrapped, "\n")
	require.Len(t, lines, 3, "%q", wrapped)
	for _, line := range lines {
		assert.True(t, strings.HasPrefix(line, open), "continuation must re-open style: %q", line)
		assert.LessOrEqual(t, ansiStringWidth(line), 5)
	}
	assert.Equal(t, "abcdefghijkl", stripANSI(strings.Join(lines, "")))
}

func TestWrapTextLongWordHasNoTrailingBlankLine(t *testing.T) {
	t.Parallel()
	p := &parser{styles: getGlobalStyles()}
	assert.Equal(t, "abcd\nefgh\nij", p.wrapText("abcdefghij", 4))
	assert.Equal(t, "hi\nabcd\nefgh\nij", p.wrapText("hi abcdefghij", 4))
}

func TestTableCompactSeparatorsWhenWideCannotHoldRunes(t *testing.T) {
	t.Parallel()
	input := "| 名 | 前 | 後 |\n| --- | --- | --- |\n| 界界 | 文字 | 注記 |\n"

	out, err := NewFastRenderer(10).Render(input)
	require.NoError(t, err)
	assertLinesWithin(t, out, 10)
	texts := tableColumnTexts(t, out, 3)
	assert.Equal(t, "名界界", texts[0])
	assert.Equal(t, "前文字", texts[1])
	assert.Equal(t, "後注記", texts[2])
}

func TestWrapTextKeepsCombiningClustersWhole(t *testing.T) {
	t.Parallel()
	p := &parser{styles: getGlobalStyles()}
	cluster := "e\u0301"
	got := p.wrapText(strings.Repeat(cluster, 6), 4)
	assert.Equal(t, strings.Repeat(cluster, 4)+"\n"+strings.Repeat(cluster, 2), got)
}

func TestWrapTextKeepsZWJSequenceWhole(t *testing.T) {
	t.Parallel()
	p := &parser{styles: getGlobalStyles()}
	family := "👨‍👩‍👧"
	got := p.wrapText("ab"+family+"cd", 6)
	assert.Equal(t, "ab\n"+family+"\ncd", got)
}

func TestTableShortColumnsKeepWordsWhole(t *testing.T) {
	t.Parallel()
	input := "| what | directory | branch / HEAD |\n| --- | --- | --- |\n| Cloud adoption (open this PR) | `" + longInlineCodePath + "` | `cloud-api-simplified` @ `e3c7c1b` |\n"

	out, err := NewFastRenderer(60).Render(input)
	require.NoError(t, err)
	assertLinesWithin(t, out, 60)

	var firstColumn []string
	for line := range strings.SplitSeq(stripANSI(out), "\n") {
		if line == "" || strings.Contains(line, "─") {
			continue
		}
		firstColumn = append(firstColumn, strings.TrimSpace(strings.Split(line, "│")[0]))
	}
	assert.Contains(t, firstColumn, "what", "header label stays whole")
	assert.Contains(t, firstColumn, "Cloud", "short data word stays whole")
	assert.Contains(t, firstColumn, "adoption", "short data word stays whole")
}
