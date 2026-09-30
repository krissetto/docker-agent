package markdown

import (
	"strings"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

// IncrementalRenderer retains completed parser blocks and re-renders only the
// final, still-mutable block. A complete lookahead line must confirm a boundary
// before it is retained, so partial list/fence/table syntax can still change it.
// IncrementalRenderer is not safe for concurrent use.
type IncrementalRenderer struct {
	width           int
	themeGeneration uint64

	// inputPrefix is the longest stable prefix of the most recent input that
	// ends at a block boundary. outputPrefix is its rendered counterpart.
	// Both are empty until the first successful incremental render.
	inputPrefix  string
	outputPrefix string
	output       strings.Builder
	lastInput    string
	lastParts    RenderedParts

	// codeBlocksPrefix is the list of code blocks emitted while rendering the
	// cached prefix, with Line indices relative to outputPrefix.
	codeBlocksPrefix []CodeBlock

	// fallback is used for the actual rendering work; it is reused across calls
	// so its parser pool (and chroma caches) stay warm.
	fallback *FastRenderer
}

// NewIncrementalRenderer creates a new incremental renderer with the given
// terminal width.
func NewIncrementalRenderer(width int) *IncrementalRenderer {
	return &IncrementalRenderer{
		width:    width,
		fallback: NewFastRenderer(width),
	}
}

// Render produces styled terminal output for input. When successive calls pass
// inputs that share a long common prefix (the streaming case), only the suffix
// is parsed and rendered; the rest is served from the cached output.
func (r *IncrementalRenderer) Render(input string) (string, error) {
	out, _, err := r.RenderWithCodeBlocks(input)
	return out, err
}

// RenderedParts exposes the reusable rendered block prefix separately from the
// still-mutable trailing block. Consumers that retain lines can splice only
// MutableTail while StablePrefix grows monotonically during streaming.
type RenderedParts struct {
	StablePrefix string
	MutableTail  string
	CodeBlocks   []CodeBlock
}

// RenderParts renders input without concatenating the stable rendered prefix
// to the mutable tail. This avoids rescanning/copying an ever-growing ANSI
// string in viewport-oriented consumers.
func (r *IncrementalRenderer) RenderParts(input string) (RenderedParts, error) {
	stable, tail, blocks, err := r.renderParts(input)
	return RenderedParts{StablePrefix: stable, MutableTail: tail, CodeBlocks: blocks}, err
}

// RenderWithCodeBlocks behaves like Render but additionally returns the list
// of fenced code blocks in the rendered output. Each entry's Line is the
// 0-indexed line within the returned string where the block's copy label is
// drawn.
func (r *IncrementalRenderer) RenderWithCodeBlocks(input string) (string, []CodeBlock, error) {
	stable, tail, blocks, err := r.renderParts(input)
	if err != nil {
		return "", nil, err
	}
	return r.joinPrefixAndTail(stable, tail), blocks, nil
}

func (r *IncrementalRenderer) renderParts(input string) (string, string, []CodeBlock, error) {
	if generation := styles.ThemeGeneration(); r.themeGeneration != generation {
		r.themeGeneration = generation
		r.Reset()
	}
	input = sanitizeForTerminal(input)
	if input == r.lastInput {
		p := r.lastParts
		return p.StablePrefix, p.MutableTail, p.CodeBlocks, nil
	}
	if !strings.HasPrefix(input, r.inputPrefix) {
		r.Reset()
	}
	tail := input[len(r.inputPrefix):]
	stable, mutable, blocks, boundary, stableBlockCount := r.fallback.renderCheckpointParts(tail)
	merged := r.mergeCodeBlocks(r.outputPrefix, r.codeBlocksPrefix, blocks)
	if boundary > 0 {
		if r.output.Len() > 0 && stable != "" {
			r.output.WriteString("\n" + strings.Repeat(" ", max(r.width, 0)) + "\n")
		}
		r.output.WriteString(stable)
		r.outputPrefix = r.output.String()
		r.inputPrefix = input[:len(r.inputPrefix)+boundary]
		r.codeBlocksPrefix = cloneCodeBlocks(merged[:len(r.codeBlocksPrefix)+stableBlockCount])
	}
	r.lastInput = input
	r.lastParts = RenderedParts{StablePrefix: r.outputPrefix, MutableTail: mutable, CodeBlocks: merged}
	return r.outputPrefix, mutable, merged, nil
}

// SetWidth updates the renderer width. Width changes invalidate the cache
// because the rendered output is width-dependent.
func (r *IncrementalRenderer) SetWidth(width int) {
	if width == r.width {
		return
	}
	r.width = width
	r.fallback = NewFastRenderer(width)
	r.Reset()
}

// Reset releases retained block output and input after an edit or theme change.
func (r *IncrementalRenderer) Reset() {
	r.inputPrefix = ""
	r.outputPrefix = ""
	r.output.Reset()
	r.codeBlocksPrefix = nil
	r.lastInput = ""
	r.lastParts = RenderedParts{}
}

// joinPrefixAndTail concatenates a previously rendered prefix and a freshly
// rendered tail to form the equivalent of a single-shot render of
// prefix-content + "\n\n" + tail-content.
//
// Two FastRenderer details drive the body of this function:
//
//  1. FastRenderer trims trailing newlines from its output, so each piece
//     ends mid-line and we have to reintroduce a separator newline.
//  2. FastRenderer's finalizeOutput pads every line (including blank ones) to
//     `width` with spaces. The blank line we insert between the two pieces
//     therefore needs the same padding to be byte-identical to a full render.
//
// If either of those FastRenderer behaviours changes in the future, the
// IncrementalRendererMatchesFullRender* tests will fail and pinpoint this
// function as the place to update.
func (r *IncrementalRenderer) joinPrefixAndTail(prefix, tail string) string {
	if prefix == "" {
		return tail
	}
	if tail == "" {
		return prefix
	}
	if r.width <= 0 {
		return prefix + "\n\n" + tail
	}
	var b strings.Builder
	b.Grow(len(prefix) + len(tail) + r.width + 2)
	b.WriteString(prefix)
	b.WriteByte('\n')
	for range r.width {
		b.WriteByte(' ')
	}
	b.WriteByte('\n')
	b.WriteString(tail)
	return b.String()
}

// joinSeparatorLines is the number of extra rendered lines that
// joinPrefixAndTail inserts between the prefix output and the tail output:
// one to terminate the prefix's final (untrailing-newlined) line, and one
// blank-padded separator line. Keep this in sync with joinPrefixAndTail.
const joinSeparatorLines = 2

// mergeCodeBlocks returns the union of code blocks from a cached prefix output
// and a freshly rendered tail. Tail block line indices are shifted past the
// prefix's lines and the separator that joinPrefixAndTail inserts.
func (r *IncrementalRenderer) mergeCodeBlocks(prefixOut string, prefixBlocks, tailBlocks []CodeBlock) []CodeBlock {
	if len(prefixBlocks) == 0 && len(tailBlocks) == 0 {
		return nil
	}
	out := make([]CodeBlock, 0, len(prefixBlocks)+len(tailBlocks))
	out = append(out, prefixBlocks...)
	if len(tailBlocks) == 0 {
		return out
	}
	offset := 0
	if prefixOut != "" {
		offset = strings.Count(prefixOut, "\n") + joinSeparatorLines
	}
	for _, b := range tailBlocks {
		out = append(out, CodeBlock{Content: b.Content, Line: b.Line + offset})
	}
	return out
}

func cloneCodeBlocks(in []CodeBlock) []CodeBlock {
	if len(in) == 0 {
		return nil
	}
	out := make([]CodeBlock, len(in))
	copy(out, in)
	return out
}
