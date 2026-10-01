package dialog

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/help"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func helpEntries(sections []help.Section) []help.Entry {
	var entries []help.Entry
	for _, section := range sections {
		entries = append(entries, section.Entries...)
	}
	return entries
}

func helpEntry(t *testing.T, sections []help.Section, id string) help.Entry {
	t.Helper()
	for _, entry := range helpEntries(sections) {
		if entry.ID == id {
			return entry
		}
	}
	t.Fatalf("missing help entry %s", id)
	return help.Entry{}
}

func TestDialogReferenceCatalogIdentitiesAliasesAndIsolation(t *testing.T) {
	sections := ReferenceHelp()
	require.Len(t, sections, 36)
	seen := map[string]bool{}
	for _, section := range sections {
		require.NotEmpty(t, section.ID)
		require.NotEmpty(t, section.Title)
		for _, entry := range section.Entries {
			require.False(t, seen[entry.ID], "duplicate action %s", entry.ID)
			seen[entry.ID] = true
			require.NotEmpty(t, entry.Description)
			require.NotEmpty(t, entry.Condition)
			require.NotEmpty(t, entry.Keys)
			aliases := map[string]bool{}
			for _, alias := range entry.Keys {
				require.False(t, aliases[alias], "duplicate alias %s in %s", alias, entry.ID)
				aliases[alias] = true
			}
			require.NotContains(t, entry.ID, "suggestion", "unconfigured suggestion actions are not active")
		}
	}
	assert.Equal(t, []string{"alt+i", "ˆ", "alt+ˆ", "̂", "alt+̂"}, helpEntry(t, sections, "dialog.file-picker.toggle-ignored-files").Keys)
	assert.Equal(t, []string{"backspace", "ctrl+h"}, helpEntry(t, sections, "dialog.plan-name.editing.delete-character-backward").Keys)
	assert.Equal(t, []string{"ctrl+a"}, helpEntry(t, sections, "dialog.command-palette.editing.line-start").Keys)
	assert.Equal(t, []string{"delete"}, helpEntry(t, sections, "dialog.session-browser.editing.delete-character-forward").Keys)
	assert.Equal(t, []string{"ctrl+shift+left", "alt+shift+left", "alt+shift+b"}, helpEntry(t, sections, "dialog.pending-message-edit.editing.select-word-backward").Keys)
	sections[0].Entries[0].Keys[0] = "changed"
	assert.Equal(t, "esc", ReferenceHelp()[0].Entries[0].Keys[0])
}

func TestDialogContextHelpEveryConcreteFamily(t *testing.T) {
	families := []Dialog{
		&helpDialog{}, &agentDetailsDialog{}, &attachmentPreviewDialog{}, &permissionsDialog{}, &skillsDialog{}, &toolsDialog{},
		&exitConfirmationDialog{}, &closeRootWithSubagentsDialog{}, &maxIterationsDialog{}, &oauthAuthorizationDialog{}, &planDeleteConfirmDialog{},
		&toolConfirmationDialog{}, &commandPaletteDialog{}, &filePickerDialog{}, &modelPickerDialog{}, &themePickerDialog{}, &sessionBrowserDialog{},
		&planBrowserDialog{}, &planStatusDialog{}, &planNameDialog{}, &planDetailDialog{}, &contextDialog{}, &costDialog{}, &effortPickerDialog{},
		&snapshotsDialog{}, &settingsDialog{}, &workingDirPickerDialog{}, &ElicitationDialog{}, &MCPPromptInputDialog{}, &multiChoiceDialog{},
		&pendingMessageEditDialog{}, &URLElicitationDialog{}, &tourOfferDialog{},
	}
	seen := map[string]bool{}
	for _, d := range families {
		family, _ := helpDialogFamily(d)
		t.Run(family, func(t *testing.T) {
			context, sections := ContextHelp(d)
			require.NotEmpty(t, context)
			require.NotEmpty(t, sections)
			require.Equal(t, "dialog."+family, sections[0].ID)
			require.False(t, seen[family])
			seen[family] = true
		})
	}
	assert.False(t, IsHelpDialog(nil))
	assert.True(t, IsHelpDialog(NewHelpDialog(help.Document{})))
}

