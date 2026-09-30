package dialog

import (
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	settingsWidthPercent = 60
	settingsMinWidth     = 52
	settingsMaxWidth     = 72
	previewMaxWidth      = 44
	previewMinWidth      = 24
)

const (
	tabAppearance = iota
	tabBehavior
	tabNotifications
	tabPanel
	tabCount
)

var settingsTabLabels = [tabCount]string{"Appearance", "Behavior", "Notifications", "Panel"}

const (
	rowTheme = iota
	rowPosition
	rowSpacing
	rowInfoMode
	rowSessionPath
	rowUsage
	rowAgents
	rowActiveAgents
	rowTools
	rowTodos
	rowTransparentBackground
	rowDimInactivePanes
	rowSplitDiff
	rowExpandThinking
	rowHideToolResults
	rowRenderImages
	rowShowBanner
	appearanceRowCount
)

const (
	rowSendMode = iota
	rowInterruptConfirmation
	rowYOLO
	rowSnapshot
	rowCacheStablePrompts
	rowRestoreTabs
	rowLean
	rowTabTitleLength
	behaviorRowCount
)

const (
	rowSound = iota
	rowSoundThreshold
	rowWarnOnCacheMiss
	notificationsRowCount
)

var sidebarPositions = []messages.SidebarPosition{
	messages.SidebarRight, messages.SidebarLeft, messages.SidebarTop, messages.SidebarBottom,
}

var positionLabels = map[messages.SidebarPosition]string{
	messages.SidebarRight: "Right", messages.SidebarLeft: "Left",
	messages.SidebarTop: "Top", messages.SidebarBottom: "Bottom",
}

var sectionSpacings = []messages.SectionSpacing{
	messages.SpacingCompact, messages.SpacingNormal, messages.SpacingRelaxed,
}

var spacingLabels = map[messages.SectionSpacing]string{
	messages.SpacingCompact: "Compact", messages.SpacingNormal: "Normal", messages.SpacingRelaxed: "Relaxed",
}

var sidebarInfoModes = []messages.SidebarInfoMode{
	messages.InfoModeCompact, messages.InfoModeDetailed,
}

var infoModeLabels = map[messages.SidebarInfoMode]string{
	messages.InfoModeCompact: "Compact", messages.InfoModeDetailed: "Detailed",
}

var sendModes = []messages.SendMode{messages.SendModeSteer, messages.SendModeQueue}

var interruptConfirmationModes = []messages.InterruptMode{
	messages.InterruptModeAlways, messages.InterruptModeDoubleTap, messages.InterruptModeNone,
}

var interruptConfirmationLabels = map[messages.InterruptMode]string{
	messages.InterruptModeAlways:    "Always (confirm dialog)",
	messages.InterruptModeDoubleTap: "Double-tap (press Esc twice)",
	messages.InterruptModeNone:      "None (immediate)",
}

type settingsFocus int

const (
	settingsControls settingsFocus = iota
	settingsActions
	settingsCategories
)

type settingsHitKind int

const (
	settingsFocusRow settingsHitKind = iota
	settingsActivate
	settingsAdjust
	settingsMovePanel
)

type settingsHit struct {
	x, y, width, height int
	row                 int
	kind                settingsHitKind
	delta               int
}

func (h settingsHit) contains(x, y int) bool {
	return x >= h.x && x < h.x+h.width && y >= h.y && y < h.y+h.height
}

type settingsBody struct {
	width int
	lines []string
	rows  map[int]int
	hits  []settingsHit
}

func (b *settingsBody) add(text string) {
	b.lines = append(b.lines, strings.Split(ansi.Hardwrap(text, b.width, true), "\n")...)
}

func (b *settingsBody) section(title string) {
	if len(b.lines) > 0 {
		b.add("")
	}
	b.add(styles.MutedStyle.Render(title))
}

type settingsDialog struct {
	BaseDialog

	original       messages.Preferences
	current        messages.Preferences
	showVisuals    bool
	tab            int
	selected       [tabCount]int
	panelOrder     []messages.PanelElement
	panelPreviewed bool
	confirmYOLO    bool
	focus          settingsFocus
	rowLines       map[int]int
	rowHits        []settingsHit
	tabHits        []settingsHit
	revealSelected bool
}

