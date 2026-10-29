package editor

import (
	"image/color"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/history"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type ghostCell struct {
	text   string
	fg, bg color.Color
}

// ghostCells decodes one rendered row into per-column cells with their SGR colors.
func ghostCells(line string) []ghostCell {
	p := ansi.GetParser()
	defer ansi.PutParser(p)
	var fg, bg color.Color
	var state byte
	var cells []ghostCell
	for line != "" {
		seq, width, n, next := ansi.DecodeSequence(line, state, p)
		if n == 0 {
			break
		}
		if ansi.HasCsiPrefix(seq) && p.Command() == 'm' {
			params := p.Params()
			if len(params) == 0 {
				fg, bg = nil, nil
			}
			for i := 0; i < len(params); i++ {
				switch param := params[i].Param(0); param {
				case 0:
					fg, bg = nil, nil
				case 39:
					fg = nil
				case 49:
					bg = nil
				case 38, 48:
					var c color.Color
					if consumed := ansi.ReadStyleColor(params[i:], &c); consumed > 0 {
						if param == 38 {
							fg = c
						} else {
							bg = c
						}
						i += consumed - 1
					}
				}
			}
		}
		for k := range width {
			text := ""
			if k == 0 {
				text = seq
			}
			cells = append(cells, ghostCell{text: text, fg: fg, bg: bg})
		}
		state, line = next, line[n:]
	}
	return cells
}

func rgba(c color.Color) color.NRGBA {
	if c == nil {
		return color.NRGBA{}
	}
	return color.NRGBAModel.Convert(c).(color.NRGBA)
}

// renderedRowAround returns the cells of the first row containing anchor and the
// column just after anchor, where the ghost suggestion begins.
func renderedRowAround(t *testing.T, view, anchor string) ([]ghostCell, int) {
	t.Helper()
	for row := range strings.SplitSeq(view, "\n") {
		plain := ansi.Strip(row)
		if idx := strings.Index(plain, anchor); idx >= 0 {
			return ghostCells(row), ansi.StringWidth(plain[:idx+len(anchor)])
		}
	}
	require.FailNow(t, "anchor not rendered", "anchor %q missing from view:\n%s", anchor, ansi.Strip(view))
	return nil, 0
}

type ghostScenario struct {
	name       string
	history    string
	value      string
	anchor     string
	typed      string // expected text of the cell before the cursor; empty skips the check
	suggestion string
}

var ghostScenarios = []ghostScenario{
	{name: "ascii", history: "hello world", value: "hello wor", anchor: "hello wor", typed: "r", suggestion: "ld"},
	{name: "space-first", history: "hello world", value: "hello", anchor: "hello", typed: "o", suggestion: " world"},
	{name: "unicode", history: "naïve 界 ok", value: "naïve 界", anchor: "naïve 界", suggestion: " ok"},
	{name: "wide-first", history: "x界", value: "x", anchor: "x", typed: "x", suggestion: "界"},
	{name: "multiline", history: "first\nsecond line", value: "first\nsecond", anchor: "second", typed: "d", suggestion: " line"},
}

var ghostThemes = []string{"black-cyan", "black-amber", "black-violet", "white-cobalt", "default-light", "nord"}

// Not parallel: ApplyTheme mutates package-wide style variables.
func TestHistoryGhostRenderedCursorAndColors(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	for _, themeName := range ghostThemes {
		theme, err := styles.LoadTheme(themeName)
		require.NoError(t, err)
		styles.ApplyTheme(theme)
		for _, sc := range ghostScenarios {
			t.Run(themeName+"/"+sc.name, func(t *testing.T) {
				hist, err := history.NewAtDir(t.TempDir())
				require.NoError(t, err)
				require.NoError(t, hist.Add(sc.history))
				e := New(hist).(*editor)
				e.SetSize(60, 4)
				e.Focus()
				e.SetValue(sc.value)
				require.Equal(t, sc.suggestion, e.suggestion)
				require.True(t, e.hasSuggestion)

				cursorBg := rgba(styles.SuggestionCursorStyle.GetBackground())
				cursorFg := rgba(styles.SuggestionCursorStyle.GetForeground())
				ghostFg := rgba(styles.SuggestionGhostStyle.GetForeground())
				editorBg := rgba(styles.EditorBg)
				require.NotEqual(t, cursorFg, cursorBg, "cursor glyph must contrast with cursor block")
				require.NotEqual(t, ghostFg, editorBg, "ghost text must contrast with editor background")

				view := e.View()
				require.Equal(t, view, e.FreshView(), "cached and fresh renders agree")

				cells, cursor := renderedRowAround(t, view, sc.anchor)
				first, rest := splitFirstGrapheme(sc.suggestion)
				firstWidth := ansi.StringWidth(first)
				require.GreaterOrEqual(t, len(cells), cursor+ansi.StringWidth(sc.suggestion))

				require.NotEqual(t, cursorBg, rgba(cells[cursor-1].bg), "typed text before cursor must not be overwritten by the cursor block")
				if sc.typed != "" {
					require.Equal(t, sc.typed, cells[cursor-1].text, "last typed glyph stays in place")
				}
				require.Equal(t, first, cells[cursor].text, "first suggested glyph sits at cursor column")
				for k := range firstWidth {
					require.Equal(t, cursorBg, rgba(cells[cursor+k].bg), "cursor block column %d", k)
					require.Equal(t, cursorFg, rgba(cells[cursor+k].fg), "cursor glyph column %d", k)
				}
				for k := firstWidth; k < firstWidth+min(ansi.StringWidth(rest), 3); k++ {
					require.Equal(t, ghostFg, rgba(cells[cursor+k].fg), "ghost fg column %d", k)
					require.Equal(t, editorBg, rgba(cells[cursor+k].bg), "ghost bg column %d", k)
				}
				for k := ansi.StringWidth(sc.suggestion); k < ansi.StringWidth(sc.suggestion)+3 && cursor+k < len(cells); k++ {
					require.Equal(t, editorBg, rgba(cells[cursor+k].bg), "background after suggestion column %d", k)
				}
			})
		}
	}
}