func TestDialogContextHelpSelectedEnterAndSnapshotPurity(t *testing.T) {
	d := NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(nil), &service.SessionState{}).(*toolConfirmationDialog)
	d.SetSize(100, 40)
	_, sections := ContextHelp(d)
	entry := helpEntry(t, sections, "dialog.tool-confirmation.selected-enter")
	assert.Equal(t, "No", entry.Description)
	assert.Equal(t, []string{"mouse wheel", "scrollbar click/drag"}, helpEntry(t, sections, "dialog.shared-body.scroll-body").Keys)
	assert.Contains(t, helpEntry(t, ReferenceHelp(), "dialog.shared-body.scroll-body").Keys, "pgup", "other dialogs retain page scrolling")
	require.Len(t, d.actions, 6)
	d.FocusDefaultAction()
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	beforeActions := slices.Clone(d.actions)
	beforeIndex, beforeResponse := d.focusedAction, d.responseSent
	context, sections := ContextHelp(d)
	assert.Contains(t, context, "actions")
	assert.Equal(t, "Yes, once", helpEntry(t, sections, "dialog.tool-confirmation.selected-enter").Description)
	enters := 0
	for _, e := range helpEntries(sections) {
		if slices.Contains(e.Keys, "enter") {
			enters++
		}
	}
	assert.Equal(t, 1, enters, "only the selected action claims Enter")
	assert.Equal(t, beforeActions, d.actions)
	assert.Equal(t, beforeIndex, d.focusedAction)
	assert.Equal(t, beforeResponse, d.responseSent)
}

func TestDialogContextHelpFilterOverridesAndAvailability(t *testing.T) {
	d := newTestPlanBrowser(t, testPlanListing())
	_, sections := ContextHelp(d)
	for _, e := range helpEntries(sections) {
		assert.NotContains(t, e.ID, ".editing.")
	}
	d.Update(letterKey('/'))
	context, sections := ContextHelp(d)
	assert.Contains(t, context, "filter")
	assert.Contains(t, helpEntry(t, sections, "dialog.plan-browser.refresh").Condition, "Unavailable now: filter mode")
	assert.Equal(t, []string{"ctrl+a"}, helpEntry(t, sections, "dialog.plan-browser.editing.line-start").Keys)
	d.FocusActions(false)
	_, sections = ContextHelp(d)
	assert.Equal(t, []string{"ctrl+f"}, helpEntry(t, sections, "dialog.plan-browser.editing.character-forward").Keys,
		"action arrows are shadowed but ctrl+f still edits the focused filter")
	session := &sessionBrowserDialog{}
	_, sections = ContextHelp(session)
	assert.Contains(t, helpEntry(t, sections, "dialog.session-browser.cycle-three-workspace-filters").Condition, "Unavailable now")
	assert.Contains(t, helpEntry(t, sections, "dialog.session-browser.delete-selected-session-immediately").Condition, "no session selected")
}

func TestDialogContextHelpFormsAndPendingState(t *testing.T) {
	d := NewPendingMessageEditDialog("s", "t", "draft", 1, nil).(*pendingMessageEditDialog)
	d.SetSize(80, 24)
	_, sections := ContextHelp(d)
	assert.Contains(t, helpEntry(t, sections, "dialog.pending-message-edit.save-full-draft").Condition, "Unavailable now")
	assert.Equal(t, []string{"enter", "ctrl+m"}, helpEntry(t, sections, "dialog.pending-message-edit.editing.insert-newline").Keys)
	d.saving = true
	context, sections := ContextHelp(d)
	assert.Contains(t, context, "saving")
	for _, e := range helpEntries(sections) {
		assert.NotContains(t, e.ID, ".editing.")
	}
	assert.Equal(t, "draft", d.input.Value())

	form := NewMultiChoiceDialog(MultiChoiceConfig{Title: "Reason", AllowSecondary: true, AllowCustom: true}).(*multiChoiceDialog)
	form.SetSize(80, 24)
	_, sections = ContextHelp(form)
	assert.Equal(t, "Skip", helpEntry(t, sections, "dialog.multi-choice-tool-rejection-reason.selected-enter").Description)
	form.FocusActions(false)
	_, sections = ContextHelp(form)
	for _, e := range helpEntries(sections) {
		assert.NotContains(t, e.ID, ".editing.")
	}
}

