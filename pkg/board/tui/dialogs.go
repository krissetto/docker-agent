package tui

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/board"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/tui/components/scrollbar"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	tuidialog "github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/widgets/key"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// closeDialog is the command every dialog uses to dismiss itself.
func closeDialog() tea.Msg { return closeDialogMsg{} }

// configPathHint is where the board's configuration lives, with the home
// directory abbreviated to ~.
func configPathHint() string {
	path := userconfig.Path()
	if home := paths.GetHomeDir(); home != "" {
		if rest, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok {
			return "~" + string(filepath.Separator) + rest
		}
	}
	return path
}

// dialogWidth clamps a preferred dialog width to the terminal.
func dialogWidth(preferred, termWidth int) int {
	return max(min(preferred, termWidth-4), 24)
}

// renderDialog renders a titled dialog box with the same chrome as the
// main TUI's dialogs (centered title, DialogStyle border and padding).
func renderDialog(title string, width int, sections ...string) string {
	content := tuidialog.NewContent(width).AddTitle(title).AddSpace()
	for _, s := range sections {
		content.AddContent(s)
	}
	return styles.DialogStyle.Render(content.Build())
}

// helpLine renders key-binding help in the same style as the main TUI's
// dialogs. bindings are (key, description) pairs.
func helpLine(width int, bindings ...string) string {
	return tuidialog.RenderHelpKeys(width, bindings...)
}

// --- new card dialog ---

// cardDialog collects the project and the first prompt of a new card.
type cardDialog struct {
	projects []board.Project
	projIdx  int
	prompt   textarea.Model

	// chipRowY is the absolute screen row of the project selector and
	// chipSpans each project name's clickable x-range, recorded by View so
	// Update can map clicks to projects.
	chipRowY  int
	chipSpans [][2]int
}

// newCardDialog collects the project and the first prompt of a new card.
// The selector starts on lastProject (by name) when it is still configured.
func newCardDialog(projects []board.Project, lastProject string) *cardDialog {
	ta := textarea.New()
	ta.SetStyles(styles.InputStyle)
	ta.Placeholder = "Describe the task for the agent…"
	ta.ShowLineNumbers = false
	ta.SetHeight(6)
	// Enter starts the agent; shift+enter inserts a newline (ctrl+j for
	// terminals that cannot distinguish shift+enter from enter).
	ta.KeyMap.InsertNewline.SetKeys("shift+enter", "ctrl+j")
	ta.Focus()

	d := &cardDialog{projects: projects, prompt: ta}
	for i, p := range projects {
		if p.Name == lastProject {
			d.projIdx = i
			break
		}
	}
	return d
}

func (d *cardDialog) Init() tea.Cmd { return textarea.Blink }

func (d *cardDialog) Update(msg tea.Msg) (dialog, tea.Cmd) {
	if click, ok := msg.(tea.MouseClickMsg); ok && click.Button == tea.MouseLeft && click.Y == d.chipRowY {
		for i, span := range d.chipSpans {
			if click.X >= span[0] && click.X < span[1] {
				d.projIdx = i
				return d, nil
			}
		}
	}
	if msg, ok := msg.(tea.KeyPressMsg); ok {
		switch msg.String() {
		case "esc":
			return d, closeDialog
		case "tab":
			if len(d.projects) > 1 {
				d.projIdx = (d.projIdx + 1) % len(d.projects)
				return d, nil
			}
		case "shift+tab":
			if len(d.projects) > 1 {
				d.projIdx = (d.projIdx + len(d.projects) - 1) % len(d.projects)
				return d, nil
			}
		case "enter":
			prompt := strings.TrimSpace(d.prompt.Value())
			if prompt == "" {
				return d, nil
			}
			project := d.projects[d.projIdx]
			return d, func() tea.Msg { return submitNewCardMsg{project: project, prompt: prompt} }
		}
	}
	var cmd tea.Cmd
	d.prompt, cmd = d.prompt.Update(msg)
	return d, cmd
}