// Ghost text that wraps or starts on a new row must follow the frame's origin,
// since the overlay is spliced after framing.
func TestHistoryGhostRendersAcrossVisualRowsInFrame(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	scenarios := []ghostScenario{
		{name: "wrap", history: "ab one two three four five six seven", value: "ab", anchor: "ab", typed: "b"},
		{name: "newline", history: "ab\ncd ef", value: "ab", anchor: "ab", typed: "b"},
		{name: "wrap-second-line", history: "x\nab one two three four five", value: "x\nab", anchor: "ab", typed: "b"},
	}
	for _, themeName := range []string{"black-cyan", "white-cobalt"} {
		theme, err := styles.LoadTheme(themeName)
		require.NoError(t, err)
		styles.ApplyTheme(theme)
		for _, sc := range scenarios {
			t.Run(themeName+"/"+sc.name, func(t *testing.T) {
				hist, err := history.NewAtDir(t.TempDir())
				require.NoError(t, err)
				require.NoError(t, hist.Add(sc.history))
				e := New(hist).(*editor)
				e.SetSize(16, 8)
				e.Focus()
				e.SetValue(sc.value)
				require.True(t, e.hasSuggestion)

				view := e.View()
				require.Equal(t, view, e.FreshView())
				frame := e.Frame()
				originX := frame.GetMarginLeft() + frame.GetBorderLeftSize() + frame.GetPaddingLeft()
				cursorCol := originX + ansi.StringWidth(sc.anchor)

				rows := strings.Split(view, "\n")
				anchorRow := -1
				for i, row := range rows {
					if strings.Contains(ansi.Strip(row), sc.anchor) {
						anchorRow = i
						break
					}
				}
				require.NotEqual(t, -1, anchorRow, "anchor row rendered")

				cursorBg := rgba(styles.SuggestionCursorStyle.GetBackground())
				cursorFg := rgba(styles.SuggestionCursorStyle.GetForeground())
				ghostFg := rgba(styles.SuggestionGhostStyle.GetForeground())
				editorBg := rgba(styles.EditorBg)

				wrapped := e.computeWrappedLines(e.suggestion, ansi.StringWidth(sc.anchor))
				require.GreaterOrEqual(t, len(wrapped), 2, "suggestion spans several visual rows")
				for i, chunk := range wrapped {
					if chunk == "" {
						continue
					}
					row := anchorRow + i
					require.Less(t, row, len(rows))
					plain := ansi.Strip(rows[row])
					col := originX
					if i == 0 {
						col = cursorCol
					}
					got := ansi.Truncate(ansi.TruncateLeft(plain, col, ""), ansi.StringWidth(chunk), "")
					require.Equal(t, chunk, got, "chunk %d placed at frame column %d", i, col)

					cells := ghostCells(rows[row])
					for k := range ansi.StringWidth(chunk) {
						cell := cells[col+k]
						if i == 0 && k == 0 {
							require.Equal(t, cursorBg, rgba(cell.bg), "cursor block")
							require.Equal(t, cursorFg, rgba(cell.fg), "cursor glyph")
							continue
						}
						require.Equal(t, ghostFg, rgba(cell.fg), "ghost fg row %d col %d", i, col+k)
						require.Equal(t, editorBg, rgba(cell.bg), "ghost bg row %d col %d", i, col+k)
					}
				}
				require.Equal(t, sc.typed, ghostCells(rows[anchorRow])[cursorCol-1].text, "typed glyph intact")
			})
		}
	}
}
