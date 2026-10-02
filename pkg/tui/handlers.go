package tui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/browser"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/evaluation"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/shellpath"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/components/tool/editfile"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// --- Session management ---

func (m *appModel) handleBranchFromEdit(msg messages.BranchFromEditMsg) (tea.Model, tea.Cmd) {
	if m.application.SessionRuntime() == nil && m.application.SessionHandle() == nil {
		return m.handleLegacyBranchFromEdit(msg)
	}
	if m.application.Session() == nil || msg.ParentSessionID != m.application.Session().ID {
		return m, notification.ErrorCmd("Session changed; reopen the message before editing")
	}
	if msg.ExpectedSnapshot == "" {
		return m, notification.ErrorCmd("Conversation changed; reopen the message before editing")
	}
	return m, m.beginBranch(runtime.BranchOptions{Position: &msg.BranchAtPosition, ExpectedSnapshot: msg.ExpectedSnapshot}, m.paneFocus(), &messages.SendMsg{Content: msg.Content, Attachments: msg.Attachments})
}

func (m *appModel) handleForkSession() (tea.Model, tea.Cmd) {
	if m.application.SessionRuntime() == nil && m.application.SessionHandle() == nil {
		return m.handleLegacyForkSession()
	}
	if m.application.Session() == nil {
		return m, notification.ErrorCmd("No active session to fork")
	}
	return m, m.beginBranch(runtime.BranchOptions{}, "", nil)
}

func (m *appModel) handleToggleSessionStar(sessionID string) (tea.Model, tea.Cmd) {
	currentSess := m.application.Session()
	if currentSess != nil && currentSess.ID == sessionID {
		if handle := m.application.SessionHandle(); handle != nil {
			if !handle.Metadata().Capabilities.SessionEditing {
				return m, notification.InfoCmd("Session editing is not supported for this session")
			}
			starred := !currentSess.Starred
			if err := m.application.SetCurrentSessionStarred(m.ctx(), starred); err != nil {
				return m, notification.ErrorCmd(fmt.Sprintf("Failed to update session: %v", err))
			}
			m.chatPage.SetSessionStarred(starred)
			return m, nil
		}
	}
	if currentSess != nil && currentSess.ID == sessionID {
		return m, notification.InfoCmd("Session editing is not supported for this session")
	}
	owner, ctx := m.supervisor, m.ctx()
	return m, func() tea.Msg {
		err := owner.WithSessionOwner(ctx, sessionID, func(ctx context.Context, resources supervisor.ViewOwnerResources) error {
			handle, err := resources.Sessions.SessionByID(sessionID)
			if err != nil {
				loader, ok := resources.Sessions.(runtime.SessionLoader)
				if !ok {
					return err
				}
				handle, _, err = loader.LoadSession(ctx, sessionID)
				if err != nil {
					return err
				}
			}
			snapshot, err := handle.Snapshot(ctx)
			if err != nil {
				return err
			}
			return handle.SetStarred(ctx, !snapshot.Starred)
		})
		if err != nil {
			return notification.ErrorCmd("Failed to update session: " + err.Error())()
		}
		return nil
	}
}

func (m *appModel) handleSetSessionTitle(title string) (tea.Model, tea.Cmd) {
	if err := m.application.UpdateSessionTitle(m.ctx(), title); err != nil {
		if errors.Is(err, app.ErrTitleGenerating) {
			return m, notification.WarningCmd("Title is being generated, please wait")
		}
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to set session title: %v", err))
	}
	return m, notification.SuccessCmd("Title set to: " + title)
}

func (m *appModel) handleRegenerateTitle() (tea.Model, tea.Cmd) {
	sess := m.application.Session()
	if sess == nil {
		return m, notification.ErrorCmd("No active session")
	}
	if len(sess.GetLastUserMessages(1)) == 0 {
		return m, notification.ErrorCmd("Cannot regenerate title: no user message in session")
	}
	if err := m.application.RegenerateSessionTitle(m.ctx()); err != nil {
		if errors.Is(err, app.ErrTitleGenerating) {
			return m, notification.WarningCmd("Title is being generated, please wait")
		}
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to regenerate title: %v", err))
	}
	spinnerCmd := m.chatPage.SetTitleRegenerating(true)
	return m, tea.Batch(spinnerCmd, notification.SuccessCmd("Regenerating title..."))
}

func (m *appModel) handleDeleteSession(sessionID string) (tea.Model, tea.Cmd) {
	if sessions := m.application.SessionRuntime(); sessions != nil {
		if err := sessions.DeleteSession(m.ctx(), sessionID); err != nil {
			return m, notification.ErrorCmd("Failed to delete session: " + err.Error())
		}
		return m, notification.SuccessCmd("Session deleted.")
	}

	store := m.application.SessionStore()
	if store == nil {
		return m, notification.ErrorCmd("No session store configured")
	}
	if err := store.DeleteSession(m.ctx(), sessionID); err != nil {
		return m, notification.ErrorCmd("Failed to delete session: " + err.Error())
	}

	return m, notification.SuccessCmd("Session deleted.")
}

// --- Eval / Export / Compact / Copy ---

func (m *appModel) handleEvalSession(filename string) (tea.Model, tea.Cmd) {
	evalFile, _ := evaluation.Save(m.application.Session(), filename)
	return m, notification.SuccessCmd("Eval saved to file " + evalFile)
}

func (m *appModel) handleExportSession(filename string) (tea.Model, tea.Cmd) {
	exportFile, err := m.application.ExportHTML(m.ctx(), filename)
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to export session: %v", err))
	}
	return m, notification.SuccessCmd("Session exported to " + exportFile)
}

func (m *appModel) handleCompactSession(msg messages.CompactSessionMsg) (tea.Model, tea.Cmd) {
	if compactTargetsCurrentSession(msg, m.application.Session()) {
		return m, m.chatPage.CompactSession(msg.AdditionalPrompt)
	}
	// Targeted compaction of a live sub-agent session: queued onto the
	// target session's own run loop, so neither the root stream nor the
	// target stream is cancelled.
	if err := m.application.CompactLiveSession(m.ctx(), msg.SessionID, msg.AdditionalPrompt); err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Compaction request failed: %v", err))
	}
	return m, notification.InfoCmd(fmt.Sprintf(
		"Compaction requested for %s; it runs at the session's next safe point.",
		compactTargetLabel(msg),
	))
}

// compactTargetsCurrentSession reports whether msg addresses the current
// root session: an empty target (the /compact command) or the root's own
// session ID (the main row of the /context team view). Both route through
// the existing root compaction path.
func compactTargetsCurrentSession(msg messages.CompactSessionMsg, current *session.Session) bool {
	return msg.SessionID == "" || (current != nil && msg.SessionID == current.ID)
}

// compactTargetLabel names the target of a targeted compaction request for
// notifications: "agent (session 0f9e8d7c)", or just the short session ID
// when the agent name is unknown.
func compactTargetLabel(msg messages.CompactSessionMsg) string {
	shortID := msg.SessionID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	if msg.AgentName == "" {
		return "session " + shortID
	}
	return fmt.Sprintf("%s (session %s)", msg.AgentName, shortID)
}

