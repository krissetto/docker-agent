package image

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCellSizeResolvesUnknownGeometry(t *testing.T) {
	assert.Equal(t, CellSize{8, 16}, CellSize{}.Resolved())
	assert.Equal(t, CellSize{9, 23}, CellSize{9, 23}.Resolved())
	assert.False(t, CellSize{600, 16}.Valid())
	assert.Equal(t, CellSize{8, 16}, CellSize{600, 16}.Resolved())
}
