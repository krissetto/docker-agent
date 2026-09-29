package dialog

import (
	"fmt"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/tui/widgets/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/help"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// helpDialog owns a detached snapshot, never the underlying input or action state.
type helpDialog struct {
	readOnlyScrollDialog

	document help.Document
	page     int
}

// NewHelpDialog displays the captured context first, then named reference pages.
func NewHelpDialog(document help.Document) Dialog {
	d := &helpDialog{document: help.Document{
		Context:   document.Context,
		Current:   copyHelpSections(document.Current),
		Reference: copyHelpSections(document.Reference),
	}}
	d.readOnlyScrollDialog = newReadOnlyScrollDialog(
		readOnlyScrollDialogSize{
			widthPercent: 70, minWidth: 60, maxWidth: 120,
			heightPercent: 80, heightMax: 40,
		}, d.renderContent,
	)
	return d
}

// IsHelpDialog lets the owner avoid stacking Help over itself.
func IsHelpDialog(d Dialog) bool {
	_, ok := d.(*helpDialog)
	return ok
}

// copyHelpSections also deduplicates aliases and repeated action identities within
// the same context section, without merging unrelated actions sharing a key.
func copyHelpSections(sections []help.Section) []help.Section {
	out := make([]help.Section, 0, len(sections))
	for _, section := range sections {
		copySection := help.Section{ID: section.ID, Title: section.Title}
		seen := make(map[string]int)
		for _, entry := range section.Entries {
			identity := entry.ID + "\x00" + entry.Condition
			if entry.ID == "" {
				identity += "\x00" + entry.Description
			}
			i, exists := seen[identity]
			if !exists {
				i = len(copySection.Entries)
				seen[identity] = i
				copySection.Entries = append(copySection.Entries, help.Entry{
					ID: entry.ID, Description: entry.Description, Condition: entry.Condition,
				})
			}
			for _, alias := range entry.Keys {
				if alias != "" && !slices.Contains(copySection.Entries[i].Keys, alias) {
					copySection.Entries[i].Keys = append(copySection.Entries[i].Keys, alias)
				}
			}
		}
		out = append(out, copySection)
	}
	return out
}

func (d *helpDialog) renderContent(contentWidth, _ int) []string {
	width := max(1, contentWidth)
	pageTitle := "This context"
	sections := d.document.Current
	if d.page > 0 {
		sections = d.document.Reference[d.page-1 : d.page]
		pageTitle = "Reference · " + sections[0].Title
	}
	// Exactly three bounded header rows retain the shared read-only layout.
	lines := []string{
		styles.DialogTitleStyle.Render(ansi.Truncate("Help · "+pageTitle, width, "…")),
		styles.DialogSeparatorStyle.Render(strings.Repeat("─", width)),
		styles.DialogHelpStyle.Render(ansi.Truncate(fmt.Sprintf("%d/%d · ←/→ categories · ↑/↓ scroll", d.page+1, len(d.document.Reference)+1), width, "…")),
	}
	appendText := func(text string, indent int, heading bool) {
		indent = min(indent, max(0, width-1))
		wrapped := ansi.Hardwrap(ansi.Wrap(text, width-indent, ""), width-indent, false)
		style := styles.DialogHelpStyle
		if heading {
			style = style.Bold(true).Foreground(styles.TextSecondary)
		}
		for line := range strings.SplitSeq(wrapped, "\n") {
			lines = append(lines, strings.Repeat(" ", indent)+style.Render(ansi.Truncate(line, width-indent, "")))
		}
	}
	if d.page == 0 {
		appendText("Captured context: "+d.document.Context, 0, true)
		appendText("These controls describe the view beneath Help. Dismiss Help to use them.", 0, false)
	} else {
		appendText("Other-context reference — not all controls are active in the captured view.", 0, false)
	}
	if d.page == 0 {
		appendText("Help: ←/→ categories; ↑/↓ or j/k scroll; pgup/pgdown pages; home/end limits; wheel or scrollbar scroll; enter/q/esc or × dismiss. F1 keeps this Help open.", 0, false)
	} else {
		appendText("pgup/pgdown scroll pages · enter/q/esc dismiss Help", 0, false)
	}
	for _, section := range sections {
		lines = append(lines, "")
		appendText(section.Title, 0, true)
		for _, entry := range section.Entries {
			appendText(strings.Join(entry.Keys, " / "), 0, true)
			appendText(entry.Description, 2, false)
			if entry.Condition != "" {
				appendText("When: "+entry.Condition, 2, false)
			}
		}
	}
	return lines
}

func (d *helpDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok && k.Mod == 0 {
		delta := 0
		switch k.Code {
		case tea.KeyLeft:
			delta = -1
		case tea.KeyRight:
			delta = 1
		}
		if delta != 0 {
			d.page = (d.page + delta + len(d.document.Reference) + 1) % (len(d.document.Reference) + 1)
			d.scrollview.ScrollToTop()
			d.renderBody(true)
			d.MarkVisualDirty()
			return d, nil
		}
	}
	_, cmd := d.readOnlyScrollDialog.Update(msg)
	return d, cmd
}

func (d *helpDialog) Bindings() []key.Binding { return nil }
