package markdown

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const cellFG = "\x1b[38;2;221;212;201m"

// A reset inside the word that wraps must not drop the style it closes on the next line.
func TestWrapTextReopensStyleBeforeWordThatClosesIt(t *testing.T) {
	t.Parallel()

	p := &parser{}
	got := p.wrapText(cellFG+"alpha beta\x1b[m", 6)

	require.Equal(t, cellFG+"alpha\x1b[m\n"+cellFG+"beta\x1b[m", got)
}

func TestWrapTextReopensStyleAcrossBrokenWord(t *testing.T) {
	t.Parallel()

	p := &parser{}
	got := p.wrapText("x "+cellFG+"abcdefghij\x1b[m tail", 4)

	require.Equal(t, "x\n"+cellFG+"abcd\x1b[m\n"+cellFG+"efgh\x1b[m\n"+cellFG+"ij\x1b[m\ntail", got)
}

// Every wrapped table cell line must reopen the cell foreground, including header rows.
func TestTableWrappedCellLinesKeepForeground(t *testing.T) {
	t.Parallel()

	input := "| Header One | Header Two |\n|------------|------------|\n| Short | This cell has a lot of text that needs to wrap properly without losing any content |\n"
	out, err := NewFastRenderer(45).Render(input)
	require.NoError(t, err)

	lines := strings.Split(out, "\n")
	idx := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(stripANSI(l), "One") })
	require.NotEqual(t, -1, idx, "wrapped header word not rendered")
	assert.Contains(t, lines[idx], cellFG+"One", "wrapped header line lost cell fg: %q", lines[idx])

	for _, word := range []string{"needs", "losing"} {
		idx = slices.IndexFunc(lines, func(l string) bool { return strings.Contains(stripANSI(l), word) })
		require.NotEqual(t, -1, idx, "wrapped word %q not rendered", word)
		assert.Contains(t, lines[idx], cellFG+word, "wrapped body line lost cell fg: %q", lines[idx])
	}
}
