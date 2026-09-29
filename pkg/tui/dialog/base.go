package dialog

import (
	"strings"
	"unicode/utf8"

	"github.com/docker/docker-agent/pkg/tui/widgets/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// Close-button rendering constants.
const (
	dialogCloseGlyph = "✕"
	dialogCloseInset = 1
)

// ConfirmButtonFocus tracks which button is focused in Yes/No confirmation dialogs.
type ConfirmButtonFocus int

const (
	ConfirmFocusNo ConfirmButtonFocus = iota
	ConfirmFocusYes
)

// ConfirmKeyAction is the outcome returned by HandleConfirmKey.
type ConfirmKeyAction int

const (
	ConfirmKeyNone ConfirmKeyAction = iota
	ConfirmKeyConfirmed
	ConfirmKeyCancelled
	ConfirmKeyFocusToggled
)

// ConfirmKeyMap defines key bindings for confirmation dialogs (Yes/No).
type ConfirmKeyMap struct {
	Yes key.Binding
	No  key.Binding
}

// DefaultConfirmKeyMap returns the standard Yes/No key bindings.
func DefaultConfirmKeyMap() ConfirmKeyMap {
	return ConfirmKeyMap{
		Yes: key.NewBinding(
			key.WithKeys("y", "Y"),
			key.WithHelp("Y", "yes"),
		),
		No: key.NewBinding(
			key.WithKeys("n", "N"),
			key.WithHelp("N", "no"),
		),
	}
}

// BaseDialog provides common functionality for dialog implementations.
// It handles size management, position calculation, and common UI patterns.
type BaseDialog struct {
	scrollviews                           []scrollviewRegistration
	responseSent                          bool
	width, height                         int
	closeHovered                          bool
	visualDirty                           bool
	confirmFocus                          ConfirmButtonFocus
	confirmBtnNoX, confirmBtnNoW          int
	confirmBtnYesX, confirmBtnYesW        int
	actionRows                            []dialogActionRow
	actions                               []Action
	actionsFocused                        bool
	focusedAction                         int
	actionLines                           []int
	actionScroll                          *scrollview.Model
	actionScrollActive                    bool
	actionFooterStart, actionFooterHeight int
	actionContentX, actionContentWidth    int
	bodyScroll                            *scrollview.Model
	bodyX, bodyY, bodyWidth, bodyHeight   int
	bodyHeaderGap                         int
	bodyTitleGap                          int
	bodyHeaderRows                        int
	bodyFooterGap                         int
	bodyPreparationCount                  uint64
	bodyMaxHeight                         int
	bodyFillHeight                        bool
	cardWidth, cardHeight                 int
}

type scrollviewRegistration struct {
	model      *scrollview.Model
	generation uint64
}

func (b *BaseDialog) newScrollview(opts ...scrollview.Option) *scrollview.Model {
	view := scrollview.New(opts...)
	b.scrollviews = append(b.scrollviews, scrollviewRegistration{model: view, generation: view.VisualGeneration()})
	return view
}

// CancelDialogCmd is the default semantic cancellation transaction. Stateful
// dialogs override it when cancellation must emit additional messages.
func (b *BaseDialog) CancelDialogCmd() tea.Cmd { return core.CmdHandler(CloseDialogMsg{}) }

// claimResponse prevents repeated input from answering before the close command arrives.
func (b *BaseDialog) claimResponse() bool {
	if b.responseSent {
		return false
	}
	b.responseSent = true
	return true
}

// SetSize updates the dialog dimensions.
func (b *BaseDialog) SetSize(width, height int) tea.Cmd {
	b.width = width
	b.height = height
	return nil
}

// Width returns the current width.
func (b *BaseDialog) Width() int {
	return b.width
}

// Height returns the current height.
func (b *BaseDialog) Height() int {
	return b.height
}

// HandleConfirmKey provides keyboard parity for confirmation pills.
func (b *BaseDialog) HandleConfirmKey(msg tea.KeyPressMsg, keyMap ConfirmKeyMap) ConfirmKeyAction {
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))), key.Matches(msg, keyMap.No):
		return ConfirmKeyCancelled
	case key.Matches(msg, keyMap.Yes):
		return ConfirmKeyConfirmed

	case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
		if b.confirmFocus == ConfirmFocusYes {
			return ConfirmKeyConfirmed
		}
		return ConfirmKeyCancelled
	}
	return ConfirmKeyNone
}

// ConfirmAndClose closes before dispatching the confirmed action.
func ConfirmAndClose(cmd tea.Cmd) tea.Cmd {
	closeCmd := func() tea.Msg { return CloseDialogMsg{} }
	if cmd == nil {
		return closeCmd
	}
	return tea.Sequence(closeCmd, cmd)
}

