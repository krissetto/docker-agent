package ui

import "strings"

const autocompleteMaxRows = 8

// Autocomplete drives the slash-command popup for commands and scoped arguments.
type Autocomplete struct {
	base        []Command
	all         []Command
	matches     []Command
	selected    int
	scopePrefix string
	Active      bool
}

func NewAutocomplete() *Autocomplete {
	return &Autocomplete{}
}

func (a *Autocomplete) SetCommands(cmds []Command) {
	a.base = cmds
	a.all = cmds
	a.scopePrefix = ""
}

// SetScopedCommands temporarily replaces the command list with completions for
// an argument-taking command. prefix excludes the leading slash and includes
// any desired separator, for example "model ".
func (a *Autocomplete) SetScopedCommands(prefix string, cmds []Command) {
	a.all = cmds
	a.scopePrefix = prefix
	a.selected = 0
}

// Sync recomputes the popup state from the current editor text. It returns true
// while the popup is showing.
func (a *Autocomplete) Sync(input string) bool {
	prefix := "/" + a.scopePrefix
	var query string
	switch {
	case a.scopePrefix != "" && strings.HasPrefix(input, prefix) && !strings.Contains(input, "\n"):
		query = strings.TrimPrefix(input, prefix)
	case strings.HasPrefix(input, "/") && !strings.ContainsAny(input, " \n"):
		query = input[1:]
	default:
		a.Active = false
		a.matches = nil
		a.selected = 0
		return false
	}
	if a.scopePrefix == "" {
		a.matches = FilterCommands(a.all, query)
	} else {
		a.matches = FilterScopedCommands(a.all, query)
	}
	a.Active = len(a.matches) > 0
	if a.selected >= len(a.matches) {
		a.selected = len(a.matches) - 1
	}
	if a.selected < 0 {
		a.selected = 0
	}
	return a.Active
}

func (a *Autocomplete) MoveUp() {
	if !a.Active {
		return
	}
	if a.selected > 0 {
		a.selected--
	}
}

func (a *Autocomplete) MoveDown() {
	if !a.Active {
		return
	}
	if a.selected < len(a.matches)-1 {
		a.selected++
	}
}

func (a *Autocomplete) Current() (Command, bool) {
	if !a.Active || a.selected >= len(a.matches) {
		return Command{}, false
	}
	return a.matches[a.selected], true
}

func (a *Autocomplete) Dismiss() {
	a.Active = false
	a.matches = nil
	a.selected = 0
	if a.scopePrefix != "" {
		a.all = a.base
		a.scopePrefix = ""
	}
}

// Completion returns the editor text for a selected completion.
func (a *Autocomplete) Completion(cmd Command) string {
	value := cmd.Value
	if value == "" {
		value = cmd.Name
	}
	return "/" + a.scopePrefix + value
}

// Render returns the popup rows (top to bottom) for the given width.
func (a *Autocomplete) Render(width int) []string {
	return a.RenderRows(width, autocompleteMaxRows)
}

// RenderRows keeps the selected completion visible in the shell's row budget.
func (a *Autocomplete) RenderRows(width, height int) []string {
	if !a.Active || len(a.matches) == 0 || width <= 0 || height <= 0 {
		return nil
	}

	height = min(height, autocompleteMaxRows)
	start := max(0, a.selected-height+1)
	end := min(start+height, len(a.matches))

	nameWidth := 0
	for _, c := range a.matches {
		if w := len(a.scopePrefix) + len(c.Name) + 1; w > nameWidth {
			nameWidth = w
		}
	}
	nameWidth = min(nameWidth, 24)

	var rows []string
	for i := start; i < end; i++ {
		c := a.matches[i]
		name := PadRight(strings.Join(strings.Fields("/"+a.scopePrefix+c.Name), " "), nameWidth)
		line := " " + name + "  " + strings.Join(strings.Fields(c.Desc), " ")
		line = Truncate(line, width)
		if i == a.selected {
			rows = append(rows, lipglossSelected(line, width))
		} else {
			rows = append(rows, StMuted().Render(line))
		}
	}
	return rows
}

// lipglossSelected highlights the selected popup row across the full width.
func lipglossSelected(line string, width int) string {
	padded := PadRight(line, width)
	return StAccent().Bold(true).Render(padded)
}
