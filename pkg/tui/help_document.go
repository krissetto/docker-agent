package tui

import (
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/tui/components/tabbar"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/help"
	"github.com/docker/docker-agent/pkg/tui/widgets/key"
	"github.com/docker/docker-agent/pkg/tui/widgets/textarea"
)

// helpDocument snapshots the routing context, not the Help overlay that will
// subsequently cover it. Reference keeps different meanings of the same key in
// their own contexts; Current removes only aliases intercepted earlier here.
func (m *appModel) helpDocument() help.Document {
	keys := core.GetKeys()
	doc := help.Document{Context: "Composer"}
	doc.Reference = m.referenceHelp()
	claimed := map[string]bool{}
	add := func(section help.Section) {
		var entries []help.Entry
		for _, entry := range section.Entries {
			entry.Keys = slices.Clone(entry.Keys)
			entry.Keys = slices.DeleteFunc(entry.Keys, func(k string) bool { return claimed[k] })
			if len(entry.Keys) == 0 {
				continue
			}
			for _, k := range entry.Keys {
				claimed[k] = true
			}
			entries = append(entries, entry)
		}
		if len(entries) > 0 {
			section.Entries = entries
			doc.Current = append(doc.Current, section)
		}
	}
	if m.paneHydration != nil && !m.dialogMgr.Open() {
		doc.Context = "Loading session pane"
		add(helpSection("panes.loading", "Loading session", "", helpEntry("panes.cancel-load", []string{"esc"}, "Cancel pending split; keep existing layout and draft")))
	}
	if m.paneGesture != nil && !m.dialogMgr.Open() {
		doc.Context = "Pane gesture"
		add(helpSection("panes.gesture", "Pane gesture", "", helpEntry("panes.cancel", []string{"esc"}, "Cancel preview without changing tabs or panes")))
		if m.paneGesture.keyboard {
			add(helpSection("panes.resize", "Resize divider", "", helpEntry("panes.arrows", []string{"left", "right", "up", "down"}, "Preview divider position"), helpEntry("panes.commit", []string{"enter"}, "Commit divider position")))
			return doc
		}
	}
	// Transcription Enter/Esc precede even modal routing. Other keys continue.
	if m.transcriber.IsRunning() {
		doc.Context += " — transcribing"
		add(helpSection("transcription", "Transcription", "", helpEntry("transcription.send", []string{"enter"}, "Stop transcription and send composer"), helpEntry("transcription.stop", []string{"esc"}, "Stop transcription")))
	}
	appEntries := []help.Entry{bindingHelp("app.quit", keys.Quit, "Request exit confirmation")}
	if m.dialogMgr.Open() {
		context, sections := dialog.ContextHelp(m.dialogMgr.TopDialog())
		doc.Context = context
		if m.dialogMgr.TopIsExitConfirmation() {
			appEntries[0].Description = "Confirm exit"
		}
		appEntries = append(appEntries, helpEntry("app.help", []string{"f1"}, "Help for this dialog"))
		add(helpSection("application", "Application", "", appEntries...))
		if m.dialogMgr.TopIsBackground() && !m.leanMode && !m.editor.IsHistorySearchActive() {
			add(m.tabHelp(true, false))
		}
		for _, section := range sections {
			entries := make([]help.Entry, 0, len(section.Entries))
			for _, entry := range section.Entries {
				entry.Keys = slices.DeleteFunc(slices.Clone(entry.Keys), func(k string) bool { return claimed[k] })
				if len(entry.Keys) > 0 {
					entries = append(entries, entry)
				}
			}
			if len(entries) > 0 {
				section.Entries = entries
				doc.Current = append(doc.Current, section)
			}
		}
		return doc
	}
	appEntries = append(appEntries, bindingHelp("app.help", keys.Help, "Open contextual help"))
	add(helpSection("application", "Application", "", appEntries...))
	if m.chatPage.IsTitleEditing() {
		doc.Context = "Sidebar title editing"
		add(titleHelp())
		add(inputHelp("title", false))
		return doc
	}
	if m.chatPage.IsInlineEditing() {
		doc.Context = "Inline user-message editing"
		add(inlineHelp(m.keyboardEnhancementsSupported))
		add(inputHelp("inline", true))
		return doc
	}
	if m.messageBar != nil && m.messageBar.Focused() {
		doc.Context = "Message actions"
		add(messageActionHelp())
	}
	search := m.focusedPanel == PanelEditor && m.editor.IsHistorySearchActive()
	if !m.leanMode && !m.editor.IsHistorySearchActive() {
		add(m.tabHelp(m.focusedPanel != PanelEditor, false))
	}
	if m.completions.Open() {
		doc.Context = "Completion popup — composer input"
		add(completionHelp())
		add(composerEarlyInputHelp())
		add(composerHelp(m.keyboardEnhancementsSupported, m.editor.HistoryNavigationActive()))
		add(inputHelp("composer", true))
		return doc
	}
	add(globalHelp())
	if m.editor.IsContextBarFocused() {
		doc.Context = "Composer context bar"
		add(contextBarHelp())
		// Other keys leave the bar and go straight to the composer, rather than
		// running the later root shortcuts (thinking/sidebar/focus/external edit).
		add(composerEarlyInputHelp())
		add(composerHelp(m.keyboardEnhancementsSupported, m.editor.HistoryNavigationActive()))
		add(inputHelp("composer", true))
		return doc
	}
	if search {
		doc.Context = "History search"
		add(historySearchHelp())
		add(inputHelp("search", false))
		return doc
	}
	if m.tour.Active() {
		entries := []help.Entry{helpEntry("tour.quit", []string{"esc"}, "Quit getting-started tour")}
		if m.focusedPanel == PanelEditor && strings.TrimSpace(m.editor.Value()) == "" {
			entries = append(entries, helpEntry("tour.next", []string{"enter"}, "Advance tour"))
		}
		add(helpSection("tour", "Getting-started tour", "Tour active", entries...))
	}
	navigation := []help.Entry{bindingHelp("app.external-editor", keys.EditExternal, "Edit composer in external editor")}
	if m.focusedPanel == PanelEditor && !m.editor.IsRecording() {
		navigation = append(navigation, bindingHelp("history.search", keys.HistorySearch, "Search input history"))
	}
	// Even when disabled, root consumes these configured chords.
	if !m.leanMode && !m.hideSidebar {
		navigation = append(navigation, bindingHelp("sidebar.toggle", keys.ToggleSidebar, "Toggle sidebar"))
	} else {
		for _, k := range keys.ToggleSidebar.Keys() {
			claimed[k] = true
		}
	}
	if m.focusedPanel != PanelEditor || m.editor.IsRecording() {
		for _, k := range keys.HistorySearch.Keys() {
			claimed[k] = true
		}
	}
	navigation = append(navigation, helpEntry("agent.thinking", []string{"shift+tab"}, "Cycle thinking effort (when supported)"))
	focus := "Complete argument, accept suggestion, or focus context/messages"
	if m.focusedPanel == PanelContent {
		focus = "Focus message actions if present, otherwise composer"
	}
	navigation = append(navigation, bindingHelp("focus.next", keys.SwitchFocus, focus))
	if m.chatPage.IsWorking() {
		navigation = append(navigation, help.Entry{ID: "response.cancel", Keys: []string{"esc"}, Description: "Press twice within 3 seconds to cancel response", Condition: "Same active response; key repeats do not confirm"})
	} else {
		navigation = append(navigation, helpEntry("transcript.clear-selection", []string{"esc"}, "Clear transcript selection"))
	}
	navigation = append(navigation, agentIndexHelp())
	add(helpSection("navigation", "Navigation and response", "", navigation...))
	if m.focusedPanel == PanelEditor {
		add(composerEarlyInputHelp())
		add(composerHelp(m.keyboardEnhancementsSupported, m.editor.HistoryNavigationActive()))
		add(inputHelp("composer", true))
	} else {
		if doc.Context == "Composer" {
			doc.Context = "Transcript"
		}
		add(transcriptHelp())
	}
	for _, section := range rootMouseHelp() {
		if section.ID == "mouse.sidebar" && (m.hideSidebar || m.leanMode) {
			continue
		}
		if section.ID == "mouse.tabs" && m.leanMode {
			continue
		}
		// Mouse targets coexist, unlike keyboard aliases with precedence.
		doc.Current = append(doc.Current, section)
	}
	if !m.leanMode {
		doc.Current = append(doc.Current, paneHelp())
	}
	if m.leanMode {
		doc.Context += " — lean layout (not the standalone lean UI)"
		doc.Current = append(doc.Current, leanSessionHelp())
	}
	return doc
}