func TestDialogContextHelpSettingsAndContextSelection(t *testing.T) {
	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	context, sections := ContextHelp(d)
	assert.Contains(t, context, "Appearance")
	assert.Equal(t, "Choose theme", helpEntry(t, sections, "dialog.settings.selected-enter").Description)
	d.tab = tabBehavior
	d.selected[tabBehavior] = rowTabTitleLength
	d.current.TabTitleMaxLength = 5
	d.prepareBody()
	_, sections = ContextHelp(d)
	assert.Equal(t, "Advance selected setting", helpEntry(t, sections, "dialog.settings.selected-enter").Description)
	assert.Contains(t, helpEntry(t, sections, "dialog.settings.control.adjust").Condition, "numeric bounds")
	d.setFocus(settingsCategories)
	_, sections = ContextHelp(d)
	assert.Equal(t, "Enter category controls", helpEntry(t, sections, "dialog.settings.enter-controls").Description)
	d.setFocus(settingsActions)
	d.prepareBody()
	_, sections = ContextHelp(d)
	assert.Equal(t, "Save", strings.TrimSpace(helpEntry(t, sections, "dialog.settings.selected-enter").Description))
	assert.Equal(t, []string{"tab", "shift+tab"}, helpEntry(t, sections, "dialog.settings.focus-zone").Keys)

	c := &contextDialog{selected: -1, breakdown: &runtime.ContextBreakdown{}}
	_, sections = ContextHelp(c)
	assert.Contains(t, helpEntry(t, sections, "dialog.context.compact-selected-live-session").Condition, "no live session")
	assert.Contains(t, helpEntry(t, sections, "dialog.context.drop-selected-attachment").Condition, "no attachment")
}

func TestHelpModalManagerEscapePreservesUnderlyingDraftAndSelection(t *testing.T) {
	mgr := New(animation.NewRuntime()).(*manager)
	defer mgr.Cleanup()
	mgr.SetSize(100, 35)
	d := NewPendingMessageEditDialog("s", "t", "unchanged draft", 42, func(string) tea.Cmd {
		t.Fatal("help must not save")
		return nil
	}).(*pendingMessageEditDialog)
	mgr.handleOpen(OpenDialogMsg{Model: d})
	d.FocusActions(false)
	focus := d.focusedAction
	context, sections := ContextHelp(mgr.TopDialog())
	h := NewHelpDialog(help.Document{Context: context, Current: sections, Reference: ReferenceHelp()})
	mgr.handleOpen(OpenDialogMsg{Model: h})
	mgr.Update(tea.KeyPressMsg{Code: tea.KeyF1})
	require.Len(t, mgr.stack, 2)
	_, cmd := mgr.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotNil(t, cmd)
	for _, msg := range collectMsgs(cmd) {
		mgr.Update(msg)
	}
	mgr.stack[len(mgr.stack)-1].anim.Cancel()
	mgr.handleTick(animation.TickMsg{})
	require.Same(t, d, mgr.TopDialog())
	assert.Equal(t, "unchanged draft", d.input.Value())
	assert.Equal(t, focus, d.focusedAction)
	assert.True(t, d.actionsFocused)
	assert.False(t, d.closed)
	assert.False(t, d.saving)
}

func TestPlanFilteringManagerEscapeClosesWithoutRewritingFilter(t *testing.T) {
	mgr := New(animation.NewRuntime()).(*manager)
	defer mgr.Cleanup()
	mgr.SetSize(100, 35)
	d := newTestPlanBrowser(t, testPlanListing())
	mgr.handleOpen(OpenDialogMsg{Model: d})
	d.Update(letterKey('/'))
	d.Update(letterKey('r'))
	before := d.filterInput.Value()
	_, cmd := mgr.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotNil(t, cmd)
	for _, msg := range collectMsgs(cmd) {
		mgr.Update(msg)
	}
	assert.True(t, mgr.Closing())
	assert.True(t, d.filtering)
	assert.Equal(t, before, d.filterInput.Value())
	assert.NotContains(t, d.View(), "Done filtering")
}