func (d *cardDialog) View(width, height int) string {
	w := dialogWidth(100, width)
	d.prompt.SetWidth(w)
	// Give the prompt most of the screen: long task descriptions are the
	// norm, not the exception.
	d.prompt.SetHeight(max(min(height-12, 24), 6))

	sections := []string{
		d.prompt.View(),
		"",
	}
	hints := []string{"enter", "start", "shift+enter", "newline", "esc", "cancel"}

	// With a single project there is nothing to choose: skip the selector.
	d.chipRowY = -1
	if len(d.projects) > 1 {
		const label = "Project  "
		chips := make([]string, 0, len(d.projects))
		spans := make([][2]int, 0, len(d.projects))
		x := lipgloss.Width(label)
		for i, p := range d.projects {
			style := styles.MutedStyle
			if i == d.projIdx {
				style = styles.BaseStyle.Foreground(projectColorAt(i)).Bold(true).Underline(true)
			}
			name := sanitize(p.Name)
			spans = append(spans, [2]int{x, x + lipgloss.Width(name)})
			x += lipgloss.Width(name) + lipgloss.Width("  ·  ")
			chips = append(chips, style.Render(name))
		}
		d.chipSpans = spans
		projectLine := styles.SecondaryStyle.Render(label) + strings.Join(chips, styles.MutedStyle.Render("  ·  "))
		sections = append([]string{toolcommon.TruncateText(projectLine, w), ""}, sections...)
		hints = []string{"enter", "start", "shift+enter", "newline", "tab", "project", "esc", "cancel"}
	}

	sections = append(sections, helpLine(w, hints...))
	out := renderDialog("Start an agent", w, sections...)

	// Translate the chip spans to absolute screen coordinates, mirroring
	// placeOverlay's centering. Inside the dialog: border (1) + padding (2)
	// on the left; border + padding + title + spacer rows above.
	if len(d.projects) > 1 {
		dx := max((width-lipgloss.Width(out))/2, 0)
		d.chipRowY = max((height-lipgloss.Height(out))/2, 0) + 4
		for i := range d.chipSpans {
			d.chipSpans[i][0] += dx + 3
			d.chipSpans[i][1] += dx + 3
		}
	}
	return out
}

// --- column prompt editor ---

// promptDialog edits the prompt a column sends to incoming cards; the result
// is persisted to the user's global config file.
type promptDialog struct {
	column board.Column
	prompt textarea.Model
}

func newPromptDialog(column board.Column) *promptDialog {
	// The column's name and emoji come from the hand-editable config file:
	// strip terminal controls (and collapse whitespace) before rendering,
	// like every other column render site.
	column.Name = strings.Join(strings.Fields(sanitize(column.Name)), " ")
	column.Emoji = strings.Join(strings.Fields(sanitize(column.Emoji)), " ")

	ta := textarea.New()
	ta.SetStyles(styles.InputStyle)
	ta.Placeholder = "Prompt sent to a card's agent when it enters " + column.Name + "…"
	ta.ShowLineNumbers = false
	ta.SetHeight(10)
	ta.KeyMap.InsertNewline.SetKeys("shift+enter", "ctrl+j")
	ta.SetValue(column.Prompt)
	ta.Focus()
	return &promptDialog{column: column, prompt: ta}
}

func (d *promptDialog) Init() tea.Cmd { return textarea.Blink }

func (d *promptDialog) Update(msg tea.Msg) (dialog, tea.Cmd) {
	if msg, ok := msg.(tea.KeyPressMsg); ok {
		switch msg.String() {
		case "esc":
			return d, closeDialog
		case "enter":
			colID, prompt := d.column.ID, strings.TrimSpace(d.prompt.Value())
			return d, func() tea.Msg { return submitPromptMsg{colID: colID, prompt: prompt} }
		}
	}
	var cmd tea.Cmd
	d.prompt, cmd = d.prompt.Update(msg)
	return d, cmd
}

func (d *promptDialog) View(width, height int) string {
	w := dialogWidth(90, width)
	d.prompt.SetWidth(w)
	d.prompt.SetHeight(max(min(height-10, 16), 4))
	return renderDialog(strings.TrimSpace(d.column.Emoji+" "+d.column.Name)+" · column prompt", w,
		d.prompt.View(),
		"",
		helpLine(w, "enter", "save", "shift+enter", "newline", "esc", "cancel"),
	)
}

// --- columns manager ---

// columnsMode is the columns dialog's active view.
type columnsMode int

const (
	columnsList columnsMode = iota
	columnsEditing
	columnsConfirming
)

// columnsDialog lists and edits the pipeline's columns, stored in the
// user's global config file. Editing keeps the column's id (and prompt),
// so existing cards stay attached across a rename. Removal asks for
// confirmation first.
type columnsDialog struct {
	columns []board.Column
	idx     int

	mode   columnsMode
	inputs []textinput.Model // name, emoji
	focus  int
	// editing is the column being edited (its id and prompt survive the
	// form); the zero value means the form adds a new column.
	editing board.Column
	// deleting is the column awaiting delete confirmation.
	deleting    board.Column
	confirmKeys tuidialog.ConfirmKeyMap
}