func (m *appModel) handleCopySessionToClipboard() (tea.Model, tea.Cmd) {
	transcript := m.application.PlainTextTranscript()
	if transcript == "" {
		return m, notification.SuccessCmd("Conversation is empty; nothing copied.")
	}
	return m, copyToClipboard(transcript, "Conversation copied to clipboard.")
}

func (m *appModel) handleCopyLastResponseToClipboard() (tea.Model, tea.Cmd) {
	sess := m.application.Session()
	if sess == nil {
		return m, notification.InfoCmd("No active session.")
	}
	lastResponse := sess.GetLastAssistantMessageContent()
	if lastResponse == "" {
		return m, notification.InfoCmd("No assistant response to copy.")
	}
	return m, copyToClipboard(lastResponse, "Last response copied to clipboard.")
}

func (m *appModel) handleUndoSnapshot() (tea.Model, tea.Cmd) {
	if m.chatPage.IsWorking() {
		return m, notification.WarningCmd("Wait for the current response to finish before undoing")
	}
	result, err := m.application.UndoLastSnapshot(m.ctx())
	if err != nil {
		if errors.Is(err, app.ErrNothingToUndo) {
			return m, notification.InfoCmd("No snapshot to undo")
		}
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to undo snapshot: %v", err))
	}

	text := fmt.Sprintf("Restored %d file%s from the last snapshot", result.RestoredFiles, plural(result.RestoredFiles))
	return m, notification.SuccessCmd(text)
}

func (m *appModel) handleShowSnapshotsDialog() (tea.Model, tea.Cmd) {
	snapshots := m.application.ListSnapshots()
	return m, core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewSnapshotsDialog(snapshots),
	})
}

func (m *appModel) handleResetSnapshot(keep int) (tea.Model, tea.Cmd) {
	if m.chatPage.IsWorking() {
		return m, notification.WarningCmd("Wait for the current response to finish before resetting")
	}
	result, err := m.application.ResetSnapshot(m.ctx(), keep)
	if err != nil {
		if errors.Is(err, app.ErrNothingToUndo) {
			return m, notification.InfoCmd("Nothing to reset")
		}
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to reset snapshot: %v", err))
	}

	target := "the original state"
	if keep > 0 {
		target = fmt.Sprintf("snapshot %d", keep)
	}
	text := fmt.Sprintf("Restored %d file%s to %s", result.RestoredFiles, plural(result.RestoredFiles), target)
	return m, notification.SuccessCmd(text)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// copyToClipboard returns a sequenced command that copies text to the system
// clipboard using both the OSC 52 escape sequence (for SSH/tmux compatibility)
// and the platform-native clipboard API, then shows a success notification.
func copyToClipboard(text, successMsg string) tea.Cmd {
	return tea.Sequence(
		tea.SetClipboard(text),
		func() tea.Msg {
			_ = clipboard.WriteAll(text)
			return nil
		},
		notification.SuccessCmd(successMsg),
	)
}

// --- Agent management ---

func (m *appModel) handleSwitchAgent(agentName string) (tea.Model, tea.Cmd) {
	if agentName == m.sessionState.CurrentAgentName() {
		return m, nil
	}

	oldWorkingDir := ""
	if oldSession := m.application.Session(); oldSession != nil {
		oldWorkingDir = oldSession.WorkingDir
	}
	commitApp, rollbackApp, err := m.application.SwitchAgentTransactional(m.ctx(), agentName)
	if err != nil {
		if errors.Is(err, runtime.ErrUnsupported) {
			return m, notification.InfoCmd("Switching agents is not supported for this session")
		}
		return m, notification.ErrorCmd("Failed to switch agent: " + err.Error())
	}
	newSess := m.application.Session()
	activeID := m.supervisor.ActiveID()
	oldPersistedID := m.persistedSessionID(activeID)
	if m.tuiStore != nil {
		if err := m.tuiStore.ReplaceTab(m.ctx(), oldPersistedID, newSess.ID, newSess.WorkingDir); err != nil {
			rollbackErr := rollbackApp()
			if rollbackErr != nil {
				return m, notification.ErrorCmd(fmt.Sprintf("Failed to persist agent switch: %v; rollback cleanup failed: %v", err, rollbackErr))
			}
			return m, notification.ErrorCmd("Failed to persist agent switch: " + err.Error())
		}
	}
	if !m.supervisor.RetargetRunner(m.ctx(), activeID, newSess.ID, newSess.WorkingDir) {
		var reverseErr error
		if m.tuiStore != nil {
			reverseErr = m.tuiStore.ReplaceTab(m.ctx(), newSess.ID, oldPersistedID, oldWorkingDir)
		}
		rollbackErr := rollbackApp()
		if reverseErr != nil || rollbackErr != nil {
			return m, notification.ErrorCmd(fmt.Sprintf("Failed to retarget active tab; persistence rollback: %v; session rollback: %v", reverseErr, rollbackErr))
		}
		return m, notification.ErrorCmd("Failed to retarget the active tab")
	}
	m.initSessionComponents(newSess.ID, m.application, newSess)
	commitApp()
	m.dialogMgr.Cleanup()
	m.dialogMgr = dialog.New(m.ar)
	m.modelPickerGeneration++
	m.persistActiveTab(newSess.ID)
	return m, tea.Sequence(m.chatPage.Init(), m.resizeAll(), m.editor.Focus())
}

// handleShowAgentDetails opens the read-only agent-details dialog for the named
// agent, looking it up in the available-agents roster. The agent's latest
// token-usage snapshot (if it has run) rides along so the dialog can show its
// context usage, as does its cumulative attributed cost (if any).
func (m *appModel) handleShowAgentDetails(agentName string) (tea.Model, tea.Cmd) {
	for _, agent := range m.sessionState.AvailableAgents() {
		if agent.Name != agentName {
			continue
		}
		cfg := m.application.AgentConfigInfo(m.ctx(), agentName)
		var usage *runtime.Usage
		if u, ok := m.sessionState.AgentUsage(agentName); ok {
			usage = &u
		}
		var cost *float64
		if c, ok := m.sessionState.AgentCost(agentName); ok {
			cost = &c
		}
		return m, core.CmdHandler(dialog.OpenDialogMsg{
			Model: dialog.NewAgentDetailsDialog(agent, cfg, usage, cost),
		})
	}
	return m, nil
}

func (m *appModel) handleCycleAgent() (tea.Model, tea.Cmd) {
	availableAgents := m.sessionState.AvailableAgents()
	if len(availableAgents) <= 1 {
		return m, notification.InfoCmd("No other agents available")
	}
	currentIndex := -1
	for i, agent := range availableAgents {
		if agent.Name == m.sessionState.CurrentAgentName() {
			currentIndex = i
			break
		}
	}
	nextIndex := (currentIndex + 1) % len(availableAgents)
	return m.handleSwitchToAgentByIndex(nextIndex)
}

func (m *appModel) handleSwitchToAgentByIndex(index int) (tea.Model, tea.Cmd) {
	availableAgents := m.sessionState.AvailableAgents()
	if index >= 0 && index < len(availableAgents) {
		agentName := availableAgents[index].Name
		if agentName != m.sessionState.CurrentAgentName() {
			return m, core.CmdHandler(messages.SwitchAgentMsg{AgentName: agentName})
		}
	}
	return m, nil
}

// --- Toggles ---

// handleToggleYolo goes through the safety mode, not the raw ToolsApproved
// flag, so toggle-off genuinely revokes an autonomous mode. Same contract
// as the server's ToggleToolApproval.
func (m *appModel) handleToggleYolo() (tea.Model, tea.Cmd) {
	if err := m.application.EditSession(m.ctx(), runtime.SessionEdit{Kind: runtime.SessionEditPolicy, ToggleToolsApproved: true}); err != nil {
		return m, notification.ErrorCmd("Failed to change tool approval: " + err.Error())
	}
	m.sessionState.SetYoloMode(m.application.Session().IsToolsApproved())
	return m.forwardChat(messages.SessionToggleChangedMsg{})
}

// handleTogglePause toggles whether the runtime loop is paused at iteration
// boundaries. The pause kicks in once the in-flight LLM request and its tool
// calls finish; running /pause again resumes the loop.
//
// The TUI reflects this in the resize-handle indicator: requesting a pause
// while the agent is working shows "Pausing…" until the runtime emits a
// RuntimePausedEvent at the next iteration boundary (flipping it to
// "Paused"); requesting a pause while idle shows "Paused" immediately.
func (m *appModel) handleTogglePause() (tea.Model, tea.Cmd) {
	if !m.application.SupportsPause() {
		return m, notification.InfoCmd("Pause is not supported for this session")
	}
	paused, err := m.application.TogglePause(m.ctx())
	if err != nil {
		if errors.Is(err, runtime.ErrUnsupported) {
			return m, notification.InfoCmd("Pause is not supported for this session")
		}
		return m, notification.ErrorCmd("Failed to toggle pause: " + err.Error())
	}
	if paused {
		if m.chatPage.IsWorking() {
			m.sessionState.SetPauseState(service.PausePausing)
		} else {
			m.sessionState.SetPauseState(service.PausePaused)
		}
		return m, nil
	}
	m.sessionState.SetPauseState(service.PauseNone)
	return m, notification.SuccessCmd("Resumed")
}

func (m *appModel) handleToggleHideToolResults() (tea.Model, tea.Cmd) {
	return m.forwardChat(messages.ToggleHideToolResultsMsg{})
}

func (m *appModel) handleToggleSplitDiff() (tea.Model, tea.Cmd) {
	m.sessionState.ToggleSplitDiffView()
	enabled := m.sessionState.SplitDiffView()

	// Persist to global userconfig
	go persistSplitDiffView(enabled)

	return m, tea.Batch(
		m.updateChatCmd(editfile.ToggleDiffViewMsg{}),
		m.updateChatCmd(messages.SessionToggleChangedMsg{}),
	)
}

// persistSplitDiffView writes the current split-diff toggle to the user
// config without blocking the UI. Errors are logged but otherwise ignored
// because losing the persistence is non-fatal.
func persistSplitDiffView(enabled bool) {
	err := userconfig.Update(func(cfg *userconfig.Config) error {
		if cfg.Settings == nil {
			cfg.Settings = &userconfig.Settings{}
		}
		cfg.Settings.SplitDiffView = &enabled
		return nil
	})
	if err != nil {
		slog.Warn("Failed to persist split diff setting to userconfig", "error", err)
	}
}

// --- Dialogs ---

func (m *appModel) handleShowCostDialog() (tea.Model, tea.Cmd) {
	sess := m.application.Session()
	return m, core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewCostDialog(sess),
	})
}