// RenderConfirmButtons renders right-aligned, terminal-cell-addressable action pills.
func (b *BaseDialog) RenderConfirmButtons(contentWidth int) string {
	out := b.RenderActions(contentWidth,
		Action{Label: "No", Key: tea.KeyPressMsg{Code: 'n', Text: "n"}, Default: b.confirmFocus != ConfirmFocusYes, HideShortcut: true},
		Action{Label: "Yes", Key: tea.KeyPressMsg{Code: 'y', Text: "y"}, Default: b.confirmFocus == ConfirmFocusYes})
	// Legacy confirmation handlers retain their measured pill columns.
	for _, row := range b.actionRows {
		for _, hit := range row.hits {
			if hit.key.Code == 'n' {
				b.confirmBtnNoX, b.confirmBtnNoW = hit.x, hit.width
			} else {
				b.confirmBtnYesX, b.confirmBtnYesW = hit.x, hit.width
			}
		}
	}
	return out
}

// DialogLayout captures rendered bounds for chrome hit testing.
//
//nolint:revive // Explicit name distinguishes dialog layout from generic layouts.
type DialogLayout struct {
	View                    string
	Row, Col, Width, Height int
}

func NewDialogLayout(view string, row, col int) DialogLayout {
	return DialogLayout{View: view, Row: row, Col: col, Width: lipgloss.Width(view), Height: lipgloss.Height(view)}
}

// CloseButtonHit reports whether a click hit the top-right close control.
func (b *BaseDialog) CloseButtonHit(msg tea.MouseClickMsg, dl DialogLayout) bool {
	x, y, ok := closeControlCell(dl.Width, dl.Height)
	return ok && msg.X == dl.Col+x && msg.Y == dl.Row+y
}

// SetCloseHover synchronizes concrete chrome with the manager pointer state.
func (b *BaseDialog) SetCloseHover(hovered bool) {
	b.visualDirty = b.visualDirty || b.closeHovered != hovered
	b.closeHovered = hovered
}

func (b *BaseDialog) ResetCloseHover() {
	b.closeHovered = false
}

// HandleMouseMotion updates close-control hover state.
func (b *BaseDialog) HandleMouseMotion(x, y int, dl DialogLayout) bool {
	hovered := b.CloseButtonHit(tea.MouseClickMsg{X: x, Y: y}, dl)
	changed := hovered != b.closeHovered
	b.closeHovered = hovered
	b.visualDirty = b.visualDirty || changed
	return changed
}

// MarkVisualDirty records an explicit pointer-driven visible mutation.
func (b *BaseDialog) MarkVisualDirty() { b.visualDirty = true }

// TakeVisualDirty reports and clears pointer-driven visible mutation state.
func (b *BaseDialog) TakeVisualDirty() bool {
	dirty := b.visualDirty
	for i := range b.scrollviews {
		child := &b.scrollviews[i]
		generation := child.model.VisualGeneration()
		dirty = dirty || generation != child.generation
		child.generation = generation
	}
	b.visualDirty = false
	return dirty
}

// HandleConfirmButtonsClick performs exact terminal-cell pill hit testing.
func (b *BaseDialog) HandleConfirmButtonsClick(msg tea.MouseClickMsg, dl DialogLayout, _ lipgloss.Style, onYes tea.Cmd) tea.Cmd {
	if msg.Button != tea.MouseLeft {
		return nil
	}
	if k, ok := b.ActionKeyAt(msg.X, msg.Y, dl); ok {
		if k.Code == 'n' {
			return closeDialogCmd()
		}
		if k.Code == 'y' {
			return ConfirmAndClose(onYes)
		}
	}
	return nil
}

// RenderCard renders dialog content with the shared top-right close control.
func (b *BaseDialog) RenderCard(style lipgloss.Style, dialogWidth int, content string) string {
	width := min(max(1, dialogWidth), max(1, b.width))
	if b.height < 8 {
		style = style.PaddingTop(0).PaddingBottom(0)
	}
	if width < 10 {
		style = style.PaddingLeft(0).PaddingRight(0)
	}
	inner := max(1, width-style.GetHorizontalFrameSize())
	lines := strings.Split(content, "\n")
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], inner, "")
	}
	available := max(1, b.height-style.GetVerticalFrameSize())
	if len(lines) > available {
		// Preserve the action rows, rather than clipping the bottom of the card.
		footer := min(max(1, len(b.actionRows)), available)
		lines = append(lines[:available-footer], lines[len(lines)-footer:]...)
	}
	view := renderCloseControl(style.Width(width).Render(strings.Join(lines, "\n")), b.closeHovered)
	view = clampRenderedFrame(view, b.width, b.height)
	b.cardWidth, b.cardHeight = lipgloss.Width(view), lipgloss.Height(view)
	b.actionContentX = style.GetBorderLeftSize() + style.GetPaddingLeft()
	b.actionContentWidth = max(0, min(inner, b.cardWidth-b.actionContentX-style.GetBorderRightSize()-style.GetPaddingRight()))
	b.actionFooterHeight = len(b.actionRows)
	if b.actionScrollActive && b.actionScroll != nil {
		b.actionFooterHeight = b.actionScroll.VisibleHeight()
	}
	b.actionFooterHeight = min(b.actionFooterHeight, len(lines))
	b.actionFooterStart = style.GetBorderTopSize() + style.GetPaddingTop() + len(lines) - b.actionFooterHeight
	return view
}

