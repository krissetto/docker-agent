package dialog

import (
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/help"
)

// ReferenceHelp describes all dialog families, including input aliases after
// their host's overrides. Its sections are other-context reference, not dispatch.
func ReferenceHelp() []help.Section {
	sections := dialogReferenceSections()
	sections[0].Entries = append(sections[0].Entries,
		help.Entry{ID: "dialog.manager.quit", Keys: slices.Clone(core.GetKeys().Quit.Keys()), Description: "Request exit confirmation", Condition: "Application routing handles configured Quit before ordinary modal input, opening exit confirmation above the modal. In exit confirmation, Quit confirms exit."},
		help.Entry{ID: "dialog.manager.help", Keys: []string{"f1"}, Description: "Show contextual Help", Condition: "Top modal; repeated F1 does not stack Help. Ctrl+h remains input editing."},
	)
	for i := range sections {
		if sections[i].ID == "dialog.exit-confirmation" {
			for j := range sections[i].Entries {
				if sections[i].Entries[j].ID == "dialog.exit-confirmation.confirm-exit" {
					sections[i].Entries[j].Keys = slices.Clone(core.GetKeys().Quit.Keys())
				}
			}
		}
		if sections[i].ID == "dialog.help" {
			sections[i].Entries = append(sections[i].Entries, help.Entry{
				ID: "dialog.help.categories", Keys: []string{"left", "right"},
				Description: "Previous/next context or reference category", Condition: "Help; wraps and resets category scrolling.",
			})
		}
	}
	return sections
}

