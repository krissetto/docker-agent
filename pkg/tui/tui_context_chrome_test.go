package tui

import (
	"fmt"
	"image/color"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type chromeCell struct {
	glyph  string
	fg, bg color.Color
}

func chromeCells(line string) []chromeCell {
	p := ansi.GetParser()
	defer ansi.PutParser(p)
	var fg, bg color.Color
	var state byte
	var cells []chromeCell
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
				case 38, 48, 58:
					var c color.Color
					if consumed := ansi.ReadStyleColor(params[i:], &c); consumed > 0 {
						if param == 38 {
							fg = c
						}
						if param == 48 {
							bg = c
						}
						i += consumed - 1
					}
				}
			}
		}
		for range width {
			cells = append(cells, chromeCell{seq, fg, bg})
		}
		state, line = next, line[n:]
	}
	return cells
}

func TestContextEditorRenderedHalfCellContinuity(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	// Synthetic legacy ayu registration exercises the compatibility path;
	// these deliberately distinct colors are not the unavailable screenshot palette.
	require.NoError(t, styles.RegisterBuiltinThemes(fstest.MapFS{"themes/ayu-dark-chrome-fixture.yaml": {Data: []byte("name: Ayu legacy fixture\ncolors:\n  tab_bg: '#123456'\n")}}))
	for _, ref := range []string{"default", "default-light", "nord", "ayu-dark-chrome-fixture"} {
		theme, err := styles.LoadTheme(ref)
		require.NoError(t, err)
		styles.ApplyTheme(theme)
		for _, width := range []int{8, 16, 40, 120} {
			for _, used := range []int64{0, 1, 20, 95, 100} {
				t.Run(fmt.Sprintf("%s/%d/%d", ref, width, used), func(t *testing.T) {
					root, _, _ := wallClockRoot(t, width, 30)
					defer root.ar.Stop()
					root.storeContextUsage("profile", "root", runtime.Usage{ContextLength: used, ContextLimit: 100})
					root.contextBar.Cancel()
					root.contextBar.SetContextUsageDirect(used, 100)
					root.viewCacheValid = false
					rows := strings.Split(root.View().Content, "\n")
					_, height := root.editor.GetSize()
					y := root.editorTop() + height
					require.Len(t, rows, 30)
					frame := chromeCells(root.View().Content)
					editor := frame[(y-1)*width : y*width]
					strip := frame[y*width : (y+1)*width]
					require.Len(t, strip, width)
					barWidth := width - 2*styles.EditorHMargin
					label := fmt.Sprintf(" Context %d%%", used)
					if len(label) <= barWidth {
						barWidth -= len(label)
					} else {
						label = ""
					}
					for x := styles.EditorHMargin; x < styles.EditorHMargin+barWidth; x++ {
						require.Equal(t, "▄", strip[x].glyph)
						require.Equal(t, color.RGBAModel.Convert(styles.EditorBg), color.RGBAModel.Convert(strip[x].bg), "upper half x=%d", x)
						require.Equal(t, color.RGBAModel.Convert(editor[x].bg), color.RGBAModel.Convert(strip[x].bg), "seam x=%d", x)
						if used > 0 && x == styles.EditorHMargin {
							require.NotEqual(t, color.RGBAModel.Convert(styles.Background), color.RGBAModel.Convert(strip[x].fg), "positive usage leading lower half")
						}
						if used == 0 {
							require.Equal(t, color.RGBAModel.Convert(styles.Background), color.RGBAModel.Convert(strip[x].fg))
						}
					}
					if label != "" {
						require.Contains(t, ansi.Strip(rows[y]), label)
						for x := styles.EditorHMargin + barWidth; x < width-styles.EditorHMargin; x++ {
							require.Equal(t, styles.Background, strip[x].bg, "label cutout x=%d", x)
						}
					}
					require.Equal(t, len(rows)-2, y, "context strip is directly above the stable message row")
					require.Equal(t, regionMessageBar, root.hitTestRegion(y+1))
					require.NotContains(t, ansi.Strip(root.View().Content), "Ctrl+c quit")
				})
			}
		}
	}
}
