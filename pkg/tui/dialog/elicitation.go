package dialog

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/rivo/uniseg"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	defaultCharLimit = 500
	numberCharLimit  = 50
	defaultWidth     = 50
)

// ElicitationField represents a form field extracted from a JSON schema.
type ElicitationField struct {
	Name, Title, Type, Description string
	Required                       bool
	EnumValues                     []string
	Default                        any
	MinLength, MaxLength           int
	Format, Pattern                string
	Minimum, Maximum               float64
	HasMinimum, HasMaximum         bool
}

// ElicitationDialog implements Dialog for MCP elicitation requests.
//
// When a schema is provided, fields are rendered as a form.
// When no schema is provided, a single free-form text input (responseInput)
// is shown so the user can type an answer.
//
// The body region (message + fields, or message + free-form input) is
// rendered inside a scrollview so long content remains accessible when it
// would otherwise overflow the terminal.
type ElicitationDialog struct {
	BaseDialog

	title         string
	message       string
	ref           ElicitationRef
	fields        []ElicitationField
	inputs        []textinput.Model
	boolValues    map[int]bool
	enumIndexes   map[int]int // selected index for enum fields
	currentField  int
	keyMap        elicitationKeyMap
	fieldErrors   map[int]string  // validation error messages per field
	responseInput textinput.Model // free-form text input used when len(fields) == 0

	scrollview *scrollview.Model
	// fieldStarts[i] is the line offset of field i's label inside the
	// scrollable body. Prepared at input and size boundaries.
	fieldStarts []int
	// fieldGeoms[i] describes field i's wrapped row layout (label height and
	// per-option row ranges) relative to fieldStarts[i]. Populated together
	// with fieldStarts by prepareLayout().
	fieldGeoms []fieldGeometry
	// scrollableRow is the absolute screen row of the first scrollable line.
	scrollableRow int
	scrollableCol int
	// Resizes reanchor focus once during layout preparation, never during rendering.
	reanchorFocus bool
}

// fieldGeometry records the row layout of a single rendered field, relative
// to the field's first line. Long labels and options wrap over several rows,
// so focus and mouse hit-testing consult these offsets instead of assuming
// one row per label/option.
type fieldGeometry struct {
	labelHeight   int   // rows used by the wrapped label
	optionStarts  []int // first row of each enum/boolean option, relative to the field start
	optionHeights []int // rows used by each option, parallel to optionStarts
}

// optionAt maps a row offset (relative to the field's first line) to the
// index of the enum/boolean option rendered on that row, or -1 when the
// offset falls outside every option (label rows, error line, trailing blank).
func (g fieldGeometry) optionAt(offset int) int {
	for j, start := range g.optionStarts {
		if offset >= start && offset < start+g.optionHeights[j] {
			return j
		}
	}
	return -1
}

type elicitationKeyMap struct {
	Up, Down, Tab, ShiftTab, Enter, Escape, Space key.Binding
}

// hasFreeFormInput returns true when no schema fields exist and the dialog
// shows a single free-form text input instead.
func (d *ElicitationDialog) hasFreeFormInput() bool {
	return len(d.fields) == 0
}