func helpEntry(id string, keys []string, description string) help.Entry {
	return help.Entry{ID: id, Keys: slices.Clone(keys), Description: description}
}

func bindingHelp(id string, binding key.Binding, description string) help.Entry {
	if !binding.Enabled() {
		return help.Entry{ID: id, Description: description}
	}
	return helpEntry(id, binding.Keys(), description)
}

func helpSection(id, title, condition string, entries ...help.Entry) help.Section {
	for i := range entries {
		if entries[i].Condition == "" {
			entries[i].Condition = condition
		}
	}
	return help.Section{ID: id, Title: title, Entries: entries}
}

func globalHelp() help.Section {
	k := core.GetKeys()
	return helpSection("global", "Application controls", "Outside dialogs and local text edits; completion keeps input precedence",
		bindingHelp("app.suspend", k.Suspend, "Suspend application"),
		bindingHelp("app.commands", k.Commands, "Open command palette"),
		bindingHelp("app.yolo", k.ToggleYolo, "Toggle YOLO mode"),
		bindingHelp("app.hide-tools", k.ToggleHideToolResults, "Toggle tool-result visibility"),
		bindingHelp("agent.cycle", k.CycleAgent, "Cycle agent"),
		bindingHelp("agent.model", k.ModelPicker, "Open model picker"))
}

