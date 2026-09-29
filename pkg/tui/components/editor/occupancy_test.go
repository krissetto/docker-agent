package editor

import (
	"image"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/history"
	"github.com/docker/docker-agent/pkg/tui/components/completion"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestOccupiedTextCellsExcludeNonInputSurfaces(t *testing.T) {
	e := New(nil).(*editor)
	e.SetSize(40, 3)
	assert.Empty(t, e.OccupiedTextCells(), "placeholder is not input")
	e.SetValue("   \n ")
	assert.Empty(t, e.OccupiedTextCells(), "blank cursor and whitespace do not obscure glyphs")
	e.SetValue("abc")
	e.suggestion, e.hasSuggestion = strings.Repeat("ghost", 5), true
	assert.Equal(t, []image.Rectangle{image.Rect(0, 0, 3, 1)}, e.OccupiedTextCells())
	e.Blur()
	assert.Empty(t, e.OccupiedTextCells())
	e.Focus()
	assert.Equal(t, []image.Rectangle{image.Rect(0, 0, 3, 1)}, e.OccupiedTextCells())
	e.SetValue("")
	assert.Empty(t, e.OccupiedTextCells())
}

func TestOccupiedTextCellsUnicodeMultilineAndAttachmentTokens(t *testing.T) {
	e := New(nil).(*editor)
	e.SetSize(40, 4)
	e.SetValue("界 e\u0301 👩‍💻\n@paste-1\n  last")
	assert.Equal(t, []image.Rectangle{
		image.Rect(0, 0, 2, 1), image.Rect(3, 0, 4, 1), image.Rect(5, 0, 7, 1),
		image.Rect(0, 1, 8, 2), image.Rect(2, 2, 6, 3),
	}, e.OccupiedTextCells())
}

func TestOccupiedTextCellsFollowWrapViewportAndResize(t *testing.T) {
	e := New(nil).(*editor)
	e.SetSize(8, 2)
	e.SetValue("abcdefghijklmno")
	assert.Equal(t, []image.Rectangle{image.Rect(0, 0, 8, 1), image.Rect(0, 1, 7, 2)}, e.OccupiedTextCells())
	e.SetSize(20, 2)
	assert.Equal(t, []image.Rectangle{image.Rect(0, 0, 15, 1)}, e.OccupiedTextCells())

	e.SetValue("first\nsecond\nlast")
	require.Positive(t, e.textarea.ScrollYOffset())
	assert.Equal(t, []image.Rectangle{image.Rect(0, 0, 6, 1), image.Rect(0, 1, 4, 2)}, e.OccupiedTextCells())
	e.ScrollByWheel(-10)
	assert.Equal(t, []image.Rectangle{image.Rect(0, 0, 5, 1), image.Rect(0, 1, 6, 2)}, e.OccupiedTextCells())
	offset, row, col := e.textarea.ScrollYOffset(), e.textarea.Line(), e.textarea.Column()
	for range 3 {
		_ = e.OccupiedTextCells()
	}
	assert.Equal(t, offset, e.textarea.ScrollYOffset())
	assert.Equal(t, row, e.textarea.Line())
	assert.Equal(t, col, e.textarea.Column())
	assert.Equal(t, "first\nsecond\nlast", e.Value())
}

func TestOccupiedTextCellsNarrowViewport(t *testing.T) {
	for _, width := range []int{1, 2, 3, 8} {
		e := New(nil).(*editor)
		e.SetViewportSize(width, 2)
		e.SetSize(width, 2)
		e.SetValue("界e\u0301abcdefgh")
		cells := e.OccupiedTextCells()
		require.NotEmpty(t, cells)
		for _, cell := range cells {
			assert.True(t, cell.In(image.Rect(0, 0, e.textarea.Width(), e.textarea.Height())), "%v width=%d", cell, width)
		}
	}
}

func TestOccupiedTextCellsCacheReuseAndOwnership(t *testing.T) {
	e := New(nil).(*editor)
	e.SetSize(40, 3)
	e.SetValue("界 e\u0301 👩‍💻\nsecond")
	first := e.OccupiedTextCells()
	require.NotEmpty(t, first)
	cached := &e.occupancy.cells[0]
	for range 3 {
		e.Update(struct{}{}) // unrelated updates and Views do not dirty occupancy
		e.View()
		assert.Equal(t, first, e.OccupiedTextCells())
		require.Same(t, cached, &e.occupancy.cells[0])
	}
	first[0] = image.Rect(99, 99, 100, 100)
	assert.NotEqual(t, first, e.OccupiedTextCells(), "callers cannot mutate the cache")
	previous := e.OccupiedTextCells()
	e.SetValue("replacement")
	e.OccupiedTextCells()
	assert.Equal(t, image.Rect(0, 0, 2, 1), previous[0], "later input cannot mutate retained cells")
}

func TestOccupiedTextCellsCacheInvalidation(t *testing.T) {
	for _, mode := range []string{"input", "insert", "resize", "scroll", "cursor", "blur", "empty"} {
		t.Run(mode, func(t *testing.T) {
			e := New(nil).(*editor)
			e.SetSize(12, 2)
			e.SetValue("first\n界 second\nlast")
			e.OccupiedTextCells()
			cached := &e.occupancy.cells[0]
			switch mode {
			case "input":
				e.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
			case "insert":
				e.InsertText(" more")
			case "resize":
				e.SetSize(7, 3)
			case "scroll":
				e.ScrollByWheel(-10)
			case "cursor":
				e.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
			case "blur":
				e.Blur()
				require.Empty(t, e.OccupiedTextCells())
				require.False(t, e.occupancy.valid)
				e.Focus()
			case "empty":
				e.SetValue("")
				require.Empty(t, e.OccupiedTextCells())
				require.False(t, e.occupancy.valid)
				e.SetValue("new")
			}
			got := e.OccupiedTextCells()
			require.NotEmpty(t, got)
			require.NotSame(t, cached, &e.occupancy.cells[0], "changed state must rescan the viewport")
			e.occupancy = textOccupancyCache{}
			assert.Equal(t, e.OccupiedTextCells(), got, "cached output matches a fresh viewport scan")
		})
	}
}

func BenchmarkOccupiedTextCellsUnchanged(b *testing.B) {
	e := New(nil).(*editor)
	e.SetSize(100, 5)
	e.SetValue(strings.Repeat("a moderately long draft with 界 and 👩‍💻 ", 52))
	e.OccupiedTextCells()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		e.OccupiedTextCells()
	}
}