func newColumnsDialog(columns []board.Column) *columnsDialog {
	return &columnsDialog{columns: columns, confirmKeys: tuidialog.DefaultConfirmKeyMap()}
}

// setColumns refreshes the list after an add, edit or delete and returns
// to the list view, keeping the cursor position (clamped).
func (d *columnsDialog) setColumns(columns []board.Column) {
	d.refreshColumns(columns)
	d.mode = columnsList
	d.editing = board.Column{}
	d.deleting = board.Column{}
	d.inputs = nil
}

// refreshColumns updates the list data without leaving the current view.
func (d *columnsDialog) refreshColumns(columns []board.Column) {
	d.columns = columns
	d.idx = min(max(d.idx, 0), max(len(columns)-1, 0))
}

// selectColumn moves the cursor to the column with the given id, if present.
func (d *columnsDialog) selectColumn(id string) {
	if i := slices.IndexFunc(d.columns, func(c board.Column) bool { return c.ID == id }); i >= 0 {
		d.idx = i
	}
}

var columnFields = []struct{ label, placeholder string }{
	{"Name", "Review"},
	{"Emoji", "\U0001f50d (optional)"},
}

// startForm opens the add/edit form. editing is the column being edited
// (the zero value when adding); its name and emoji pre-fill the fields.
func (d *columnsDialog) startForm(editing board.Column) tea.Cmd {
	d.mode = columnsEditing
	d.editing = editing
	d.focus = 0
	d.inputs = make([]textinput.Model, len(columnFields))
	for i, f := range columnFields {
		ti := textinput.New()
		ti.SetStyles(styles.DialogInputStyle)
		ti.Placeholder = f.placeholder
		ti.SetWidth(56)
		d.inputs[i] = ti
	}
	d.inputs[0].SetValue(editing.Name)
	d.inputs[1].SetValue(editing.Emoji)
	d.inputs[0].Focus()
	return textinput.Blink
}

func (d *columnsDialog) Init() tea.Cmd { return nil }

func (d *columnsDialog) Update(msg tea.Msg) (dialog, tea.Cmd) {
	press, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return d, nil
	}
	switch d.mode {
	case columnsEditing:
		return d.updateEditing(press)
	case columnsConfirming:
		return d.updateConfirming(press)
	}

	switch press.String() {
	case "esc", "q":
		return d, closeDialog
	case "up", "k":
		d.idx = max(d.idx-1, 0)
	case "down", "j":
		d.idx = min(d.idx+1, max(len(d.columns)-1, 0))
	case "a", "n":
		cmd := d.startForm(board.Column{})
		return d, cmd
	case "e", "enter":
		if len(d.columns) > 0 {
			cmd := d.startForm(d.columns[d.idx])
			return d, cmd
		}
	case "p":
		// The prompt is long-form: hand it to the dedicated prompt editor.
		if len(d.columns) > 0 {
			column := d.columns[d.idx]
			return d, func() tea.Msg { return editColumnPromptMsg{column: column} }
		}
	case "shift+up", "K":
		cmd := d.moveColumn(-1)
		return d, cmd
	case "shift+down", "J":
		cmd := d.moveColumn(1)
		return d, cmd
	case "x", "d", "backspace", "delete":
		if len(d.columns) > 0 {
			d.mode = columnsConfirming
			d.deleting = d.columns[d.idx]
		}
	}
	return d, nil
}

// updateConfirming handles the delete confirmation prompt.
func (d *columnsDialog) updateConfirming(press tea.KeyPressMsg) (dialog, tea.Cmd) {
	switch {
	case key.Matches(press, d.confirmKeys.Yes), press.String() == "enter":
		id := d.deleting.ID
		d.mode = columnsList
		d.deleting = board.Column{}
		return d, func() tea.Msg { return deleteColumnMsg{id: id} }
	case key.Matches(press, d.confirmKeys.No), press.String() == "esc", press.String() == "q":
		d.mode = columnsList
		d.deleting = board.Column{}
	}
	return d, nil
}