func (m *appModel) tabHelp(closeEnabled, reference bool) help.Section {
	k := tabbar.DefaultKeyMap()
	entries := []help.Entry{bindingHelp("tab.new", k.NewTab, "New session tab")}
	if reference || (m.tabBar != nil && m.tabBar.Count() > 1) {
		entries = append(entries, bindingHelp("tab.next", k.NextTab, "Next tab"), bindingHelp("tab.previous", k.PrevTab, "Previous tab"))
		entries[1].Condition = "More than one tab"
		entries[2].Condition = "More than one tab"
	}
	if closeEnabled {
		entries = append(entries, help.Entry{ID: "tab.close", Keys: k.CloseTab.Keys(), Description: "Close active tab (sole tab requests exit confirmation)", Condition: "Outside composer, inline/title editing and history search"})
	}
	return helpSection("tabs", "Session tabs", "Non-lean layout; outside history search and local edits; background dialogs allow navigation", entries...)
}

func agentIndexHelp() help.Entry {
	return helpEntry("agent.index", []string{"ctrl+1", "ctrl+2", "ctrl+3", "ctrl+4", "ctrl+5", "ctrl+6", "ctrl+7", "ctrl+8", "ctrl+9"}, "Switch agent by index (not session tab)")
}

// The composer handles rich paste and grapheme backspace before configurable
// send/newline, unlike inline textarea editing. Preserve those actual aliases.
func composerEarlyInputHelp() help.Section {
	k := textarea.DefaultKeyMap()
	return helpSection("composer.input", "Composer input", "Composer focused",
		bindingHelp("input.composer.Paste", k.Paste, "Paste from clipboard (including attachments)"),
		bindingHelp("input.composer.DeleteCharacterBackward", k.DeleteCharacterBackward, "Delete previous grapheme"))
}

func composerHelp(enhanced, history bool) help.Section {
	k := core.GetKeys()
	entries := []help.Entry{
		bindingHelp("composer.send", k.EditorSend, "Send message"),
		helpEntry("composer.followup", []string{"alt+enter"}, "Send end-of-turn follow-up"),
		helpEntry("composer.newline", core.EditorNewlineKeys(enhanced), "Insert newline"),
		{ID: "completion.commands", Keys: []string{"/"}, Description: "Open command completion", Condition: "Empty composer"},
		helpEntry("completion.files", []string{"@"}, "Open file completion"),
	}
	if history {
		entries = append(entries, help.Entry{ID: "history.previous", Keys: []string{"up"}, Description: "Previous input history", Condition: "Draft not manually typed"}, help.Entry{ID: "history.next", Keys: []string{"down"}, Description: "Next input history", Condition: "Draft not manually typed"})
	}
	return helpSection("composer", "Composer", "Composer focused; send requires nonempty input", entries...)
}

func inlineHelp(enhanced bool) help.Section {
	return helpSection("inline", "Inline user-message edit", "Inline edit active",
		bindingHelp("inline.save", core.GetKeys().EditorSend, "Save edited message"),
		helpEntry("inline.newline", core.EditorNewlineKeys(enhanced), "Insert newline"),
		helpEntry("inline.cancel", []string{"esc"}, "Cancel edit without changing saved message"))
}