// NewElicitationDialog creates a new elicitation dialog answering ref.
func NewElicitationDialog(message string, schema any, meta map[string]any, ref ElicitationRef) Dialog {
	fields := parseElicitationSchema(schema)

	// Determine dialog title from meta, defaulting to "Question"
	title := "Question"
	if meta != nil {
		if t, ok := meta["cagent/title"].(string); ok && t != "" {
			title = t
		}
	}

	base := BaseDialog{}
	scrollviewView := base.newScrollview(scrollview.WithReserveScrollbarSpace(true))
	base.bodyScroll = scrollviewView
	d := &ElicitationDialog{
		BaseDialog:  base,
		title:       title,
		message:     message,
		ref:         ref,
		fields:      fields,
		inputs:      make([]textinput.Model, len(fields)),
		boolValues:  make(map[int]bool),
		enumIndexes: make(map[int]int),
		fieldErrors: make(map[int]string),
		keyMap: elicitationKeyMap{
			Up:       key.NewBinding(key.WithKeys("up")),
			Down:     key.NewBinding(key.WithKeys("down")),
			Tab:      key.NewBinding(key.WithKeys("tab")),
			ShiftTab: key.NewBinding(key.WithKeys("shift+tab")),
			Enter:    key.NewBinding(key.WithKeys("enter")),
			Escape:   key.NewBinding(key.WithKeys("esc")),
			Space:    key.NewBinding(key.WithKeys("space")),
		},
		// Up/Down stay reserved for selection inside enum/boolean fields;
		// the scrollview consumes mouse wheel/scrollbar plus PgUp/PgDn/Home/End.
		scrollview: scrollviewView,
	}

	// If no schema fields, add a free-form text input for the response
	if len(fields) == 0 {
		ti := textinput.New()
		ti.SetStyles(styles.DialogInputStyle)
		ti.SetWidth(defaultWidth)
		ti.Prompt = ""
		ti.Placeholder = "Type your response"
		ti.CharLimit = defaultCharLimit
		ti.Focus()
		d.responseInput = ti
	}

	d.initInputs()
	return d
}

func (d *ElicitationDialog) Init() tea.Cmd {
	if d.hasFreeFormInput() || len(d.inputs) > 0 {
		return textinput.Blink
	}
	return nil
}

// OutsideClickDismissCmd cancels exactly like Escape, atomically closing the
// dialog and answering the runtime waiter.
func (d *ElicitationDialog) OutsideClickDismissCmd() tea.Cmd {
	return d.close(tools.ElicitationActionCancel, nil)
}

func (d *ElicitationDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg.(type) {
	case tea.KeyPressMsg, tea.PasteMsg, tea.MouseClickMsg:
		defer d.prepareLayout()
	}

	// Let the scrollview consume mouse wheel/scrollbar drag and the
	// PgUp/PgDn keys before falling through to dialog handling.
	if keyMsg, isKey := msg.(tea.KeyPressMsg); !isKey || keyMsg.String() == "pgup" || keyMsg.String() == "pgdown" {
		if handled, cmd := d.UpdateBodyScroll(msg); handled {
			return d, cmd
		}
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.PasteMsg:
		if d.ActionsFocused() {
			return d, nil
		}
		// Forward paste to the active text input
		if d.hasFreeFormInput() {
			var cmd tea.Cmd
			d.responseInput, cmd = d.responseInput.Update(msg)
			d.EnsureBodyLineVisible(len(d.layout().bodyLines) - 1)
			return d, cmd
		}
		if d.isTextInputField() {
			var cmd tea.Cmd
			d.inputs[d.currentField], cmd = d.inputs[d.currentField].Update(msg)
			d.ensureFocusVisible()
			return d, cmd
		}
		return d, nil
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft {
			return d.handleMouseClick(msg)
		}
		return d, nil
	case tea.KeyPressMsg:
		return d.handleKeyPress(msg)
	}
	return d, nil
}

