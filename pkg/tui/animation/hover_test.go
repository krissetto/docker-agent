package animation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestHoverStepSettlesExactlyAtThreeFiftyMillisecondFrames(t *testing.T) {
	value := 0.0
	for range 3 {
		value = HoverStep(value, 1, 50*time.Millisecond)
	}
	require.Equal(t, 1.0, value)
	for range 3 {
		value = HoverStep(value, 0, 50*time.Millisecond)
	}
	require.Zero(t, value, "floating-point residue must not retain a finite lease")
	require.Equal(t, 0.5, HoverStep(HoverStep(0.5, 0, 50*time.Millisecond), 1, 50*time.Millisecond))
}
