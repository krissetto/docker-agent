package dialog

import (
	"strings"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// Close-button rendering constants.
const (
	dialogCloseGlyph   = "✕"
	dialogCloseInset   = 1
	confirmEnterSuffix = " ↵"
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

var confirmFocusToggleKeys = key.NewBinding(key.WithKeys("tab", "shift+tab", "left", "right"))

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
	actionScroll                          *scrollview.Model
	actionScrollActive                    bool
	actionFooterStart, actionFooterHeight int
	actionContentX, actionContentWidth    int
	bodyScroll                            *scrollview.Model
	bodyX, bodyY, bodyWidth, bodyHeight   int
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
	case key.Matches(msg, confirmFocusToggleKeys):
		if b.confirmFocus == ConfirmFocusYes {
			b.confirmFocus = ConfirmFocusNo
		} else {
			b.confirmFocus = ConfirmFocusYes
		}
		return ConfirmKeyFocusToggled
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
	noLabel, yesLabel := "No", "Yes"
	if b.confirmFocus == ConfirmFocusYes {
		yesLabel += confirmEnterSuffix
	} else {
		noLabel += confirmEnterSuffix
	}
	out := b.RenderActions(contentWidth, Action{Label: noLabel, Key: tea.KeyPressMsg{Code: 'n', Text: "n"}}, Action{Label: yesLabel, Key: tea.KeyPressMsg{Code: 'y', Text: "y"}})
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
	return msg.Y == dl.Row+styles.DialogStyle.GetBorderTopSize() && msg.X == dl.Col+dl.Width-styles.DialogStyle.GetBorderRightSize()-1-dialogCloseInset
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
	line := styles.DialogStyle.GetBorderTopSize()
	if line < 0 || line >= len(lines) {
		return strings.Join(lines, "\n")
	}
	glyphStyle := styles.NoStyle.Foreground(styles.TextSecondary)
	if hovered {
		glyphStyle = glyphStyle.Foreground(styles.Error).Bold(true)
	}
	glyph := glyphStyle.Render(dialogCloseGlyph)
	target := lipgloss.Width(ansi.Strip(lines[line])) - styles.DialogStyle.GetBorderRightSize() - 1 - dialogCloseInset
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
	return style.Width(contentWidth).Render(title)
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
	Label string
	Key   tea.KeyPressMsg
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
	for _, action := range actions {
		pill := button.Render(ansi.Truncate(action.Label, max(1, width-2), ""))
		if row != "" && lipgloss.Width(row)+1+lipgloss.Width(pill) > width {
			flush()
		}
		if row != "" {
			row += " "
		}
		hits = append(hits, dialogActionHit{x: lipgloss.Width(row), width: lipgloss.Width(pill), key: action.Key})
		row += pill
	}
	flush()
	return strings.Join(rendered, "\n")
}

// RenderActionKeys reuses family key bindings while omitting redundant navigation hints.
func (b *BaseDialog) RenderActionKeys(width int, bindings ...string) string {
	var actions []Action
	for i := 0; i+1 < len(bindings); i += 2 {
		name, label := bindings[i], bindings[i+1]
		switch label {
		case "navigate", "scroll", "up", "down":
			continue
		}
		name = strings.Split(name, "/")[0]
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
		actions = append(actions, Action{Label: label, Key: k})
	}
	return b.RenderActions(width, actions...)
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
	if b.bodyScroll == nil {
		b.bodyScroll = b.newScrollview(scrollview.WithKeyMap(&scrollview.ScrollKeyMap{PageUp: key.NewBinding(key.WithKeys("pgup")), PageDown: key.NewBinding(key.WithKeys("pgdown"))}), scrollview.WithReserveScrollbarSpace(true))
	}
	style, width, inner, available := b.bodyFrame(style, dialogWidth)
	headers, footers := bodyChrome(header, footer, available)
	b.actionScrollActive = len(footers) > max(1, available-1)
	if b.actionScrollActive {
		if b.actionScroll == nil {
			b.actionScroll = b.newScrollview(scrollview.WithKeyMap(nil), scrollview.WithFadeEffectDisabled())
		}
		b.actionScroll.SetSize(inner+2, max(1, available-1))
		b.actionScroll.SetContent(footers, len(footers))
	}
	footerHeight := len(footers)
	if b.actionScrollActive {
		footerHeight = b.actionScroll.VisibleHeight()
	}
	lines := wrapBodyLines(body, max(1, inner-b.bodyScroll.ReservedCols()))
	viewport := max(1, available-len(headers)-footerHeight)
	if !b.bodyFillHeight {
		viewport = min(viewport, max(1, len(lines)))
	}
	b.bodyScroll.SetSize(inner, viewport)
	b.bodyScroll.SetContent(lines, len(lines))
	total := len(headers) + viewport + footerHeight + style.GetVerticalFrameSize()
	row, col := CenterPosition(b.width, b.height, width, min(b.height, total))
	b.bodyX, b.bodyY = col+style.GetBorderLeftSize()+style.GetPaddingLeft(), row+style.GetBorderTopSize()+style.GetPaddingTop()+len(headers)
	b.bodyWidth, b.bodyHeight = inner, viewport
	b.bodyScroll.SetPosition(b.bodyX, b.bodyY)
	if b.actionScrollActive {
		b.actionScroll.SetPosition(b.bodyX, b.bodyY+viewport)
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

func bodyChrome(header, footer string, available int) ([]string, []string) {
	var headers, footers []string
	if header != "" {
		headers = strings.Split(header, "\n")
	}
	if footer != "" {
		footers = strings.Split(footer, "\n")
	}
	if len(headers)+len(footers)+1 > available {
		headers = headers[:min(1, len(headers))]
	}
	if len(headers)+len(footers)+1 > available {
		headers = nil
	}
	return headers, footers
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
		if m.Y < b.bodyY+b.bodyHeight || m.Y >= b.bodyY+b.bodyHeight+b.actionScroll.VisibleHeight() {
			return false, nil
		}
	case messages.WheelCoalescedMsg:
		if m.Y < b.bodyY+b.bodyHeight || m.Y >= b.bodyY+b.bodyHeight+b.actionScroll.VisibleHeight() {
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
	case tea.MouseMotionMsg, tea.MouseWheelMsg, tea.MouseReleaseMsg, messages.WheelCoalescedMsg:
		return false
	default:
		return true
	}
}