func NewSettingsDialog(preferences messages.Preferences, showVisuals bool) Dialog {
	preferences.Layout.SidebarPosition = messages.ParseSidebarPosition(string(preferences.Layout.SidebarPosition))
	preferences.Layout.SectionSpacing = messages.ParseSectionSpacing(string(preferences.Layout.SectionSpacing))
	preferences.Layout.SidebarInfoMode = messages.ParseSidebarInfoMode(string(preferences.Layout.SidebarInfoMode))
	preferences.SendMode = messages.ParseSendMode(string(preferences.SendMode))
	if preferences.TabTitleMaxLength <= 0 {
		preferences.TabTitleMaxLength = 20
	}
	if preferences.SoundThreshold <= 0 {
		preferences.SoundThreshold = 10
	}
	if preferences.InterruptConfirmation == "" {
		preferences.InterruptConfirmation = messages.InterruptModeAlways
	}
	preferences.Panel = messages.NormalizePanelSettings(preferences.Panel)
	current := preferences
	current.Panel = messages.NormalizePanelSettings(preferences.Panel)
	order := slices.Clone(current.Panel.Elements)
	for _, element := range messages.DefaultPanelSettings().Elements {
		if !slices.Contains(order, element) {
			order = append(order, element)
		}
	}
	return &settingsDialog{BaseDialog: BaseDialog{bodyFillHeight: true, bodyMaxHeight: 38}, original: preferences, current: current, showVisuals: showVisuals, panelOrder: order}
}

func (d *settingsDialog) Init() tea.Cmd { return nil }

func (d *settingsDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if preparesDialogBody(msg) {
		defer func() {
			d.prepareBody()
			if d.revealSelected {
				d.revealRow()
				d.revealSelected = false
			}
		}()
	}
	if handled, cmd := d.UpdateBodyScroll(msg); handled {
		return d, cmd
	}
	switch msg := msg.(type) {
	case tea.MouseClickMsg:
		if msg.Button != tea.MouseLeft {
			return d, nil
		}
		view := d.View()
		row, col := d.Position()
		dl := NewDialogLayout(view, row, col)
		if d.CloseButtonHit(msg, dl) {
			return d, d.cancel()
		}
		if action, hit := d.ActionKeyAt(msg.X, msg.Y, dl); hit {
			return d, d.handleKey(action)
		}
		x, y, width, height := d.BodyScrollBounds()
		if tabY, visible := d.headerRow(1); visible && msg.Y == tabY {
			for _, hit := range d.tabHits {
				if hit.contains(msg.X-x, 0) {
					d.setFocus(settingsCategories)
					d.selectTab(hit.row)
					return d, nil
				}
			}
		}
		if msg.X >= x && msg.X < x+width && msg.Y >= y && msg.Y < y+height {
			for _, hit := range d.rowHits {
				if !hit.contains(msg.X-x, msg.Y-y+d.BodyScrollOffset()) || !d.selectable(d.tab, hit.row) {
					continue
				}
				if d.focus != settingsControls || d.selected[d.tab] != hit.row {
					d.confirmYOLO = false
				}
				d.focus = settingsControls
				d.BlurActions()
				d.selected[d.tab] = hit.row
				switch hit.kind {
				case settingsActivate:
					return d, d.changeValue(1)
				case settingsAdjust:
					return d, d.changeValue(hit.delta)
				case settingsMovePanel:
					return d, d.reorderPanel(hit.delta)
				case settingsFocusRow:
					return d, nil
				}
			}
		}
	case tea.WindowSizeMsg:
		return d, d.SetSize(msg.Width, msg.Height)
	case tea.KeyPressMsg:
		return d, d.handleKey(msg)
	}
	return d, nil
}

func (d *settingsDialog) setFocus(focus settingsFocus) {
	d.focus = focus
	d.confirmYOLO = false
	d.BlurActions()
	if focus == settingsActions {
		d.FocusActions(false)
	}
	if focus == settingsControls {
		d.revealSelected = true
	}
	d.MarkVisualDirty()
}