func renderCloseControl(view string, hovered bool) string {
	lines := strings.Split(view, "\n")
	target, line, ok := closeControlCell(lipgloss.Width(view), len(lines))
	if !ok {
		return strings.Join(lines, "\n")
	}
	glyphStyle := styles.NoStyle.Foreground(styles.TextSecondary)
	if hovered {
		glyphStyle = glyphStyle.Foreground(styles.Error).Bold(true)
	}
	glyph := glyphStyle.Render(dialogCloseGlyph)
	if idx := visibleColumnByteIndex(lines[line], target); idx >= 0 {
		_, size := utf8.DecodeRuneInString(lines[line][idx:])
		lines[line] = lines[line][:idx] + glyph + lines[line][idx+size:]
	}
	return strings.Join(lines, "\n")
}

func visibleColumnByteIndex(s string, target int) int {
	col := 0
	for i := 0; i < len(s); {
		if s[i] == '\x1b' {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			if i < len(s) {
				i++
			}
			continue
		}
		if col == target {
			return i
		}
		_, n := utf8.DecodeRuneInString(s[i:])
		i += n
		col++
	}
	return -1
}

// ComputeDialogWidth calculates dialog width based on screen percentage with bounds.
func (b *BaseDialog) ComputeDialogWidth(percent, minWidth, maxWidth int) int {
	width := b.width * percent / 100
	if width < minWidth {
		width = max(20, min(b.width-4, minWidth))
	}
	if width > maxWidth {
		width = min(maxWidth, b.width-4)
	}
	return min(max(1, width), max(1, b.width))
}

// ContentWidth calculates the inner content width given dialog width and padding.
func (b *BaseDialog) ContentWidth(dialogWidth, paddingX int) int {
	// Border takes one character on each side
	frameHorizontal := (paddingX * 2) + 2
	return max(1, dialogWidth-frameHorizontal)
}

// CenterDialog returns the (row, col) position to center a rendered dialog.
func (b *BaseDialog) CenterDialog(renderedDialog string) (row, col int) {
	dialogWidth := lipgloss.Width(renderedDialog)
	dialogHeight := lipgloss.Height(renderedDialog)
	return CenterPosition(b.width, b.height, dialogWidth, dialogHeight)
}

// ContentStartRow returns the absolute Y row where content begins inside a dialog.
// dialogRow is the top-left row of the dialog, and headerContent is the rendered
// header text above the target content area. The dialog frame (border + padding)
// is accounted for automatically using DialogStyle.
func ContentStartRow(dialogRow int, headerContent string) int {
	frameTop := styles.DialogStyle.GetBorderTopSize() + styles.DialogStyle.GetPaddingTop()
	return dialogRow + frameTop + lipgloss.Height(headerContent)
}

// ContentEndRow returns the absolute Y row of the last content line inside a dialog.
// dialogRow is the top-left row and dialogHeight is the total rendered height.
// The dialog frame (border + padding) is accounted for automatically using DialogStyle.
func ContentEndRow(dialogRow, dialogHeight int) int {
	frameBottom := styles.DialogStyle.GetBorderBottomSize() + styles.DialogStyle.GetPaddingBottom()
	return dialogRow + dialogHeight - 1 - frameBottom
}

// ElicitationRef correlates an elicitation dialog with the runtime request it
// answers: the session that raised it, the interaction the response must
// name, and the elicitation's own id. Answers without SessionID/RequestID
// cannot be routed to a session.
type ElicitationRef struct {
	SessionID     string
	RequestID     string
	ElicitationID string
}

// ElicitationRefFor extracts the correlation of an elicitation request event.
func ElicitationRefFor(ev *runtime.ElicitationRequestEvent) ElicitationRef {
	return ElicitationRef{SessionID: ev.SessionID, RequestID: ev.RequestID, ElicitationID: ev.ElicitationID}
}

// CloseWithElicitationResponse returns a command that closes the dialog and
// sends the elicitation response for ref.
func CloseWithElicitationResponse(action tools.ElicitationAction, content map[string]any, ref ElicitationRef) tea.Cmd {
	return tea.Sequence(
		core.CmdHandler(CloseDialogMsg{}),
		core.CmdHandler(messages.InteractionResponseMsg{
			SessionID: ref.SessionID,
			Response: runtime.InteractionResponse{
				InteractionID: ref.RequestID,
				Kind:          runtime.InteractionElicitation,
				ElicitationID: ref.ElicitationID,
				Elicitation:   runtime.ElicitationResult{Action: action, Content: content},
			},
		}),
	)
}

