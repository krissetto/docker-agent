package messages

// layoutIndex is a Fenwick (binary indexed) tree over per-item effective
// heights. Prefix sums, point updates, appends and "which item holds line N"
// are all O(log n), so geometry queries never walk the transcript.
type layoutIndex struct {
	tree []int // 1-based Fenwick tree; tree[0] is unused
	vals []int // current effective height per item, 0-based
	sum  int   // total of vals

	// touched counts Fenwick nodes read or written. It is a work counter only.
	touched uint64
}

// NodesTouched reports how many Fenwick nodes this index has read or written.
func (x *layoutIndex) NodesTouched() uint64 { return x.touched }

// Len reports the number of items.
func (x *layoutIndex) Len() int { return len(x.vals) }

// Total is the sum of every item height.
func (x *layoutIndex) Total() int { return x.sum }

// Height returns the effective height of item i.
func (x *layoutIndex) Height(i int) int { return x.vals[i] }

// Offset returns the first line of item i, that is the sum of heights before it.
func (x *layoutIndex) Offset(i int) int { return x.prefix(i) }

// Append adds an item of height v at the end in O(log n).
func (x *layoutIndex) Append(v int) {
	if len(x.tree) == 0 {
		x.tree = make([]int, 1, 2)
	}
	pos := len(x.vals) + 1 // new 1-based position
	low := pos & -pos
	x.vals = append(x.vals, v)
	// tree[pos] covers (pos-low, pos]; the part before the new item is already in the tree.
	x.tree = append(x.tree, x.prefix(pos-1)-x.prefix(pos-low)+v)
	x.sum += v
}

// Set changes the height of item i in O(log n).
func (x *layoutIndex) Set(i, v int) {
	delta := v - x.vals[i]
	if delta == 0 {
		return
	}
	x.vals[i] = v
	x.sum += delta
	for p, n := i+1, len(x.vals); p <= n; p += p & -p {
		x.tree[p] += delta
		x.touched++
	}
}

// Truncate keeps the first n items in O(n-len) time, dropping the rest.
func (x *layoutIndex) Truncate(n int) {
	for len(x.vals) > n {
		x.sum -= x.vals[len(x.vals)-1]
		x.vals = x.vals[:len(x.vals)-1]
	}
	if len(x.tree) > n+1 {
		x.tree = x.tree[:n+1]
	}
}

// Find returns the item that holds line target: the last item whose offset is
// at or before target. Zero-height items are skipped. It returns Len() when
// target is at or past Total().
func (x *layoutIndex) Find(target int) int {
	n := len(x.vals)
	pos := 0
	rem := target
	step := 1
	for step<<1 <= n {
		step <<= 1
	}
	for ; step > 0; step >>= 1 {
		x.touched++
		if next := pos + step; next <= n && x.tree[next] <= rem {
			pos = next
			rem -= x.tree[next]
		}
	}
	return pos
}

// prefix returns the sum of the first k items.
func (x *layoutIndex) prefix(k int) int {
	s := 0
	for p := k; p > 0; p -= p & -p {
		s += x.tree[p]
		x.touched++
	}
	return s
}

// Rebuild replaces every item height with vals in O(n) time.
func (x *layoutIndex) Rebuild(vals []int) {
	x.vals = append(x.vals[:0], vals...)
	x.tree = make([]int, len(vals)+1)
	x.sum = 0
	for i, v := range vals {
		x.tree[i+1] += v
		x.sum += v
		if p := i + 1 + ((i + 1) & -(i + 1)); p <= len(vals) {
			x.tree[p] += x.tree[i+1]
		}
	}
}