// handleShowContextDialog computes the context breakdown in a tea.Cmd
// goroutine: the computation lists the agent's tools, which may start
// not-yet-started toolsets (e.g. MCP servers) and block for a while, so it
// must not run inside the Update loop. The dialog opens when the data is
// ready, together with the live-session team view (current root plus every
// running sub-agent session; empty for runtimes without live tracking).
func (m *appModel) handleShowContextDialog() (tea.Model, tea.Cmd) {
	appRef := m.application
	ctx := m.ctx()
	return m, func() tea.Msg {
		breakdown, err := appRef.ContextBreakdown(ctx)
		switch {
		case errors.Is(err, runtime.ErrUnsupported):
			return notification.ShowMsg{
				Text: "Context breakdown is not supported for this session",
				Type: notification.TypeInfo,
			}
		case err != nil:
			return notification.ShowMsg{
				Text: fmt.Sprintf("Failed to compute context breakdown: %v", err),
				Type: notification.TypeError,
			}
		}
		return dialog.OpenDialogMsg{Model: dialog.NewContextDialog(breakdown, appRef.LiveSessions(ctx)...)}
	}
}

// handleDropAttachedFile removes an attached file from the current session
// so it stops being shared with sub-agents and skill prompts.
func (m *appModel) handleDropAttachedFile(path string) (tea.Model, tea.Cmd) {
	dropped, err := m.application.DropAttachedFile(m.ctx(), path)
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to drop attachment: %v", err))
	}
	return m, notification.SuccessCmd(fmt.Sprintf("Dropped %s from the session context.", filepath.Base(dropped)))
}

func (m *appModel) handleShowPermissionsDialog() (tea.Model, tea.Cmd) {
	return m, m.capabilityCommand(func(ctx context.Context, a *app.App) tea.Msg {
		info, err := a.EffectivePermissions(ctx)
		if err != nil {
			return notification.ShowMsg{Text: "Failed to load permissions: " + err.Error(), Type: notification.TypeError}
		}
		return dialog.OpenDialogMsg{Model: dialog.NewPermissionsDialog(app.CombinedPermissions(info), info.ToolsApproved)}
	})
}

func (m *appModel) handleShowToolsDialog() (tea.Model, tea.Cmd) {
	return m, m.capabilityCommand(func(ctx context.Context, a *app.App) tea.Msg {
		info, err := a.InspectTools(ctx)
		if err != nil {
			return notification.ShowMsg{Text: "Failed to load tools: " + err.Error(), Type: notification.TypeError}
		}
		return dialog.OpenDialogMsg{Model: dialog.NewToolsDialog(app.ToolsetStatuses(info), info.Tools)}
	})
}

func (m *appModel) handleShowSkillsDialog() (tea.Model, tea.Cmd) {
	return m, m.capabilityCommand(func(ctx context.Context, a *app.App) tea.Msg {
		list, err := a.CurrentAgentSkillsContext(ctx)
		if err != nil {
			return notification.ShowMsg{Text: "Failed to discover skills: " + err.Error(), Type: notification.TypeError}
		}
		return dialog.OpenDialogMsg{Model: dialog.NewSkillsDialog(list)}
	})
}

