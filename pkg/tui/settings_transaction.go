package tui

import (
	"fmt"
	"log/slog"
	"sync/atomic"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/userconfig"
)

var nextSettingsID atomic.Uint64

type sessionDisplaySettings struct{ split, thinking, results bool }
type settingsThemeSnapshot struct {
	theme *styles.Theme
	auto  bool
}
type settingsThemePicker struct {
	id, revision uint64
	model        dialog.Dialog
	original     settingsThemeSnapshot
}
type settingsTransaction struct {
	id, revision      uint64
	editor            dialog.SettingsEditor
	original, preview messages.Preferences
	runtime           messages.Preferences
	sessions          map[string]sessionDisplaySettings
	theme             settingsThemeSnapshot
	picker            *settingsThemePicker
}

func currentSettingsTheme() settingsThemeSnapshot {
	return settingsThemeSnapshot{styles.CurrentTheme(), styles.AutoThemeEnabled()}
}
func (m *appModel) settingsActive(id uint64) bool {
	return id != 0 && m.settingsTransaction != nil && m.settingsTransaction.id == id && !m.settingsTransaction.editor.Disposed()
}
func (m *appModel) handleOpenSettingsDialog() (tea.Model, tea.Cmd) {
	var old dialog.Dialog
	if tx := m.settingsTransaction; tx != nil {
		old = tx.editor
	}
	rollback := m.rollbackSettings()
	if old != nil {
		rollback = tea.Batch(rollback, m.updateDialogCmd(dialog.CloseDialogByModelMsg{Model: old}))
	}
	original := preferencesFromConfig(userconfig.Get())
	runtime := original
	runtime.Layout, runtime.Panel = m.layoutSettings, messages.NormalizePanelSettings(m.panelSettings)
	runtime.ShowBanner, runtime.DimInactivePanes, runtime.TransparentBackground = m.showBanner, m.dimInactivePanes, m.transparentBackground
	if m.imageWriter != nil {
		runtime.RenderImages = m.imageWriter.Enabled()
	}
	theme := currentSettingsTheme()
	// The draft names the actual selection; an untouched CLI override must not persist.
	original.Theme = theme.theme.Ref
	if theme.auto {
		original.Theme = styles.AutoThemeRef
	}
	id := nextSettingsID.Add(1)
	editor := dialog.NewSettingsDialog(original, !m.hideSidebar, id).(dialog.SettingsEditor)
	tx := &settingsTransaction{id: id, editor: editor, original: original, preview: original, runtime: runtime, theme: theme, sessions: make(map[string]sessionDisplaySettings)}
	for id, state := range m.sessionStates {
		tx.sessions[id] = sessionDisplaySettings{state.SplitDiffView(), state.ExpandThinking(), state.HideToolResults()}
	}
	m.settingsTransaction = tx
	return m, tea.Batch(rollback, core.CmdHandler(dialog.OpenDialogMsg{Model: editor}))
}

func (m *appModel) handleSettingsPreview(msg messages.PreviewSettingsMsg) (tea.Model, tea.Cmd) {
	if !m.settingsActive(msg.TransactionID) {
		return m, nil
	}
	tx := m.settingsTransaction
	if msg.Revision <= tx.revision || msg.Revision != tx.editor.Revision() {
		return m, nil
	}
	tx.revision = msg.Revision
	cmd := m.previewSettings(tx.preview, msg.Preferences)
	tx.preview = msg.Preferences
	return m, cmd
}

func (m *appModel) previewSettings(before, p messages.Preferences) tea.Cmd {
	var cmds []tea.Cmd
	if before.Layout != p.Layout {
		_, cmd := m.applyLayoutSettings(p.Layout)
		cmds = append(cmds, cmd)
	}
	if !before.Panel.Equal(p.Panel) {
		_, cmd := m.applyPanelSettings(p.Panel)
		cmds = append(cmds, cmd)
	}
	if before.ShowBanner != p.ShowBanner {
		m.showBanner = p.ShowBanner
		for _, page := range m.chatPages {
			page.SetShowBanner(p.ShowBanner)
		}
	}
	if before.DimInactivePanes != p.DimInactivePanes {
		m.dimInactivePanes = p.DimInactivePanes
	}
	if before.TransparentBackground != p.TransparentBackground {
		m.transparentBackground = p.TransparentBackground
	}
	if before.RenderImages != p.RenderImages && m.imageWriter != nil {
		m.imageWriter.SetEnabled(p.RenderImages)
		tuiimage.SetRenderingEnabled(m.imageWriter.RenderingEnabled())
	}
	for id, state := range m.sessionStates {
		if tx := m.settingsTransaction; tx != nil {
			if _, ok := tx.sessions[id]; !ok {
				tx.sessions[id] = sessionDisplaySettings{state.SplitDiffView(), state.ExpandThinking(), state.HideToolResults()}
			}
		}
		if before.SplitDiffView != p.SplitDiffView {
			state.SetSplitDiffView(p.SplitDiffView)
		}
		if before.ExpandThinking != p.ExpandThinking {
			state.SetExpandThinking(p.ExpandThinking)
		}
		if before.HideToolResults != p.HideToolResults {
			state.SetHideToolResults(p.HideToolResults)
		}
		if page := m.chatPages[id]; page != nil {
			_, cmd := page.Update(messages.SessionToggleChangedMsg{})
			cmds = append(cmds, cmd)
		}
	}
	m.viewCacheValid = false
	return tea.Batch(append(cmds, m.resizeAll())...)
}