func (d *ElicitationDialog) handleKeyPress(msg tea.KeyPressMsg) (layout.Model, tea.Cmd) {
	backward := msg.String() == "shift+tab"
	boundary := (msg.String() == "tab" && (d.hasFreeFormInput() || d.currentField == len(d.fields)-1)) || (backward && (d.hasFreeFormInput() || d.currentField == 0))
	if d.ActionsFocused() || boundary {
		wasFocused := d.ActionsFocused()
		if action, handled := d.HandleActionKey(msg); handled {
			if d.ActionsFocused() {
				d.responseInput.Blur()
				for i := range d.inputs {
					d.inputs[i].Blur()
				}
			} else if wasFocused {
				switch {
				case d.hasFreeFormInput():
					d.responseInput.Focus()
				case backward:
					d.focusField(len(d.fields) - 1)
				default:
					d.focusField(0)
				}
			}
			if action.Code == 0 {
				return d, nil
			}
			msg = action
		}
	}

	switch {
	case key.Matches(msg, d.keyMap.Space) && !d.isTextInputField() && !d.hasFreeFormInput():
		// Space cycles forward through options, same as down arrow
		d.moveSelection(1)
		return d, nil
	case key.Matches(msg, d.keyMap.Escape):
		cmd := d.close(tools.ElicitationActionCancel, nil)
		return d, cmd
	case key.Matches(msg, d.keyMap.Up):
		// Up/down navigate within selection fields (enum/boolean)
		d.moveSelection(-1)
		return d, nil
	case key.Matches(msg, d.keyMap.Down):
		d.moveSelection(1)
		return d, nil
	case key.Matches(msg, d.keyMap.ShiftTab):
		d.moveFocus(-1)
		return d, nil
	case key.Matches(msg, d.keyMap.Tab):
		d.moveFocus(1)
		return d, nil
	case key.Matches(msg, d.keyMap.Enter):
		return d.submit()
	default:
		return d.updateCurrentInput(msg)
	}
}

// moveSelection moves the selection up/down within a boolean or enum field.
func (d *ElicitationDialog) moveSelection(delta int) {
	if d.currentField >= len(d.fields) {
		return
	}
	delete(d.fieldErrors, d.currentField)

	switch field := d.fields[d.currentField]; field.Type {
	case "boolean":
		// Boolean only has two options: toggle
		d.boolValues[d.currentField] = !d.boolValues[d.currentField]
	case "enum":
		n := len(field.EnumValues)
		if n == 0 {
			return
		}
		d.enumIndexes[d.currentField] = (d.enumIndexes[d.currentField] + delta + n) % n
	}
	d.ensureFocusVisible()
}

func (d *ElicitationDialog) submit() (layout.Model, tea.Cmd) {
	// Free-form response: no schema fields, just a text input
	if d.hasFreeFormInput() {
		val := strings.TrimSpace(d.responseInput.Value())
		var content map[string]any
		if val != "" {
			content = map[string]any{"response": val}
		}
		cmd := d.close(tools.ElicitationActionAccept, content)
		return d, cmd
	}

	// Schema-based form: validate all fields
	d.fieldErrors = make(map[int]string)
	content, firstErrorIdx := d.collectAndValidate()

	if firstErrorIdx >= 0 {
		d.focusField(firstErrorIdx)
		return d, nil
	}

	cmd := d.close(tools.ElicitationActionAccept, content)
	return d, cmd
}

func (d *ElicitationDialog) updateCurrentInput(msg tea.KeyPressMsg) (layout.Model, tea.Cmd) {
	if d.ActionsFocused() {
		return d, nil
	}
	if d.hasFreeFormInput() {
		var cmd tea.Cmd
		d.responseInput, cmd = d.responseInput.Update(msg)
		d.EnsureBodyLineVisible(len(d.layout().bodyLines) - 1)
		return d, cmd
	}
	if d.isTextInputField() {
		delete(d.fieldErrors, d.currentField)
		var cmd tea.Cmd
		d.inputs[d.currentField], cmd = d.inputs[d.currentField].Update(msg)
		// If the field was below the fold (e.g. the dialog opened scrolled to
		// the top with a tall message), reveal it as soon as the user starts
		// typing so they can see what they're entering.
		d.ensureFocusVisible()
		return d, cmd
	}
	return d, nil
}

func (d *ElicitationDialog) moveFocus(delta int) {
	if len(d.fields) == 0 {
		return
	}
	newField := (d.currentField + delta + len(d.fields)) % len(d.fields)
	d.focusField(newField)
}