// handleRestartToolset asks the runtime to restart the named toolset.
// The actual call can block for up to ~35s (the supervisor's
// reconnect timeout), so we run it inside a tea.Cmd goroutine and
// surface the result via a notification toast on completion.
func (m *appModel) handleRestartToolset(name string) (tea.Model, tea.Cmd) {
	if name == "" {
		return m, notification.ErrorCmd("usage: /toolset-restart <name>")
	}
	return m, m.capabilityCommand(func(ctx context.Context, a *app.App) tea.Msg {
		if err := a.RestartToolset(ctx, name); err != nil {
			return notification.ShowMsg{Text: "Failed to restart toolset: " + err.Error(), Type: notification.TypeError}
		}
		return notification.ShowMsg{Text: fmt.Sprintf("Toolset %q restarted", name), Type: notification.TypeSuccess}
	})
}

// --- MCP prompts ---

func (m *appModel) handleShowMCPPromptInput(promptName string, promptInfo any) (tea.Model, tea.Cmd) {
	info, ok := promptInfo.(mcptools.PromptInfo)
	if !ok {
		return m, notification.ErrorCmd("Invalid prompt info")
	}
	return m, core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewMCPPromptInputDialog(promptName, info),
	})
}

func (m *appModel) handleMCPPrompt(promptName string, arguments map[string]string) (tea.Model, tea.Cmd) {
	return m, m.capabilityCommand(func(ctx context.Context, a *app.App) tea.Msg {
		content, err := a.ExecuteMCPPrompt(ctx, promptName, arguments)
		if err != nil {
			return notification.ShowMsg{Text: "Failed to execute MCP prompt: " + err.Error(), Type: notification.TypeError}
		}
		// Prompt retrieval returns text only; the user chooses whether to submit it.
		return messages.RestorePendingMessagesMsg{Content: content}
	})
}

// --- Model picker ---

func (m *appModel) handleOpenModelPicker() (tea.Model, tea.Cmd) {
	if !m.application.SupportsModelSwitching() {
		return m, notification.InfoCmd("Model switching is unavailable for this session")
	}
	m.modelPickerGeneration++
	m.modelPickerApp = m.application
	appRef, ctx := m.application, m.ctx()
	generation, sessionID := m.modelPickerGeneration, appRef.Session().ID
	return m, func() tea.Msg {
		return messages.ModelPickerLoadedMsg{Models: appRef.AvailableModels(ctx), SessionID: sessionID, Generation: generation}
	}
}

func (m *appModel) modelPickerResultCurrent(sessionID string, generation uint64) bool {
	return generation != 0 && generation == m.modelPickerGeneration && m.application == m.modelPickerApp &&
		m.application != nil && m.application.Session() != nil && m.application.Session().ID == sessionID
}

func (m *appModel) handleModelPickerLoaded(msg messages.ModelPickerLoadedMsg) (tea.Model, tea.Cmd) {
	if !m.modelPickerResultCurrent(msg.SessionID, msg.Generation) {
		return m, nil
	}
	if msg.Err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to load models: %v", msg.Err))
	}
	if len(msg.Models) == 0 {
		return m, notification.InfoCmd("No models available for selection")
	}
	return m.forwardDialog(dialog.OpenDialogMsg{Model: dialog.NewModelPickerDialog(msg.Models)})
}

func (m *appModel) handleRefreshModelPicker(query string) (tea.Model, tea.Cmd) {
	if !m.application.SupportsModelSwitching() {
		return m, notification.InfoCmd("Model switching is unavailable for this session")
	}
	ctx := m.ctx()
	m.modelPickerGeneration++
	m.modelPickerApp = m.application
	appRef := m.application
	generation, sessionID := m.modelPickerGeneration, appRef.Session().ID
	return m, tea.Batch(
		notification.InfoCmd("Refreshing models…"),
		func() tea.Msg {
			catalogRefreshed := false
			var err error
			if appRef.SupportsModelCatalogRefresh() {
				err = appRef.RefreshModelsCatalog(ctx)
				catalogRefreshed = err == nil
			}
			if errors.Is(err, runtime.ErrUnsupported) {
				err = nil
			}
			if err != nil {
				return messages.ModelPickerRefreshedMsg{SessionID: sessionID, Generation: generation, Query: query, Err: err}
			}
			return messages.ModelPickerRefreshedMsg{SessionID: sessionID, Generation: generation, Models: appRef.AvailableModels(ctx), Query: query, CatalogRefreshed: catalogRefreshed}
		},
	)
}

func (m *appModel) handleModelPickerRefreshed(msg messages.ModelPickerRefreshedMsg) (tea.Model, tea.Cmd) {
	if !m.modelPickerResultCurrent(msg.SessionID, msg.Generation) {
		return m, nil
	}
	if msg.Err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to refresh models catalog: %v", msg.Err))
	}
	if len(msg.Models) == 0 {
		return m, notification.InfoCmd("No models available for selection")
	}

	modelDialog := dialog.NewModelPickerDialogWithQuery(msg.Models, msg.Query)
	toast := "Model list reloaded"
	if msg.CatalogRefreshed {
		toast = "Models refreshed"
	}
	return m, tea.Batch(
		notification.SuccessCmd(toast),
		m.updateDialogCmd(dialog.OpenDialogMsg{Model: modelDialog}),
	)
}

// handleScopedThinkingCycle rejects a delayed footer action after its canonical
// session, routing generation, agent, primary or displayed model has changed. Mutation
// then uses the same synchronous canonical operation as Shift+Tab.
func (m *appModel) handleScopedThinkingCycle(msg messages.CycleThinkingLevelMsg) (tea.Model, tea.Cmd) {
	if m.application == nil || m.application.Session() == nil || m.supervisor == nil || msg.RouteGeneration == 0 ||
		msg.SessionID != m.application.Session().ID || msg.AgentName != m.sessionState.CurrentAgentName() || msg.ModelRef != m.thinkingModelReference() {
		return m, nil
	}
	displayed := ""
	if m.sessionState.GetCurrentAgent().PrimaryThinking != nil {
		displayed = m.thinkingDisplayReference()
	}
	if msg.DisplayedModelRef != displayed {
		return m, nil
	}
	generation, exists := m.supervisor.RouteGeneration(m.paneFocus())
	if !exists || generation != msg.RouteGeneration {
		return m, nil
	}
	return m.handleCycleThinkingLevel()
}

// thinkingModelReference reads already-applied projection metadata only. A
// configured alias may name the same canonical provider/model; an intervening
// AgentInfo model change invalidates the old projection until TeamInfo arrives.
func (m *appModel) thinkingModelReference() string {
	display := m.thinkingDisplayReference()
	details := m.sessionState.GetCurrentAgent()
	canonical := details.ModelID
	if details.Provider != "" {
		canonical = details.Provider + "/" + canonical
	}
	if details.PrimaryThinking != nil && display == canonical {
		return details.PrimaryThinking.ModelRef
	}
	return display
}

