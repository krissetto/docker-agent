package editor

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/history"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/widgets/cursor"
)

func TestRetainedEditorMutationsMatchFresh(t *testing.T) {
	h, err := history.New(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, h.Add("hello 界 e\u0301 👩‍💻 from history"))
	e := New(h).(*editor)
	e.SetSize(20, 3)
	steps := []struct {
		name   string
		mutate func()
	}{
		{"initial", func() {}},
		{"suggestion", func() { e.SetValue("hello") }},
		{"type", func() { e.Update(tea.KeyPressMsg{Code: ' ', Text: " "}) }},
		{"cursor", func() { e.Update(tea.KeyPressMsg{Code: tea.KeyLeft}) }},
		{"selection", func() { e.Update(tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModShift}) }},
		{"same value resets selection", func() { e.SetValue(e.Value()) }},
		{"unicode paste", func() { e.Update(tea.PasteMsg{Content: "界 e\u0301 👩‍💻\r\nnext"}) }},
		{"wheel", func() { e.ScrollByWheel(-1) }},
		{"resize", func() { e.SetSize(9, 2) }},
		{"blur", func() { e.Blur() }},
		{"focus", func() { e.Focus() }},
		{"stale blink", func() { e.Update(cursor.BlinkMsg{}) }},
		{"search", func() { e.EnterHistorySearch() }},
		{"query", func() { e.Update(tea.KeyPressMsg{Code: 'h', Text: "h"}) }},
		{"search exit", func() { e.Update(tea.KeyPressMsg{Code: tea.KeyEscape}) }},
		{"recording", func() { e.SetValue(""); e.SetRecording(true) }},
		{"recording tick", func() { e.Update(recordingDotsTickMsg{}) }},
		{"stop recording", func() { e.SetRecording(false) }},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			step.mutate()
			got := e.View()
			before := e.RenderDiagnostics()
			for range 3 {
				require.Equal(t, got, e.View())
			}
			before.Body += 3
			require.Equal(t, before, e.RenderDiagnostics(), "read-only composition reuses widgets/joins but paints the complete frame")
			row, col, offset, selected, value := e.textarea.Line(), e.textarea.Column(), e.textarea.ScrollYOffset(), e.textarea.SelectedText(), e.Value()
			require.Equal(t, got, e.FreshView())
			require.Equal(t, got, e.View(), "fresh render must not invalidate the retained artifact")
			require.Equal(t, row, e.textarea.Line())
			require.Equal(t, col, e.textarea.Column())
			require.Equal(t, offset, e.textarea.ScrollYOffset())
			require.Equal(t, selected, e.textarea.SelectedText())
			require.Equal(t, value, e.Value())
		})
	}
}

func TestRetainedEditorLazyUpdateAndBatching(t *testing.T) {
	e := New(nil).(*editor)
	e.SetSize(40, 3)
	e.View()
	before := e.RenderDiagnostics()
	for range 5 {
		e.Update(struct{}{})
	}
	require.Equal(t, before, e.RenderDiagnostics(), "no-op updates must not eagerly paint widgets")
	e.View()
	after := e.RenderDiagnostics()
	require.Equal(t, before.Textarea+1, after.Textarea, "unknown updates conservatively invalidate, but coalesce")
	require.Equal(t, before.Body+1, after.Body, "authoritative frame always paints")
	require.Equal(t, before.Composition, after.Composition, "equal actual ANSI input reuses inner composition")
	e.InsertText("界")
	e.InsertText("e\u0301")
	e.textarea.MoveToBegin()
	e.textarea.MoveToEnd()
	require.Equal(t, after, e.RenderDiagnostics(), "mutation batches stay lazy")
	e.View()
	require.Equal(t, after.Textarea+1, e.RenderDiagnostics().Textarea)
	require.Greater(t, e.RenderDiagnostics().Composition, after.Composition)
}