// focusField moves focus to the specified field index.
func (d *ElicitationDialog) focusField(idx int) {
	if idx < 0 || idx >= len(d.fields) {
		return
	}
	if len(d.inputs) > 0 && d.currentField < len(d.inputs) {
		d.inputs[d.currentField].Blur()
	}
	d.BlurActions()
	d.currentField = idx
	// Only focus text input for fields that use it
	if d.isTextInputField() {
		d.inputs[d.currentField].Focus()
	}
	d.ensureFocusVisible()
}

// ensureFocusVisible scrolls so that the focused field's active rows stay
// in view. No-op before the first View() populates fieldStarts.
func (d *ElicitationDialog) ensureFocusVisible() {
	if start, end := d.focusRange(); start >= 0 {
		d.scrollview.EnsureRangeVisible(start, end)
	}
}

// focusRange returns the first and last line offsets (within the scrollable
// body) of the focused field's active rows — the selected option for
// enums/booleans, the input line for text fields. Wrapping can spread an
// option over several rows, hence a range. Returns (-1, -1) if no field is
// focused or layouts haven't been computed yet.
func (d *ElicitationDialog) focusRange() (start, end int) {
	if d.currentField < 0 || d.currentField >= len(d.fieldStarts) || len(d.fieldGeoms) != len(d.fieldStarts) {
		return -1, -1
	}
	fieldStart := d.fieldStarts[d.currentField]
	geom := d.fieldGeoms[d.currentField]

	idx := -1
	switch f := d.fields[d.currentField]; f.Type {
	case "boolean":
		idx = 1 // "No"
		if d.boolValues[d.currentField] {
			idx = 0 // "Yes"
		}
	case "enum":
		idx = max(0, min(d.enumIndexes[d.currentField], len(f.EnumValues)-1))
	}
	if idx < 0 || idx >= len(geom.optionStarts) {
		line := fieldStart + geom.labelHeight // input line
		return line, line
	}
	start = fieldStart + geom.optionStarts[idx]
	return start, start + geom.optionHeights[idx] - 1
}

// isTextInputField returns true if the current field uses a text input (not boolean/enum).
func (d *ElicitationDialog) isTextInputField() bool {
	if d.currentField >= len(d.fields) || len(d.inputs) == 0 {
		return false
	}
	ft := d.fields[d.currentField].Type
	return ft != "boolean" && ft != "enum"
}

func (d *ElicitationDialog) CancelDialogCmd() tea.Cmd {
	return d.close(tools.ElicitationActionCancel, nil)
}

func (d *ElicitationDialog) close(action tools.ElicitationAction, content map[string]any) tea.Cmd {
	if !d.claimResponse() {
		return nil
	}
	return CloseWithElicitationResponse(action, content, d.ref)
}

// collectAndValidate validates all fields and returns the collected values.
// Returns the content map and the index of the first field with an error (-1 if valid).
func (d *ElicitationDialog) collectAndValidate() (map[string]any, int) {
	content := make(map[string]any)
	firstErrorIdx := -1

	for i, field := range d.fields {
		switch field.Type {
		case "boolean":
			content[field.Name] = d.boolValues[i]
		case "enum":
			idx := d.enumIndexes[i]
			if idx < 0 || idx >= len(field.EnumValues) {
				if field.Required {
					d.fieldErrors[i] = "Selection required"
					if firstErrorIdx < 0 {
						firstErrorIdx = i
					}
				}
				continue
			}
			content[field.Name] = field.EnumValues[idx]
		default:
			raw := d.inputs[i].Value()
			val := strings.TrimSpace(raw)
			if val == "" {
				if field.Required {
					d.fieldErrors[i] = "This field is required"
					if firstErrorIdx < 0 {
						firstErrorIdx = i
					}
				}
				continue
			}
			// Secret fields (e.g. a sudo password) may legitimately contain
			// leading/trailing whitespace; store the raw value. The trimmed
			// copy above is only used for the required/empty check.
			if field.Format == "password" {
				val = raw
			}
			parsed, errMsg := d.parseAndValidateField(val, field)
			if errMsg != "" {
				d.fieldErrors[i] = errMsg
				if firstErrorIdx < 0 {
					firstErrorIdx = i
				}
				continue
			}
			content[field.Name] = parsed
		}
	}
	return content, firstErrorIdx
}

