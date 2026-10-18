package review

import (
	"strings"
	"sync/atomic"

	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/components/syntax"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	// Beyond these bounds a document renders plain; content and copies are unchanged.
	highlightMaxLines    = 5000
	highlightMaxBytes    = 512 << 10
	highlightMaxLineSize = 2000
)

// tokenizeCalls counts tokenizer runs so tests can prove View never lexes.
var tokenizeCalls atomic.Int64

type token struct {
	text string
	typ  chroma.TokenType
}

// highlighter holds per-line tokens computed once per document. Rendering only
// maps token types to theme colors.
type highlighter struct {
	tokens [][]token

	themeGen uint64
	palette  map[chroma.TokenType]lipgloss.Style
}

// highlightJob tokenizes a document off the event loop. It reads only the
// immutable document lines captured when it was created.
type highlightJob func() [][]token

// prepare clears tokens and returns the job that will color this document,
// or nil when the document renders plain (unknown language, binary, oversized).
func (h *highlighter) prepare(doc *Document, text []string) highlightJob {
	h.tokens = nil
	if doc == nil || doc.Binary || len(doc.Lines) == 0 || len(doc.Lines) > highlightMaxLines {
		return nil
	}
	size := 0
	var sample strings.Builder
	for _, l := range doc.Lines {
		size += len(l.Text) + 1
		if sample.Len() < 4096 && l.Kind != LineHunk && l.Kind != LineNote {
			sample.WriteString(l.Text)
			sample.WriteByte('\n')
		}
	}
	if size > highlightMaxBytes {
		return nil
	}
	lexer := syntax.LexerForFile(doc.Key.Path, sample.String())
	if lexer == nil {
		return nil
	}
	lines := doc.Lines
	files := doc.Key.Mode == ModeFiles
	return func() [][]token {
		out := make([][]token, len(lines))
		if files {
			stream(lexer, lines, text, out, func(Line) bool { return true })
			return out
		}
		stream(lexer, lines, text, out, func(l Line) bool { return l.Kind == LineContext || l.Kind == LineRemove })
		stream(lexer, lines, text, out, func(l Line) bool { return l.Kind == LineContext || l.Kind == LineAdd })
		return out
	}
}

// stream tokenizes one code stream (old or new side, or the whole file) so
// multi-line comments, strings and heredocs color correctly.
func stream(lexer chroma.Lexer, lines []Line, text []string, out [][]token, include func(Line) bool) {
	var code strings.Builder
	var index []int
	for i, l := range lines {
		if l.Kind == LineHunk || l.Kind == LineNote || !include(l) {
			continue
		}
		code.WriteString(l.Text)
		code.WriteByte('\n')
		index = append(index, i)
	}
	if len(index) == 0 {
		return
	}
	tokenizeCalls.Add(1)
	tokenized, err := syntax.Lines(lexer, code.String())
	if err != nil {
		return
	}
	for n, i := range index {
		if n >= len(tokenized) || len(text[i]) > highlightMaxLineSize {
			continue
		}
		toks := make([]token, 0, len(tokenized[n]))
		width := 0
		for _, t := range tokenized[n] {
			v := sanitize(t.Value)
			width += ansi.StringWidth(v)
			toks = append(toks, token{text: v, typ: t.Type})
		}
		// Keep plain rendering if sanitizing changed the line's geometry.
		if width == ansi.StringWidth(text[i]) {
			out[i] = toks
		}
	}
}

func (h *highlighter) style(typ chroma.TokenType, base lipgloss.Style) lipgloss.Style {
	if gen := styles.ThemeGeneration(); h.palette == nil || gen != h.themeGen {
		h.palette, h.themeGen = map[chroma.TokenType]lipgloss.Style{}, gen
		theme := styles.ChromaStyle()
		types := []chroma.TokenType{
			chroma.Keyword, chroma.KeywordType, chroma.Name, chroma.NameFunction, chroma.NameBuiltin, chroma.NameClass,
			chroma.LiteralString, chroma.LiteralNumber, chroma.Comment, chroma.Operator, chroma.Punctuation, chroma.NameTag, chroma.NameAttribute, chroma.GenericHeading,
		}
		for _, t := range types {
			entry := theme.Get(t)
			s := lipgloss.NewStyle()
			if entry.Colour.IsSet() {
				s = s.Foreground(lipgloss.Color(entry.Colour.String()))
			}
			if entry.Bold == chroma.Yes {
				s = s.Bold(true)
			}
			if entry.Italic == chroma.Yes {
				s = s.Italic(true)
			}
			h.palette[t] = s
		}
	}
	for t := typ; t != 0; t = t.Parent() {
		s, ok := h.palette[t]
		if !ok {
			continue
		}
		st := base.Bold(s.GetBold()).Italic(s.GetItalic())
		if _, none := s.GetForeground().(lipgloss.NoColor); !none {
			st = st.Foreground(s.GetForeground())
		}
		return st
	}
	return base
}

// render draws cells [from, to) of a line with syntax colors over base.
func (h *highlighter) render(i int, text string, from, to int, base lipgloss.Style) string {
	if i >= len(h.tokens) || h.tokens[i] == nil {
		return base.Render(ansi.Cut(text, from, to))
	}
	var out strings.Builder
	at := 0
	for _, t := range h.tokens[i] {
		w := ansi.StringWidth(t.text)
		start, end := max(from, at), min(to, at+w)
		if start < end {
			out.WriteString(h.style(t.typ, base).Render(ansi.Cut(t.text, start-at, end-at)))
		}
		at += w
		if at >= to {
			break
		}
	}
	return out.String()
}