// moveColumn asks the model to reorder the selected column by delta
// positions; the model persists the order and moves the cursor along.
func (d *columnsDialog) moveColumn(delta int) tea.Cmd {
	if len(d.columns) == 0 {
		return nil
	}
	id := d.columns[d.idx].ID
	return func() tea.Msg { return moveColumnMsg{id: id, delta: delta} }
}

func (d *columnsDialog) updateEditing(press tea.KeyPressMsg) (dialog, tea.Cmd) {
	switch press.String() {
	case "esc":
		d.mode = columnsList
		d.editing = board.Column{}
		d.inputs = nil
		return d, nil
	case "tab", "down":
		cmd := d.setFocus((d.focus + 1) % len(d.inputs))
		return d, cmd
	case "shift+tab", "up":
		cmd := d.setFocus((d.focus + len(d.inputs) - 1) % len(d.inputs))
		return d, cmd
	case "enter":
		// The id and prompt ride along untouched: the form only edits the
		// display fields.
		column := d.editing
		column.Name = strings.TrimSpace(d.inputs[0].Value())
		column.Emoji = strings.TrimSpace(d.inputs[1].Value())
		oldID := d.editing.ID
		return d, func() tea.Msg { return submitColumnMsg{column: column, oldID: oldID} }
	}
	var cmd tea.Cmd
	d.inputs[d.focus], cmd = d.inputs[d.focus].Update(press)
	return d, cmd
}

func (d *columnsDialog) setFocus(focus int) tea.Cmd {
	d.inputs[d.focus].Blur()
	d.focus = focus
	return d.inputs[d.focus].Focus()
}

func (d *columnsDialog) View(width, _ int) string {
	w := dialogWidth(70, width)
	switch d.mode {
	case columnsEditing:
		return d.viewForm(w)
	case columnsConfirming:
		return renderDialog("Remove column?", w,
			styles.BaseStyle.Render(toolcommon.TruncateText(sanitize(strings.TrimSpace(d.deleting.Emoji+" "+d.deleting.Name)), w)),
			styles.MutedStyle.Render("A column that still has cards cannot be removed."),
			"",
			helpLine(w, d.confirmKeys.Yes.Help().Key, "remove", d.confirmKeys.No.Help().Key+"/esc", "cancel"),
		)
	}

	rows := make([]string, 0, len(d.columns))
	for i, c := range d.columns {
		marker, nameStyle := "  ", styles.BaseStyle.Foreground(columnHeaderColor(i, len(d.columns)))
		if i == d.idx {
			marker, nameStyle = styles.SuccessStyle.Render("\u276f "), nameStyle.Bold(true)
		}
		line := marker + nameStyle.Render(sanitize(strings.TrimSpace(c.Emoji+" "+c.Name)))
		if prompt := strings.Join(strings.Fields(sanitize(c.Prompt)), " "); prompt != "" {
			line += styles.MutedStyle.Render("  " + prompt)
		}
		rows = append(rows, toolcommon.TruncateText(line, w))
	}

	return renderDialog("Columns", w,
		lipgloss.JoinVertical(lipgloss.Left, rows...),
		"",
		helpLine(w, "a", "add", "e", "edit", "p", "prompt", "x", "remove", "shift+\u2191\u2193", "reorder", "\u2191\u2193", "select", "esc", "close"),
	)
}

func (d *columnsDialog) viewForm(w int) string {
	rows := make([]string, 0, len(columnFields)*2)
	for i, f := range columnFields {
		label := styles.SecondaryStyle.Render(f.label)
		if i == d.focus {
			label = styles.HighlightWhiteStyle.Render(f.label)
		}
		rows = append(rows, label, d.inputs[i].View())
	}
	title := "Add column"
	if d.editing.ID != "" {
		title = "Edit column"
	}
	return renderDialog(title, w,
		lipgloss.JoinVertical(lipgloss.Left, rows...),
		"",
		helpLine(w, "enter", "save", "tab", "next field", "esc", "back"),
	)
}

// --- projects manager ---

// projectsMode is the projects dialog's active view.
type projectsMode int

const (
	projectsList projectsMode = iota
	projectsPicking
	projectsEditing
	projectsConfirming
)

// projectsDialog lists and edits the projects stored in the user's global
// config file. Adding a project starts with a directory picker, then a
// pre-filled form; editing opens the same form pre-filled from the
// selected project. Removal asks for confirmation first.
type projectsDialog struct {
	projects []board.Project
	idx      int

	mode   projectsMode
	picker *dirPicker
	inputs []textinput.Model // name, path, agent
	focus  int
	// editing is the original name of the project being edited; empty when
	// the form adds a new project.
	editing string
	// deleting is the name of the project awaiting delete confirmation.
	deleting    string
	confirmKeys tuidialog.ConfirmKeyMap
}