func (d *settingsDialog) selectTab(tab int) {
	d.tab = (tab + tabCount) % tabCount
	d.confirmYOLO = false
	d.revealSelected = true
	d.MarkVisualDirty()
}

func (d *settingsDialog) rowCount() int {
	switch d.tab {
	case tabBehavior:
		return behaviorRowCount
	case tabNotifications:
		return notificationsRowCount
	case tabPanel:
		return len(d.panelOrder)
	default:
		return appearanceRowCount
	}
}

func (d *settingsDialog) selectable(tab, row int) bool {
	if tab == tabAppearance && !d.showVisuals && row >= rowPosition && row <= rowTodos {
		return false
	}
	if tab == tabAppearance && row == rowActiveAgents && d.current.Layout.HideAgents {
		return false
	}
	if tab == tabNotifications && row == rowSoundThreshold && !d.current.Sound {
		return false
	}
	return true
}

func (d *settingsDialog) moveSelection(delta int) {
	for next := d.selected[d.tab] + delta; next >= 0 && next < d.rowCount(); next += delta {
		if d.selectable(d.tab, next) {
			d.selected[d.tab] = next
			return
		}
	}
}

func (d *settingsDialog) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "q", "ctrl+c":
		return d.cancel()
	case "ctrl+s":
		return d.apply()
	case "tab", "shift+tab":
		delta := 1
		if msg.Mod&tea.ModShift != 0 {
			delta = -1
		}
		d.setFocus(settingsFocus((int(d.focus) + delta + 3) % 3))
		return nil
	}
	if d.focus == settingsActions {
		if action, handled := d.HandleActionKey(msg); handled && action.Code != 0 {
			return d.handleKey(action)
		}
		return nil
	}
	if d.focus == settingsCategories {
		switch msg.String() {
		case "left", "up", "h", "k":
			d.selectTab(d.tab - 1)
		case "right", "l":
			d.selectTab(d.tab + 1)
		case "enter", "space", "down", "j":
			d.setFocus(settingsControls)
		}
		return nil
	}
	switch msg.String() {
	case "ctrl+up":
		d.revealSelected = true
		return d.reorderPanel(-1)
	case "ctrl+down":
		d.revealSelected = true
		return d.reorderPanel(1)
	case "up", "k", "ctrl+k":
		d.confirmYOLO = false
		d.moveSelection(-1)
		d.revealSelected = true
	case "down", "j", "ctrl+j":
		d.confirmYOLO = false
		d.moveSelection(1)
		d.revealSelected = true
	case "home", "g":
		d.confirmYOLO = false
		d.selected[d.tab] = 0
		if !d.selectable(d.tab, 0) {
			d.moveSelection(1)
		}
		d.revealSelected = true
	case "end", "G":
		d.confirmYOLO = false
		d.selected[d.tab] = d.rowCount() - 1
		if !d.selectable(d.tab, d.selected[d.tab]) {
			d.moveSelection(-1)
		}
		d.revealSelected = true
	case "left", "h", "right", "l":
		if d.adjustable(d.selected[d.tab]) {
			delta := 1
			if msg.String() == "left" || msg.String() == "h" {
				delta = -1
			}
			return d.changeValue(delta)
		}
	case "enter", "space":
		return d.changeValue(1)
	}
	return nil
}

func (d *settingsDialog) adjustable(row int) bool {
	return (d.tab == tabAppearance && row >= rowPosition && row <= rowInfoMode) ||
		(d.tab == tabBehavior && (row == rowSendMode || row == rowInterruptConfirmation || row == rowTabTitleLength)) ||
		(d.tab == tabNotifications && row == rowSoundThreshold)
}

