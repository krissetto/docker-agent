package animation

// FrameCache memoizes a fixed set of rendered fragments for the latest generation.
// Its zero value is ready to use. Keep one cache per rendering variant.
type FrameCache struct {
	generation uint64
	frames     []string
}

// Frame returns the indexed fragment, rebuilding the set when the generation changes.
func (c *FrameCache) Frame(generation uint64, count, index int, render func(int) string) string {
	if c.generation != generation || len(c.frames) != count {
		if len(c.frames) != count {
			c.frames = make([]string, count)
		}
		for i := range c.frames {
			c.frames[i] = render(i)
		}
		c.generation = generation
	}
	return c.frames[index]
}