// RenderTitle renders a dialog title with the given style and width.
func RenderTitle(title string, contentWidth int, style lipgloss.Style) string {
	return style.Width(max(1, contentWidth)).Render(ansi.Truncate(title, max(1, contentWidth), "…"))
}

// RenderSeparator renders a horizontal separator line.
func RenderSeparator(contentWidth int) string {
	separatorWidth := max(1, contentWidth)
	return styles.DialogSeparatorStyle.
		Align(lipgloss.Center).
		Width(contentWidth).
		Render(strings.Repeat("─", separatorWidth))
}

// RenderGroupSeparator renders a labelled section separator inside a list,
// like "── Custom themes ──────────────". It is used to visually divide
// groups of items in a picker list.
func RenderGroupSeparator(label string, contentWidth int) string {
	prefix := "── " + strings.TrimSpace(label) + " "
	dashes := max(0, contentWidth-lipgloss.Width(prefix)-2)
	return styles.MutedStyle.Render(prefix + strings.Repeat("─", dashes))
}

// RenderHelp renders help text at the bottom of a dialog in italic muted style.
func RenderHelp(text string, contentWidth int) string {
	return styles.DialogHelpStyle.Width(contentWidth).Align(lipgloss.Center).Render(text)
}

// helpKeysLine formats key bindings into a single line, styled like the main
// TUI's status bar. Each binding is a pair of [key, description] strings. It
// returns "" for empty or malformed input.
func helpKeysLine(bindings ...string) string {
	if len(bindings) == 0 || len(bindings)%2 != 0 {
		return ""
	}

	var parts []string
	for i := 0; i < len(bindings); i += 2 {
		keyPart := styles.HighlightWhiteStyle.Render(bindings[i])
		descPart := styles.SecondaryStyle.Render(bindings[i+1])
		parts = append(parts, keyPart+" "+descPart)
	}

	return strings.Join(parts, "  ")
}

// RenderHelpKeys renders key bindings in the same style as the main TUI's status bar.
// Each binding is a pair of [key, description] strings.
func RenderHelpKeys(contentWidth int, bindings ...string) string {
	if len(bindings) == 0 || len(bindings)%2 != 0 {
		return ""
	}
	return styles.BaseStyle.Width(contentWidth).Align(lipgloss.Center).Render(helpKeysLine(bindings...))
}

// HelpKeysWidth returns the rendered width of the given key bindings laid out
// on a single line, using the same formatting as RenderHelpKeys. It returns 0
// for empty or malformed input.
func HelpKeysWidth(bindings ...string) int {
	return lipgloss.Width(helpKeysLine(bindings...))
}

// HandleQuit handles a quit key locally as semantic cancellation. The root
// owns opening exit confirmation when no dialog is active.
func HandleQuit(msg tea.KeyPressMsg) tea.Cmd {
	if key.Matches(msg, core.GetKeys().Quit) {
		return core.CmdHandler(CloseDialogMsg{})
	}
	return nil
}

// HandleConfirmKeys handles Yes/No key presses for confirmation dialogs.
// Returns the command to execute and whether a key was matched.
func HandleConfirmKeys(msg tea.KeyPressMsg, keyMap ConfirmKeyMap, onYes, onNo func() (layout.Model, tea.Cmd)) (layout.Model, tea.Cmd, bool) {
	switch {
	case key.Matches(msg, keyMap.Yes):
		model, cmd := onYes()
		return model, cmd, true
	case key.Matches(msg, keyMap.No):
		model, cmd := onNo()
		return model, cmd, true
	}
	return nil, nil, false
}

// Content helps build dialog content with consistent structure.
type Content struct {
	width int
	parts []string
}

// NewContent creates a new dialog content builder.
func NewContent(contentWidth int) *Content {
	return &Content{width: contentWidth}
}

// AddTitle adds a styled title to the dialog.
func (dc *Content) AddTitle(title string) *Content {
	dc.parts = append(dc.parts, RenderTitle(title, dc.width, styles.DialogTitleStyle))
	return dc
}

// AddSeparator adds a horizontal separator line.
func (dc *Content) AddSeparator() *Content {
	dc.parts = append(dc.parts, RenderSeparator(dc.width))
	return dc
}

// AddSpace adds an empty line for spacing.
func (dc *Content) AddSpace() *Content {
	dc.parts = append(dc.parts, "")
	return dc
}

// AddQuestion adds a styled question text.
func (dc *Content) AddQuestion(question string) *Content {
	dc.parts = append(dc.parts, styles.DialogQuestionStyle.Width(dc.width).Render(question))
	return dc
}

// AddContent adds raw content to the dialog.
func (dc *Content) AddContent(content string) *Content {
	dc.parts = append(dc.parts, content)
	return dc
}

// AddHelpKeys adds key binding help at the bottom.
func (dc *Content) AddHelpKeys(bindings ...string) *Content {
	dc.parts = append(dc.parts, RenderHelpKeys(dc.width, bindings...))
	return dc
}