func (d *settingsDialog) changeValue(delta int) tea.Cmd {
	if !d.selectable(d.tab, d.selected[d.tab]) {
		return nil
	}
	switch d.tab {
	case tabPanel:
		return d.togglePanel()
	case tabAppearance:
		switch d.selected[d.tab] {
		case rowTheme:
			if delta > 0 {
				return core.CmdHandler(messages.OpenThemePickerMsg{})
			}
		case rowPosition:
			d.current.Layout.SidebarPosition = cycleValue(sidebarPositions, d.current.Layout.SidebarPosition, delta)
		case rowSpacing:
			d.current.Layout.SectionSpacing = cycleValue(sectionSpacings, d.current.Layout.SectionSpacing, delta)
		case rowInfoMode:
			d.current.Layout.SidebarInfoMode = cycleValue(sidebarInfoModes, d.current.Layout.SidebarInfoMode, delta)
		case rowSessionPath:
			d.current.Layout.HideSessionPath = !d.current.Layout.HideSessionPath
		case rowUsage:
			d.current.Layout.HideUsage = !d.current.Layout.HideUsage
		case rowAgents:
			d.current.Layout.HideAgents = !d.current.Layout.HideAgents
		case rowActiveAgents:
			if !d.current.Layout.HideAgents {
				d.current.Layout.ActiveAgentsOnly = !d.current.Layout.ActiveAgentsOnly
			}
		case rowTools:
			d.current.Layout.HideTools = !d.current.Layout.HideTools
		case rowTodos:
			d.current.Layout.HideTodos = !d.current.Layout.HideTodos
		case rowTransparentBackground:
			d.current.TransparentBackground = !d.current.TransparentBackground
		case rowDimInactivePanes:
			d.current.DimInactivePanes = !d.current.DimInactivePanes
		case rowSplitDiff:
			d.current.SplitDiffView = !d.current.SplitDiffView
		case rowExpandThinking:
			d.current.ExpandThinking = !d.current.ExpandThinking
		case rowHideToolResults:
			d.current.HideToolResults = !d.current.HideToolResults
		case rowRenderImages:
			d.current.RenderImages = !d.current.RenderImages
		case rowShowBanner:
			d.current.ShowBanner = !d.current.ShowBanner
		}
		if d.selected[d.tab] >= rowPosition && d.selected[d.tab] <= rowTodos {
			return core.CmdHandler(messages.PreviewLayoutMsg{Layout: d.current.Layout})
		}
	case tabBehavior:
		switch d.selected[d.tab] {
		case rowSendMode:
			d.current.SendMode = cycleValue(sendModes, d.current.SendMode, delta)
		case rowYOLO:
			switch {
			case d.current.YOLO:
				d.current.YOLO = false
				d.confirmYOLO = false
			case d.confirmYOLO:
				d.current.YOLO = true
				d.confirmYOLO = false
			default:
				d.confirmYOLO = true
			}
		case rowRestoreTabs:
			d.current.RestoreTabs = !d.current.RestoreTabs
		case rowSnapshot:
			d.current.Snapshot = !d.current.Snapshot
		case rowCacheStablePrompts:
			d.current.CacheStablePrompts = !d.current.CacheStablePrompts
		case rowLean:
			d.current.Lean = !d.current.Lean
		case rowTabTitleLength:
			d.current.TabTitleMaxLength = stepValue(d.current.TabTitleMaxLength, delta, 1, 5, 100)
		case rowInterruptConfirmation:
			d.current.InterruptConfirmation = cycleValue(interruptConfirmationModes, d.current.InterruptConfirmation, delta)
		}
	case tabNotifications:
		switch d.selected[d.tab] {
		case rowSound:
			d.current.Sound = !d.current.Sound
			if !d.current.Sound && d.selected[d.tab] == rowSoundThreshold {
				d.selected[d.tab] = rowSound
			}
		case rowSoundThreshold:
			if d.current.Sound {
				d.current.SoundThreshold = stepValue(d.current.SoundThreshold, delta, 1, 1, 300)
			}
		case rowWarnOnCacheMiss:
			d.current.WarnOnCacheMiss = !d.current.WarnOnCacheMiss
		}
	}
	return nil
}

