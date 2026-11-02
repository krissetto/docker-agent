package messages

import (
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"
)

// naiveLayout is the reference: explicit heights summed by walking.
type naiveLayout []int

func (n naiveLayout) offset(i int) int {
	s := 0
	for _, v := range n[:i] {
		s += v
	}
	return s
}

func (n naiveLayout) find(line int) int {
	off := 0
	for i, v := range n {
		if line < off+v {
			return i
		}
		off += v
	}
	return len(n)
}

func TestLayoutIndexMatchesNaiveLayout(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewSource(7))
	var idx layoutIndex
	var ref naiveLayout
	for step := range 4000 {
		switch op := rng.Intn(10); {
		case op < 4 || len(ref) == 0:
			v := rng.Intn(6) // zero-height items are allowed
			idx.Append(v)
			ref = append(ref, v)
		case op < 8:
			i := rng.Intn(len(ref))
			v := rng.Intn(9)
			idx.Set(i, v)
			ref[i] = v
		default:
			n := rng.Intn(len(ref) + 1)
			idx.Truncate(n)
			ref = ref[:n]
		}
		require.Equal(t, len(ref), idx.Len(), "step %d", step)
		require.Equal(t, ref.offset(len(ref)), idx.Total(), "total at step %d", step)
		for i := range ref {
			require.Equal(t, ref.offset(i), idx.Offset(i), "offset %d at step %d", i, step)
			require.Equal(t, ref[i], idx.Height(i))
		}
		for line := -1; line <= idx.Total(); line += 1 + step%3 {
			require.Equal(t, ref.find(line), idx.Find(line), "find(%d) at step %d", line, step)
		}
	}
}

func TestLayoutIndexFindSkipsZeroHeightItems(t *testing.T) {
	t.Parallel()

	var idx layoutIndex
	for _, v := range []int{3, 0, 0, 2, 0} {
		idx.Append(v)
	}
	require.Equal(t, 0, idx.Find(0))
	require.Equal(t, 0, idx.Find(2))
	require.Equal(t, 3, idx.Find(3))
	require.Equal(t, 3, idx.Find(4))
	require.Equal(t, idx.Len(), idx.Find(5))
}
