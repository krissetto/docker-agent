package image

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMarkerColumnsRecognizesOnlyWellFormedMarkers(t *testing.T) {
	columns, marker := MarkerColumns(markerPrefix + "7;12;3;0\x1b\\")
	assert.Equal(t, 12, columns)
	assert.True(t, marker)

	columns, marker = MarkerColumns("plain text")
	assert.Zero(t, columns)
	assert.False(t, marker)

	columns, marker = MarkerColumns(markerPrefix + "7;0;3;0\x1b\\")
	assert.Zero(t, columns, "zero-width markers are rejected")
	assert.True(t, marker)

	columns, marker = MarkerColumns(markerPrefix + "7;12;3\x1b\\")
	assert.Zero(t, columns, "truncated markers are rejected")
	assert.True(t, marker)
}

func TestStripMarkersRemovesGraphicsAndKeepsText(t *testing.T) {
	marker := markerPrefix + "7;2;1;0\x1b\\"
	assert.Equal(t, "abcd", StripMarkers("ab"+marker+"cd"))
	assert.Equal(t, "plain", StripMarkers("plain"))
	assert.Equal(t, "\x1b[31mred\x1b[m", StripMarkers("\x1b[31mred\x1b[m"+marker))
}