// parseAndValidateField parses and validates a field value, returning the parsed value and an error message.
func (d *ElicitationDialog) parseAndValidateField(val string, field ElicitationField) (any, string) {
	if val == "" {
		return nil, ""
	}

	switch field.Type {
	case "number":
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return nil, "Must be a valid number"
		}
		if errMsg := validateNumberFieldWithMessage(f, field); errMsg != "" {
			return nil, errMsg
		}
		return f, ""

	case "integer":
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return nil, "Must be a whole number"
		}
		if errMsg := validateNumberFieldWithMessage(float64(n), field); errMsg != "" {
			return nil, errMsg
		}
		return n, ""

	case "enum":
		if !slices.Contains(field.EnumValues, val) {
			return nil, "Invalid selection"
		}
		return val, ""

	default: // string
		if errMsg := validateStringFieldWithMessage(val, field); errMsg != "" {
			return nil, errMsg
		}
		return val, ""
	}
}

// elicitationLayout captures the geometry computed once per render. View()
// and Position() share it so layout math lives in exactly one place.
type elicitationLayout struct {
	dialogWidth  int
	contentWidth int             // inside dialog frame
	bodyLines    []string        // pre-rendered body, one entry per line
	fieldStarts  []int           // line offset of each field's label
	fieldGeoms   []fieldGeometry // wrapped row layout per field, relative to fieldStarts
}

func (d *ElicitationDialog) layout() elicitationLayout {
	dialogWidth := d.ComputeDialogWidth(70, 60, 90)
	contentWidth := d.ContentWidth(dialogWidth, 2)
	innerWidth := d.BodyContentWidth(dialogWidth)

	bodyLines, fieldStarts, fieldGeoms := d.buildBody(innerWidth)

	return elicitationLayout{
		dialogWidth:  dialogWidth,
		contentWidth: contentWidth,
		bodyLines:    bodyLines,
		fieldStarts:  fieldStarts,
		fieldGeoms:   fieldGeoms,
	}
}

// buildBody renders the scrollable body using the existing Content-based
// helpers and records the line offset of every field's label plus each
// field's wrapped row geometry. Tracks line count incrementally to keep
// buildBody O(N) in the number of fields.
func (d *ElicitationDialog) buildBody(width int) (lines []string, fieldStarts []int, fieldGeoms []fieldGeometry) {
	body := NewContent(width)
	lineCount := 0

	if d.message != "" {
		msgRendered := renderMarkdownMessage(d.message, width)
		body.AddContent(msgRendered)
		lineCount += lipgloss.Height(msgRendered)
	}

	switch {
	case len(d.fields) > 0:
		body.AddSeparator()
		lineCount++ // separator adds 1 line

		fieldStarts = make([]int, len(d.fields))
		fieldGeoms = make([]fieldGeometry, len(d.fields))
		for i, field := range d.fields {
			// Record the current line count as this field's start position.
			// This avoids O(N²) by tracking line count incrementally instead
			// of calling body.Build() in the loop.
			fieldStarts[i] = lineCount

			// Render the field into a temporary Content to measure its height
			// without rebuilding the entire body.
			tempContent := NewContent(width)
			fieldGeoms[i] = d.renderField(tempContent, i, field, width)
			fieldRendered := tempContent.Build()
			fieldHeight := lipgloss.Height(fieldRendered)

			// Add the pre-rendered field to the main body
			body.AddContent(fieldRendered)
			lineCount += fieldHeight

			if i < len(d.fields)-1 {
				body.AddSpace()
				lineCount++ // blank line separator
			}
		}

	case d.hasFreeFormInput():
		body.AddSeparator()
		input := d.responseInput
		input.SetStyles(styles.DialogInputStyle)
		input.SetWidth(width)
		body.AddContent(input.View())
	}

	return strings.Split(body.Build(), "\n"), fieldStarts, fieldGeoms
}