func TestDialogContextHelpEditingAliasesMatchModalManager(t *testing.T) {
	mgr := New(animation.NewRuntime()).(*manager)
	defer mgr.Cleanup()
	mgr.SetSize(100, 35)
	d := newTestPlanBrowser(t, testPlanListing())
	mgr.handleOpen(OpenDialogMsg{Model: d})
	mgr.Update(letterKey('/'))
	mgr.Update(letterKey('a'))
	mgr.Update(letterKey('b'))
	_, sections := ContextHelp(mgr.TopDialog())
	require.Contains(t, helpEntry(t, sections, "dialog.plan-browser.editing.delete-character-backward").Keys, "ctrl+h")
	mgr.Update(tea.KeyPressMsg{Code: 'h', Mod: tea.ModCtrl})
	assert.Equal(t, "a", d.filterInput.Value(), "modal Ctrl+h is editing, not Help")
	assert.Same(t, d, mgr.TopDialog())
	mgr.Update(tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
	mgr.Update(letterKey('z'))
	assert.Equal(t, "za", d.filterInput.Value(), "Ctrl+a remains a reachable line-start alias")
}

func TestDialogHelpQuitDocumentsApplicationPrecedence(t *testing.T) {
	entry := helpEntry(t, ReferenceHelp(), "dialog.manager.quit")
	assert.Equal(t, "Request exit confirmation", entry.Description)
	assert.Contains(t, entry.Condition, "before ordinary modal input")
	_, sections := ContextHelp(&planBrowserDialog{})
	assert.Equal(t, "Request exit confirmation", helpEntry(t, sections, "dialog.manager.quit").Description)
	_, sections = ContextHelp(&exitConfirmationDialog{})
	for _, e := range helpEntries(sections) {
		if slices.Contains(e.Keys, "ctrl+c") {
			assert.Equal(t, "Confirm exit", e.Description)
		}
	}
}

func TestDialogContextHelpEnterRequiresActualEnabledAction(t *testing.T) {
	assertNoSelected := func(sections []help.Section) {
		t.Helper()
		for _, section := range sections {
			assert.NotEqual(t, "Selected Enter action", section.Title)
		}
	}
	url := NewURLElicitationDialog(t.Context(), "Complete request", "", ElicitationRef{}).(*URLElicitationDialog)
	url.SetSize(80, 24)
	_, sections := ContextHelp(url)
	assertNoSelected(sections)
	for _, e := range helpEntries(sections) {
		assert.NotContains(t, e.Keys, "enter", "URL has no content Enter binding")
	}
	url.FocusActions(false)
	_, sections = ContextHelp(url)
	assert.Equal(t, "Decline", helpEntry(t, sections, "dialog.url-elicitation.selected-enter").Description)

	context := NewContextDialog(nil).(*contextDialog)
	context.SetSize(80, 24)
	_, sections = ContextHelp(context)
	assertNoSelected(sections)
	assert.Contains(t, helpEntry(t, sections, "dialog.context.compact-selected-live-session").Condition, "Unavailable now")

	form := NewMCPPromptInputDialog("required", mcptools.PromptInfo{Arguments: []mcptools.PromptArgument{{Name: "name", Required: true}}}).(*MCPPromptInputDialog)
	form.SetSize(80, 24)
	_, sections = ContextHelp(form)
	assertNoSelected(sections)
	assert.Contains(t, helpEntry(t, sections, "dialog.mcp-prompt-input.execute-prompt-with-trimmed-arguments").Condition, "required arguments are missing")

	pending := NewPendingMessageEditDialog("s", "t", "draft", 1, nil).(*pendingMessageEditDialog)
	pending.saving = true
	pending.SetSize(80, 24)
	_, sections = ContextHelp(pending)
	assertNoSelected(sections)
	assert.Contains(t, helpEntry(t, sections, "dialog.pending-message-edit.save-full-draft").Condition, "saving or closed")
}