func TestEditorInputDetachedFromOwnerAndTheme(t *testing.T) { //nolint:paralleltest // theme globals
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	e := New(nil).(*editor)
	e.SetSize(24, 3)
	e.SetValue("hello 界")
	in := e.captureInput(false)
	want := drawEditor(in)
	e.SetValue("changed")
	e.SetSize(8, 1)
	theme, err := styles.LoadTheme("nord")
	require.NoError(t, err)
	styles.ApplyTheme(theme)
	require.Equal(t, want, drawEditor(in), "snapshot has no model/theme aliases")
	changed := e.View() // no ThemeChangedMsg
	require.Equal(t, changed, e.FreshView())
	require.NotEqual(t, want, changed)
}

func TestEditorFrameMatchesExistingComposite(t *testing.T) { //nolint:paralleltest // style globals
	original := styles.EditorStyle
	t.Cleanup(func() { styles.EditorStyle = original })
	for _, c := range []struct {
		name  string
		style lipgloss.Style
	}{
		{"theme", styles.EditorStyle},
		{"basic", lipgloss.NewStyle().Foreground(ansi.Red).Background(ansi.Blue).Padding(1, 2).Margin(0, 1)},
		{"indexed", lipgloss.NewStyle().Foreground(ansi.IndexedColor(42)).Background(ansi.IndexedColor(123)).Padding(0, 1)},
		{"default", lipgloss.NewStyle()},
		{"bold", original.Bold(true)},
		{"border", original.Border(lipgloss.RoundedBorder()).BorderForeground(ansi.Red)},
		{"alignment", original.Align(lipgloss.Center, lipgloss.Bottom).Height(9)},
		{"transform", original.Transform(func(s string) string { return "prefix " + strings.ReplaceAll(s, "link", "LINK") })},
		{"margin background", original.MarginBackground(ansi.Green)},
		{"whitespace off", original.ColorWhitespace(false)},
		{"custom padding and margins", original.PaddingChar('.').MarginChar('-')},
		{"hyperlink", original.Hyperlink("https://example.org")},
		{"underlying text", original.SetString("prefix")},
	} {
		t.Run(c.name, func(t *testing.T) {
			content := "\x1b[1m界 e\u0301\x1b[0m\n\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\"
			styles.EditorStyle = c.style
			e := New(nil).(*editor)
			e.SetSize(20, 3)
			want := styles.RenderComposite(c.style.Width(20+c.style.GetHorizontalPadding()+c.style.GetHorizontalBorderSize()), content)
			require.Equal(t, want, e.materializeFrame(content))
			e.SetValue("visible 界 text")
			got := e.View()
			require.Equal(t, got, e.FreshView())
		})
	}
}

func TestRetainedBannerReflowsBeforePaint(t *testing.T) {
	e := New(nil).(*editor)
	e.banner.SetSize(60)
	e.banner.SetItems([]bannerItem{{label: "界-file (2 KB)", placeholder: "@file"}})
	regions := slices.Clone(e.banner.regions)
	require.NotEmpty(t, regions, "mutation prepares hit regions before painting")
	got := e.BannerView(60)
	before := e.RenderDiagnostics()
	require.Equal(t, got, e.BannerView(60))
	require.Equal(t, before, e.RenderDiagnostics())
	require.Equal(t, regions, e.banner.regions)
	require.Equal(t, got, e.FreshBannerView(60))
	for _, mutate := range []func(){func() { e.ToggleContextBar() }, func() { e.SetContextBarFocused(true) }, func() { e.SetBannerMaxHeight(1) }, func() { e.banner.SetSize(8) }, func() { e.banner.SetItems(nil) }} {
		mutate()
		regions = slices.Clone(e.banner.regions)
		got = e.BannerView(e.banner.width)
		require.Equal(t, regions, e.banner.regions, "painting cannot rebuild geometry")
		require.Equal(t, got, e.FreshBannerView(e.banner.width))
	}
}

func TestRetainedSuggestionAtScrolledEnd(t *testing.T) {
	e := New(nil).(*editor)
	e.SetSize(12, 2)
	e.SetValue(strings.Repeat("界 hello\n", 10) + "end")
	e.suggestion, e.hasSuggestion = " suggested e\u0301", true
	got := e.View()
	before := e.RenderDiagnostics()
	require.Equal(t, got, e.View())
	before.Body++
	require.Equal(t, before, e.RenderDiagnostics(), "suggestion must not trigger viewport normalization each paint")
	require.Equal(t, got, e.FreshView())
}