func (m *appModel) handleApplySettings(msg messages.ApplySettingsMsg) (tea.Model, tea.Cmd) {
	if !m.settingsActive(msg.TransactionID) {
		return m, nil
	}
	tx := m.settingsTransaction
	if msg.Revision <= tx.revision || msg.Revision != tx.editor.Revision() || tx.picker != nil {
		return m, nil
	}
	tx.revision = msg.Revision
	msg.Preferences.SendMode = messages.ParseSendMode(string(msg.Preferences.SendMode))
	msg.Preferences.InterruptConfirmation = messages.ParseInterruptMode(string(msg.Preferences.InterruptConfirmation))
	preview := m.previewSettings(tx.preview, msg.Preferences)
	tx.preview = msg.Preferences
	if !tx.original.Equal(msg.Preferences) {
		if err := saveChangedPreferences(tx.original, msg.Preferences); err != nil {
			slog.Warn("Failed to save settings to user config", "error", err)
			tx.editor.SaveFailed(err)
			return m, tea.Batch(preview, notification.WarningCmd("Settings could not be saved: "+err.Error()))
		}
	}
	m.settingsTransaction = nil
	tx.editor.Finish()
	p := msg.Preferences
	if p.SendMode != tx.original.SendMode || m.sendMode != p.SendMode {
		m.sendMode = p.SendMode
		for _, page := range m.chatPages {
			page.SetSendMode(p.SendMode)
		}
	}
	if p.InterruptConfirmation != tx.original.InterruptConfirmation || m.interruptMode != p.InterruptConfirmation {
		m.interruptMode = p.InterruptConfirmation
		for _, page := range m.chatPages {
			page.SetInterruptMode(p.InterruptConfirmation)
		}
	}
	var titleCmd tea.Cmd
	if p.TabTitleMaxLength != tx.original.TabTitleMaxLength && m.tabBar != nil {
		titleCmd = m.tabBar.SetMaxTitleLength(p.TabTitleMaxLength)
	}
	closeCmd := m.updateDialogCmd(dialog.CloseDialogByModelMsg{Model: tx.editor})
	if tx.original.Equal(p) {
		return m, tea.Batch(preview, titleCmd, closeCmd)
	}
	return m, tea.Batch(preview, titleCmd, closeCmd, notification.SuccessCmd("Settings saved"))
}

func (m *appModel) handleSettingsCancel(msg messages.CancelSettingsMsg) (tea.Model, tea.Cmd) {
	if !m.settingsActive(msg.TransactionID) || msg.Revision <= m.settingsTransaction.revision || msg.Revision != m.settingsTransaction.editor.Revision() {
		return m, nil
	}
	editor := m.settingsTransaction.editor
	rollback := m.rollbackSettings()
	return m, tea.Batch(rollback, m.updateDialogCmd(dialog.CloseDialogByModelMsg{Model: editor}))
}