func (m *appModel) thinkingDisplayReference() string {
	current := m.application.CurrentAgentModel(m.ctx())
	details := m.sessionState.GetCurrentAgent()
	if details.ModelID == "" {
		return current
	}
	canonical := details.ModelID
	if details.Provider != "" {
		canonical = details.Provider + "/" + canonical
	}
	if current != "" && current != canonical && current != details.Model && current != details.Provider+"/"+details.Model {
		return current
	}
	return canonical
}

// handleCycleThinkingLevel advances the current agent's thinking-effort level
// (shift+tab). On success the new level is reflected in the sidebar via the
// re-emitted agent info; only failures surface a notification.
func (m *appModel) handleCycleThinkingLevel() (tea.Model, tea.Cmd) {
	if !m.application.SupportsThinkingLevels() {
		return m, notification.InfoCmd("Current model does not support thinking levels")
	}
	if _, err := m.application.CycleAgentThinkingLevel(m.ctx()); err != nil {
		if errors.Is(err, runtime.ErrUnsupported) {
			return m, notification.InfoCmd("Current model does not support thinking levels")
		}
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to change thinking level: %v", err))
	}
	return m, nil
}

// handleSetThinkingLevel applies the /effort command: it sets the current
// model's reasoning-effort level to the requested value. An empty level
// opens the effort picker dialog; unsupported levels surface the model's
// supported list via the runtime error.
func (m *appModel) handleSetThinkingLevel(level string) (tea.Model, tea.Cmd) {
	if !m.application.SupportsThinkingLevels() {
		return m, notification.InfoCmd("Current model does not support thinking levels")
	}
	if level == "" {
		return m.openEffortPicker()
	}
	parsed, ok := effort.Parse(level)
	if !ok {
		return m, notification.ErrorCmd(fmt.Sprintf("Unknown effort level %q (valid: none, minimal, low, medium, high, xhigh, max)", level))
	}
	applied, err := m.application.SetAgentThinkingLevel(m.ctx(), parsed)
	if err != nil {
		if errors.Is(err, runtime.ErrUnsupported) {
			return m, notification.InfoCmd("Current model does not support thinking levels")
		}
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to set thinking level: %v", err))
	}
	return m, notification.SuccessCmd("Reasoning effort set to " + applied.String())
}

// openEffortPicker opens the effort picker dialog listing the thinking-effort
// levels supported by the current agent's model (/effort without arguments).
// The sidebar's thinking label doubles as the support signal: it is empty
// exactly when the runtime reports no selectable thinking configuration.
func (m *appModel) openEffortPicker() (tea.Model, tea.Cmd) {
	levels := m.application.CurrentAgentThinkingLevels(m.ctx())
	if len(levels) == 0 {
		return m, notification.InfoCmd("Current model does not support thinking levels")
	}
	current := m.application.CurrentAgentThinkingLevel(m.ctx())
	return m, core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewEffortPickerDialog(levels, current),
	})
}

func (m *appModel) handleChangeModel(modelRef string) (tea.Model, tea.Cmd) {
	if err := m.application.SetCurrentAgentModel(m.ctx(), modelRef); err != nil {
		if errors.Is(err, runtime.ErrUnsupported) {
			return m, notification.InfoCmd("Model switching is unavailable for this session")
		}
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to change model: %v", err))
	}
	if modelRef == "" {
		return m, notification.SuccessCmd("Model reset to default")
	}
	return m, notification.SuccessCmd("Model changed to " + modelRef)
}

// --- Theme picker ---

func (m *appModel) handleOpenThemePicker() (tea.Model, tea.Cmd) { return m.openThemePicker(0) }
func (m *appModel) openThemePicker(settingsID uint64) (tea.Model, tea.Cmd) {
	if settingsID != 0 && !m.settingsActive(settingsID) {
		return m, nil
	}
	themeRefs, err := styles.ListThemeRefs()
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to list themes: %v", err))
	}
	currentTheme := styles.CurrentTheme()
	currentRef := currentTheme.Ref
	autoEnabled := styles.AutoThemeEnabled()

	// The auto entry resolves to the configured light/dark pair at apply
	// time. While it is active, the resolved concrete theme is not marked
	// current so only one entry carries the badge.
	choices := []dialog.ThemeChoice{{
		Ref:       styles.AutoThemeRef,
		Name:      styles.AutoThemeDisplayName,
		IsCurrent: autoEnabled,
		IsBuiltin: true,
	}}
	for _, ref := range themeRefs {
		theme, loadErr := styles.LoadTheme(ref)
		if loadErr != nil {
			continue
		}
		name := theme.Name
		if name == "" {
			name = strings.TrimPrefix(ref, styles.UserThemePrefix)
		}
		choices = append(choices, dialog.ThemeChoice{
			Ref:       ref,
			Name:      name,
			IsCurrent: !autoEnabled && ref == currentRef,
			IsDefault: ref == styles.DefaultThemeRef,
			IsBuiltin: styles.IsBuiltinTheme(ref),
		})
	}
	if settingsID != 0 {
		return m.openSettingsThemePicker(choices, currentRef)
	}
	return m, core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewThemePickerDialog(choices, currentRef),
	})
}

func (m *appModel) handleChangeTheme(themeRef string) (tea.Model, tea.Cmd) {
	selectingAuto := themeRef == styles.AutoThemeRef
	if styles.GetPersistedThemeRef() == themeRef && styles.AutoThemeEnabled() == selectingAuto {
		return m, nil
	}
	theme, err := styles.LoadTheme(styles.ResolveThemeRef(themeRef))
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to load theme: %v", err))
	}
	wasAuto := styles.AutoThemeEnabled()
	styles.SetAutoThemeEnabled(selectingAuto)
	styles.ApplyTheme(theme)
	m.invalidateCachesForThemeChange()

	if err := styles.SaveThemeToUserConfig(themeRef); err != nil {
		slog.Warn("Failed to save theme to user config", "theme", themeRef, "error", err)
	}

	displayName := theme.Name
	if selectingAuto {
		displayName = styles.AutoThemeDisplayName
	}
	cmds := []tea.Cmd{
		notification.SuccessCmd("Theme changed to " + displayName),
		core.CmdHandler(messages.ThemeChangedMsg{}),
	}
	// Keep terminal color-scheme reporting (DEC mode 2031) in sync with the
	// auto selection: enable it and re-query the polarity when auto is picked
	// mid-session, reset it when a concrete theme replaces auto.
	switch {
	case selectingAuto && !m.lightDarkModeSet:
		m.lightDarkModeSet = true
		cmds = append(cmds, tea.Raw(ansi.SetModeLightDark), tea.RequestBackgroundColor)
	case !selectingAuto && wasAuto && m.lightDarkModeSet:
		m.lightDarkModeSet = false
		cmds = append(cmds, tea.Raw(ansi.ResetModeLightDark))
	}
	return m, tea.Sequence(cmds...)
}

func (m *appModel) handleThemePreview(themeRef string) (tea.Model, tea.Cmd) {
	themeRef = styles.ResolveThemeRef(themeRef)
	if current := styles.CurrentTheme(); current != nil && current.Ref == themeRef {
		return m, nil
	}
	theme, err := styles.LoadTheme(themeRef)
	if err != nil {
		return m, nil
	}
	styles.ApplyTheme(theme)
	return m.applyThemeChanged()
}