func (d *ElicitationDialog) content() (width int, header, body, footer string) {
	l := d.layout()
	header = lipgloss.JoinVertical(lipgloss.Left, RenderTitle(d.title, l.contentWidth, styles.DialogTitleStyle), RenderSeparator(l.contentWidth))
	body = strings.Join(l.bodyLines, "\n")
	footer = d.RenderActions(l.contentWidth, Action{Label: "Submit", Default: true, Key: tea.KeyPressMsg{Code: tea.KeyEnter}})
	return l.dialogWidth, header, body, footer
}

func (d *ElicitationDialog) View() string {
	width, header, body, footer := d.content()
	return d.RenderScrollableBody(styles.DialogStyle, width, header, body, footer)
}

func (d *ElicitationDialog) renderField(content *Content, i int, field ElicitationField, contentWidth int) fieldGeometry {
	// Use Title if available, otherwise capitalize the property name
	label := field.Title
	if label == "" {
		label = capitalizeFirst(field.Name)
	}
	if field.Required {
		label += "*"
	}

	// Check if this field has an error
	hasError := d.fieldErrors[i] != ""
	labelStyle := styles.DialogContentStyle.Bold(true)
	if hasError {
		labelStyle = labelStyle.Foreground(styles.Error)
	}
	// Wrap long agent-supplied labels to the content width; anything wider
	// would be silently clipped by the scrollview.
	renderedLabel := labelStyle.Width(contentWidth).Render(label)
	content.AddContent(renderedLabel)
	geom := fieldGeometry{labelHeight: lipgloss.Height(renderedLabel)}

	// Render field input based on type
	isFocused := i == d.currentField
	switch field.Type {
	case "boolean":
		d.renderBooleanField(content, &geom, i, isFocused, contentWidth)
	case "enum":
		d.renderEnumField(content, &geom, i, field, isFocused, contentWidth)
	default:
		input := d.inputs[i]
		input.SetStyles(styles.DialogInputStyle)
		input.SetWidth(contentWidth)
		content.AddContent(input.View())
	}

	// Show error message if present
	if hasError {
		errorStyle := styles.DialogContentStyle.Foreground(styles.Error).Italic(true)
		content.AddContent(errorStyle.Render("  ⚠ " + d.fieldErrors[i]))
	}
	return geom
}

func (d *ElicitationDialog) renderBooleanField(content *Content, geom *fieldGeometry, i int, isFocused bool, contentWidth int) {
	selectedIdx := 1
	if d.boolValues[i] {
		selectedIdx = 0
	}
	d.renderSelectionField(content, geom, []string{"Yes", "No"}, selectedIdx, isFocused, contentWidth)
}

func (d *ElicitationDialog) renderEnumField(content *Content, geom *fieldGeometry, i int, field ElicitationField, isFocused bool, contentWidth int) {
	d.renderSelectionField(content, geom, field.EnumValues, d.enumIndexes[i], isFocused, contentWidth)
}

func (d *ElicitationDialog) renderSelectionField(content *Content, geom *fieldGeometry, options []string, selectedIdx int, isFocused bool, contentWidth int) {
	selectedStyle := styles.DialogContentStyle.Foreground(styles.TextBright).Bold(true)
	unselectedStyle := styles.DialogContentStyle.Foreground(styles.TextMuted)

	// All prefixes are equally wide, so option heights don't change with
	// focus/selection and wrapped rows hang-indent under the option text.
	textWidth := max(1, contentWidth-lipgloss.Width("  ○ "))
	row := geom.labelHeight
	for j, option := range options {
		prefix := "  ○ "
		style := unselectedStyle
		if j == selectedIdx {
			prefix = "  ● "
			if isFocused {
				prefix = "› ● "
			}
			style = selectedStyle
		}
		// Wrap long agent-supplied options instead of letting the scrollview
		// clip them; JoinHorizontal keeps continuation rows aligned under the
		// first row's text column.
		rendered := lipgloss.JoinHorizontal(lipgloss.Top, style.Render(prefix), style.Width(textWidth).Render(option))
		content.AddContent(rendered)
		height := lipgloss.Height(rendered)
		geom.optionStarts = append(geom.optionStarts, row)
		geom.optionHeights = append(geom.optionHeights, height)
		row += height
	}
}

