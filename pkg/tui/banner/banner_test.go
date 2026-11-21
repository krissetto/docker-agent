package banner

import (
	"strings"
	"testing"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestWhaleArtworkAndDimensions(t *testing.T) {
	// Preserve Braille blanks and the rounded outline without adding invisible format characters.
	const source = `⠀⣠⣶⣿⣿⣿⣿⣿⣿⣿⣿⣿⠀⠀⣿⣿⣿⣿⣿⣷⣄⣠⣾⣿⣿⣿⣿⣿
⣼⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⡆⠀⢻⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⠟
⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣦⣀⠈⠙⠛⠿⣿⣿⣿⡟⠛⠛⠋⠁
⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣶⣦⣄⣼⣿⣿⡇
⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⡇
⢻⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⠃
⠀⠙⠿⢿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⣿⠿⠟⠁`
	require.Equal(t, source, strings.Join(Lines, "\n"))
	require.Len(t, Lines, Height)
	width := 0
	for _, line := range Lines {
		width = max(width, ansi.StringWidth(line))
		for _, r := range line {
			require.False(t, unicode.Is(unicode.Cf, r), "artwork must not contain invisible format characters")
			require.True(t, r >= 0x2800 && r <= 0x28ff, "artwork uses only Braille cells")
		}
	}
	require.Equal(t, Width, width)
}

func TestWhaleLowerRightContourTapersInward(t *testing.T) {
	t.Parallel()
	previousEdge := Width * 2
	for _, line := range Lines[Height-3:] {
		for _, rowBits := range [][2]uint{{0, 3}, {1, 4}, {2, 5}, {6, 7}} {
			rightEdge := 0
			for column, cell := range []rune(line) {
				for side, bit := range rowBits {
					if (cell-0x2800)&(1<<bit) != 0 {
						rightEdge = column*2 + side + 1
					}
				}
			}
			require.Positive(t, rightEdge)
			require.LessOrEqual(t, rightEdge, previousEdge, "lower-right outline must taper inward, never bulge outward")
			previousEdge = rightEdge
		}
	}
}

func TestWhaleBackToFinContourHasNoAbruptStep(t *testing.T) {
	t.Parallel()
	rowBits := [][2]uint{{0, 3}, {1, 4}, {2, 5}, {6, 7}}
	previousLeft, previousRight := 0, 0
	for y := 8; y <= 14; y++ {
		line := []rune(Lines[y/4])
		filled := func(x int) bool {
			return (line[x/2]-0x2800)&(1<<rowBits[y%4][x%2]) != 0
		}
		left := 24
		for left < len(line)*2 && filled(left) {
			left++
		}
		right := left
		for right < len(line)*2 && !filled(right) {
			right++
		}
		require.Less(t, left, right, "the back-to-fin cutout must remain open through the curve")
		require.Less(t, right, len(line)*2, "the fin must bound the cutout")
		if y > 8 {
			require.InDelta(t, previousLeft, left, 3, "back contour must curve without a flat ledge")
			require.InDelta(t, previousRight, right, 3, "fin contour must curve without a flat ledge")
		}
		previousLeft, previousRight = left, right
	}
}