// ContextHelp takes a read-only snapshot of the concrete top dialog. It never
// prepares a view, changes focus, normalizes actions, or touches a draft.
func ContextHelp(dialog Dialog) (string, []help.Section) {
	family, base := helpDialogFamily(dialog)
	if family == "" {
		return "Dialog", nil
	}
	var sections []help.Section
	context := "Dialog"
	for _, section := range ReferenceHelp() {
		if section.ID == "dialog."+family {
			context = section.Title
			sections = append(sections, section)
		}
	}
	if base == nil {
		return context, sections
	}
	if base.actionsFocused {
		context += " — actions"
	} else {
		context += " — content"
	}
	switch d := dialog.(type) {
	case *planBrowserDialog:
		if d.filtering {
			context += " / filter"
		}
	case *workingDirPickerDialog:
		context += " / " + map[dirSection]string{sectionBrowse: "Browse", sectionRecent: "Recent", sectionPinned: "Pinned"}[d.section]
	case *settingsDialog:
		context += " / " + settingsTabLabels[d.tab] + " / " + map[settingsFocus]string{settingsControls: "Controls", settingsCategories: "Categories", settingsActions: "Actions"}[d.focus]
	case *multiChoiceDialog:
		if d.config.Title != "" {
			context = d.config.Title + strings.TrimPrefix(context, "Choices and rejection reason")
		}
	case *pendingMessageEditDialog:
		if d.saving {
			context += " / saving"
		}
	}
	selected := base.selectedAction(base.actions)
	for si := range sections {
		entries := sections[si].Entries[:0]
		for _, entry := range sections[si].Entries {
			if d, ok := dialog.(*settingsDialog); ok {
				if strings.HasSuffix(entry.ID, ".adjust-setting") || strings.HasSuffix(entry.ID, ".activate-setting") {
					continue // Selected control semantics are described below.
				}
				if (strings.HasPrefix(entry.Condition, "Controls") && d.focus != settingsControls) ||
					(strings.HasPrefix(entry.Condition, "Categories") && d.focus != settingsCategories) {
					continue
				}
			}
			entry.Keys = slices.Clone(entry.Keys)
			if d, ok := dialog.(*multiChoiceDialog); ok && slices.Contains(entry.Keys, "1") {
				count := len(d.config.Options)
				if d.config.AllowCustom {
					count++
				}
				entry.Keys = entry.Keys[:min(10, count)]
				if d.selected == selectionCustom && !base.actionsFocused {
					continue // Numbers are ordinary text in the custom input.
				}
			}
			if strings.Contains(entry.ID, ".editing.") {
				if !helpInputActive(dialog, base) {
					continue
				}
				if base.actionsFocused {
					entry.Condition = "Filter input remains focused even in actions; action navigation consumes modified arrows, Enter and Tab first."
				}
			}
			// A focused action owns every modified arrow/Enter/Tab by key Code.
			// Content Enter belongs to its actual selected/default action below.
			entry.Keys = slices.DeleteFunc(entry.Keys, func(k string) bool {
				if family == "settings" && (k == "tab" || k == "shift+tab") {
					return false
				}
				if k == "enter" && (selected >= 0 || base.actionsFocused) {
					return true
				}
				if !base.actionsFocused {
					return false
				}
				for _, code := range []string{"left", "right", "up", "down", "enter", "tab"} {
					if k == code || strings.HasSuffix(k, "+"+code) {
						return true
					}
				}
				return false
			})
			if len(entry.Keys) == 0 {
				continue
			}
			if reason := helpUnavailable(dialog, entry); reason != "" {
				entry.Condition = "Unavailable now: " + reason + ". " + entry.Condition
			}
			entries = append(entries, entry)
		}
		sections[si].Entries = entries
	}
	if d, ok := dialog.(*settingsDialog); ok && d.focus == settingsControls {
		label := "Toggle selected setting"
		if d.adjustable(d.selected[d.tab]) {
			label = "Advance selected setting"
		} else if d.tab == tabAppearance && d.selected[d.tab] == rowTheme {
			label = "Choose theme"
		}
		controls := help.Section{ID: "dialog.settings.controls", Title: "Selected setting controls", Entries: []help.Entry{{
			ID: "dialog.settings.selected-enter", Keys: []string{"enter", "space"}, Description: label,
			Condition: "Controls; display changes preview until Save. Theme selection is part of the draft.",
		}}}
		if d.adjustable(d.selected[d.tab]) {
			controls.Entries = append(controls.Entries, help.Entry{ID: "dialog.settings.control.adjust", Keys: []string{"left", "h", "right", "l"}, Description: "Previous/decrease or next/increase", Condition: "Enabled selected enum or number; numeric bounds apply."})
		}
		sections = append(sections, controls)
	}
	if selected >= 0 {
		action := base.actions[selected]
		condition := "Current " + map[bool]string{true: "focused", false: "default"}[base.actionsFocused] + " enabled action; other shortcuts retain their own meaning."
		if reason := helpUnavailable(dialog, help.Entry{Keys: []string{action.Key.String()}}); reason != "" {
			condition = "Unavailable now: " + reason
		}
		sections = append(sections, help.Section{ID: "dialog." + family + ".selected", Title: "Selected Enter action", Entries: []help.Entry{{
			ID: "dialog." + family + ".selected-enter", Keys: []string{"enter"}, Description: action.Label,
			Condition: condition,
		}}})
	}
	for _, section := range ReferenceHelp() {
		switch section.ID {
		case "dialog.manager":
			// Family cancel/close/negative entries already cover physical Escape.
			for i := range section.Entries {
				if section.Entries[i].ID == "dialog.manager.quit" && family == "exit-confirmation" {
					section.Entries[i].Description = "Confirm exit"
					section.Entries[i].Condition = "Exit confirmation's special second Quit behavior."
				}
			}
			sections = append(sections, section)
		case "dialog.shared-body":
			if base.bodyScroll != nil {
				hasScroll := false
				for _, current := range sections {
					for _, entry := range current.Entries {
						hasScroll = hasScroll || slices.Contains(entry.Keys, "pgup")
					}
				}
				if !hasScroll {
					if family == "tool-confirmation" {
						for i := range section.Entries {
							section.Entries[i].Keys = []string{"mouse wheel", "scrollbar click/drag"}
						}
					}
					sections = append(sections, section)
				}
			}
		case "dialog.shared-actions":
			if len(base.actions) == 0 {
				continue
			}
			// Enter is described by the selected action, not a duplicate generic row.
			section.Entries[2].Keys = []string{"mouse action pill"}
			if !base.actionsFocused {
				section.Entries = append(section.Entries[:1], section.Entries[2])
			}
			sections = append(sections, section)
		}
	}
	return context, deduplicateCurrentHelp(sections)
}