// capitalizeFirst returns the string with its first letter capitalized.
func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	runes := []rune(s)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

// handleMouseClick handles mouse click events for field focus and selection toggling.
func (d *ElicitationDialog) handleMouseClick(msg tea.MouseClickMsg) (layout.Model, tea.Cmd) {
	view := d.View()
	row, col := d.CenterDialog(view)
	dl := NewDialogLayout(view, row, col)
	if d.CloseButtonHit(msg, dl) {
		cmd := d.CancelDialogCmd()
		return d, cmd
	}
	if action, ok := d.ActionKeyAt(msg.X, msg.Y, dl); ok {
		d.BlurActions()
		return d.Update(action)
	}
	if msg.X < d.scrollableCol || msg.X >= d.scrollableCol+d.scrollview.ContentWidth() {
		return d, nil
	}
	if d.hasFreeFormInput() {
		line := msg.Y - d.scrollableRow + d.BodyScrollOffset()
		if msg.Y >= d.scrollableRow && msg.Y < d.scrollableRow+d.scrollview.VisibleHeight() && line == len(d.layout().bodyLines)-1 {
			d.BlurActions()
			cmd := d.responseInput.Focus()
			SetTextInputCursorAtCell(&d.responseInput, msg.X-d.scrollableCol)
			return d, cmd
		}
		return d, nil
	}
	if len(d.fieldStarts) == 0 || len(d.fieldGeoms) != len(d.fieldStarts) {
		return d, nil
	}
	relY := msg.Y - d.scrollableRow
	if relY < 0 || relY >= d.scrollview.VisibleHeight() {
		return d, nil
	}
	line := d.scrollview.ScrollOffset() + relY

	// Walk backwards: the field whose start is just at or above `line` owns it.
	// Clicks on the blank separator after a field still focus that field.
	for i := range slices.Backward(d.fieldStarts) {
		start := d.fieldStarts[i]
		if line < start {
			continue
		}
		d.focusField(i)
		delete(d.fieldErrors, i)
		// Any wrapped row of an option counts as a click on that option.
		opt := d.fieldGeoms[i].optionAt(line - start)
		if opt < 0 {
			if d.isTextInputField() && line-start == d.fieldGeoms[i].labelHeight {
				SetTextInputCursorAtCell(&d.inputs[i], msg.X-d.scrollableCol)
			}
			return d, nil
		}
		switch f := d.fields[i]; f.Type {
		case "boolean":
			d.boolValues[i] = opt == 0
		case "enum":
			if opt < len(f.EnumValues) {
				d.enumIndexes[i] = opt
			}
		}
		return d, nil
	}
	return d, nil
}

func (d *ElicitationDialog) Position() (row, col int) {
	return d.CenterDialog(d.View())
}

// --- Input initialization ---

func (d *ElicitationDialog) initInputs() {
	for i, field := range d.fields {
		d.inputs[i] = d.createInput(field, i)
	}
	// Focus the first text input field
	if d.isTextInputField() {
		d.inputs[0].Focus()
	}
}

