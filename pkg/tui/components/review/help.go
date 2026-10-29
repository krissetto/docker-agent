package review

import "github.com/docker/docker-agent/pkg/tui/help"

// Help describes the review's current key context for the host help document.
func (m *Model) Help() (string, []help.Section) {
	entry := func(id string, keys []string, description string) help.Entry {
		return help.Entry{ID: "review." + id, Keys: keys, Description: description}
	}
	common := help.Section{ID: "review.common", Title: "Changes review", Entries: []help.Entry{
		entry("draft", []string{"R"}, "Show or hide the review draft"),
		entry("split", []string{"t"}, "Toggle side-by-side diff (shown when wide enough)"),
		entry("refresh", []string{"r"}, "Refresh repositories and folders"),
		entry("navigator", []string{"ctrl+b"}, "Hide or show the navigator"),
		entry("resize", []string{"+", "-", "="}, "Widen, narrow or reset the navigator"),
		entry("file", []string{"J", "K"}, "Next or previous file"),
		entry("counterpart", []string{"f"}, "Switch between the diff and whole-file view at the closest line"),
		entry("external", []string{"o"}, "Open a local whole file in the configured external editor (Files view)"),
		entry("general-comment", []string{"alt+c"}, "Add a general review comment in the composer"),
		entry("repo", []string{"}", "{"}, "Next or previous repository or worktree"),
		entry("dock", []string{"D"}, "Show or hide the chat beside the review (single-session workspaces)"),
		entry("close", []string{"q"}, "Close review and return to chat"),
		entry("ctrl-w", []string{"ctrl+w"}, "Does nothing here; never closes sessions"),
	}}
	switch {
	case m.filterActive || m.searchOn:
		name := "search"
		if m.filterActive {
			name = "filter"
		}
		return "Changes review — typing " + name, []help.Section{{ID: "review.input", Title: "Review " + name, Entries: []help.Entry{
			entry("input.accept", []string{"enter", "tab"}, "Keep the "+name+" and resume navigation"),
			entry("input.cancel", []string{"esc"}, "Clear the "+name),
			entry("input.move", []string{"up", "down"}, "Move through filtered files (filter only)"),
			entry("input.scope", nil, "Filter searches "+m.filterScope()+"; search covers the open file only"),
			entry("input.tabs", []string{"ctrl+n", "ctrl+p", "ctrl+t"}, "Ignored while typing"),
		}}}
	case m.area == areaReader && m.readerVisible():
		return "Changes review — reader", []help.Section{{ID: "review.reader", Title: "Reader", Entries: []help.Entry{
			entry("reader.move", []string{"up", "down", "k", "j"}, "Move the line cursor"),
			entry("reader.page", []string{"pgup", "pgdown", "home", "end", "g", "G"}, "Page or jump"),
			entry("reader.hunk", []string{"]", "["}, "Next or previous hunk"),
			entry("reader.search", []string{"/"}, "Search this file"),
			entry("reader.match", []string{"n", "N"}, "Next or previous match"),
			entry("reader.select", []string{"drag", "v", "shift+up", "shift+down"}, "Drag to select exact text; v or Shift+↑/↓ selects lines"),
			entry("reader.copy", []string{"x"}, "Copy selected lines as shown, keeping +/- markers"),
			entry("reader.patch", []string{"C"}, "Copy complete hunks as a patch (not for truncated, binary or conflicted files)"),
			entry("reader.path", []string{"y"}, "Copy the file path"),
			entry("reader.comment", []string{"c", "enter"}, "Comment on the selected text or current line (added to the review draft)"),
			entry("reader.expand-comment", []string{"space"}, "Expand or collapse an anchored comment preview"),
			entry("reader.back", []string{"left", "h", "esc"}, "Back to the navigator (Esc clears selection, then search, first)"),
		}}, common}
	}
	return "Changes review — navigator", []help.Section{{ID: "review.navigator", Title: "Navigator", Entries: []help.Entry{
		entry("nav.move", []string{"up", "down", "k", "j"}, "Move"),
		entry("nav.page", []string{"pgup", "pgdown", "home", "end", "g", "G"}, "Page or jump"),
		entry("nav.open", []string{"enter", "right", "l"}, "Open a file in the reader, or expand a group"),
		entry("nav.toggle", []string{"space"}, "Fold a group, or preview a file without moving focus"),
		entry("nav.fold", []string{"left", "h"}, "Collapse, or jump to the parent"),
		entry("nav.filter", []string{"/"}, "Filter paths: changed files in Changes; only already loaded folders in Files"),
		entry("nav.copy", []string{"y"}, "Copy the path"),
		entry("nav.patch", []string{"C"}, "Copy the open file's complete patch"),
		entry("nav.close", []string{"esc"}, "Clear the filter, then close review"),
	}}, common}
}