// AddHelp adds help text at the bottom.
func (dc *Content) AddHelp(text string) *Content {
	dc.parts = append(dc.parts, RenderHelp(text, dc.width))
	return dc
}

// Build returns the final dialog content as a vertical join.
func (dc *Content) Build() string {
	return lipgloss.JoinVertical(lipgloss.Left, dc.parts...)
}

// Action connects a visible action pill to its existing keyboard path.
type Action struct {
	Label        string
	Key          tea.KeyPressMsg
	Disabled     bool
	Default      bool
	HideShortcut bool
}
type dialogActionRow struct {
	text string
	hits []dialogActionHit
}
type dialogActionHit struct {
	x, width int
	key      tea.KeyPressMsg
}

func (b *BaseDialog) RenderActions(contentWidth int, actions ...Action) string {
	b.actionRows = nil
	b.actions = append(b.actions[:0], actions...)
	b.actionLines = make([]int, len(actions))
	width := max(1, contentWidth)
	button := styles.NoStyle.Padding(0, 1).Bold(true).Foreground(styles.TextPrimary).Background(styles.BackgroundAlt)
	var rendered []string
	var row string
	var hits []dialogActionHit
	flush := func() {
		if row == "" {
			return
		}
		offset := max(0, width-lipgloss.Width(row))
		for i := range hits {
			hits[i].x += offset
		}
		text := strings.Repeat(" ", offset) + row
		b.actionRows = append(b.actionRows, dialogActionRow{text: ansi.Strip(text), hits: hits})
		rendered = append(rendered, text)
		row, hits = "", nil
	}
	selected := b.selectedAction(actions)
	for index, action := range actions {
		pillStyle := button
		if action.Disabled {
			pillStyle = pillStyle.Foreground(styles.TextMuted).Bold(false)
		}
		if index == selected {
			pillStyle = pillStyle.Foreground(styles.SelectedFg).Background(styles.Selected)
		}
		label := action.Label
		shortcut := ""
		if !action.HideShortcut && (action.Key.Code != tea.KeyEnter || action.Key.Mod != 0) {
			shortcut = action.Key.Keystroke()
		}
		if index == selected {
			if shortcut != "" {
				shortcut = "↵ " + shortcut
			} else {
				shortcut = "↵"
			}
		}
		labelWidth := max(1, width-2)
		if shortcut != "" && lipgloss.Width(shortcut)+2 < labelWidth {
			label = ansi.Truncate(label, labelWidth-lipgloss.Width(shortcut)-1, "") + " " + shortcut
		}
		pill := pillStyle.Render(ansi.Truncate(label, labelWidth, ""))
		if row != "" && lipgloss.Width(row)+1+lipgloss.Width(pill) > width {
			flush()
		}
		if row != "" {
			row += " "
		}
		b.actionLines[index] = len(b.actionRows)
		if !action.Disabled {
			hits = append(hits, dialogActionHit{x: lipgloss.Width(row), width: lipgloss.Width(pill), key: action.Key})
		}
		row += pill
	}
	flush()
	return strings.Join(rendered, "\n")
}

// RenderActionKeys reuses family key bindings while omitting redundant navigation hints.
func (b *BaseDialog) RenderActionKeys(width int, bindings ...string) string {
	return b.RenderActions(width, actionsForKeys(bindings...)...)
}

func actionsForKeys(bindings ...string) []Action {
	var actions []Action
	for i := 0; i+1 < len(bindings); i += 2 {
		name, label := bindings[i], bindings[i+1]
		switch label {
		case "navigate", "scroll", "up", "down":
			continue
		}
		if name != "/" {
			name = strings.Split(name, "/")[0]
		}
		k := tea.KeyPressMsg{}
		switch name {
		case "enter", "Enter":
			k.Code = tea.KeyEnter
		case "esc":
			k.Code = tea.KeyEscape
		case "left":
			k.Code = tea.KeyLeft
		case "right":
			k.Code = tea.KeyRight
		case "tab":
			k.Code = tea.KeyTab
		case "space":
			k.Code = tea.KeySpace
		case "backspace":
			k.Code = tea.KeyBackspace
		default:
			if strings.HasPrefix(name, "alt+") {
				k.Mod = tea.ModAlt
				name = strings.TrimPrefix(name, "alt+")
			}
			if strings.HasPrefix(name, "ctrl+") {
				k.Mod = tea.ModCtrl
				name = strings.TrimPrefix(name, "ctrl+")
			}
			runes := []rune(name)
			if len(runes) != 1 {
				continue
			}
			k.Code = runes[0]
			if k.Mod == 0 {
				k.Text = name
			}
		}
		actions = append(actions, Action{Label: label, Key: k, Default: k.Code == tea.KeyEnter && k.Mod == 0})
	}
	return actions
}