func titleHelp() help.Section {
	return helpSection("title", "Session title edit", "Title input active",
		helpEntry("title.save", []string{"enter"}, "Save title"), helpEntry("title.cancel", []string{"esc"}, "Cancel title edit"))
}

func completionHelp() help.Section {
	return helpSection("completion", "Completion popup", "Popup open; other keys edit composer, not root globals (except Help/Quit and tab controls)",
		helpEntry("completion.previous", []string{"up"}, "Previous suggestion (wrap)"), helpEntry("completion.next", []string{"down"}, "Next suggestion (wrap)"),
		help.Entry{ID: "completion.submit", Keys: []string{"enter"}, Description: "Select and auto-submit", Condition: "Enabled candidate"},
		help.Entry{ID: "completion.accept", Keys: []string{"tab"}, Description: "Select without auto-submit", Condition: "Enabled candidate"},
		helpEntry("completion.dismiss", []string{"esc"}, "Dismiss completion"))
}

func historySearchHelp() help.Section {
	return helpSection("history", "History search", "Search active; application globals still precede input",
		helpEntry("history.previous-match", []string{"up", "ctrl+p"}, "Previous substring match (wrap)"),
		helpEntry("history.next-match", []string{"down", "ctrl+n"}, "Next substring match (wrap)"),
		helpEntry("history.accept", []string{"enter"}, "Accept match into draft, without sending"),
		helpEntry("history.cancel", []string{"esc", "ctrl+g"}, "Leave history search"))
}

func transcriptHelp() help.Section {
	return helpSection("transcript", "Transcript", "Content focused; no inline edit",
		helpEntry("transcript.previous", []string{"up", "k"}, "Select previous message"), helpEntry("transcript.next", []string{"down", "j"}, "Select next message"),
		help.Entry{ID: "transcript.copy", Keys: []string{"c"}, Description: "Copy selected message", Condition: "Message selected"},
		help.Entry{ID: "transcript.edit", Keys: []string{"e"}, Description: "Edit selected message", Condition: "Persisted user message selected"},
		helpEntry("transcript.page-up", []string{"pgup"}, "Scroll page up"), helpEntry("transcript.page-down", []string{"pgdown"}, "Scroll page down"),
		helpEntry("transcript.top", []string{"home", "g"}, "Scroll to top"), helpEntry("transcript.bottom", []string{"end", "G"}, "Scroll to bottom"),
		help.Entry{ID: "queue.restore", Keys: []string{"alt+up"}, Description: "Withdraw eligible pending messages and restore composer draft", Condition: "Content focus only; queue double-click instead edits the canonical pending item"})
}

func contextBarHelp() help.Section {
	return helpSection("contextbar", "Composer context bar", "Context bar focused",
		helpEntry("contextbar.toggle", []string{"enter", "space"}, "Toggle context bar"), helpEntry("contextbar.messages", []string{"tab"}, "Focus messages"))
}

func messageActionHelp() help.Section {
	return helpSection("messagebar", "Message actions", "Action strip focused",
		helpEntry("messagebar.previous", []string{"left"}, "Previous action"), helpEntry("messagebar.next", []string{"right"}, "Next action"),
		helpEntry("messagebar.execute", []string{"enter", "space"}, "Execute selected action"),
		helpEntry("messagebar.editor", []string{"tab", "esc"}, "Return to composer"), helpEntry("messagebar.content", []string{"shift+tab"}, "Return to transcript"))
}