func (d *ElicitationDialog) createInput(field ElicitationField, idx int) textinput.Model {
	ti := textinput.New()
	ti.SetStyles(styles.DialogInputStyle)
	ti.SetWidth(defaultWidth)
	ti.Prompt = "" // Remove the "> " prefix

	// Configure based on field type
	switch field.Type {
	case "boolean":
		d.boolValues[idx], _ = field.Default.(bool)
		return ti // Boolean fields don't use text input

	case "enum":
		// Initialize enum selection to first option
		d.enumIndexes[idx] = 0
		return ti // Enum fields don't use text input

	case "number", "integer":
		ti.Placeholder = cmp.Or(field.Description, "Enter a number")
		ti.CharLimit = numberCharLimit

	default: // string
		ti.Placeholder = cmp.Or(field.Description, "Enter value")
		ti.CharLimit = cmp.Or(field.MaxLength, defaultCharLimit)
		// Mask secret fields (JSON Schema "format": "password"), e.g. the sudo
		// password prompt, so the value is never echoed to the screen.
		if field.Format == "password" {
			ti.EchoMode = textinput.EchoPassword
		}
	}

	// Set default value
	if field.Default != nil {
		ti.SetValue(fmt.Sprintf("%v", field.Default))
	}

	return ti
}

// renderMarkdownMessage renders a message string as markdown for display in dialogs.
// Falls back to plain text rendering if the markdown renderer fails.
func renderMarkdownMessage(message string, contentWidth int) string {
	// No copy icon: dialogs do not hit-test clicks on code blocks.
	rendered, err := markdown.NewRendererWithoutCopyIcon(contentWidth).Render(message)
	if err != nil {
		return styles.DialogContentStyle.Width(contentWidth).Render(message)
	}
	return strings.TrimRight(rendered, "\n")
}

// SetTextInputCursorAtCell maps a cell inside the text area (excluding its prompt)
// to a rune position without losing the input's history-dependent viewport.
func SetTextInputCursorAtCell(input *textinput.Model, cell int) {
	probe := *input
	probeStyles := probe.Styles()
	prefixRunes, captured := 0, false
	capture := func(prefix string) string {
		if !captured {
			prefixRunes, captured = utf8.RuneCountInString(prefix), true
		}
		return prefix
	}
	// View renders the visible text before its cursor first. Probe a value copy
	// so neither its appearance nor the editor's viewport changes during hit testing.
	probeStyles.Focused.Text = lipgloss.NewStyle().Transform(capture)
	probeStyles.Blurred.Text = probeStyles.Focused.Text
	probe.SetStyles(probeStyles)
	_ = probe.View()
	value := []rune(input.Value())
	position := min(len(value), max(0, input.Position()-prefixRunes))
	graphemes := uniseg.NewGraphemes(string(value[position:]))
	remaining := max(0, cell)
	for graphemes.Next() {
		width := graphemes.Width()
		if width > remaining {
			break
		}
		remaining -= width
		position += len(graphemes.Runes())
	}
	input.SetCursor(position)
}

func (d *ElicitationDialog) SetSize(width, height int) tea.Cmd {
	d.reanchorFocus = d.Width() > 0 && (width != d.Width() || height != d.Height())
	cmd := d.BaseDialog.SetSize(width, height)
	d.prepareLayout()
	return cmd
}

func (d *ElicitationDialog) prepareLayout() {
	width := d.BodyContentWidth(d.ComputeDialogWidth(70, 60, 90))
	d.responseInput.SetWidth(width)
	for i := range d.inputs {
		d.inputs[i].SetWidth(width)
	}
	l := d.layout()
	d.fieldStarts, d.fieldGeoms = l.fieldStarts, l.fieldGeoms
	frameWidth, header, body, footer := d.content()
	d.PrepareScrollableBody(styles.DialogStyle, frameWidth, header, body, footer)
	if d.reanchorFocus {
		d.reanchorFocus = false
		d.ensureFocusVisible()
		d.PrepareScrollableBody(styles.DialogStyle, frameWidth, header, body, footer)
	}
	d.scrollableCol, d.scrollableRow, _, _ = d.BodyScrollBounds()
}
