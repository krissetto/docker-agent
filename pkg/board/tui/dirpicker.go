package tui

import (
	"os"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// pickerEntryKind distinguishes the rows of the directory picker.
type pickerEntryKind int

const (
	// entryUseDir selects the directory currently being browsed.
	entryUseDir pickerEntryKind = iota
	// entryParent navigates to the parent directory.
	entryParent
	// entryDir navigates into a subdirectory.
	entryDir
)

type pickerEntry struct {
	label string
	path  string
	kind  pickerEntryKind
	git   bool
}

// pickerVisibleRows caps the directory list height.
const pickerVisibleRows = 12

// dirPicker is a minimal directory browser used to pick a project's
// repository: type to filter, enter descends, picking the current directory
// confirms. It is embedded in the projects dialog rather than being a
// dialog itself.
type dirPicker struct {
	dir      string
	entries  []pickerEntry
	filtered []pickerEntry
	selected int
	filter   textinput.Model
	loadErr  error
}

// pickerStartDir returns where browsing starts: the given hint when it is
// an existing directory, otherwise the home directory.
func pickerStartDir(hint string) string {
	if hint != "" {
		if info, err := os.Stat(hint); err == nil && info.IsDir() {
			return hint
		}
	}
	if home := paths.GetHomeDir(); home != "" {
		return home
	}
	return "/"
}

func newDirPicker(start string) *dirPicker {
	ti := textinput.New()
	ti.SetStyles(boardTextinputStyles())
	ti.Placeholder = "Type to filter directories…"
	ti.Focus()
	p := &dirPicker{filter: ti}
	p.load(start)
	return p
}

// load reads dir's subdirectories (hidden ones excluded) and resets the
// filter and selection.
func (p *dirPicker) load(dir string) {
	p.dir = dir
	p.filter.SetValue("")
	p.loadErr = nil
	p.entries = []pickerEntry{{label: "use this directory", path: dir, kind: entryUseDir, git: isGitDir(dir)}}
	if parent := filepath.Dir(dir); parent != dir {
		p.entries = append(p.entries, pickerEntry{label: "..", path: parent, kind: entryParent})
	}

	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		p.loadErr = err
	}
	for _, e := range dirEntries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		p.entries = append(p.entries, pickerEntry{label: e.Name(), path: path, kind: entryDir, git: isGitDir(path)})
	}
	p.applyFilter()
}

// isGitDir cheaply reports whether dir looks like a git repository root.
func isGitDir(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// applyFilter narrows the subdirectory rows to those matching the filter;
// the "use this directory" and ".." rows always stay.
func (p *dirPicker) applyFilter() {
	query := strings.ToLower(strings.TrimSpace(p.filter.Value()))
	p.filtered = p.filtered[:0]
	for _, e := range p.entries {
		if query == "" || e.kind != entryDir || strings.Contains(strings.ToLower(e.label), query) {
			p.filtered = append(p.filtered, e)
		}
	}
	p.selected = min(p.selected, max(len(p.filtered)-1, 0))
}

// Update handles one key press. It returns the chosen directory (empty
// until one is picked) and whether the picker is done (picked or
// cancelled).
func (p *dirPicker) Update(key tea.KeyPressMsg) (chosen string, done bool, cmd tea.Cmd) {
	switch key.String() {
	case "esc":
		return "", true, nil
	case "up":
		p.selected = max(p.selected-1, 0)
		return "", false, nil
	case "down":
		p.selected = min(p.selected+1, max(len(p.filtered)-1, 0))
		return "", false, nil
	case "enter":
		if p.selected >= len(p.filtered) {
			return "", false, nil
		}
		entry := p.filtered[p.selected]
		if entry.kind == entryUseDir {
			return entry.path, true, nil
		}
		p.load(entry.path)
		p.selected = 0
		return "", false, nil
	case "backspace":
		// With an empty filter, backspace walks up one directory.
		if p.filter.Value() == "" {
			if parent := filepath.Dir(p.dir); parent != p.dir {
				p.load(parent)
				p.selected = 0
			}
			return "", false, nil
		}
	}
	p.filter, cmd = p.filter.Update(key)
	p.applyFilter()
	return "", false, cmd
}

// View renders the picker's content (the surrounding dialog chrome is the
// caller's).
func (p *dirPicker) View(width int) string {
	p.filter.SetWidth(width)

	lines := []string{
		styles.MutedStyle.Render(toolcommon.TruncateText(sanitize(p.dir), width)),
		p.filter.View(),
		"",
	}

	// Window the list around the selection.
	start := clamp(p.selected-pickerVisibleRows+1, 0, max(len(p.filtered)-pickerVisibleRows, 0))
	end := min(start+pickerVisibleRows, len(p.filtered))
	for i := start; i < end; i++ {
		lines = append(lines, p.renderEntry(p.filtered[i], i == p.selected, width))
	}
	switch {
	case p.loadErr != nil:
		lines = append(lines, styles.ErrorStyle.Render(toolcommon.TruncateText(sanitize(p.loadErr.Error()), width)))
	case end < len(p.filtered):
		lines = append(lines, styles.MutedStyle.Render(toolcommon.TruncateText("… more", width)))
	}
	return strings.Join(lines, "\n")
}

func (p *dirPicker) renderEntry(entry pickerEntry, selected bool, width int) string {
	marker, style := "  ", styles.BaseStyle
	if selected {
		marker, style = styles.SuccessStyle.Render("❯ "), styles.HighlightWhiteStyle
	}

	var label, suffix string
	switch entry.kind {
	case entryUseDir:
		label = "◉ " + entry.label
		if entry.git {
			suffix = styles.SuccessStyle.Render("  git repository")
		} else {
			suffix = styles.WarningStyle.Render("  not a git repository")
		}
	case entryParent:
		label = "📁 .."
	case entryDir:
		label = "📁 " + sanitize(entry.label)
		if entry.git {
			suffix = styles.SuccessStyle.Render(" ●")
		}
	}
	return marker + style.Render(toolcommon.TruncateText(label, width-4)) + suffix
}