// Remove duplicate descriptions of shared controls only. Same-key actions in
// different contexts (or conditional unavailable entries) stay distinct.
func deduplicateCurrentHelp(sections []help.Section) []help.Section {
	familyKeys := make(map[string]bool)
	for _, section := range sections {
		if section.ID == "dialog.manager" || section.ID == "dialog.shared-actions" {
			continue
		}
		for _, entry := range section.Entries {
			for _, k := range entry.Keys {
				familyKeys[k] = true
			}
		}
	}
	for si := range sections {
		shared := sections[si].ID == "dialog.shared-actions"
		manager := sections[si].ID == "dialog.manager"
		if !shared && !manager {
			continue
		}
		entries := sections[si].Entries[:0]
		for _, entry := range sections[si].Entries {
			entry.Keys = slices.DeleteFunc(entry.Keys, func(k string) bool {
				return familyKeys[k] && (shared || k == "esc" || entry.ID == "dialog.manager.quit")
			})
			if len(entry.Keys) > 0 {
				entries = append(entries, entry)
			}
		}
		sections[si].Entries = entries
	}
	return sections
}

func helpInputActive(dialog Dialog, base *BaseDialog) bool {
	if base.actionsFocused {
		switch dialog.(type) {
		case *commandPaletteDialog, *filePickerDialog, *modelPickerDialog, *themePickerDialog, *sessionBrowserDialog, *planBrowserDialog, *workingDirPickerDialog:
			// These filters deliberately remain focused; do not hide their
			// still-reachable editing aliases behind a universal focus claim.
		default:
			return false
		}
	}
	switch d := dialog.(type) {
	case *planBrowserDialog:
		return d.filtering
	case *workingDirPickerDialog:
		return d.section == sectionBrowse
	case *pendingMessageEditDialog:
		return !d.saving && !d.closed
	case *multiChoiceDialog:
		return d.config.AllowCustom && d.customInput.Focused()
	case *ElicitationDialog:
		return len(d.fields) == 0 || (d.currentField >= 0 && d.currentField < len(d.inputs) && d.inputs[d.currentField].Focused())
	default:
		return true
	}
}

func helpUnavailable(dialog Dialog, entry help.Entry) string {
	has := func(k string) bool { return slices.Contains(entry.Keys, k) }
	switch d := dialog.(type) {
	case *commandPaletteDialog:
		if has("enter") && len(d.filtered) == 0 {
			return "no command selected"
		}
	case *filePickerDialog:
		if has("enter") && len(d.filtered) == 0 {
			return "no file or directory selected"
		}
	case *modelPickerDialog:
		if has("enter") && len(d.filtered) == 0 {
			return "no model selected"
		}
	case *themePickerDialog:
		if has("enter") && len(d.filtered) == 0 {
			return "no theme selected"
		}
	case *sessionBrowserDialog:
		if has("ctrl+g") && d.workspace == nil {
			return "workspace filtering is not available"
		}
		if (has("enter") || has("ctrl+s") || has("ctrl+y") || has("ctrl+d")) && !strings.Contains(entry.ID, ".editing.") && len(d.filtered) == 0 {
			return "no session selected"
		}
	case *planBrowserDialog:
		if strings.Contains(entry.ID, ".editing.") {
			return ""
		}
		if d.filtering && (has("/") || has("r") || has("x") || has("s") || has("d") || has("n") || has("e")) {
			return "filter mode owns text keys"
		}
		p, ok := d.selectedPlan()
		if (has("enter") || has("x") || has("s") || has("d") || has("e")) && !ok {
			return "no plan selected"
		}
		if (has("s") || has("d") || has("e")) && p.Version == nil {
			return "selected plan is not versioned and shared"
		}
	case *planDetailDialog:
		if (has("s") || has("d") || has("e")) && d.plan.Version == nil {
			return "plan is not versioned and shared"
		}
	case *contextDialog:
		if has("enter") {
			if _, ok := d.selectedLiveSession(); !ok {
				return "no live session selected"
			}
		}
		if has("delete") {
			if d.breakdown == nil {
				return "no attachment selected"
			}
			if _, ok := d.selectedAttachedIndex(); !ok {
				return "no attachment selected"
			}
		}
	case *workingDirPickerDialog:
		if has("ctrl+p") {
			if _, ok := d.selectedTogglePath(); !ok {
				return "no pinnable directory selected"
			}
		}
		if has("enter") {
			s := d.activeSection()
			if *s.selected < 0 || *s.selected >= len(s.entries) {
				return "no directory selected"
			}
		}
		if has("text") && d.section != sectionBrowse {
			return "only Browse has a filter input"
		}
	case *ElicitationDialog:
		if has("space") && (d.isTextInputField() || d.hasFreeFormInput()) {
			return "selected field is text; space inserts text and up/down do not cycle options"
		}
		if has("text") && d.actionsFocused {
			return "input is blurred while actions are focused"
		}
	case *MCPPromptInputDialog:
		if has("text") && d.actionsFocused {
			return "input is blurred while actions are focused"
		}
		if has("enter") && !d.canExecute() {
			return "required arguments are missing"
		}
	case *multiChoiceDialog:
		if has("text") && d.actionsFocused {
			return "custom input is blurred while actions are focused"
		}
		if has("ctrl+s") && !d.config.AllowSecondary {
			return "secondary action is not allowed"
		}
		if has("ctrl+enter") && !d.hasSelection() {
			return "no valid selection or custom text"
		}
		if has("text") && !d.config.AllowCustom {
			return "custom input is not allowed"
		}
	case *pendingMessageEditDialog:
		if d.saving || d.closed {
			return "editor is saving or closed"
		}
		if has("ctrl+enter") && d.save == nil {
			return "save callback is unavailable"
		}
	case *URLElicitationDialog:
		if has("o") && d.url == "" {
			return "no URL was supplied"
		}
	case *snapshotsDialog:
		if has("r") && len(d.fileCounts) == 0 {
			return "no snapshots exist"
		}
	case *effortPickerDialog:
		if has("enter") && len(d.levels) == 0 {
			return "no supported thinking level"
		}
	}
	_, base := helpDialogFamily(dialog)
	if base != nil {
		if base.responseSent && !has("esc") && !has("q") {
			return "request already answered"
		}
		for _, action := range base.actions {
			if action.Disabled && has(action.Key.String()) {
				return action.Label + " is disabled for the current selection"
			}
		}
	}
	return ""
}