func newProjectsDialog(projects []board.Project) *projectsDialog {
	return &projectsDialog{projects: projects, confirmKeys: tuidialog.DefaultConfirmKeyMap()}
}

// setProjects refreshes the list after an add, edit or delete and returns
// to the list view, keeping the cursor position (clamped).
func (d *projectsDialog) setProjects(projects []board.Project) {
	d.refreshProjects(projects)
	d.mode = projectsList
	d.editing = ""
	d.deleting = ""
	d.inputs = nil
}

// refreshProjects updates the list data without leaving the current view.
func (d *projectsDialog) refreshProjects(projects []board.Project) {
	d.projects = projects
	d.idx = min(max(d.idx, 0), max(len(projects)-1, 0))
}

// selectProject moves the cursor to the named project, if present.
func (d *projectsDialog) selectProject(name string) {
	if i := slices.IndexFunc(d.projects, func(p board.Project) bool { return p.Name == name }); i >= 0 {
		d.idx = i
	}
}

var projectFields = []struct{ label, placeholder string }{
	{"Name", "my-project"},
	{"Path", "/path/to/git/repository"},
	{"Agent", "default (or any agent ref)"},
}

// startForm opens the add/edit form. editing is the original name of the
// project being edited, or empty when adding; name/path/agent pre-fill the
// fields.
func (d *projectsDialog) startForm(editing, name, path, agent string) tea.Cmd {
	d.mode = projectsEditing
	d.editing = editing
	d.focus = 0
	d.inputs = make([]textinput.Model, len(projectFields))
	for i, f := range projectFields {
		ti := textinput.New()
		ti.SetStyles(styles.DialogInputStyle)
		ti.Placeholder = f.placeholder
		ti.SetWidth(56)
		d.inputs[i] = ti
	}
	d.inputs[0].SetValue(name)
	d.inputs[1].SetValue(path)
	d.inputs[2].SetValue(agent)
	d.inputs[0].Focus()
	return textinput.Blink
}

func (d *projectsDialog) Init() tea.Cmd { return nil }

func (d *projectsDialog) Update(msg tea.Msg) (dialog, tea.Cmd) {
	press, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return d, nil
	}
	switch d.mode {
	case projectsPicking:
		return d.updatePicking(press)
	case projectsEditing:
		return d.updateEditing(press)
	case projectsConfirming:
		return d.updateConfirming(press)
	}

	switch press.String() {
	case "esc", "q":
		return d, closeDialog
	case "up", "k":
		d.idx = max(d.idx-1, 0)
	case "down", "j":
		d.idx = min(d.idx+1, max(len(d.projects)-1, 0))
	case "a", "n":
		d.mode = projectsPicking
		d.picker = newDirPicker(pickerStartDir(""))
		return d, textinput.Blink
	case "e", "enter":
		if len(d.projects) > 0 {
			p := d.projects[d.idx]
			cmd := d.startForm(p.Name, p.Name, p.Path, p.Agent)
			return d, cmd
		}
	case "shift+up", "K":
		cmd := d.moveProject(-1)
		return d, cmd
	case "shift+down", "J":
		cmd := d.moveProject(1)
		return d, cmd
	case "x", "d", "backspace", "delete":
		if len(d.projects) > 0 {
			d.mode = projectsConfirming
			d.deleting = d.projects[d.idx].Name
		}
	}
	return d, nil
}

// updateConfirming handles the delete confirmation prompt.
func (d *projectsDialog) updateConfirming(press tea.KeyPressMsg) (dialog, tea.Cmd) {
	switch {
	case key.Matches(press, d.confirmKeys.Yes), press.String() == "enter":
		name := d.deleting
		d.mode = projectsList
		d.deleting = ""
		return d, func() tea.Msg { return deleteProjectMsg{name: name} }
	case key.Matches(press, d.confirmKeys.No), press.String() == "esc", press.String() == "q":
		d.mode = projectsList
		d.deleting = ""
	}
	return d, nil
}

// moveProject asks the model to reorder the selected project by delta
// positions; the model persists the order and moves the cursor along.
func (d *projectsDialog) moveProject(delta int) tea.Cmd {
	if len(d.projects) == 0 {
		return nil
	}
	name := d.projects[d.idx].Name
	return func() tea.Msg { return moveProjectMsg{name: name, delta: delta} }
}

