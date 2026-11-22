package animation

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFrameCacheReusesFramesForCurrentGeneration(t *testing.T) {
	t.Parallel()
	var cache FrameCache
	calls := 0
	render := func(i int) string {
		calls++
		return strconv.Itoa(calls) + ":" + strconv.Itoa(i)
	}
	require.Equal(t, "2:1", cache.Frame(0, 2, 1, render))
	require.Equal(t, "1:0", cache.Frame(0, 2, 0, render))
	require.Equal(t, "2:1", cache.Frame(0, 2, 1, render))
	require.Equal(t, 2, calls)
	require.Equal(t, "4:1", cache.Frame(1, 2, 1, render))
	require.Equal(t, "3:0", cache.Frame(1, 2, 0, render))
	require.Equal(t, 4, calls)
}
