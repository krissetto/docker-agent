// Package syntax is the shared lexer and tokenization pipeline used by the
// message code renderer and the review reader. It returns token types only;
// callers own styling and caching of rendered output.
package syntax

import (
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"

	"github.com/docker/docker-agent/pkg/concurrent"
)

var lexerCache concurrent.Map[string, chroma.Lexer]

// Lexer returns a cached, coalesced lexer for a code-fence language or file
// extension, or nil when the language is unknown.
func Lexer(lang string) chroma.Lexer {
	if lang == "" {
		return nil
	}
	if lexer, ok := lexerCache.Load(lang); ok {
		return lexer
	}
	lexer := lexers.Get(lang)
	if lexer == nil {
		lexer = lexers.Match("file." + lang)
	}
	if lexer == nil {
		return nil
	}
	lexer = chroma.Coalesce(lexer)
	lexerCache.Store(lang, lexer)
	return lexer
}

// LexerForFile picks a lexer from a file path, falling back to content
// analysis; plain text yields nil.
func LexerForFile(path, sample string) chroma.Lexer {
	lexer := lexers.Match(path)
	if lexer == nil && sample != "" {
		lexer = lexers.Analyse(sample)
	}
	if lexer == nil || lexer.Config().Name == "plaintext" {
		return nil
	}
	return chroma.Coalesce(lexer)
}

// Tokens tokenizes code; callers fall back to plain text on error.
func Tokens(lexer chroma.Lexer, code string) ([]chroma.Token, error) {
	iterator, err := lexer.Tokenise(nil, code)
	if err != nil {
		return nil, err
	}
	return iterator.Tokens(), nil
}

// Lines tokenizes code as one stream (so multi-line constructs are correct)
// and splits the tokens back into per-line slices.
func Lines(lexer chroma.Lexer, code string) ([][]chroma.Token, error) {
	tokens, err := Tokens(lexer, code)
	if err != nil {
		return nil, err
	}
	lines := [][]chroma.Token{nil}
	for _, t := range tokens {
		for {
			before, after, found := cutNewline(t.Value)
			if before != "" {
				lines[len(lines)-1] = append(lines[len(lines)-1], chroma.Token{Type: t.Type, Value: before})
			}
			if !found {
				break
			}
			lines = append(lines, nil)
			t.Value = after
		}
	}
	return lines, nil
}

func cutNewline(s string) (before, after string, found bool) {
	for i := range len(s) {
		if s[i] == '\n' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}