func (b *BaseDialog) ActionKeyAt(x, y int, dl DialogLayout) (tea.KeyPressMsg, bool) {
	lines := strings.Split(ansi.Strip(dl.View), "\n")
	line := y - dl.Row
	if line < 0 || line >= len(lines) || line < b.actionFooterStart || line >= b.actionFooterStart+b.actionFooterHeight || x < dl.Col+b.actionContentX || x >= dl.Col+b.actionContentX+b.actionContentWidth {
		return tea.KeyPressMsg{}, false
	}
	rowIndex := line - b.actionFooterStart
	if b.actionScrollActive && b.actionScroll != nil {
		rowIndex += b.actionScroll.ScrollOffset()
	}
	if rowIndex >= 0 && rowIndex < len(b.actionRows) {
		rel := x - dl.Col - b.actionContentX
		for _, hit := range b.actionRows[rowIndex].hits {
			if rel >= hit.x && rel < min(hit.x+hit.width, b.actionContentWidth) {
				return hit.key, true
			}
		}
	}
	return tea.KeyPressMsg{}, false
}

// PrepareScrollableBody updates body and footer viewports only at input/size lifecycle boundaries.
func (b *BaseDialog) PrepareScrollableBody(style lipgloss.Style, dialogWidth int, header, body, footer string) {
	b.normalizeActionFocus()
	b.bodyPreparationCount++
	if b.bodyScroll == nil {
		b.bodyScroll = b.newScrollview(scrollview.WithKeyMap(&scrollview.ScrollKeyMap{PageUp: key.NewBinding(key.WithKeys("pgup")), PageDown: key.NewBinding(key.WithKeys("pgdown"))}), scrollview.WithReserveScrollbarSpace(true))
	}
	style, width, inner, available := b.bodyFrame(style, dialogWidth)
	headers, footers := bodyChrome(header, footer, available)
	b.bodyHeaderRows = len(headers)
	b.bodyTitleGap = 0
	if len(headers) > 2 && headers[1] == "" {
		b.bodyTitleGap = 1
	}
	b.bodyHeaderGap = 0
	if len(headers) > 0 && headers[len(headers)-1] == "" {
		b.bodyHeaderGap = 1
	}
	b.actionScrollActive = len(footers) > max(1, available-1)
	if b.actionScrollActive {
		if b.actionScroll == nil {
			b.actionScroll = b.newScrollview(scrollview.WithKeyMap(nil), scrollview.WithFadeEffectDisabled())
		}
		b.actionScroll.SetSize(inner+2, max(1, available-1))
		b.actionScroll.SetContent(footers, len(footers))
		if b.actionsFocused && b.focusedAction < len(b.actionLines) {
			b.actionScroll.EnsureLineVisible(b.actionLines[b.focusedAction])
		}
	}
	footerHeight := len(footers)
	if b.actionScrollActive {
		footerHeight = b.actionScroll.VisibleHeight()
	}
	lines := wrapBodyLines(body, max(1, inner-b.bodyScroll.ReservedCols()))
	b.bodyFooterGap = 0
	if footerHeight > 0 && !b.actionScrollActive && len(headers)+footerHeight+2 <= available {
		b.bodyFooterGap = 1
	}
	viewport := max(1, available-len(headers)-footerHeight-b.bodyFooterGap)
	if !b.bodyFillHeight {
		viewport = min(viewport, max(1, len(lines)))
	}
	b.bodyScroll.SetSize(inner, viewport)
	b.bodyScroll.SetContent(lines, len(lines))
	total := len(headers) + viewport + b.bodyFooterGap + footerHeight + style.GetVerticalFrameSize()
	row, col := CenterPosition(b.width, b.height, width, min(b.height, total))
	b.bodyX, b.bodyY = col+style.GetBorderLeftSize()+style.GetPaddingLeft(), row+style.GetBorderTopSize()+style.GetPaddingTop()+len(headers)
	b.bodyWidth, b.bodyHeight = inner, viewport
	b.bodyScroll.SetPosition(b.bodyX, b.bodyY)
	if b.actionScrollActive {
		b.actionScroll.SetPosition(b.bodyX, b.bodyY+viewport+b.bodyFooterGap)
	}
}

// RenderScrollableBody composes current themed content using already-prepared viewport geometry.
func (b *BaseDialog) RenderScrollableBody(style lipgloss.Style, dialogWidth int, header, body, footer string) string {
	style, width, inner, available := b.bodyFrame(style, dialogWidth)
	headers, footers := bodyChrome(header, footer, available)
	if b.bodyScroll == nil {
		return b.RenderCard(style, width, strings.Join(append(append(headers, body), footers...), "\n"))
	}
	lines := wrapBodyLines(body, max(1, inner-b.bodyScroll.ReservedCols()))
	start := min(b.bodyScroll.ScrollOffset(), len(lines))
	out := b.bodyScroll.ViewWithRestyledLines(lines[start:])
	if b.actionScrollActive && b.actionScroll != nil {
		start := min(b.actionScroll.ScrollOffset(), len(footers))
		footers = strings.Split(b.actionScroll.ViewWithRestyledLines(footers[start:]), "\n")
	}
	parts := append(headers, out)
	if b.bodyFooterGap > 0 {
		parts = append(parts, "")
	}
	parts = append(parts, footers...)
	return b.RenderCard(style, width, strings.Join(parts, "\n"))
}

