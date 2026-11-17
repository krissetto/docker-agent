package markdown

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const codeFG = "\x1b[38;2;255;206;165;48;2;29;29;29m"

// inlineWrapStyleStates returns the SGR state of every visible non-space rune.
// State resets at each newline because wrapped lines must reopen their styles.
func inlineWrapStyleStates(s string) []string {
	var out []string
	state := map[string]string{}
	for i := 0; i < len(s); {
		if s[i] == '\x1b' {
			end, kind := scanAnsiSequence(s, i)
			if kind == '[' && end-i >= 3 && s[end-1] == 'm' {
				applyInlineWrapSGR(state, s[i+2:end-1])
			}
			i = end
			continue
		}
		switch s[i] {
		case '\n':
			state = map[string]string{}
		case ' ':
		default:
			out = append(out, inlineWrapStateKey(state)+"|"+string(s[i]))
		}
		i++
	}
	return out
}

func applyInlineWrapSGR(state map[string]string, body string) {
	parts := strings.Split(body, ";")
	for j := 0; j < len(parts); j++ {
		switch parts[j] {
		case "", "0":
			clear(state)
		case "1":
			state["bold"] = "1"
		case "22":
			delete(state, "bold")
		case "3":
			state["italic"] = "1"
		case "23":
			delete(state, "italic")
		case "9":
			state["strike"] = "1"
		case "29":
			delete(state, "strike")
		case "39":
			delete(state, "fg")
		case "49":
			delete(state, "bg")
		case "38", "48":
			if j+4 < len(parts) && parts[j+1] == "2" {
				key := "fg"
				if parts[j] == "48" {
					key = "bg"
				}
				state[key] = strings.Join(parts[j+2:j+5], ",")
				j += 4
			}
		}
	}
}

func inlineWrapStateKey(state map[string]string) string {
	var b strings.Builder
	for _, k := range []string{"fg", "bg", "bold", "italic", "strike"} {
		if v := state[k]; v != "" {
			fmt.Fprintf(&b, "%s=%s;", k, v)
		}
	}
	return b.String()
}

// Wrapping must not change the style of any rune, including inline code and
// the styles around it, whatever the break position.
func TestInlineCodeWrapKeepsRuneStyles(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"see `alpha beta gamma delta epsilon zeta eta theta` ok",
		"see `averyveryverylongidentifiernothingfitsatall` ok",
		"a `bc de` fg",
		"`ab cd` ef gh ij",
		"# heading with `code alpha beta gamma delta` tail",
		"> quote `code alpha beta gamma delta epsilon` end",
		"- item `code alpha beta gamma delta` end",
		"**bold `code alpha beta gamma delta epsilon` bold end**",
		"~~strike `code alpha beta gamma delta` end~~",
		"_it `code wrap here` tail_",
	}
	for _, in := range inputs {
		unwrapped, err := NewFastRenderer(500).Render(in)
		require.NoError(t, err)
		want := inlineWrapStyleStates(unwrapped)
		for width := 4; width <= 24; width++ {
			wrapped, err := NewFastRenderer(width).Render(in)
			require.NoError(t, err)
			assert.Equal(t, want, inlineWrapStyleStates(wrapped), "input %q width %d:\n%s", in, width, wrapped)
		}
	}
}

// Regression: the word closing a code span carries its reset; the break before it
// must reopen the code style, not drop it.
func TestInlineCodeWrapReopensStyleBeforeClosingWord(t *testing.T) {
	t.Parallel()

	out, err := NewFastRenderer(10).Render("see `alpha beta gamma delta epsilon zeta eta theta` ok")
	require.NoError(t, err)

	idx := slices.IndexFunc(strings.Split(out, "\n"), func(l string) bool { return strings.Contains(stripANSI(l), "theta") })
	require.NotEqual(t, -1, idx, "wrapped word not rendered")
	line := strings.Split(out, "\n")[idx]
	assert.Contains(t, line, codeFG+"theta", "wrapped code word lost its style: %q", line)
}

func TestInlineCodeWrapRandomizedRuneStyles(t *testing.T) {
	t.Parallel()

	pieces := []string{
		"see ", "`code alpha beta gamma delta`", "`averyveryverylongidentifiernothingfits`",
		" word ", "**bold** ", "*it* ", "`x`", "tail.", "`a b`,", "longerwordhere",
		"\n\n", "\n- ", "\n> ", "\n# ", "- item ", "> quote ",
		"**`bold code wrap words here`** ", "~~`strike code wrap here`~~ ", "_`it code wrap here`_ ",
	}
	r := rand.New(rand.NewSource(1))
	for range 1500 {
		var sb strings.Builder
		for range 2 + r.Intn(6) {
			sb.WriteString(pieces[r.Intn(len(pieces))])
		}
		in := sb.String()
		width := 5 + r.Intn(30)

		unwrapped, err := NewFastRenderer(500).Render(in)
		require.NoError(t, err)
		wrapped, err := NewFastRenderer(width).Render(in)
		require.NoError(t, err)
		require.Equal(t, inlineWrapStyleStates(unwrapped), inlineWrapStyleStates(wrapped), "input %q width %d", in, width)
	}
}