// updatePicking drives the directory picker; a picked directory pre-fills
// the add form. When the picker was opened from a form in progress
// (ctrl+o), only the path field is replaced and esc returns to the form.
func (d *projectsDialog) updatePicking(press tea.KeyPressMsg) (dialog, tea.Cmd) {
	chosen, done, cmd := d.picker.Update(press)
	switch {
	case chosen != "":
		if d.inputs != nil {
			d.mode = projectsEditing
			d.inputs[1].SetValue(chosen)
			return d, nil
		}
		cmd := d.startForm("", filepath.Base(chosen), chosen, "")
		return d, cmd
	case done:
		if d.inputs != nil {
			d.mode = projectsEditing
		} else {
			d.mode = projectsList
		}
	}
	return d, cmd
}

func (d *projectsDialog) updateEditing(press tea.KeyPressMsg) (dialog, tea.Cmd) {
	switch press.String() {
	case "esc":
		d.mode = projectsList
		d.editing = ""
		d.inputs = nil
		return d, nil
	case "ctrl+o":
		// Re-open the browser, starting from the path typed so far.
		d.mode = projectsPicking
		d.picker = newDirPicker(pickerStartDir(strings.TrimSpace(d.inputs[1].Value())))
		return d, textinput.Blink
	case "tab", "down":
		cmd := d.setFocus((d.focus + 1) % len(d.inputs))
		return d, cmd
	case "shift+tab", "up":
		cmd := d.setFocus((d.focus + len(d.inputs) - 1) % len(d.inputs))
		return d, cmd
	case "enter":
		project := board.Project{
			Name:  strings.TrimSpace(d.inputs[0].Value()),
			Path:  strings.TrimSpace(d.inputs[1].Value()),
			Agent: strings.TrimSpace(d.inputs[2].Value()),
		}
		oldName := d.editing
		return d, func() tea.Msg { return submitProjectMsg{project: project, oldName: oldName} }
	}
	var cmd tea.Cmd
	d.inputs[d.focus], cmd = d.inputs[d.focus].Update(press)
	return d, cmd
}

func (d *projectsDialog) setFocus(focus int) tea.Cmd {
	d.inputs[d.focus].Blur()
	d.focus = focus
	return d.inputs[d.focus].Focus()
}

func (d *projectsDialog) View(width, _ int) string {
	w := dialogWidth(70, width)
	switch d.mode {
	case projectsPicking:
		return renderDialog("Add project · select repository", w,
			d.picker.View(w),
			"",
			helpLine(w, "↑↓", "select", "enter", "open/pick", "backspace", "up", "esc", "back"),
		)
	case projectsEditing:
		return d.viewForm(w)
	case projectsConfirming:
		return renderDialog("Remove project?", w,
			styles.BaseStyle.Render(toolcommon.TruncateText(sanitize(d.deleting), w)),
			styles.MutedStyle.Render("Removes it from the config; existing cards are unaffected."),
			"",
			helpLine(w, d.confirmKeys.Yes.Help().Key, "remove", d.confirmKeys.No.Help().Key+"/esc", "cancel"),
		)
	}

	var rows []string
	if len(d.projects) == 0 {
		rows = append(rows, styles.MutedStyle.Italic(true).Render("No projects yet — press a to add one."))
	}
	for i, p := range d.projects {
		marker, nameStyle := "  ", styles.BaseStyle.Foreground(projectColorAt(i))
		if i == d.idx {
			marker, nameStyle = styles.SuccessStyle.Render("❯ "), nameStyle.Bold(true)
		}
		agent := p.Agent
		if agent == "" {
			agent = board.DefaultAgent
		}
		line := marker + nameStyle.Render(sanitize(p.Name)) +
			styles.MutedStyle.Render("  "+sanitize(p.Path)+"  ·  ") +
			styles.SecondaryStyle.Render(sanitize(agent))
		rows = append(rows, toolcommon.TruncateText(line, w))
	}

	return renderDialog("Projects", w,
		lipgloss.JoinVertical(lipgloss.Left, rows...),
		"",
		helpLine(w, "a", "add", "e", "edit", "x", "remove", "shift+↑↓", "reorder", "↑↓", "select", "esc", "close"),
	)
}