func TestOccupiedTextCellsCursorVisibilityOnlyChangesStyling(t *testing.T) {
	e := New(nil).(*editor)
	e.SetSize(40, 3)
	e.SetValue("界 e\u0301 👩‍💻")
	// A static visible cursor and a hidden cursor exercise the two render
	// branches also used by blink, including a cursor over a Unicode glyph.
	e.textarea.MoveToBegin()
	s := e.textarea.Styles()
	s.Cursor.Blink = false
	e.textarea.SetStyles(s)
	e.textarea.SetVirtualCursor(true)
	visible := e.OccupiedTextCells()
	view := ansi.Strip(e.textarea.View())
	cached := &e.occupancy.cells[0]
	e.textarea.SetVirtualCursor(false)
	assert.Equal(t, view, ansi.Strip(e.textarea.View()))
	assert.Equal(t, visible, e.OccupiedTextCells())
	require.Same(t, cached, &e.occupancy.cells[0], "cursor visibility need not invalidate glyph occupancy")
	e.occupancy = textOccupancyCache{}
	assert.Equal(t, visible, e.OccupiedTextCells(), "hidden cursor's fresh scan agrees with cached visible cursor")
}

func TestAppendOccupiedTextCellsReuseAndOwnership(t *testing.T) {
	e := New(nil).(*editor)
	e.SetSize(100, 5)
	e.SetValue(strings.Repeat("work item 界 unicode words ", 80))
	origin := image.Pt(3, 20)
	cells := e.AppendOccupiedTextCells(nil, origin)
	want := e.OccupiedTextCells()
	for i := range want {
		want[i] = want[i].Add(origin)
	}
	require.Equal(t, want, cells)
	require.Zero(t, testing.AllocsPerRun(20, func() {
		cells = e.AppendOccupiedTextCells(cells[:0], origin)
	}), "stable occupancy must not rebuild the value or allocate coordinate slices")
	cells[0] = image.Rect(99, 99, 100, 100)
	require.Equal(t, want, e.AppendOccupiedTextCells(nil, origin), "caller storage cannot mutate cached cells")
	e.SetValue("replacement")
	e.AppendOccupiedTextCells(nil, origin)
	require.Equal(t, image.Rect(99, 99, 100, 100), cells[0], "editor updates cannot mutate retained caller storage")
}

func TestOccupiedTextCellsValueSnapshotMutationPaths(t *testing.T) {
	for _, mode := range []string{"replace same length", "insert", "paste", "completion", "suggestion", "history", "search", "send", "attachment"} {
		t.Run(mode, func(t *testing.T) {
			h, err := history.New(t.TempDir())
			require.NoError(t, err)
			require.NoError(t, h.Add("history replacement"))
			e := New(h).(*editor)
			e.SetSize(40, 3)
			e.SetValue("abc def")
			e.OccupiedTextCells()
			switch mode {
			case "replace same length":
				e.SetValue("a bcdef")
			case "insert":
				e.InsertText(" new")
			case "paste":
				e.Update(tea.PasteMsg{Content: " pasted"})
			case "completion":
				e.Update(completion.SelectedMsg{Value: "completed"})
			case "suggestion":
				e.suggestion, e.hasSuggestion = " suggestion", true
				e.AcceptSuggestion()
			case "history":
				e.userTyped = false
				e.Update(tea.KeyPressMsg{Code: tea.KeyUp})
			case "search":
				e.EnterHistorySearch()
				require.Empty(t, e.OccupiedTextCells())
				e.Update(tea.KeyPressMsg{Code: 'h', Text: "h"})
				e.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			case "send":
				e.SendContent()
			case "attachment":
				path := filepath.Join(t.TempDir(), "file.txt")
				require.NoError(t, os.WriteFile(path, []byte("contents"), 0o600))
				require.NoError(t, e.AttachFile(path))
			}
			got := e.OccupiedTextCells()
			e.occupancy, e.occupancyValueValid = textOccupancyCache{}, false
			require.Equal(t, e.OccupiedTextCells(), got, "snapshot result agrees with fresh textarea scan")
		})
	}
}

func TestOccupiedTextCellsThemeAndOverlayPreserveActualViewport(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	e := New(nil).(*editor)
	e.SetSize(12, 2)
	e.SetValue("first\nlast 界")
	e.suggestion, e.hasSuggestion = " suggested words wrapping below", true
	e.OccupiedTextCells()
	e.View() // suggestion rendering may reposition the real viewport
	theme := *original
	theme.Colors.TextMuted = "#123456"
	styles.ApplyTheme(&theme)
	got := e.OccupiedTextCells()
	e.occupancy, e.occupancyValueValid = textOccupancyCache{}, false
	require.Equal(t, e.OccupiedTextCells(), got)
}
