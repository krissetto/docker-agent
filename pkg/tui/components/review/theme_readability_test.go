package review

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestReaderCommentProseIsNeutralBesideSelectedText(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates package-wide styles.
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })

	r := readerState{}
	doc := Document{Key: DocumentKey{Mode: ModeFiles, Path: "file.txt"}, State: StateReady, Lines: []Line{
		{Kind: LineContext, Old: 1, New: 1, Text: "sample text"},
	}}
	r.setDocument(&doc, false)
	r.anchor = 0
	r.charSelection = &readerCharSelection{start: textPoint{line: 0, column: 0}, end: textPoint{line: 0, column: 6}}
	r.comments = map[int][]commentNote{0: {{Preview: "note", Body: "comment prose", Expanded: true}}}

	rows := r.render(60, 3, true, matcher{})
	require.Len(t, rows, 3)
	assert.Contains(t, rows[0], styles.BaseStyle.Background(styles.Selected).Foreground(styles.SelectedFg).Render("sample"), "the actual selected text retains selection colors")
	assert.Contains(t, rows[2], styles.InfoStyle.Render("   "), "the expanded comment keeps its informational gutter")
	neutral := styles.BaseStyle.Background(styles.CardBg).Foreground(styles.TextPrimary).Render("   comment prose")
	assert.Contains(t, rows[2], neutral, "comment prose uses the neutral card surface and primary text")
	assert.NotContains(t, rows[2], styles.BaseStyle.Background(styles.Selected).Foreground(styles.SelectedFg).Render("   comment prose"), "comment prose is not mistaken for a selected span")
}

func TestReaderDiffAndSearchColorsComposeAcrossBuiltinThemes(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates package-wide styles.
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })

	refs, err := styles.ListThemeRefs()
	require.NoError(t, err)
	for _, ref := range refs {
		if !styles.IsBuiltinTheme(ref) {
			continue
		}
		t.Run(ref, func(t *testing.T) {
			theme, err := styles.LoadTheme(ref)
			require.NoError(t, err)
			styles.ApplyTheme(theme)

			doc := Document{Key: DocumentKey{Mode: ModeChanges, Path: "file.txt"}, State: StateReady, Lines: []Line{
				{Kind: LineHunk, Text: "@@ -1 +1 @@"},
				{Kind: LineRemove, Old: 1, Text: "removed needle"},
				{Kind: LineAdd, New: 1, Text: "added needle"},
			}}
			for _, split := range []bool{false, true} {
				layout := "unified"
				if split {
					layout = "split"
				}
				t.Run(layout, func(t *testing.T) {
					r := readerState{noCursor: true}
					r.setDocument(&doc, false)
					r.setSplit(split)
					rows := r.render(120, 10, false, matcher{})

					// Compare the real row rendering against the live semantic styles;
					// this checks foreground/background together without duplicating palettes.
					if split {
						assert.Contains(t, rows[1], styles.DiffRemoveStyle.Render("removed needle"), "old side keeps its removal colors")
						assert.Contains(t, rows[1], styles.DiffAddStyle.Render("added needle"), "new side keeps its addition colors")
					} else {
						assert.Contains(t, rows[1], styles.DiffRemoveStyle.Render("removed needle"), "removal row keeps its diff colors")
						assert.Contains(t, rows[2], styles.DiffAddStyle.Render("added needle"), "addition row keeps its diff colors")
					}

					searched := r.render(120, r.total, false, newMatcher("needle"))
					removeMatch := styles.DiffRemoveStyle.Reverse(true).Render("needle")
					addMatch := styles.DiffAddStyle.Reverse(true).Render("needle")
					if split {
						assert.Contains(t, searched[1], removeMatch, "search reverse composes over the removed side")
						assert.Contains(t, searched[1], addMatch, "search reverse composes over the added side")
					} else {
						assert.Contains(t, searched[1], removeMatch, "search reverse composes over removal colors")
						assert.Contains(t, searched[2], addMatch, "search reverse composes over addition colors")
						assert.Contains(t, ansi.Strip(strings.Join(searched, "\n")), "added needle")
					}

					r.charSelection = &readerCharSelection{start: textPoint{line: 2, column: 0}, end: textPoint{line: 2, column: 12}}
					selected := r.render(120, 10, false, newMatcher("needle"))
					selectedRow := 2 // Unified rows keep removals and additions separate.
					if split {
						selectedRow = 1 // Split rows pair the removed and added lines.
					}
					selectedSpan := styles.BaseStyle.Background(styles.Selected).Foreground(styles.SelectedFg).Render("added needle")
					assert.Contains(t, selected[selectedRow], selectedSpan, "character selection retains priority and its semantic colors")
					assert.NotContains(t, selected[selectedRow], addMatch, "search does not override an active character selection")
					if split {
						assert.Contains(t, selected[1], removeMatch, "split selection affects the new side without suppressing old-side search")
					}
				})
			}
		})
	}
}