func (d *projectsDialog) viewForm(w int) string {
	rows := make([]string, 0, len(projectFields)*2)
	for i, f := range projectFields {
		label := styles.SecondaryStyle.Render(f.label)
		if i == d.focus {
			label = styles.HighlightWhiteStyle.Render(f.label)
		}
		rows = append(rows, label, d.inputs[i].View())
	}
	title := "Add project"
	if d.editing != "" {
		title = "Edit project"
	}
	return renderDialog(title, w,
		lipgloss.JoinVertical(lipgloss.Left, rows...),
		"",
		helpLine(w, "enter", "save", "tab", "next field", "ctrl+o", "browse", "esc", "back"),
	)
}

// --- diff viewer ---

// diffDialog shows a card's worktree diff in a scrollable viewport. The
// agent may still be working, so r reloads the diff in place.
type diffDialog struct {
	cardID string
	title  string
	view   viewport.Model
	bar    *scrollbar.Model
	empty  bool
}

// maxDiffBytes caps how much diff the viewer renders. Beyond this the diff
// is cut with a notice: colorizing and holding hundreds of thousands of
// styled lines in the viewport would freeze the UI.
const maxDiffBytes = 1 << 20

func newDiffDialog(cardID, title, diff string, offset int) *diffDialog {
	vp := viewport.New()
	vp.SoftWrap = false
	// Both the title (agent-controlled) and the diff (repository content)
	// are untrusted; strip terminal controls before rendering.
	title = strings.Join(strings.Fields(sanitize(title)), " ")
	diff = sanitize(diff)
	if len(diff) > maxDiffBytes {
		cut := strings.LastIndexByte(diff[:maxDiffBytes], '\n')
		if cut < 0 {
			cut = maxDiffBytes
		}
		diff = diff[:cut] + "\n\n… diff truncated — open the worktree to see the rest"
	}
	d := &diffDialog{cardID: cardID, title: title, view: vp, bar: scrollbar.New(), empty: strings.TrimSpace(diff) == ""}
	if !d.empty {
		d.view.SetContent(colorizeDiff(diff))
		d.view.SetYOffset(offset)
	}
	return d
}

func (d *diffDialog) Init() tea.Cmd { return nil }

func (d *diffDialog) Update(msg tea.Msg) (dialog, tea.Cmd) {
	if press, ok := msg.(tea.KeyPressMsg); ok {
		switch press.String() {
		case "esc", "q", "d":
			return d, closeDialog
		case "r":
			cardID, title, offset := d.cardID, d.title, d.view.YOffset()
			return d, func() tea.Msg { return reloadDiffMsg{cardID: cardID, title: title, offset: offset} }
		}
	}
	var cmd tea.Cmd
	d.view, cmd = d.view.Update(msg)
	return d, cmd
}

func (d *diffDialog) View(width, height int) string {
	w := dialogWidth(110, width)
	h := max(min(height-6, 40), 5)
	if d.empty {
		return renderDialog("Diff · "+d.title, w,
			styles.MutedStyle.Italic(true).Render("No changes yet."),
			"",
			helpLine(w, "r", "refresh", "esc", "close"),
		)
	}
	d.view.SetWidth(w - scrollbar.Width)
	d.view.SetHeight(h)
	// Re-clamp the offset now that the real dimensions are known: it may
	// have been restored (refresh) or invalidated (resize, shrunken diff)
	// while the viewport height was still zero.
	d.view.SetYOffset(d.view.YOffset())

	// Scrollbar column, kept even when the content fits so the layout is
	// stable across refreshes.
	d.bar.SetDimensions(h, d.view.TotalLineCount())
	d.bar.SetScrollOffset(d.view.YOffset())
	bar := d.bar.View()
	if bar == "" {
		bar = strings.TrimRight(strings.Repeat(" \n", h), "\n")
	}

	return renderDialog("Diff · "+d.title, w,
		lipgloss.JoinHorizontal(lipgloss.Top, d.view.View(), bar),
		"",
		helpLine(w, "↑↓", "scroll "+percentLabel(d.view.ScrollPercent()), "r", "refresh", "esc", "close"),
	)
}

// percentLabel formats a scroll fraction (0..1) as a percentage string.
func percentLabel(frac float64) string {
	return strconv.Itoa(min(max(int(frac*100), 0), 100)) + "%"
}