func (b *BaseDialog) bodyFrame(style lipgloss.Style, dialogWidth int) (lipgloss.Style, int, int, int) {
	width := min(max(1, dialogWidth), max(1, b.width))
	if b.height < 8 {
		style = style.PaddingTop(0).PaddingBottom(0)
	}
	if width < 10 {
		style = style.PaddingLeft(0).PaddingRight(0)
	}
	height := b.height
	if b.bodyMaxHeight > 0 {
		height = min(height, b.bodyMaxHeight)
	}
	return style, width, max(1, width-style.GetHorizontalFrameSize()), max(1, height-style.GetVerticalFrameSize())
}

// bodyChrome owns structural spacing; callers supply title/header content and
// actions without margins. Body rows are deliberately not normalized here.
func bodyChrome(header, footer string, available int) ([]string, []string) {
	headers := trimChromeLines(header)
	footers := trimChromeLines(footer)
	// Older callers may already separate the title from header controls. Remove
	// only that structural gap before allocating one shared, adaptive gap.
	for len(headers) > 1 && strings.TrimSpace(ansi.Strip(headers[1])) == "" {
		headers = append(headers[:1], headers[2:]...)
	}
	if len(headers)+len(footers)+1 > available {
		headers = headers[:min(1, len(headers))]
	}
	if len(headers)+len(footers)+1 > available {
		headers = nil
	}
	// Reclaim decorative spacing before dropping header controls or body rows.
	if len(headers) > 1 && len(headers)+len(footers)+3 <= available {
		headers = append(headers[:1], append([]string{""}, headers[1:]...)...)
	}
	if len(headers) > 0 && len(headers)+len(footers)+2 <= available {
		headers = append(headers, "")
	}
	return headers, footers
}

