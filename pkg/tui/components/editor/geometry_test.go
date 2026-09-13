package editor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/history"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestExpandedContextGeometryResizeClicksAndDraft(t *testing.T) {
	e := New(nil).(*editor)
	e.SetSize(40, 5)
	for _, name := range []string{"first.txt", "界-second.txt"} {
		path := filepath.Join(t.TempDir(), name)
		require.NoError(t, os.WriteFile(path, []byte(name+"\nexact  text"), 0o600))
		require.NoError(t, e.AttachFile(path))
	}
	draft := "before  界\nsecond line  after"
	e.SetValue(draft)
	e.textarea.SetCursorColumn(4)
	row, col := e.textarea.Line(), e.textarea.Column()
	require.Equal(t, 3, e.BannerHeight())
	e.ToggleContextBar()
	require.Equal(t, 5, e.BannerHeight())
	e.SetContextBarFocused(true)
	require.True(t, e.IsContextBarFocused())

	for _, width := range []int{60, 18, 40} {
		e.SetSize(width, 7)
		view := e.BannerView(width)
		assert.Equal(t, e.BannerHeight(), lipgloss.Height(view))
		assert.LessOrEqual(t, lipgloss.Width(view), width)
		for i, att := range e.attachments {
			preview, ok := e.AttachmentAtPosition(styles.AppPadding, 3+i)
			require.True(t, ok)
			assert.Equal(t, filepath.Base(att.path)+"\nexact  text", preview.Content)
		}
		for _, y := range []int{0, 1, 2, 5} {
			_, ok := e.AttachmentAtPosition(styles.AppPadding, y)
			assert.False(t, ok, "non-pill row %d", y)
		}
		_, ok := e.AttachmentAtPosition(width, 3)
		assert.False(t, ok, "clipped cells cannot activate attachments")
		assert.Equal(t, draft, e.Value())
		assert.Equal(t, row, e.textarea.Line())
		assert.Equal(t, col, e.textarea.Column())
		assert.Equal(t, 7, e.textarea.Height(), "external context does not subtract textarea rows")
	}
	e.ToggleContextBar()
	_ = e.BannerView(100)
	_, ok := e.AttachmentAt(styles.AppPadding)
	assert.True(t, ok, "legacy summary hit remains supported")
	e.banner.SetItems(nil)
	assert.Zero(t, e.BannerHeight())
	assert.False(t, e.IsContextBarFocused())
	_, ok = e.AttachmentAtPosition(styles.AppPadding, 3)
	assert.False(t, ok, "removed attachments invalidate stale hit regions")
}

func TestExpandedEditorResizeFocusInputAndHistoryDraft(t *testing.T) {
	h, err := history.New(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, h.Add("old  entry"))
	e := New(h).(*editor)
	e.SetSize(12, 2)
	draft := "first  wrapped 界 line\nsecond  line"
	e.SetValue(draft)
	e.EnterHistorySearch()
	e.SetSize(20, 8)
	assert.Equal(t, 7, e.textarea.Height(), "search row belongs inside the editor")
	e.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, draft, e.Value())
	e.Blur()
	e.SetSize(16, 5)
	e.Update(tea.MouseClickMsg{X: 1, Y: 1, Button: tea.MouseLeft})
	require.True(t, e.textarea.Focused())
	e.textarea.MoveToEnd()
	e.Update(tea.KeyPressMsg{Text: "!"})
	assert.Equal(t, draft+"!", e.Value(), "focus and resize cannot inject wrapping spaces")
	assert.Equal(t, []string{"old  entry"}, h.Messages, "geometry never records runtime notices or drafts")
	width, height := e.GetSize()
	assert.Equal(t, 16+styles.EditorStyle.GetHorizontalFrameSize(), width)
	assert.Equal(t, 5+styles.EditorStyle.GetVerticalFrameSize(), height)
}

func TestContentLineCountMatchesTextareaWrapBoundaries(t *testing.T) {
	for _, width := range []int{1, 2, 10, 24} {
		for _, value := range []string{"", "abcdefghij", "a  b    c", "界界界", "e\u0301e\u0301e\u0301", "one\ntwo long lines  here\n"} {
			e := New(nil).(*editor)
			e.textarea.SetWidth(width)
			e.SetValue(value)
			want := 0
			for line := range strings.SplitSeq(value, "\n") {
				probe := New(nil).(*editor)
				probe.textarea.SetWidth(width)
				probe.SetValue(line)
				want += probe.textarea.LineInfo().Height
			}
			assert.Equal(t, want, e.ContentLineCount(), "width=%d value=%q", width, value)
			assert.Equal(t, value, e.Value())
		}
	}
}

func BenchmarkEditorContentGeometry(b *testing.B) {
	e := New(nil).(*editor)
	e.SetSize(80, 8)
	e.SetValue(strings.Repeat("fixed  input 界 e\u0301 with wrapping\n", 16))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = e.ContentLineCount()
	}
}

func TestEditorViewWidthIncludesMarginsExactlyOnceAfterResize(t *testing.T) {
	e := New(nil).(*editor)
	draft := strings.Repeat("exact  text 界 e\u0301 ", 12) + "\nlast  line"
	e.SetValue(draft)
	frameWidth := styles.EditorStyle.GetHorizontalFrameSize()
	require.Positive(t, styles.EditorStyle.GetHorizontalMargins(), "exercise the outer-margin contract")

	for _, outerWidth := range []int{120, 24, 80, 120} {
		contentWidth := outerWidth - frameWidth
		e.SetSize(contentWidth, 5)
		assert.Equal(t, contentWidth, e.textarea.Width())
		width, _ := e.GetSize()
		assert.Equal(t, outerWidth, width)
		for _, scroll := range []int{0, -2, 2} {
			e.ScrollByWheel(scroll)
			view := e.View()
			assert.Equal(t, outerWidth, lipgloss.Width(view))
			for row, line := range strings.Split(view, "\n") {
				assert.Equal(t, outerWidth, ansi.StringWidth(line), "outer width=%d row=%d scroll=%d", outerWidth, row, scroll)
			}
			assert.Equal(t, draft, e.Value())
		}
	}
}

func TestNarrowEditorHonorsAllocatedWidth(t *testing.T) {
	for _, width := range []int{1, 2, 8, 10, 40} {
		e := New(nil).(*editor)
		e.SetSize(width, 1)
		require.Equal(t, width, e.textarea.Width())
		_, height := e.GetSize()
		require.Equal(t, height, lipgloss.Height(e.View()), "placeholder must not wrap outside the allocation")
		require.Equal(t, width+styles.EditorStyle.GetHorizontalFrameSize(), lipgloss.Width(e.View()))
	}
}
