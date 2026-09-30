package markdown

// streamCache belongs to one mutable renderer, never the global syntax LRU.
// Each pass retains only operations used by that pass, within a byte budget.
type streamCache struct {
	previous, current map[streamKey]any
	bytes             int
}

type streamKey struct {
	kind           uint8
	width          int
	text, language string
}

const streamCacheBudget = 2 << 20

func (c *streamCache) begin() {
	c.previous, c.current = c.current, make(map[streamKey]any)
	c.bytes = 0
}

func (c *streamCache) get(key streamKey) (any, bool) {
	if value, ok := c.current[key]; ok {
		return value, true
	}
	value, ok := c.previous[key]
	return value, ok
}

func (c *streamCache) keep(key streamKey, value any, size int) {
	if _, exists := c.current[key]; exists {
		return
	}
	size += len(key.text) + len(key.language) + 128
	if size <= streamCacheBudget-c.bytes {
		c.current[key] = value
		c.bytes += size
	}
}

func (c *streamCache) end() { c.previous = nil }

func (p *parser) cachedText(key streamKey, render func() string) string {
	if p.stream == nil {
		return render()
	}
	value, ok := p.stream.get(key)
	if !ok {
		value = render()
	}
	result := value.(string)
	p.stream.keep(key, result, len(result))
	return result
}