func (m *appModel) referenceHelp() []help.Section {
	k := core.GetKeys()
	sections := []help.Section{
		helpSection("application", "Application and focus", "Root controls; overridden by more specific input contexts",
			bindingHelp("app.quit", k.Quit, "Request exit confirmation; repeat on confirmation to exit"), bindingHelp("app.help", k.Help, "Contextual help (only F1 over a dialog)"),
			bindingHelp("app.external-editor", k.EditExternal, "Edit composer externally (composer or content focus)"), bindingHelp("history.search", k.HistorySearch, "Search input history (composer, not recording)"),
			bindingHelp("sidebar.toggle", k.ToggleSidebar, "Toggle sidebar (unless hidden or lean layout)"), bindingHelp("focus.next", k.SwitchFocus, "Complete argument, accept suggestion, then cycle composer/context/messages/actions focus"),
			helpEntry("agent.thinking", []string{"shift+tab"}, "Cycle model thinking effort when supported"), agentIndexHelp(),
			helpEntry("response.cancel", []string{"esc then esc"}, "Working response: confirm cancellation within 3 seconds for the same response"),
			helpEntry("transcript.clear-selection", []string{"esc"}, "Idle transcript: clear selection")),
		globalHelp(), m.tabHelp(true, true), composerHelp(m.keyboardEnhancementsSupported, true), inputHelp("composer", true), completionHelp(), historySearchHelp(), inputHelp("search", false), transcriptHelp(), inlineHelp(m.keyboardEnhancementsSupported), inputHelp("inline", true), titleHelp(), inputHelp("title", false), contextBarHelp(), messageActionHelp(),
		helpSection("transcription", "Transcription and tour", "Context-specific controls",
			helpEntry("transcription.send", []string{"enter"}, "Transcribing: stop and send composer"), helpEntry("transcription.stop", []string{"esc"}, "Transcribing: stop"),
			helpEntry("tour.quit", []string{"esc"}, "Tour active: quit tour"), helpEntry("tour.next", []string{"enter"}, "Tour active, empty composer focused: advance")),
	}
	sections = append(sections, rootMouseHelp()...)
	if !m.leanMode {
		sections = append(sections, paneHelp())
	} else {
		sections = append(sections, leanSessionHelp())
	}
	if m.buildCommandCategories != nil {
		for _, category := range m.commandCategories() {
			section := help.Section{ID: "commands." + category.Name, Title: "Commands — " + category.Name}
			for _, command := range category.Commands {
				if command.SlashCommand == "" {
					continue
				}
				section.Entries = append(section.Entries, help.Entry{ID: "command." + command.ID, Keys: []string{command.SlashCommand}, Description: command.Description, Condition: "Submit slash command from composer; runtime availability applies"})
			}
			if len(section.Entries) > 0 {
				sections = append(sections, section)
			}
		}
	}
	return append(sections, dialog.ReferenceHelp()...)
}

func paneHelp() help.Section {
	return helpSection("panes", "Session panes", "Full TUI only; no pane gestures behind dialogs",
		helpEntry("panes.resume", []string{"/resume"}, "Restored · paused: Any queued work or reports will wait until you resume this session. Relatives require their own resume."),
		helpEntry("panes.actions", []string{"/panes"}, "Open dedicated Panes chooser; /panes left|right|up|down [session], next|prev|remove|single; resize [divider number] previews with arrows, Enter commits, Esc cancels"),
		helpEntry("panes.split", []string{"drag tab upward to transcript edge"}, "Split or move the canonical session; self-edge fills remainder with next hidden tab; center/sidebar/composer drops cancel"),
		helpEntry("panes.focus", []string{"click pane"}, "Focus session and its existing draft/attachments; tiled sidebar width/collapse stay layout-wide"),
		helpEntry("panes.scroll", []string{"wheel over pane"}, "Scroll only that pane without switching composer"),
		helpEntry("panes.divider", []string{"drag divider"}, "Preview a clamped divider; release commits"),
		helpEntry("panes.cancel-reference", []string{"esc"}, "Cancel gesture before response interruption; outside release, Blur, resize or dialog opening also cancel"),
		helpEntry("panes.remove", []string{"/panes → Remove pane", "/panes → Single view"}, "Change layout only; never close/cancel the tab; single view restores focused tab sidebar preference"))
}

func leanSessionHelp() help.Section {
	return helpSection("lean.sessions", "Lean session navigation", "Normal-screen layout; dialogs remain keyboard-operable; panes and tour require the full TUI",
		helpEntry("lean.subagents", []string{"/subagents"}, "Browse the subagent tree; arrows navigate, Enter opens, Esc closes without cancelling"),
		helpEntry("lean.resume", []string{"/resume"}, "Restored · paused: Any queued work or reports will wait until you resume this session. Relatives require their own resume."),
		helpEntry("lean.back", []string{"/back"}, "Return to the previous still-open session; preserve drafts and ongoing work"),
		helpEntry("lean.operations", []string{"/settings", "/plans", "/permissions", "/pause"}, "Use the ordinary nonvisual command handlers and keyboard dialogs"))
}