func (m *appModel) handleThemeCancelPreview(originalRef string) (tea.Model, tea.Cmd) {
	if current := styles.CurrentTheme(); current != nil && current.Ref == originalRef {
		return m, nil
	}
	styles.ApplyThemeRef(originalRef)
	return m.applyThemeChanged()
}

func (m *appModel) invalidateCachesForThemeChange() {
	// markdown's style cache resets itself via styles.OnThemeChange.
	m.viewCacheValid = false
	if m.tabBar != nil {
		m.tabBar.InvalidateCache()
	}
}

func (m *appModel) applyThemeChanged() (tea.Model, tea.Cmd) {
	m.invalidateCachesForThemeChange()
	// Re-target the file watcher: theme changes (picker selection, preview,
	// hot reload) can move the active theme to a different backing file.
	m.watchCurrentTheme()
	cmds := []tea.Cmd{m.updateDialogCmd(messages.ThemeChangedMsg{})}
	if m.messageBar != nil {
		cmds = append(cmds, m.messageBar.Update(messages.ThemeChangedMsg{}))
	}
	for _, ed := range m.editors {
		_, cmd := ed.Update(messages.ThemeChangedMsg{})
		cmds = append(cmds, cmd)
	}
	for _, page := range m.chatPages {
		_, cmd := page.Update(messages.ThemeChangedMsg{})
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

// handleThemeFileChanged hot-reloads a theme that was modified on disk.
func (m *appModel) handleThemeFileChanged(themeRef string) (tea.Model, tea.Cmd) {
	theme, err := styles.LoadTheme(themeRef)
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to hot-reload theme: %v", err))
	}
	styles.ApplyTheme(theme)
	return m, tea.Batch(
		notification.SuccessCmd("Theme hot-reloaded"),
		core.CmdHandler(messages.ThemeChangedMsg{}),
	)
}

// --- Settings (/settings) ---

func preferencesFromConfig(settings *userconfig.Settings) messages.Preferences {
	return messages.Preferences{
		Theme:                 settings.Theme,
		Layout:                layoutSettingsFromConfig(settings.GetLayout()),
		Panel:                 panelSettingsFromConfig(settings.GetPanel()),
		SendMode:              messages.ParseSendMode(settings.GetBusySendMode()),
		SplitDiffView:         settings.GetSplitDiffView(),
		ExpandThinking:        settings.GetExpandThinking(),
		HideToolResults:       settings.HideToolResults,
		RenderImages:          settings.GetRenderImages(),
		ShowBanner:            settings.GetShowBanner(),
		DimInactivePanes:      settings.GetDimInactivePanes(),
		TransparentBackground: settings.GetTransparentBackground(),
		YOLO:                  settings.YOLO,
		RestoreTabs:           settings.GetRestoreTabs(),
		Snapshot:              settings.SnapshotsEnabled(),
		CacheStablePrompts:    settings.CacheStablePromptsEnabled(),
		WarnOnCacheMiss:       settings.CacheMissWarningsEnabled(),
		Lean:                  settings.Lean,
		TabTitleMaxLength:     settings.GetTabTitleMaxLength(),
		Sound:                 settings.GetSound(),
		SoundThreshold:        settings.GetSoundThreshold(),
		InterruptConfirmation: messages.ParseInterruptMode(settings.GetInterruptConfirmation()),
	}
}

// applyLayoutSettings applies the given layout to every chat page (all tabs
// share the same layout) without persisting it.
func (m *appModel) applyLayoutSettings(settings messages.LayoutSettings) (tea.Model, tea.Cmd) {
	settings.SidebarPosition = messages.ParseSidebarPosition(string(settings.SidebarPosition))
	settings.SectionSpacing = messages.ParseSectionSpacing(string(settings.SectionSpacing))
	settings.SidebarInfoMode = messages.ParseSidebarInfoMode(string(settings.SidebarInfoMode))
	m.layoutSettings = settings

	var cmds []tea.Cmd
	for _, page := range m.chatPages {
		if cmd := page.SetLayoutSettings(settings); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	cmds = append(cmds, m.resizeAll())

	return m, tea.Batch(cmds...)
}

// layoutSettingsFromConfig converts persisted layout settings to their runtime form.
func layoutSettingsFromConfig(l userconfig.LayoutSettings) messages.LayoutSettings {
	return messages.LayoutSettings{
		SidebarPosition:  messages.ParseSidebarPosition(l.SidebarPosition),
		SectionSpacing:   messages.ParseSectionSpacing(l.SectionSpacing),
		SidebarInfoMode:  messages.ParseSidebarInfoMode(l.SidebarInfoMode),
		ActiveAgentsOnly: l.ActiveAgentsOnly,
		HideSessionPath:  l.HideSessionPath,
		HideUsage:        l.HideUsage,
		HideAgents:       l.HideAgents,
		HideTools:        l.HideTools,
		HideTodos:        l.HideTodos,
	}
}

// panelSettingsFromConfig preserves the distinction between unset defaults
// and an explicitly empty list (including a present panel with null elements).
func panelSettingsFromConfig(p *userconfig.PanelSettings) messages.PanelSettings {
	if p == nil {
		return messages.DefaultPanelSettings()
	}
	elements := make([]messages.PanelElement, len(p.Elements))
	for i, element := range p.Elements {
		elements[i] = messages.PanelElement(element)
	}
	return messages.NormalizePanelSettings(messages.PanelSettings{Elements: elements})
}

// savePreferences persists every value managed by the settings dialog.
// Values matching their defaults are omitted to keep the config minimal.
func savePreferences(p messages.Preferences) error {
	return userconfig.Update(func(cfg *userconfig.Config) error { return writePreferences(cfg, p) })
}

func writePreferences(cfg *userconfig.Config, p messages.Preferences) error {
	if cfg.Settings == nil {
		cfg.Settings = &userconfig.Settings{}
	}
	s := cfg.Settings
	if p.SendMode == messages.SendModeSteer {
		s.BusySendMode = string(messages.SendModeSteer)
	} else {
		s.BusySendMode = ""
	}
	s.SplitDiffView = boolPreference(p.SplitDiffView, true)
	s.ExpandThinking = boolPreference(p.ExpandThinking, false)
	s.RestoreTabs = boolPreference(p.RestoreTabs, false)
	s.Snapshot = boolPreference(p.Snapshot, false)
	s.CacheStablePrompts = boolPreference(p.CacheStablePrompts, false)
	s.WarnOnCacheMiss = boolPreference(p.WarnOnCacheMiss, false)
	s.HideToolResults = p.HideToolResults
	s.RenderImages = boolPreference(p.RenderImages, true)
	s.ShowBanner = boolPreference(p.ShowBanner, true)
	s.DimInactivePanes = boolPreference(p.DimInactivePanes, true)
	s.TransparentBackground = boolPreference(p.TransparentBackground, true)
	s.YOLO = p.YOLO
	s.Lean = p.Lean
	s.Sound = p.Sound
	s.InterruptConfirmation = string(p.InterruptConfirmation)
	if p.SoundThreshold == userconfig.DefaultSoundThreshold {
		s.SoundThreshold = 0
	} else {
		s.SoundThreshold = p.SoundThreshold
	}
	if p.TabTitleMaxLength == userconfig.DefaultTabTitleMaxLength {
		s.TabTitleMaxLength = 0
	} else {
		s.TabTitleMaxLength = p.TabTitleMaxLength
	}

	panel := messages.NormalizePanelSettings(p.Panel)
	if panel.Equal(messages.DefaultPanelSettings()) {
		s.Panel = nil
	} else {
		elements := make([]string, len(panel.Elements))
		for i, element := range panel.Elements {
			elements[i] = string(element)
		}
		s.Panel = &userconfig.PanelSettings{Elements: elements}
	}

	layout := p.Layout
	// Normalize before the default comparison so an unnormalized zero
	// value ("") and the explicit default ("compact") both clear the entry.
	layout.SidebarInfoMode = messages.ParseSidebarInfoMode(string(layout.SidebarInfoMode))
	if layout == (messages.LayoutSettings{SidebarPosition: messages.SidebarRight, SectionSpacing: messages.SpacingNormal, SidebarInfoMode: messages.InfoModeCompact}) {
		s.Layout = nil
		return nil
	}
	position := string(layout.SidebarPosition)
	if layout.SidebarPosition == messages.SidebarRight {
		position = ""
	}
	spacing := string(layout.SectionSpacing)
	if layout.SectionSpacing == messages.SpacingNormal {
		spacing = ""
	}
	infoMode := string(layout.SidebarInfoMode)
	if layout.SidebarInfoMode == messages.InfoModeCompact {
		infoMode = ""
	}
	s.Layout = &userconfig.LayoutSettings{
		SidebarPosition: position, SectionSpacing: spacing, SidebarInfoMode: infoMode,
		ActiveAgentsOnly: layout.ActiveAgentsOnly,
		HideSessionPath:  layout.HideSessionPath, HideUsage: layout.HideUsage,
		HideAgents: layout.HideAgents, HideTools: layout.HideTools, HideTodos: layout.HideTodos,
	}
	return nil
}

func boolPreference(value, defaultValue bool) *bool {
	if value == defaultValue {
		return nil
	}
	return &value
}

// handleColorSchemeChange reacts to a terminal light/dark report (a DEC mode
// 2031 event or an OSC 11 response). The polarity is always recorded so a
// later switch to the auto theme starts from the freshest value; the theme
// itself only changes while the auto theme is active.
func (m *appModel) handleColorSchemeChange(dark bool) (tea.Model, tea.Cmd) {
	styles.SetTerminalDark(dark)
	if !styles.AutoThemeEnabled() {
		return m, nil
	}
	resolved := styles.ResolveThemeRef(styles.AutoThemeRef)
	if current := styles.CurrentTheme(); current != nil && current.Ref == resolved {
		return m, nil
	}
	theme, err := styles.LoadTheme(resolved)
	if err != nil {
		slog.Warn("Failed to load auto theme for terminal background change", "theme", resolved, "error", err)
		return m, nil
	}
	styles.ApplyTheme(theme)
	return m, core.CmdHandler(messages.ThemeChangedMsg{})
}

// --- Miscellaneous ---

func (m *appModel) handleOpenURL(url string) (tea.Model, tea.Cmd) {
	if err := browser.Open(m.ctx(), url); err != nil {
		slog.Warn("Failed to open URL", "url", url, "error", err)
		return m, notification.ErrorCmd("Failed to open URL in browser")
	}
	return m, nil
}

func (m *appModel) handleAgentCommand(command string) (tea.Model, tea.Cmd) {
	ctx := m.ctx()

	// Inspect the command before resolving so we can detect /commands that
	// switch to a sub-agent. For those, we switch first and only then send
	// the resolved message — otherwise the message would be processed by
	// the previous agent.
	cmd, _, ok := m.application.LookupCommand(ctx, command)

	// URL commands open the configured URL in the browser instead of sending
	// a prompt to the agent.
	if ok && cmd.URL != "" {
		return m, core.CmdHandler(messages.OpenURLMsg{URL: m.expandURLPlaceholders(cmd.URL)})
	}

	resolved := m.application.ResolveCommand(ctx, command)

	var cmds []tea.Cmd
	switchSucceeded := true
	if ok && cmd.Agent != "" && cmd.Agent != m.sessionState.CurrentAgentName() {
		// Attempt to switch agents. If the switch fails, handleSwitchAgent
		// returns an error notification command. We check if the agent actually
		// changed to determine success, rather than relying on the command type.
		prevAgent := m.sessionState.CurrentAgentName()
		switched, switchCmd := m.handleSwitchAgent(cmd.Agent)
		var ok bool
		if m, ok = switched.(*appModel); !ok {
			// This should never happen, but if it does, log and continue with the original model
			slog.WarnContext(ctx, "handleSwitchAgent returned unexpected type", "type", fmt.Sprintf("%T", switched))
			switchSucceeded = false
		} else {
			// Check if the agent actually changed to determine if the switch succeeded.
			// If it failed, we must not send the message to the wrong agent.
			switchSucceeded = m.sessionState.CurrentAgentName() != prevAgent
		}
		if switchCmd != nil {
			cmds = append(cmds, switchCmd)
		}
	}

	if resolved != "" && switchSucceeded {
		cmds = append(cmds, core.CmdHandler(messages.SendMsg{Content: resolved, BypassQueue: true}))
	}

	return m, tea.Batch(cmds...)
}

// expandURLPlaceholders substitutes runtime placeholders in a command URL.
// Currently only {{session_id}} is supported. The token is intentionally
// distinct from the ${...} syntax used for config-time JS expansion, since
// the session ID is only known at dispatch time.
func (m *appModel) expandURLPlaceholders(url string) string {
	var sessionID string
	if m.application != nil {
		if sess := m.application.Session(); sess != nil {
			sessionID = sess.ID
		}
	}
	return expandSessionPlaceholder(url, sessionID)
}

// expandSessionPlaceholder replaces the {{session_id}} token with sessionID,
// URL-query-escaped so it can't break the URL or inject extra parameters.
func expandSessionPlaceholder(url, sessionID string) string {
	if !strings.Contains(url, "{{session_id}}") {
		return url
	}
	return strings.ReplaceAll(url, "{{session_id}}", neturl.QueryEscape(sessionID))
}

func (m *appModel) handleAttachFile(filePath string) (tea.Model, tea.Cmd) {
	if filePath != "" {
		if err := m.editor.AttachFile(filePath); err != nil {
			slog.Warn("failed to attach file", "path", filePath, "error", err)
			// Attachment failed — open the file picker with an error notification
			return m, tea.Batch(
				notification.ErrorCmd("Failed to attach "+filePath),
				core.CmdHandler(dialog.OpenDialogMsg{
					Model: dialog.NewFilePickerDialog(filePath),
				}),
			)
		}
		return m, notification.SuccessCmd("File attached: " + filePath)
	}

	// No path provided — open the file picker dialog
	return m, core.CmdHandler(dialog.OpenDialogMsg{
		Model: dialog.NewFilePickerDialog(filePath),
	})
}

// --- Speech-to-text ---

func (m *appModel) handleStartSpeak() (tea.Model, tea.Cmd) {
	if m.transcriber.IsRunning() {
		return m, nil
	}

	// Close any previous channel to unblock stale waitForTranscript goroutines.
	m.closeTranscriptCh()

	ch := make(chan string, 100)
	m.transcriptCh = ch
	err := m.transcriber.Start(m.ctx(), func(delta string) {
		select {
		case ch <- delta:
		default:
		}
	})
	if err != nil {
		m.closeTranscriptCh()
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to start listening: %v", err))
	}

	return m, tea.Batch(
		notification.InfoCmd("🎤 Listening... (ENTER to send or ESC to cancel)"),
		m.editor.SetRecording(true),
		m.waitForTranscript(),
	)
}

func (m *appModel) handleStopSpeak() (tea.Model, tea.Cmd) {
	if !m.transcriber.IsRunning() {
		return m, nil
	}

	m.transcriber.Stop()
	m.closeTranscriptCh()

	return m, tea.Batch(m.editor.SetRecording(false), notification.SuccessCmd("Stopped listening"))
}

// waitForTranscript returns a command that blocks until the next transcript
// delta arrives and delivers it as a SpeakTranscriptMsg.
func (m *appModel) waitForTranscript() tea.Cmd {
	ch := m.transcriptCh
	return func() tea.Msg {
		delta, ok := <-ch
		if !ok {
			return nil
		}
		return messages.SpeakTranscriptMsg{Delta: delta}
	}
}

// closeTranscriptCh closes the transcript channel and sets it to nil,
// unblocking any goroutines waiting in waitForTranscript.
func (m *appModel) closeTranscriptCh() {
	if m.transcriptCh != nil {
		close(m.transcriptCh)
		m.transcriptCh = nil
	}
}

func (m *appModel) handleInteractionResponse(msg messages.InteractionResponseMsg) (tea.Model, tea.Cmd) {
	response := msg.Response
	if msg.SessionID == "" || response.InteractionID == "" || m.application == nil || m.application.SessionRuntime() == nil {
		return m, notification.ErrorCmd("Failed to answer interaction: missing session correlation")
	}
	if response.Kind == runtime.InteractionElicitation && response.ElicitationID == "" {
		return m, notification.ErrorCmd("Failed to answer interaction: missing elicitation correlation")
	}
	handle, err := m.application.SessionRuntime().SessionByID(msg.SessionID)
	if err == nil {
		err = handle.Respond(m.ctx(), response)
	}
	if err != nil {
		return m, notification.ErrorCmd("Failed to answer interaction: " + err.Error())
	}
	return m, nil
}

func (m *appModel) startShell() (tea.Model, tea.Cmd) {
	cmd := shellpath.InteractiveShellCmd("Type 'exit' to return to " + m.appName)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Run the shell in the active session's working directory so it matches
	// where the tools operate (e.g. the worktree created by --worktree),
	// rather than inheriting the process CWD.
	if runner := m.supervisor.GetRunner(m.supervisor.ActiveID()); runner != nil && runner.WorkingDir != "" {
		cmd.Dir = runner.WorkingDir
	}
	return m, tea.ExecProcess(cmd, nil)
}

func (m *appModel) handleResumeSession(msg messages.ResumeSessionMsg) (tea.Model, tea.Cmd) {
	return m.handleRoutedResume(m.paneFocus(), msg)
}

func (m *appModel) handleRoutedResume(origin string, msg messages.ResumeSessionMsg) (tea.Model, tea.Cmd) {
	if m.supervisor == nil {
		return m, nil
	}
	runner := m.supervisor.GetRunner(origin)
	generation, exists := m.supervisor.RouteGeneration(origin)
	if !exists || runner == nil || runner.App == nil || msg.RouteGeneration == 0 || generation != msg.RouteGeneration {
		return m, nil
	}
	application := runner.App
	if application.Session() == nil || (msg.SessionID != "" && msg.SessionID != application.Session().ID) {
		return m, nil
	}
	if err := application.EditSession(m.ctx(), runtime.SessionEdit{Kind: runtime.SessionEditResume}); err != nil {
		return m, notification.ErrorCmd("Cannot resume session: " + err.Error())
	}
	return m, nil
}

// Legacy presentation-only embedders retain their local store workflow. Bound
// sessions never enter this path, even if a peer lacks branching capability.
func (m *appModel) handleLegacyBranchFromEdit(msg messages.BranchFromEditMsg) (tea.Model, tea.Cmd) {
	store := m.application.SessionStore()
	if store == nil {
		return m, notification.ErrorCmd("No session store configured")
	}
	if msg.ParentSessionID == "" {
		return m, notification.ErrorCmd("No parent session for branch")
	}
	ctx := m.ctx()

	parent, err := store.GetSession(ctx, msg.ParentSessionID)
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to load parent session: %v", err))
	}

	newSess, err := session.BranchSession(parent, msg.BranchAtPosition)
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to branch session: %v", err))
	}

	// Apply live-session state before the store write so mid-session
	// changes the store copy lacks (e.g. a mode downgrade) are persisted
	// with the branch, not just patched in memory.
	if current := m.application.Session(); current != nil {
		newSess.AgentName = current.AgentName
		newSess.HideToolResults = current.HideToolResults
		newSess.SetSafetyPolicy(current.GetSafetyPolicy())
		// SetSafetyPolicy clears the toggle memory; restore the live one
		// so a branch taken while escalated keeps its toggle-back
		// destination. newSess is not yet shared — direct write is safe.
		newSess.PriorSafetyPolicy = current.GetPriorSafetyPolicy()
	}

	if err := store.AddSession(ctx, newSess); err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to save branched session: %v", err))
	}

	cmd := m.beginHostedLoad(newSess.ID, m.paneFocus(), &messages.SendMsg{Content: msg.Content, Attachments: msg.Attachments})
	return m, cmd
}

func (m *appModel) handleLegacyForkSession() (tea.Model, tea.Cmd) {
	currentSession := m.application.Session()
	if currentSession == nil {
		return m, notification.ErrorCmd("No active session to fork")
	}

	store := m.application.SessionStore()
	if store == nil {
		return m, notification.ErrorCmd("No session store configured")
	}

	ctx := m.ctx()

	// Fork the session and clone all messages.
	forkedSession, err := session.BranchSession(currentSession, len(currentSession.Messages))
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to fork session: %v", err))
	}

	// BranchSession intentionally omits transient agent delegation. A private
	// user fork retains the selected canonical destination before persistence.
	forkedSession.AgentName = currentSession.AgentName
	if err := store.AddSession(ctx, forkedSession); err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Failed to save forked session: %v", err))
	}

	cmd := m.beginHostedLoad(forkedSession.ID, "", nil)
	return m, cmd
}