func trimChromeLines(content string) []string {
	lines := strings.Split(content, "\n")
	for len(lines) > 0 && strings.TrimSpace(ansi.Strip(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// headerRow locates a header content row in the prepared frame, excluding the
// shared title gap. Hidden compact headers have no pointer target.
func (b *BaseDialog) headerRow(line int) (int, bool) {
	if line > 0 {
		line += b.bodyTitleGap
	}
	if line < 0 || line >= b.bodyHeaderRows-b.bodyHeaderGap {
		return 0, false
	}
	return b.bodyY - b.bodyHeaderRows + line, true
}

func wrapBodyLines(body string, width int) []string {
	var lines []string
	for line := range strings.SplitSeq(body, "\n") {
		if lipgloss.Width(strings.TrimRight(ansi.Strip(line), " ")) <= width {
			lines = append(lines, line)
		} else {
			lines = append(lines, strings.Split(ansi.Hardwrap(line, width, true), "\n")...)
		}
	}
	return lines
}

func (b *BaseDialog) UpdateActionScroll(msg tea.Msg) (bool, tea.Cmd) {
	if !b.actionScrollActive || b.actionScroll == nil {
		return false, nil
	}
	switch m := msg.(type) {
	case tea.MouseWheelMsg:
		if m.Y < b.bodyY+b.bodyHeight+b.bodyFooterGap || m.Y >= b.bodyY+b.bodyHeight+b.bodyFooterGap+b.actionScroll.VisibleHeight() {
			return false, nil
		}
	case messages.WheelCoalescedMsg:
		if m.Y < b.bodyY+b.bodyHeight+b.bodyFooterGap || m.Y >= b.bodyY+b.bodyHeight+b.bodyFooterGap+b.actionScroll.VisibleHeight() {
			return false, nil
		}
	}
	return b.actionScroll.Update(msg)
}

func (b *BaseDialog) UpdateBodyScroll(msg tea.Msg) (bool, tea.Cmd) {
	if handled, cmd := b.UpdateActionScroll(msg); handled {
		return handled, cmd
	}
	if b.bodyScroll == nil {
		return false, nil
	}
	return b.bodyScroll.Update(msg)
}

func (b *BaseDialog) BodyScrollBounds() (x, y, width, height int) {
	return b.bodyX, b.bodyY, b.bodyWidth, b.bodyHeight
}

func (b *BaseDialog) BodyScrollOffset() int {
	if b.bodyScroll == nil {
		return 0
	}
	return b.bodyScroll.ScrollOffset()
}

func (b *BaseDialog) EnsureBodyLineVisible(line int) {
	if b.bodyScroll != nil {
		b.bodyScroll.EnsureLineVisible(line)
	}
}

// BodyContentWidth is the text width after the reserved scrollbar columns.
func (b *BaseDialog) BodyContentWidth(dialogWidth int) int {
	return max(1, b.ContentWidth(dialogWidth, 2)-2)
}

func preparesDialogBody(msg tea.Msg) bool {
	switch msg.(type) {
	case animation.TickMsg, tea.MouseMotionMsg, tea.MouseWheelMsg, tea.MouseReleaseMsg, messages.WheelCoalescedMsg:
		return false
	default:
		return true
	}
}

// ActionsFocused reports whether arrows and Enter belong to the action section.
func (b *BaseDialog) ActionsFocused() bool { return b.actionsFocused }

// HandleActionKey is called at the content section's Tab boundary, or while actions are focused.
// A handled result with a zero key only changes focus; a nonzero key follows the existing action route.
func (b *BaseDialog) HandleActionKey(msg tea.KeyPressMsg) (action tea.KeyPressMsg, handled bool) {
	b.normalizeActionFocus()
	if msg.Code == tea.KeyTab {
		if b.actionsFocused {
			b.actionsFocused = false
			b.MarkVisualDirty()
			return tea.KeyPressMsg{}, true
		}
		if b.FocusActions(msg.Mod&tea.ModShift != 0) {
			return tea.KeyPressMsg{}, true
		}
		return tea.KeyPressMsg{}, false
	}
	if !b.actionsFocused {
		return tea.KeyPressMsg{}, false
	}
	switch msg.Code {
	case tea.KeyLeft, tea.KeyUp, tea.KeyRight, tea.KeyDown:
		delta := 1
		if msg.Code == tea.KeyLeft || msg.Code == tea.KeyUp {
			delta = -1
		}
		for step := 1; step <= len(b.actions); step++ {
			i := (b.focusedAction + delta*step + len(b.actions)) % len(b.actions)
			if !b.actions[i].Disabled {
				b.focusedAction = i
				b.MarkVisualDirty()
				b.revealAction()
				break
			}
		}
		return tea.KeyPressMsg{}, true
	case tea.KeyEnter:
		if b.focusedAction >= 0 && b.focusedAction < len(b.actions) && !b.actions[b.focusedAction].Disabled {
			k := b.actions[b.focusedAction].Key
			b.actionsFocused = false
			b.MarkVisualDirty()
			return k, true
		}
		return tea.KeyPressMsg{}, true
	}
	return tea.KeyPressMsg{}, false
}

func (b *BaseDialog) normalizeActionFocus() {
	if !b.actionsFocused {
		return
	}
	if b.focusedAction >= 0 && b.focusedAction < len(b.actions) && !b.actions[b.focusedAction].Disabled {
		return
	}
	for i, a := range b.actions {
		if !a.Disabled {
			b.focusedAction = i
			return
		}
	}
	b.actionsFocused = false
}

func (b *BaseDialog) revealAction() {
	if b.actionScrollActive && b.actionScroll != nil && b.focusedAction < len(b.actionLines) {
		b.actionScroll.EnsureLineVisible(b.actionLines[b.focusedAction])
	}
}

// FocusActions enters the first or last enabled action from a content boundary.
func (b *BaseDialog) FocusActions(last bool) bool {
	start, delta := 0, 1
	if last {
		start, delta = len(b.actions)-1, -1
	}
	for i := start; i >= 0 && i < len(b.actions); i += delta {
		if b.actions[i].Disabled {
			continue
		}
		b.actionsFocused = true
		b.focusedAction = i
		b.MarkVisualDirty()
		b.revealAction()
		return true
	}
	return false
}

// BlurActions restores keyboard ownership to dialog content.
func (b *BaseDialog) BlurActions() {
	if b.actionsFocused {
		b.actionsFocused = false
		b.MarkVisualDirty()
	}
}

func closeControlCell(width, height int) (x, y int, ok bool) {
	if width < 3 || height < 2 {
		return 0, 0, false
	}
	return max(1, width-styles.DialogStyle.GetBorderRightSize()-1-dialogCloseInset), styles.DialogStyle.GetBorderTopSize(), true
}

func (b *BaseDialog) selectedAction(actions []Action) int {
	if b.actionsFocused && b.focusedAction >= 0 && b.focusedAction < len(actions) && !actions[b.focusedAction].Disabled {
		return b.focusedAction
	}
	if !b.actionsFocused {
		for i, a := range actions {
			if a.Default && !a.Disabled {
				return i
			}
		}
	}
	return -1
}

// FocusDefaultAction promotes the explicit content default into keyboard action focus.
func (b *BaseDialog) FocusDefaultAction() bool {
	for i, a := range b.actions {
		if !a.Default || a.Disabled {
			continue
		}
		b.actionsFocused = true
		b.focusedAction = i
		b.MarkVisualDirty()
		b.revealAction()
		return true
	}
	return false
}

// SelectedActionKey returns the enabled focused or default action without changing focus.
func (b *BaseDialog) SelectedActionKey() (tea.KeyPressMsg, bool) {
	index := b.selectedAction(b.actions)
	if index < 0 {
		return tea.KeyPressMsg{}, false
	}
	return b.actions[index].Key, true
}
