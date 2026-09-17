package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/completion"
	"github.com/docker/docker-agent/pkg/tui/components/editor"
)

func TestRootLayerCompositionMatchesExistingCellsAndPaintOrder(t *testing.T) {
	for _, base := range []string{"abcdefghijklmnop", "ab界cd界efghijkl", "\x1b[38;2;120;80;40;48;2;10;20;30mabcdefghijklmnop\x1b[m", "\x1b]8;;https://example.invalid\x1b\\abcdefghijklmnop\x1b]8;;\x1b\\"} {
		for _, x := range []int{0, 2, 3, 5, 14} {
			layers := []*lipgloss.Layer{
				lipgloss.NewLayer(paneClipped(base+"\n"+base, 16, 3)),
				lipgloss.NewLayer("\x1b[38;2;1;2;3;48;2;40;50;60mXY\x1b[m\nQ").X(x).Z(1),
				lipgloss.NewLayer(" ! ").X(4).Y(1).Z(2),
			}
			got := composeRootLayers(layers, 16, 3)
			old := lipgloss.NewCompositor(layers...).Render()
			require.Equal(t, chromeCells(paneClipped(old, 16, 3)), chromeCells(paneClipped(got, 16, 3)), "nonlossy glyph/style cells retain inherited overlay semantics at x=%d", x)
			for line := range strings.SplitSeq(got, "\n") {
				require.LessOrEqual(t, ansi.StringWidth(line), 16)
			}
		}
	}
	layers := []*lipgloss.Layer{lipgloss.NewLayer("base"), lipgloss.NewLayer("first").Z(2), lipgloss.NewLayer("last").Z(2)}
	require.Equal(t, "lastt", ansi.Strip(composeRootLayers(layers, 5, 1)), "equal-Z overlays retain paint order")
}

func TestRootLayerCompositionRetainsExactGraphemesAndLinkBoundaries(t *testing.T) {
	for _, cluster := range []string{"e\u0301", "界", "👩‍💻"} {
		width := ansi.StringWidth(cluster)
		left := "\x1b]8;;https://example.invalid\x1b\\\x1b[38;2;100;150;200m" + cluster + "\x1b[m\x1b]8;;\x1b\\"
		layers := []*lipgloss.Layer{lipgloss.NewLayer(left + " original " + left), lipgloss.NewLayer("popup").X(width + 1)}
		got := composeRootLayers(layers, 24, 2)
		require.Contains(t, got, cluster, "raw grapheme bytes cannot be normalized or serialized away")
		require.Equal(t, 2, strings.Count(got, cluster))
		require.Contains(t, ansi.Strip(got), cluster+" popup")
		before, _, found := strings.Cut(got, "popup")
		require.True(t, found)
		require.Greater(t, strings.LastIndex(before, ansi.ResetHyperlink()), strings.LastIndex(before, "\x1b]8;;https://example.invalid"), "link closes before overlay text")
		for line := range strings.SplitSeq(got, "\n") {
			require.LessOrEqual(t, ansi.StringWidth(line), 24)
		}
	}
	got := composeRootLayers([]*lipgloss.Layer{lipgloss.NewLayer("e\u0301XYZ").X(-1).Y(-1), lipgloss.NewLayer("界e\u0301").X(-1)}, 2, 1)
	require.Equal(t, " e\u0301", ansi.Strip(got), "partial wide glyph is blank, following cluster stays in its exact cell")
}

func TestRootComposerCombiningBytesSurviveLeanAndPopupComposition(t *testing.T) {
	for _, lean := range []bool{false, true} {
		root := splitTestRoot(t)
		root.leanMode = lean
		recorder := &viewportRecordingEditor{Editor: root.editor, ViewportLayout: root.editor.(editor.ViewportLayout)}
		root.editor = recorder
		draft := "e\u0301 界 reply"
		root.editor.SetValue(draft)
		for _, size := range [][2]int{{120, 40}, {40, 10}, {16, 7}, {40, 10}} {
			root.handleWindowResize(size[0], size[1])
			// Keep the canonical decomposed grapheme inside the actual viewport;
			// at16columns the end cursor legitimately scrolls the first word out.
			for range len([]rune(draft)) {
				root.updateEditorCmd(tea.KeyPressMsg{Code: tea.KeyLeft})
			}
			for _, popup := range []bool{false, true} {
				if popup {
					root.updateCompletionsCmd(completion.OpenMsg{Items: []completion.Item{{Label: "candidate e\u0301", Value: draft}}})
				} else {
					root.updateCompletionsCmd(completion.CloseMsg{})
				}
				root.viewCacheValid = false
				editorView := root.editor.View()
				require.Contains(t, ansi.Strip(editorView), "e\u0301")
				require.Equal(t, 1, strings.Count(editorView, "\u0301"), "raw accent byte survives inverse cursor styling")
				frame := root.View().Content
				require.Contains(t, ansi.Strip(frame), "e\u0301", "lean=%v popup=%v size=%v", lean, popup, size)
				rows := strings.Split(frame, "\n")
				_, height := root.editor.GetSize()
				painted := strings.Join(rows[root.editorTop():root.editorTop()+height], "\n")
				require.Equal(t, ansi.Strip(editorView), ansi.Strip(painted), "actual allocated editor cells survive root composition unchanged")
				require.Equal(t, strings.Count(editorView, "\u0301"), strings.Count(painted, "\u0301"), "raw combining bytes stay on their original editor cells")
				require.Len(t, rows, size[1])
				for _, line := range rows {
					require.Equal(t, size[0], ansi.StringWidth(line))
				}
				x := root.editorFrame().GetMarginLeft() + root.editorFrame().GetPaddingLeft()
				y := root.editorTop() + root.editorFrame().GetPaddingTop()
				before := len(recorder.clicks)
				root.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
				require.Len(t, recorder.clicks, before+1)
				require.Zero(t, recorder.clicks[before].X)
				require.Zero(t, recorder.clicks[before].Y)
				require.Equal(t, draft, root.editor.Value())
			}
		}
	}
}

func TestRootCellSliceCombiningMarksFollowTheirOwnedBaseAcrossSGR(t *testing.T) {
	line := "a\x1b[7me\x1b[27m\u0301Z"
	for _, tc := range []struct {
		start, end int
		want       string
	}{{0, 1, "a"}, {1, 2, "e\u0301"}, {2, 3, "Z"}} {
		got := rootCellSlice(line, tc.start, tc.end)
		require.Equal(t, tc.want, ansi.Strip(got))
		require.Equal(t, strings.Count(tc.want, "\u0301"), strings.Count(got, "\u0301"))
		require.Equal(t, tc.end-tc.start, ansi.StringWidth(got))
	}
	got := composeRootLayers([]*lipgloss.Layer{lipgloss.NewLayer(line), lipgloss.NewLayer("X").X(1)}, 3, 1)
	require.Equal(t, "aXZ", ansi.Strip(got), "a covered base's accent cannot migrate onto its replacement")
	require.NotContains(t, got, "\u0301")
}