func (m *appModel) rollbackSettings() tea.Cmd {
	tx := m.settingsTransaction
	if tx == nil {
		return nil
	}
	m.settingsTransaction = nil
	tx.editor.Finish()
	// Restore each session's own runtime state, not a persisted global default.
	cmds := []tea.Cmd{m.previewSettings(tx.preview, tx.runtime)}
	if tx.picker != nil {
		dialog.CleanupDialog(tx.picker.model)
		cmds = append(cmds, m.updateDialogCmd(dialog.CloseDialogByModelMsg{Model: tx.picker.model}))
	}
	for id, snapshot := range tx.sessions {
		if state := m.sessionStates[id]; state != nil {
			state.SetSplitDiffView(snapshot.split)
			state.SetExpandThinking(snapshot.thinking)
			state.SetHideToolResults(snapshot.results)
			if page := m.chatPages[id]; page != nil {
				_, cmd := page.Update(messages.SessionToggleChangedMsg{})
				cmds = append(cmds, cmd)
			}
		}
	}
	if styles.CurrentTheme() != tx.theme.theme || styles.AutoThemeEnabled() != tx.theme.auto {
		cmds = append(cmds, m.restoreSettingsTheme(tx.theme))
	}
	return tea.Batch(cmds...)
}
func (m *appModel) reconcileSettings() tea.Cmd {
	tx := m.settingsTransaction
	if tx == nil {
		return nil
	}
	if tx.editor.Disposed() {
		return m.rollbackSettings()
	}
	if tx.picker != nil {
		if disposed, ok := tx.picker.model.(interface{ Disposed() bool }); ok && disposed.Disposed() {
			original := tx.picker.original
			tx.picker = nil
			return m.restoreSettingsTheme(original)
		}
	}
	return nil
}

func (m *appModel) restoreSettingsTheme(snapshot settingsThemeSnapshot) tea.Cmd {
	styles.SetAutoThemeEnabled(snapshot.auto)
	styles.ApplyTheme(snapshot.theme)
	_, cmd := m.applyThemeChanged()
	return tea.Batch(cmd, m.syncSettingsThemeMode())
}
func (m *appModel) syncSettingsThemeMode() tea.Cmd {
	enabled := styles.AutoThemeEnabled()
	if enabled == m.lightDarkModeSet {
		return nil
	}
	m.lightDarkModeSet = enabled
	if enabled {
		return tea.Batch(tea.Raw(ansi.SetModeLightDark), tea.RequestBackgroundColor)
	}
	return tea.Raw(ansi.ResetModeLightDark)
}
func (m *appModel) openSettingsThemePicker(choices []dialog.ThemeChoice, ref string) (tea.Model, tea.Cmd) {
	tx := m.settingsTransaction
	if tx.picker != nil {
		return m, nil
	}
	id := nextSettingsID.Add(1)
	picker := dialog.NewSettingsThemePickerDialog(choices, ref, tx.id, id)
	tx.picker = &settingsThemePicker{id: id, model: picker, original: currentSettingsTheme()}
	return m, core.CmdHandler(dialog.OpenDialogMsg{Model: picker})
}
func (m *appModel) handleSettingsTheme(msg tea.Msg) (tea.Model, tea.Cmd) {
	var settingsID, pickerID, revision uint64
	var ref string
	commit, cancel := false, false
	switch v := msg.(type) {
	case messages.ThemePreviewMsg:
		settingsID, pickerID, revision, ref = v.SettingsID, v.PickerID, v.Revision, v.ThemeRef
	case messages.ChangeThemeMsg:
		settingsID, pickerID, revision, ref = v.SettingsID, v.PickerID, v.Revision, v.ThemeRef
		commit = true
	case messages.ThemeCancelPreviewMsg:
		settingsID, pickerID, revision = v.SettingsID, v.PickerID, v.Revision
		cancel = true
	}
	if !m.settingsActive(settingsID) {
		return m, nil
	}
	tx := m.settingsTransaction
	picker := tx.picker
	if picker == nil || picker.id != pickerID || revision <= picker.revision {
		return m, nil
	}
	if state, ok := picker.model.(interface {
		Disposed() bool
		Revision() uint64
	}); ok && (state.Disposed() || state.Revision() != revision) {
		return m, nil
	}
	picker.revision = revision
	if cancel {
		tx.picker = nil
		dialog.CleanupDialog(picker.model)
		return m, tea.Batch(m.restoreSettingsTheme(picker.original), m.updateDialogCmd(dialog.CloseDialogByModelMsg{Model: picker.model}))
	}
	theme, err := styles.LoadTheme(styles.ResolveThemeRef(ref))
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to load theme: %v", err))
	}
	styles.SetAutoThemeEnabled(ref == styles.AutoThemeRef)
	styles.ApplyTheme(theme)
	_, cmd := m.applyThemeChanged()
	cmd = tea.Batch(cmd, m.syncSettingsThemeMode())
	if commit {
		tx.picker = nil
		dialog.CleanupDialog(picker.model)
		tx.editor.SetTheme(ref)
		tx.preview.Theme = ref
		cmd = tea.Batch(cmd, m.updateDialogCmd(dialog.CloseDialogByModelMsg{Model: picker.model}))
	}
	return m, cmd
}