// colorizeDiff applies standard diff colors line by line.
func colorizeDiff(diff string) string {
	addStyle := lipgloss.NewStyle().Foreground(styles.DiffAddFg)
	delStyle := lipgloss.NewStyle().Foreground(styles.DiffRemoveFg)
	hunkStyle := styles.InfoStyle
	fileStyle := styles.HighlightWhiteStyle

	lines := strings.Split(diff, "\n")
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "diff "), strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			lines[i] = fileStyle.Render(line)
		case strings.HasPrefix(line, "@@"):
			lines[i] = hunkStyle.Render(line)
		case strings.HasPrefix(line, "+"):
			lines[i] = addStyle.Render(line)
		case strings.HasPrefix(line, "-"):
			lines[i] = delStyle.Render(line)
		}
	}
	return strings.Join(lines, "\n")
}

// --- delete confirmation ---

type confirmDialog struct {
	card *board.Card
	keys tuidialog.ConfirmKeyMap
}

func newConfirmDialog(card *board.Card) *confirmDialog {
	return &confirmDialog{card: card, keys: tuidialog.DefaultConfirmKeyMap()}
}

func (d *confirmDialog) Init() tea.Cmd { return nil }

func (d *confirmDialog) Update(msg tea.Msg) (dialog, tea.Cmd) {
	press, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return d, nil
	}
	switch {
	case key.Matches(press, d.keys.Yes), press.String() == "enter":
		cardID := d.card.ID
		return d, func() tea.Msg { return confirmDeleteMsg{cardID: cardID} }
	case key.Matches(press, d.keys.No), press.String() == "esc", press.String() == "q":
		return d, closeDialog
	}
	return d, nil
}

func (d *confirmDialog) View(width, _ int) string {
	w := dialogWidth(60, width)
	return renderDialog("Delete card?", w,
		styles.BaseStyle.Render(toolcommon.TruncateText(sanitize(d.card.Title), w)),
		styles.MutedStyle.Render("Kills the agent session and deletes its worktree and branch."),
		"",
		helpLine(w, d.keys.Yes.Help().Key, "delete", d.keys.No.Help().Key+"/esc", "cancel"),
	)
}

// --- help ---

type helpDialog struct{}

func newHelpDialog() *helpDialog { return &helpDialog{} }

func (d *helpDialog) Init() tea.Cmd { return nil }

func (d *helpDialog) Update(msg tea.Msg) (dialog, tea.Cmd) {
	if _, ok := msg.(tea.KeyPressMsg); ok {
		return d, closeDialog
	}
	return d, nil
}

func (d *helpDialog) View(width, _ int) string {
	w := dialogWidth(64, width)
	bindings := []struct{ key, desc string }{
		{keys.New.Help().Key, "create a card (launches an agent in a worktree)"},
		{keys.Attach.Help().Key, keys.Attach.Help().Desc},
		{keys.Diff.Help().Key, "view the card's worktree diff"},
		{keys.Editor.Help().Key, keys.Editor.Help().Desc + " ($DOCKER_AGENT_BOARD_EDITOR, default code)"},
		{keys.Shell.Help().Key, keys.Shell.Help().Desc},
		{"[ / ]", "move card (forward sends the column's prompt)"},
		{keys.MoveTo.Help().Key, keys.MoveTo.Help().Desc},
		{keys.Delete.Help().Key, "delete card, its session and worktree"},
		{keys.Projects.Help().Key, keys.Projects.Help().Desc},
		{keys.Columns.Help().Key, "manage columns (add, edit, reorder, remove)"},
		{keys.Prompt.Help().Key, "edit the selected column's prompt"},
		{"←↓↑→ hjkl", "navigate (g/G first/last card)"},
		{"mouse", "click selects · double-click attaches · drag moves · wheel scrolls"},
		{keys.Quit.Help().Key, "quit (agents keep running in tmux)"},
	}
	// Same row styling as the main TUI's help dialog.
	keyStyle := styles.DialogHelpStyle.Foreground(styles.TextSecondary).Bold(true).Width(12)
	descStyle := styles.DialogHelpStyle
	rows := make([]string, 0, len(bindings))
	for _, b := range bindings {
		rows = append(rows, keyStyle.Render(b.key)+descStyle.Render(b.desc))
	}
	rows = append(rows, "",
		styles.MutedStyle.Render("Projects, columns, and their prompts are stored in the global"),
		styles.MutedStyle.Render("config file ("+toolcommon.TruncateText(configPathHint(), w-8)+")."),
	)
	return renderDialog("Help", w, lipgloss.JoinVertical(lipgloss.Left, rows...))
}