func helpDialogFamily(dialog Dialog) (string, *BaseDialog) {
	switch d := dialog.(type) {
	case *helpDialog:
		return "help", &d.BaseDialog
	case *agentDetailsDialog:
		return "agent-details", &d.BaseDialog
	case *attachmentPreviewDialog:
		return "attachment-preview", &d.BaseDialog
	case *permissionsDialog:
		return "permissions", &d.BaseDialog
	case *skillsDialog:
		return "skills", &d.BaseDialog
	case *toolsDialog:
		return "tools", &d.BaseDialog
	case *exitConfirmationDialog:
		return "exit-confirmation", &d.BaseDialog
	case *closeRootWithSubagentsDialog:
		return "close-root-with-subagents", &d.BaseDialog
	case *maxIterationsDialog:
		return "max-iterations", &d.BaseDialog
	case *oauthAuthorizationDialog:
		return "oauth-authorization", &d.BaseDialog
	case *planDeleteConfirmDialog:
		return "plan-delete-confirmation", &d.BaseDialog
	case *toolConfirmationDialog:
		return "tool-confirmation", &d.BaseDialog
	case *commandPaletteDialog:
		return "command-palette", &d.BaseDialog
	case *filePickerDialog:
		return "file-picker", &d.BaseDialog
	case *modelPickerDialog:
		return "model-picker", &d.BaseDialog
	case *themePickerDialog:
		return "theme-picker", &d.BaseDialog
	case *sessionBrowserDialog:
		return "session-browser", &d.BaseDialog
	case *planBrowserDialog:
		return "plan-browser", &d.BaseDialog
	case *planStatusDialog:
		return "plan-status", &d.BaseDialog
	case *planNameDialog:
		return "plan-name", &d.BaseDialog
	case *planDetailDialog:
		return "plan-detail", &d.BaseDialog
	case *contextDialog:
		return "context", &d.BaseDialog
	case *costDialog:
		return "cost", &d.BaseDialog
	case *effortPickerDialog:
		return "effort-picker", &d.BaseDialog
	case *snapshotsDialog:
		return "snapshot", &d.BaseDialog
	case *settingsDialog:
		return "settings", &d.BaseDialog
	case *workingDirPickerDialog:
		return "working-dir-picker", &d.BaseDialog
	case *ElicitationDialog:
		return "elicitation", &d.BaseDialog
	case *MCPPromptInputDialog:
		return "mcp-prompt-input", &d.BaseDialog
	case *multiChoiceDialog:
		return "multi-choice-tool-rejection-reason", &d.BaseDialog
	case *pendingMessageEditDialog:
		return "pending-message-edit", &d.BaseDialog
	case *URLElicitationDialog:
		return "url-elicitation", &d.BaseDialog
	case *tourOfferDialog:
		return "tour-offer", &d.BaseDialog
	default:
		return "", nil
	}
}