func cycleValue[T comparable](values []T, current T, delta int) T {
	idx := 0
	for i, v := range values {
		if v == current {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(values)) % len(values)
	return values[idx]
}

func stepValue(current, delta, step, minimum, maximum int) int {
	return max(minimum, min(maximum, current+delta*step))
}

func (d *settingsDialog) apply() tea.Cmd {
	if d.current.Equal(d.original) {
		return d.cancel()
	}
	preferences := d.current
	preferences.Panel = messages.NormalizePanelSettings(preferences.Panel)
	return tea.Sequence(closeDialogCmd(), core.CmdHandler(messages.ApplySettingsMsg{Preferences: preferences}))
}

func (d *settingsDialog) CancelDialogCmd() tea.Cmd { return d.cancel() }

func (d *settingsDialog) cancel() tea.Cmd {
	cmds := []tea.Cmd{closeDialogCmd()}
	if d.current.Layout != d.original.Layout {
		cmds = append(cmds, core.CmdHandler(messages.CancelLayoutPreviewMsg{Original: d.original.Layout}))
	}
	if d.panelPreviewed || !d.current.Panel.Equal(d.original.Panel) {
		cmds = append(cmds, core.CmdHandler(messages.CancelPanelPreviewMsg{Original: messages.NormalizePanelSettings(d.original.Panel)}))
	}
	if len(cmds) == 1 {
		return cmds[0]
	}
	return tea.Sequence(cmds...)
}

func (d *settingsDialog) Position() (row, col int) { return d.CenterDialog(d.View()) }

func (d *settingsDialog) View() string {
	width, header, body, footer, _ := d.bodyParts()
	return d.RenderScrollableBody(styles.DialogStyle, width, header, body, footer)
}

func (d *settingsDialog) bodyParts() (int, string, string, string, map[int]int) {
	width := d.ComputeDialogWidth(settingsWidthPercent, settingsMinWidth, settingsMaxWidth)
	inner := max(1, d.ContentWidth(width, 2)-2)
	body := &settingsBody{width: inner, rows: make(map[int]int)}
	switch d.tab {
	case tabPanel:
		d.renderPanelTab(body)
	case tabBehavior:
		d.renderBehaviorTab(body)
	case tabNotifications:
		d.renderNotificationsTab(body)
	default:
		d.renderAppearanceTab(body)
	}
	d.rowHits = body.hits
	header := RenderTitle("Settings", inner, styles.DialogTitleStyle) + "\n" + d.renderTabBar(inner)
	actions := d.actions()
	selected := d.selectedAction(actions)
	for i := range actions {
		if i != selected {
			actions[i].Label += "  " // Reserve the selected Enter marker without reflowing the footer.
		}
	}
	footer := d.RenderActions(inner, actions...)
	enter := "edit"
	if d.focus == settingsCategories {
		enter = "open"
	} else if d.focus == settingsActions {
		enter = "act"
	}
	helpText := "Tab focus · ↑↓ move · Enter " + enter + " · Ctrl+S apply"
	if inner < lipgloss.Width(helpText) {
		helpText = "Tab focus · Enter " + enter + " · Ctrl+S apply"
	}
	if inner < lipgloss.Width(helpText) {
		helpText = "Tab focus · ↵ " + enter
	}
	help := styles.MutedStyle.Render(ansi.Truncate(helpText, inner, ""))
	d.actionRows = append([]dialogActionRow{{text: ansi.Strip(help)}}, d.actionRows...)
	for i := range d.actionLines {
		d.actionLines[i]++
	}
	return width, header, strings.Join(body.lines, "\n"), help + "\n" + footer, body.rows
}

func (d *settingsDialog) prepareBody() {
	width, header, body, footer, rows := d.bodyParts()
	d.rowLines = rows
	d.PrepareScrollableBody(styles.DialogStyle, width, header, body, footer)
}

func (d *settingsDialog) revealRow() {
	if row, ok := d.rowLines[d.selected[d.tab]]; ok {
		d.EnsureBodyLineVisible(row)
	}
}

func (d *settingsDialog) SetSize(width, height int) tea.Cmd {
	cmd := d.BaseDialog.SetSize(width, height)
	d.prepareBody()
	d.revealRow()
	return cmd
}

func (d *settingsDialog) renderTabBar(width int) string {
	d.tabHits = nil
	start := 0
	for start < d.tab {
		required := 0
		for i := start; i <= d.tab; i++ {
			required += len(settingsTabLabels[i]) + 3
		}
		if required-3 <= width {
			break
		}
		start++
	}
	var tabs []string
	x := 0
	for i := start; i < tabCount && x < width; i++ {
		if i != d.tab && len(settingsTabLabels[i]) > width-x {
			break
		}
		label := ansi.Truncate(settingsTabLabels[i], width-x, "")
		style := styles.MutedStyle
		if i == d.tab {
			style = styles.BaseStyle.Bold(true)
			if d.focus == settingsCategories {
				style = style.Foreground(styles.SelectedFg).Background(styles.Selected)
			}
		}
		tabs = append(tabs, style.Render(label))
		d.tabHits = append(d.tabHits, settingsHit{x: x, width: lipgloss.Width(label), height: 1, row: i})
		x += lipgloss.Width(label) + 3
	}
	return strings.Join(tabs, "   ")
}

func (d *settingsDialog) renderAppearanceTab(body *settingsBody) {
	theme := styles.GetPersistedThemeRef()
	if theme == "" {
		theme = styles.DefaultThemeRef
	}
	d.addControl(body, rowTheme, "Theme", theme+" · Choose…", false, false)
	body.add(styles.MutedStyle.Render("Theme picker saves separately from Settings."))
	if d.showVisuals {
		body.section("Sidebar layout")
		d.addControl(body, rowPosition, "Sidebar position", positionLabels[d.current.Layout.SidebarPosition], false, false)
		d.addControl(body, rowSpacing, "Section spacing", spacingLabels[d.current.Layout.SectionSpacing], false, false)
		d.addControl(body, rowInfoMode, "Sidebar info mode", infoModeLabels[d.current.Layout.SidebarInfoMode], false, false)
		body.section("Sidebar sections")
		d.addToggle(body, rowSessionPath, "Session path", !d.current.Layout.HideSessionPath)
		d.addToggle(body, rowUsage, "Token usage", !d.current.Layout.HideUsage)
		d.addToggle(body, rowAgents, "Agents", !d.current.Layout.HideAgents)
		d.addToggle(body, rowActiveAgents, "  Active agents only", d.current.Layout.ActiveAgentsOnly)
		if d.current.Layout.HideAgents {
			body.add(styles.MutedStyle.Render("    Enable Agents to edit its filter."))
		}
		d.addToggle(body, rowTools, "Tools", !d.current.Layout.HideTools)
		d.addToggle(body, rowTodos, "Todos", !d.current.Layout.HideTodos)
		if d.height >= 30 && body.width >= previewMinWidth {
			body.add(renderLayoutPreview(d.current.Layout, body.width))
		} else {
			body.add(styles.MutedStyle.Render(ansi.Truncate("Preview: sidebar "+positionLabels[d.current.Layout.SidebarPosition]+" · chat + input", body.width, "")))
		}
	}
	body.section("Display")
	d.addToggle(body, rowTransparentBackground, "Transparent background", d.current.TransparentBackground)
	d.addToggle(body, rowDimInactivePanes, "Dim inactive panes", d.current.DimInactivePanes)
	d.addToggle(body, rowSplitDiff, "Split diff view", d.current.SplitDiffView)
	d.addToggle(body, rowExpandThinking, "Expand thinking by default", d.current.ExpandThinking)
	d.addToggle(body, rowHideToolResults, "Hide tool results by default", d.current.HideToolResults)
	d.addToggle(body, rowRenderImages, "Render images", d.current.RenderImages)
	d.addToggle(body, rowShowBanner, "Show startup banner", d.current.ShowBanner)
}

var panelElementLabels = map[messages.PanelElement]string{
	messages.PanelWorkspace: "Workspace",
	messages.PanelSubagents: "Subagents",
	messages.PanelTodos:     "Todos",
}

func (d *settingsDialog) renderPanelTab(body *settingsBody) {
	body.add(styles.MutedStyle.Render("Bottom panel · disable all to hide"))
	for row, element := range d.panelOrder {
		d.addToggle(body, row, panelElementLabels[element], slices.Contains(d.current.Panel.Elements, element))
		for _, move := range []struct {
			label string
			delta int
		}{{"Move up", -1}, {"Move down", 1}} {
			text := "  " + move.label
			start := len(body.lines)
			style := styles.SecondaryStyle
			enabled := row+move.delta >= 0 && row+move.delta < len(d.panelOrder)
			if !enabled {
				style = styles.MutedStyle
			}
			body.add(style.Render(text))
			if enabled {
				body.hits = append(body.hits, settingsHit{y: start, width: min(body.width, lipgloss.Width(text)), height: len(body.lines) - start, row: row, kind: settingsMovePanel, delta: move.delta})
			}
		}
	}
	body.add(styles.MutedStyle.Render("Ctrl+↑/↓ reorders the selected element."))
}

func (d *settingsDialog) panelPreview() tea.Cmd {
	d.panelPreviewed = true
	return core.CmdHandler(messages.PreviewPanelMsg{Panel: messages.NormalizePanelSettings(d.current.Panel)})
}

func (d *settingsDialog) togglePanel() tea.Cmd {
	element := d.panelOrder[d.selected[tabPanel]]
	enabled := !slices.Contains(d.current.Panel.Elements, element)
	elements := make([]messages.PanelElement, 0, len(d.panelOrder))
	for _, candidate := range d.panelOrder {
		if (candidate == element && enabled) || (candidate != element && slices.Contains(d.current.Panel.Elements, candidate)) {
			elements = append(elements, candidate)
		}
	}
	d.current.Panel.Elements = elements
	return d.panelPreview()
}

func (d *settingsDialog) reorderPanel(delta int) tea.Cmd {
	if d.tab != tabPanel {
		return nil
	}
	row := d.selected[tabPanel]
	next := row + delta
	if next < 0 || next >= len(d.panelOrder) {
		return nil
	}
	d.panelOrder[row], d.panelOrder[next] = d.panelOrder[next], d.panelOrder[row]
	d.selected[tabPanel] = next
	elements := make([]messages.PanelElement, 0, len(d.current.Panel.Elements))
	for _, element := range d.panelOrder {
		if slices.Contains(d.current.Panel.Elements, element) {
			elements = append(elements, element)
		}
	}
	d.current.Panel.Elements = elements
	return d.panelPreview()
}

func (d *settingsDialog) renderBehaviorTab(body *settingsBody) {
	body.section("Interaction")
	mode, description := "Steer", "Send to the working agent mid-turn."
	if d.current.SendMode == messages.SendModeQueue {
		mode, description = "Queue", "Hold until the current turn ends."
	}
	d.addControl(body, rowSendMode, "While agent is working", mode, false, false)
	body.add(styles.MutedStyle.Render(description))
	d.addControl(body, rowInterruptConfirmation, "Interrupt confirmation", interruptConfirmationLabels[d.current.InterruptConfirmation], false, false)
	body.section("Automation")
	d.addToggle(body, rowYOLO, "Auto-approve tools by default", d.current.YOLO)
	warning := "Tools require permission unless approved."
	if d.confirmYOLO {
		warning = "Tools may run unconfirmed. Activate again to enable."
	} else if d.current.YOLO {
		warning = "Tools may run without confirmation."
	}
	warningRows := lipgloss.Height(ansi.Hardwrap("Tools may run unconfirmed. Activate again to enable.", body.width, true))
	body.add(styles.MutedStyle.Height(warningRows).Render(ansi.Hardwrap(warning, body.width, true)))
	d.addToggle(body, rowSnapshot, "Automatic snapshots", d.current.Snapshot)
	d.addToggle(body, rowCacheStablePrompts, "Cache-stable dynamic prompts", d.current.CacheStablePrompts)
	body.section("Startup and tabs")
	d.addToggle(body, rowRestoreTabs, "Restore tabs on launch", d.current.RestoreTabs)
	d.addToggle(body, rowLean, "Lean UI by default", d.current.Lean)
	d.addControl(body, rowTabTitleLength, "Tab title max length", fmt.Sprintf("%d chars", d.current.TabTitleMaxLength), false, false)
	body.add(styles.MutedStyle.Render("Restore tabs and lean UI apply on next launch."))
}

func (d *settingsDialog) renderNotificationsTab(body *settingsBody) {
	body.section("Completion")
	d.addToggle(body, rowSound, "Sound on task completion", d.current.Sound)
	d.addControl(body, rowSoundThreshold, "Sound threshold", fmt.Sprintf("%d seconds", d.current.SoundThreshold), false, !d.current.Sound)
	body.add(styles.MutedStyle.Render("Enable sound to edit its minimum task duration."))
	body.section("Cache")
	d.addToggle(body, rowWarnOnCacheMiss, "Warn when a turn misses the cache", d.current.WarnOnCacheMiss)
}

func (d *settingsDialog) addToggle(body *settingsBody, row int, label string, enabled bool) {
	check := "[ ]"
	if enabled {
		check = "[x]"
	}
	d.addControl(body, row, check+" "+label, "", true, !d.selectable(d.tab, row))
}

func (d *settingsDialog) addControl(body *settingsBody, row int, label, value string, toggle, disabled bool) {
	body.rows[row] = len(body.lines)
	prefix := "  "
	style := styles.BaseStyle.Foreground(styles.TextPrimary)
	if d.focus == settingsControls && d.selected[d.tab] == row && !disabled {
		prefix = "› "
		style = style.Foreground(styles.SelectedFg).Background(styles.Selected)
	}
	if disabled {
		style = styles.MutedStyle
	}
	adjustable := d.adjustable(row)
	if adjustable {
		value = "‹ " + value + " ›"
	}
	left := prefix + label
	start := len(body.lines)
	valueX := max(lipgloss.Width(left)+1, body.width-lipgloss.Width(value))
	if value != "" && valueX+lipgloss.Width(value) <= body.width {
		body.add(style.Render(left + strings.Repeat(" ", valueX-lipgloss.Width(left)) + value))
	} else {
		body.add(style.Render(left))
		if value != "" {
			valueX = 0
			body.add(style.Render(value))
		}
	}
	if disabled {
		return
	}
	kind := settingsFocusRow
	if toggle || (d.tab == tabAppearance && row == rowTheme) {
		kind = settingsActivate
	}
	end := len(body.lines)
	if adjustable {
		// Register the value's arrow cells before the encompassing focus rectangle.
		valueStart := start
		if valueX == 0 {
			valueStart = end - lipgloss.Height(ansi.Hardwrap(value, body.width, true))
		}
		body.hits = append(body.hits, settingsHit{x: valueX, y: valueStart, width: 1, height: 1, row: row, kind: settingsAdjust, delta: -1})
		wrapped := strings.Split(ansi.Hardwrap(value, body.width, true), "\n")
		lastX := valueX + lipgloss.Width(wrapped[len(wrapped)-1]) - 1
		body.hits = append(body.hits, settingsHit{x: lastX, y: valueStart + len(wrapped) - 1, width: 1, height: 1, row: row, kind: settingsAdjust, delta: 1})
	}
	body.hits = append(body.hits, settingsHit{y: start, width: body.width, height: end - start, row: row, kind: kind})
}

// visibleSectionLabels returns the sidebar section labels that are visible
// under the given settings. The session block is always shown; its label
// reads "session/path" while the session path is visible and "session" once
// it is hidden.
func visibleSectionLabels(s messages.LayoutSettings) []string {
	sessionLabel := "session/path"
	if s.HideSessionPath {
		sessionLabel = "session"
	}
	labels := []string{sessionLabel}
	if !s.HideUsage {
		labels = append(labels, "usage")
	}
	if !s.HideAgents {
		labels = append(labels, "agents")
	}
	if !s.HideTools {
		labels = append(labels, "tools")
	}
	if !s.HideTodos {
		labels = append(labels, "todos")
	}
	return labels
}

// renderLayoutPreview keeps the schematic subordinate to the actual controls.
func renderLayoutPreview(s messages.LayoutSettings, maxWidth int) string {
	width := max(1, min(previewMaxWidth, maxWidth))
	position := positionLabels[messages.ParseSidebarPosition(string(s.SidebarPosition))]
	sections := strings.Join(visibleSectionLabels(s), " · ")
	heading := ansi.Truncate("Sidebar "+position+" · chat + input", width, "")
	return styles.MutedStyle.Render(heading + "\n" + ansi.Wrap(sections, width, ""))
}

func (d *settingsDialog) actions() []Action {
	return actionsForKeys("esc", "Cancel", "ctrl+s", "Apply")
}
