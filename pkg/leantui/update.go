package leantui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/modelpicker"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/subagentindex"
	tuitypes "github.com/docker/docker-agent/pkg/tui/types"
)

func (m *model) handleKey(ctx context.Context, k ui.Key) {
	if k.Typ == ui.KeyBackgroundDark || k.Typ == ui.KeyBackgroundLight {
		m.handleBackgroundColor(k.Typ == ui.KeyBackgroundDark)
		return
	}
	if m.screen.Subagents != nil {
		m.handleSubagentPickerKey(ctx, k)
		return
	}
	if m.transcriber != nil && m.transcriber.IsRunning() && (k.Typ == ui.KeyEnter || k.Typ == ui.KeyEsc) {
		m.stopSpeech()
		if k.Typ == ui.KeyEsc {
			return
		}
	}
	if m.interruptPending {
		if k.Typ == ui.KeyRune && len(k.Runes) > 0 && (k.Runes[0] == 'y' || k.Runes[0] == 'Y') {
			m.interruptPending = false
			m.handleInterrupt()
			m.clearInterruptIntent()
		} else if k.Typ == ui.KeyEsc || k.Typ == ui.KeyCtrlC || (k.Typ == ui.KeyRune && len(k.Runes) > 0 && (k.Runes[0] == 'n' || k.Runes[0] == 'N')) {
			m.interruptPending = false
			m.clearInterruptIntent()
		}
		return
	}
	if m.screen.Confirm != nil {
		m.handleConfirmKey(ctx, k)
		return
	}

	switch k.Typ {
	case ui.KeyCtrlC:
		m.requestInterrupt()
	case ui.KeyCtrlD:
		if m.screen.Editor.IsEmpty() {
			m.quit()
		} else {
			m.screen.Editor.DeleteForward()
		}
	case ui.KeyEnter:
		m.handleEnter(ctx)
	case ui.KeyShiftEnter:
		m.screen.Editor.Insert([]rune{'\n'})
	case ui.KeyAltEnter:
		m.submitEditorMode(ctx, m.screen.Editor.Text(), busySubmitFollowUp)
	case ui.KeyAltUp:
		m.cancelPendingMessages(ctx)
	case ui.KeyTab:
		m.handleTab()
	case ui.KeyShiftTab:
		m.handleCycleThinkingLevel(ctx)
	case ui.KeyUp:
		if m.screen.Autocomplete.Active {
			m.screen.Autocomplete.MoveUp()
		} else if !m.screen.Editor.Up(m.width) {
			m.screen.Editor.HistoryPrev()
		}
	case ui.KeyDown:
		if m.screen.Autocomplete.Active {
			m.screen.Autocomplete.MoveDown()
		} else if !m.screen.Editor.Down(m.width) {
			m.screen.Editor.HistoryNext()
		}
	case ui.KeyLeft:
		m.screen.Editor.MoveLeft()
	case ui.KeyRight:
		m.screen.Editor.MoveRight()
	case ui.KeyWordLeft:
		m.screen.Editor.MoveWordLeft()
	case ui.KeyWordRight:
		m.screen.Editor.MoveWordRight()
	case ui.KeyHome:
		m.screen.Editor.MoveLineStart()
	case ui.KeyEnd:
		m.screen.Editor.MoveLineEnd()
	case ui.KeyBackspace:
		m.screen.Editor.Backspace()
	case ui.KeyDelete:
		m.screen.Editor.DeleteForward()
	case ui.KeyCtrlU:
		m.screen.Editor.DeleteToLineStart()
	case ui.KeyCtrlK:
		m.screen.Editor.DeleteToLineEnd()
	case ui.KeyCtrlW:
		m.screen.Editor.DeleteWordBack()
	case ui.KeyEsc:
		if m.busy() {
			m.requestInterrupt()
		} else {
			m.screen.Autocomplete.Dismiss()
			return // Keep the matching draft from immediately reopening completion.
		}
	case ui.KeyCtrlL:
		m.clearScreen()
	case ui.KeyRune, ui.KeyPaste:
		m.screen.Editor.Insert(k.Runes)
	}

	m.syncSubagentsCompletion()
	m.screen.Autocomplete.Sync(m.screen.Editor.Text())
}

func (m *model) cancelPendingMessages(ctx context.Context) bool {
	if m.app == nil || len(m.pendingUsers) == 0 {
		return false
	}

	cancelled := make([]string, 0, len(m.pendingUsers))
	remaining := make([]ui.PendingUserMessage, 0, len(m.pendingUsers))
	for _, pending := range m.pendingUsers {
		withdrawn, err := m.app.CancelPendingMessage(ctx, pending.TurnID)
		if err != nil || !withdrawn {
			remaining = append(remaining, pending)
			continue
		}
		cancelled = append(cancelled, pending.Display)
	}
	if len(cancelled) == 0 {
		return false
	}

	m.pendingUsers = remaining
	m.screen.Editor.SetText(strings.Join(cancelled, "\n"))
	m.screen.Autocomplete.Sync(m.screen.Editor.Text())
	return true
}

func (m *model) clearInterruptIntent() {
	m.interruptPending = false
	m.interruptCancel, m.interruptApp = nil, nil
	m.interruptIdentity = app.SessionEventMsg{}
	m.lastInterrupt = time.Time{}
}

func (m *model) captureInterruptIntent() {
	if m.app != nil {
		m.interruptCancel = m.app.CaptureCancelRun()
		m.interruptApp = m.app
		m.interruptIdentity = m.app.CurrentSessionEventIdentity()
	}
}

func (m *model) requestInterrupt() {
	if m.busy() {
		switch m.interruptMode {
		case "always":
			m.captureInterruptIntent()
			m.interruptPending = true
			m.reportCapability("Cancel this session's current response? [y] yes [n/Esc] keep running", nil)
			return
		case "double-tap":
			now := time.Now()
			if now.Sub(m.lastInterrupt) > time.Second {
				m.lastInterrupt = now
				m.captureInterruptIntent()
				m.reportCapability("Press interrupt again within one second to cancel.", nil)
				return
			}
		}
	}
	m.handleInterrupt()
}

func (m *model) handleInterrupt() {
	switch {
	case m.busy():
		outcome := runtime.CancelNotActive
		if m.interruptCancel != nil {
			if m.app == m.interruptApp && m.app.IsCurrentSessionEvent(m.interruptIdentity) {
				outcome = m.interruptCancel()
			}
		} else if m.app != nil {
			outcome = m.app.CancelRun()
		}
		m.clearInterruptIntent()
		if outcome == runtime.CancelNotActive {
			m.addNotice("⚠ ", "Could not cancel current response", ui.StWarning())
		}
		m.queue = nil
		m.pendingUsers = nil
		m.screen.Confirm = nil
		m.cancelMarkerPending = true
	case !m.screen.Editor.IsEmpty():
		m.screen.Editor.Reset()
		m.screen.Autocomplete.Dismiss()
	default:
		m.quit()
	}
}

func (m *model) handleEnter(ctx context.Context) {
	if m.screen.Autocomplete.Active {
		if cmd, ok := m.screen.Autocomplete.Current(); ok {
			completion := m.screen.Autocomplete.Completion(cmd)
			m.screen.Autocomplete.Dismiss()
			m.submitEditor(ctx, completion)
			return
		}
	}
	m.submitEditor(ctx, m.screen.Editor.Text())
}

func (m *model) handleTab() {
	if !m.screen.Autocomplete.Active {
		return
	}
	if cmd, ok := m.screen.Autocomplete.Current(); ok {
		m.screen.Editor.SetText(m.screen.Autocomplete.Completion(cmd) + " ")
		m.screen.Autocomplete.Sync(m.screen.Editor.Text())
	}
}

func (m *model) handleCycleThinkingLevel(ctx context.Context) {
	if !m.thinkingLevelChangeable() {
		return
	}
	level, err := m.app.CycleAgentThinkingLevel(ctx)
	if err != nil {
		m.reportThinkingLevelError("change", err)
		return
	}
	if m.status.ThinkingMode == "" {
		m.status.Thinking = level.String()
	}
}

// handleSetThinkingLevel applies the /effort command: it sets the current
// model's reasoning-effort level to the requested value.
func (m *model) handleSetThinkingLevel(ctx context.Context, level string) {
	if !m.thinkingLevelChangeable() {
		return
	}
	if level == "" {
		var choices []ui.Command
		for _, level := range m.app.CurrentAgentThinkingLevels(ctx) {
			choices = append(choices, ui.Command{Name: string(level), Value: string(level)})
		}
		m.completeArgument("effort", choices)
		return
	}
	parsed, ok := effort.Parse(level)
	if !ok {
		m.addNotice("✗ ", fmt.Sprintf("Unknown effort level %q (valid: none, minimal, low, medium, high, xhigh, max)", level), ui.StError())
		return
	}
	applied, err := m.app.SetAgentThinkingLevel(ctx, parsed)
	if err != nil {
		m.reportThinkingLevelError("set", err)
		return
	}
	if m.status.ThinkingMode == "" {
		m.status.Thinking = applied.String()
	}
	m.addNotice("", "Reasoning effort set to "+applied.String(), ui.StMuted())
}

// thinkingLevelChangeable reports whether the reasoning-effort level can be
// changed, emitting an explanatory notice when it cannot.
func (m *model) thinkingLevelChangeable() bool {
	if m.app == nil {
		return false
	}
	if !m.app.SupportsThinkingLevels() {
		m.addNotice("", "Current model does not support thinking levels", ui.StMuted())
		return false
	}
	return true
}

// reportThinkingLevelError emits a notice for a failed thinking-level change,
// distinguishing the unsupported-model case from other failures.
func (m *model) reportThinkingLevelError(action string, err error) {
	if errors.Is(err, runtime.ErrUnsupported) {
		m.addNotice("", "Current model does not support thinking levels", ui.StMuted())
		return
	}
	m.addNotice("✗ ", fmt.Sprintf("Failed to %s thinking level: %v", action, err), ui.StError())
}

type busySubmitMode int

const (
	busySubmitSteer busySubmitMode = iota
	busySubmitQueue
	busySubmitFollowUp
)

type submitOptions struct {
	fromEditor bool
	busyMode   busySubmitMode
}

func (m *model) submitEditor(ctx context.Context, text string) {
	mode := busySubmitSteer
	if m.queueSendMode {
		mode = busySubmitFollowUp
	}
	m.submitEditorMode(ctx, text, mode)
}

func (m *model) submitEditorMode(ctx context.Context, text string, mode busySubmitMode) {
	m.submit(ctx, text, submitOptions{fromEditor: true, busyMode: mode})
}

func (m *model) submitFollowUp(ctx context.Context, text string) {
	m.submit(ctx, text, submitOptions{busyMode: busySubmitQueue})
}

func (m *model) submit(ctx context.Context, text string, opts submitOptions) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" && len(m.draftAttachments) == 0 {
		return
	}
	if opts.fromEditor {
		if err := m.screen.Editor.RememberHistory(trimmed); err != nil {
			slog.WarnContext(ctx, "Failed to save command history", "error", err)
		}
		m.screen.Editor.Reset()
		m.screen.Autocomplete.Dismiss()
	}

	if command, ok := strings.CutPrefix(trimmed, "!"); ok {
		m.runBangCommand(ctx, command)
		return
	}

	if strings.HasPrefix(trimmed, "/") && m.handleSlash(ctx, trimmed, opts.busyMode) {
		return
	}

	m.dispatchUserMessage(ctx, trimmed, trimmed, opts.busyMode)
}

func (m *model) runBangCommand(ctx context.Context, command string) {
	if m.app.IsReadOnly() {
		m.addNotice("⚠ ", "This session is read-only.", ui.StWarning())
		return
	}
	m.app.RunBangCommand(ctx, command)
}

// handleSlash dispatches a slash command. It returns true when the command was
// fully handled (built-in, skill, or agent command) and false when the input
// should be treated as a normal message.
func (m *model) handleSlash(ctx context.Context, text string, mode busySubmitMode) bool {
	name, rest := splitCommand(text)
	if m.disabledCommands[name] {
		m.addNotice("⚠ ", "Command /"+name+" is disabled.", ui.StWarning())
		return true
	}
	if m.handleCapabilityCommand(ctx, name, rest, mode) {
		return true
	}
	switch name {
	case "exit", "quit", "q":
		m.quit()
		return true
	case "new":
		if m.app != nil && m.app.AttachedSubagent() != nil && m.spawnSession == nil {
			m.reportCapability("A host spawner is required to create an independent session from an attached viewer.", nil)
			return true
		}
		if rest != "" || m.spawnSession != nil {
			m.spawnViewer(ctx, rest, false)
			return true
		}
		m.app.NewSession()
		m.resetConversation()
		m.addNotice("", "Started a new session.", ui.StMuted())
		m.refreshCommands(ctx)
		return true
	case "clear":
		m.clearScreen()
		return true
	case "copy", "copy-last":
		m.copyLastResponse()
		return true
	case "help":
		m.commitHelp()
		return true
	case "sessions":
		m.handleSessionsCommand(ctx, rest)
		return true
	case "load":
		m.handleLoadSession(ctx, rest)
		return true
	case "delete":
		m.handleDeleteSession(ctx, rest)
		return true
	case "star":
		m.handleStarSession(ctx, rest)
		return true
	case "compact":
		m.addUserEcho(text)
		m.startCompact(ctx, rest)
		return true
	case "model":
		m.handleModelCommand(ctx, rest)
		return true
	case "effort":
		m.handleSetThinkingLevel(ctx, rest)
		return true
	}

	if skillName, task, ok, err := m.app.SkillCommandForkResult(ctx, text); err != nil {
		m.addNotice("⚠ ", "Could not resolve skill: "+err.Error(), ui.StWarning())
		return true
	} else if ok {
		m.addUserEcho(text)
		m.startSkillFork(ctx, skillName, task)
		return true
	}

	if _, _, ok := m.app.LookupCommand(ctx, text); ok {
		resolved, err := m.app.ResolveInputOnce(ctx, text)
		if err != nil {
			m.addNotice("⚠ ", "Could not resolve input: "+err.Error(), ui.StWarning())
			return true
		}
		m.dispatchUserMessage(ctx, text, resolved.Content, mode)
		return true
	}

	if resolved, err := m.app.ResolveSkillCommand(ctx, text); err == nil && resolved != "" {
		m.dispatchUserMessage(ctx, text, resolved, mode)
		return true
	}

	return false
}

func (m *model) copyLastResponse() {
	if m.app == nil || m.app.Session() == nil {
		m.addNotice("", "No active session.", ui.StMuted())
		return
	}
	lastResponse := m.app.Session().GetLastAssistantMessageContent()
	if lastResponse == "" {
		m.addNotice("", "No assistant response to copy.", ui.StMuted())
		return
	}
	if m.term != nil {
		m.term.SetClipboard(lastResponse)
	}
	_ = writeClipboard(lastResponse)
	m.addNotice("", "Last response copied to clipboard.", ui.StMuted())
}

var writeClipboard = clipboard.WriteAll

func (m *model) sessionCatalog() (runtime.SessionCatalog, bool) {
	catalog, ok := m.app.SessionRuntime().(runtime.SessionCatalog)
	if !ok {
		m.addNotice("", "Session browsing is not supported for this runtime", ui.StMuted())
	}
	return catalog, ok
}

func (m *model) handleSessionsCommand(ctx context.Context, sessionID string) {
	if sessionID != "" && m.selectRestoredViewer(ctx, sessionID) {
		return
	}
	if sessionID == "" && m.viewers != nil && len(m.viewers.cold) > 0 {
		for _, entry := range m.viewers.cold {
			m.reportCapability("/sessions "+entry.SessionID+" · "+entry.WorkingDir, nil)
		}
	}
	if m.busy() {
		m.addNotice("", "Wait for the current response to finish before switching sessions", ui.StMuted())
		return
	}
	if sessionID != "" {
		m.resumeSession(ctx, sessionID)
		return
	}
	catalog, ok := m.sessionCatalog()
	if !ok {
		return
	}
	summaries, err := catalog.ListSessions(ctx)
	if err != nil {
		m.addNotice("✗ ", "Failed to load sessions: "+err.Error(), ui.StError())
		return
	}

	currentDir := cleanDirectory(m.app.Session().WorkingDir)
	currentID := m.app.Session().ID
	cmds := make([]ui.Command, 0, len(summaries))
	for _, summary := range summaries {
		if !summary.Loadable || summary.SessionID == currentID || cleanDirectory(summary.WorkingDir) != currentDir {
			continue
		}
		title := strings.TrimSpace(summary.Title)
		if title == "" {
			title = "Untitled"
		}
		row := summary
		cmds = append(cmds, ui.Command{
			Name:  title,
			Desc:  fmt.Sprintf("%s · %d messages", row.CreatedAt.Local().Format("Jan 2 15:04"), row.NumMessages),
			Value: row.SessionID,
			MatchScore: func(query string) (int, bool) {
				query = strings.ToLower(strings.TrimSpace(query))
				if query == "" {
					return 0, true
				}
				if strings.Contains(strings.ToLower(title), query) || strings.Contains(strings.ReplaceAll(strings.ToLower(row.SessionID), "-", ""), strings.ReplaceAll(query, "-", "")) {
					return 1, true
				}
				return 0, false
			},
			Kind: ui.CmdBuiltin,
		})
	}
	if len(cmds) == 0 {
		m.addNotice("", "No previous sessions found in this directory", ui.StMuted())
		return
	}
	m.screen.Autocomplete.SetScopedCommands("sessions ", cmds)
	m.screen.Editor.SetText("/sessions ")
	m.screen.Autocomplete.Sync(m.screen.Editor.Text())
}

func cleanDirectory(dir string) string {
	if dir == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return filepath.Clean(dir)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return filepath.Clean(abs)
}

func (m *model) resumeSession(ctx context.Context, sessionID string) {
	if m.selectRestoredViewer(ctx, sessionID) {
		return
	}
	if m.sessionViews != nil && m.viewers != nil {
		m.acquireSessionView(ctx, sessionID)
		return
	}
	if m.busy() {
		m.reportCapability("Wait for the current response to finish before switching sessions", nil)
		return
	}
	catalog, ok := m.sessionCatalog()
	if !ok {
		return
	}
	rows, err := catalog.ListSessions(ctx)
	if err != nil {
		m.addNotice("✗ ", "Failed to load sessions: "+err.Error(), ui.StError())
		return
	}
	var selected *runtime.SessionCatalogEntry
	for i := range rows {
		if rows[i].SessionID == sessionID {
			selected = &rows[i]
			break
		}
	}
	if selected == nil || !selected.Loadable {
		m.addNotice("✗ ", "Session is not attachable", ui.StError())
		return
	}
	if cleanDirectory(selected.WorkingDir) != cleanDirectory(m.app.Session().WorkingDir) {
		m.addNotice("✗ ", "Session is not from the current directory", ui.StError())
		return
	}
	committed, err := runtime.RestoreSessionView(ctx, m.app.SessionRuntime(), sessionID)
	if err != nil {
		m.reportCapability("Failed to load session", err)
		return
	}
	sess := committed.Info.Session
	if cleanDirectory(sess.WorkingDir) != cleanDirectory(m.app.Session().WorkingDir) {
		m.addNotice("✗ ", "Session is not from the current directory", ui.StError())
		return
	}
	application, err := app.NewResolvedFromTemplate(ctx, m.app.SessionRuntime(), committed, m.app)
	if err != nil {
		m.reportCapability("Failed to attach session", err)
		return
	}
	previousID := m.app.Session().ID
	m.app.Close()
	m.app = application
	if m.viewers != nil {
		m.subscribeViewer(m.viewers.ctx(), application)
	}
	application.Start(ctx)

	if m.viewers != nil && m.viewers.store != nil {
		if err := m.viewers.store.ReplaceTab(ctx, previousID, sess.ID, sess.WorkingDir); err != nil {
			m.reportCapability(nil, err)
		}
		for i := range m.viewers.cold {
			if m.viewers.cold[i].SessionID == previousID {
				m.viewers.cold[i].SessionID = sess.ID
				m.viewers.cold[i].WorkingDir = sess.WorkingDir
			}
		}
	}
	m.resetConversation()
	m.screen.Transcript = ui.NewTranscript()
	m.sessionState = service.NewSessionState(sess)
	m.loadSessionTranscript(sess)
	if m.app.Runtime() != nil {
		m.refreshCommands(ctx)
	}
	title := strings.TrimSpace(sess.Title)
	if title == "" {
		title = sess.ID
	}
	m.addNotice("", "Resumed session: "+title, ui.StMuted())
}

func (m *model) loadInitialSessionTranscript() {
	if m.app == nil || m.app.Session() == nil || len(m.app.Session().OwnMessages()) == 0 {
		return
	}
	m.loadSessionTranscript(m.app.Session())
}

func (m *model) handleLoadSession(ctx context.Context, id string) {
	if id == "" {
		m.addNotice("", "Usage: /load <session-id>", ui.StMuted())
		return
	}
	m.resumeSession(ctx, id)
}

func (m *model) handleDeleteSession(ctx context.Context, id string) {
	if id == "" {
		m.addNotice("", "Usage: /delete <session-id>", ui.StMuted())
		return
	}
	if err := m.app.SessionRuntime().DeleteSession(ctx, id); err != nil {
		m.addNotice("✗ ", err.Error(), ui.StError())
		return
	}
	if m.viewers != nil && m.viewers.store != nil {
		if err := m.viewers.store.RemoveTab(ctx, id); err != nil {
			m.reportCapability(nil, err)
		}
		for i, entry := range m.viewers.cold {
			if entry.SessionID == id {
				m.viewers.cold = append(m.viewers.cold[:i], m.viewers.cold[i+1:]...)
				break
			}
		}
	}
	m.addNotice("", "Deleted session "+id, ui.StMuted())
}

func (m *model) handleStarSession(ctx context.Context, arg string) {
	id, value, _ := strings.Cut(arg, " ")
	if id == "" {
		id = m.app.Session().ID
	}
	loader, ok := m.app.SessionRuntime().(runtime.SessionLoader)
	if !ok {
		m.addNotice("", "Session starring is not supported", ui.StMuted())
		return
	}
	handle, _, err := loader.LoadSession(ctx, id)
	if err != nil {
		m.addNotice("✗ ", err.Error(), ui.StError())
		return
	}
	if !handle.Metadata().Capabilities.SessionEditing {
		m.addNotice("", "Session starring is not supported", ui.StMuted())
		return
	}
	starred := value != "off"
	if value == "" && m.app.Session() != nil && id == m.app.Session().ID {
		starred = !m.app.Session().Starred
	}
	if err := handle.SetStarred(ctx, starred); err != nil {
		m.addNotice("✗ ", err.Error(), ui.StError())
		return
	}
	if m.app.Session() != nil && id == m.app.Session().ID {
		m.app.Session().Starred = starred
	}
	m.addNotice("", "Updated session star", ui.StMuted())
}

func (m *model) loadSessionTranscript(sess *session.Session) {
	m.inputParentSessionID = sess.ParentID
	m.inputReferences = subagentindex.New()
	if snapshot := sess.GetSubagentTree(); snapshot != nil {
		m.inputReferences.Reset(*snapshot)
	}
	m.inputReplay.Reset(sess)
	storedMessages := sess.OwnMessages()
	toolResults := make(map[string]chat.Message)
	for _, msg := range storedMessages {
		if lifecycle.VisibleTranscriptMessage(&msg) && msg.Message.Role == chat.MessageRoleTool && msg.Message.ToolCallID != "" {
			toolResults[msg.Message.ToolCallID] = msg.Message
		}
	}

	for _, msg := range storedMessages {
		if msg.Pending && !msg.Implicit && lifecycle.IsUserInput(msg.InputOrigin) {
			kind := ui.PendingUserFollowUp
			if msg.InputMode == "steer" {
				kind = ui.PendingUserSteer
			}
			m.addPendingUser(msg.Message.Content, msg.Message.Content, msg.TurnID, kind)
		}
		if !lifecycle.VisibleTranscriptMessage(&msg) {
			continue
		}
		content := msg.Message.Content
		switch msg.Message.Role {
		case chat.MessageRoleUser:
			m.addInputEcho(msg.InputOrigin, msg.InputMode, msg.SenderName, msg.SenderID, content, msg.ReportOutcome)
		case chat.MessageRoleAssistant:
			if msg.Message.ReasoningContent != "" {
				reasoning := msg.Message.ReasoningContent
				m.screen.Transcript.AddBlock(func(w int) []string { return ui.RenderReasoningLines(reasoning, w) })
			}
			if content != "" {
				answer := content
				m.screen.Transcript.AddBlock(func(w int) []string { return ui.RenderAssistantLines(answer, w) })
			}
			for i, toolCall := range msg.Message.ToolCalls {
				toolDef := tools.Tool{}
				if i < len(msg.Message.ToolDefinitions) {
					toolDef = msg.Message.ToolDefinitions[i]
				}
				m.screen.Transcript.UpsertTool(msg.AgentName, toolCall, toolDef, tuitypes.ToolStatusCompleted)

				result := toolResults[toolCall.ID]
				toolResult := &tools.ToolCallResult{Output: result.Content, IsError: result.IsError}
				m.screen.Transcript.FinishTool(toolCall.ID, ui.ToolResult{
					Response:       result.Content,
					Result:         toolResult,
					AgentName:      msg.AgentName,
					ToolDefinition: toolDef,
				}, m.sessionState)
			}
		}
	}
}

func (m *model) handleModelCommand(ctx context.Context, modelRef string) {
	if m.app == nil || !m.app.SupportsModelSwitching() {
		m.addNotice("", "Model switching is not supported with this runtime", ui.StMuted())
		return
	}

	if modelRef == "" {
		models := m.app.AvailableModels(ctx)
		if len(models) == 0 {
			m.addNotice("", "No models available for selection", ui.StMuted())
			return
		}
		cmds := make([]ui.Command, 0, len(models))
		for _, choice := range models {
			label := choice.ModelName
			if label == "" {
				label = choice.Ref
			}
			desc := choice.Name
			if choice.ModelID != "" {
				desc = strings.TrimSpace(desc + " · " + choice.Provider + "/" + choice.ModelID)
			}
			if choice.IsCurrent {
				desc = strings.TrimSpace(desc + " (current)")
			} else if choice.IsDefault {
				desc = strings.TrimSpace(desc + " (default)")
			}
			value := choice.Ref
			if choice.IsDefault {
				value = "default"
			}
			cmds = append(cmds, ui.Command{
				Name:  label,
				Desc:  desc,
				Value: value,
				MatchScore: func(query string) (int, bool) {
					return modelpicker.Score(choice, query)
				},
				Kind: ui.CmdBuiltin,
			})
		}
		m.screen.Autocomplete.SetScopedCommands("model ", cmds)
		m.screen.Editor.SetText("/model ")
		m.screen.Autocomplete.Sync(m.screen.Editor.Text())
		return
	}

	if modelRef == "default" {
		modelRef = ""
	}
	if err := m.app.SetCurrentAgentModel(ctx, modelRef); err != nil {
		m.addNotice("✗ ", "Failed to change model: "+err.Error(), ui.StError())
		return
	}
	if m.app.Runtime() != nil {
		m.refreshCommands(ctx)
	}
	if modelRef == "" {
		m.addNotice("", "Model reset to default", ui.StMuted())
		return
	}
	m.addNotice("", "Model changed to "+modelRef, ui.StMuted())
}

func (m *model) dispatchUserMessage(ctx context.Context, display, content string, mode busySubmitMode) {
	for _, attachment := range m.draftAttachments {
		if _, err := validatedAttachment(attachment.FilePath, m.app.Session().WorkingDir); err != nil {
			m.screen.Editor.SetText(display)
			m.reportCapability(nil, err)
			return
		}
	}
	if m.app.IsReadOnly() {
		m.addUserEcho(display)
		m.addNotice("⚠ ", "This session is read-only.", ui.StWarning())
		m.screen.Editor.SetText(display)
		return
	}
	if m.busy() {
		switch mode {
		case busySubmitSteer:
			submission, err := m.app.SteerMessage(ctx, content, m.draftAttachments)
			if err != nil {
				m.addNotice("⚠ ", "Could not steer current response: "+err.Error(), ui.StWarning())
				m.screen.Editor.SetText(display)
				return
			}
			kind := ui.PendingUserSteer
			if submission.Disposition == runtime.SubmissionDispositionQueued {
				kind = ui.PendingUserFollowUp
			}
			m.draftAttachments = nil
			m.addPendingUser(display, content, submission.TurnID, kind)
			return
		case busySubmitFollowUp:
			submission, err := m.app.FollowUpMessage(ctx, content, m.draftAttachments)
			if err != nil {
				m.addNotice("⚠ ", "Could not enqueue follow-up: "+err.Error(), ui.StWarning())
				m.screen.Editor.SetText(display)
				return
			}
			m.draftAttachments = nil
			m.addPendingUser(display, content, submission.TurnID, ui.PendingUserFollowUp)
			return
		default:
			if len(m.draftAttachments) > 0 {
				m.dispatchUserMessage(ctx, display, content, busySubmitFollowUp)
				return
			}
			m.enqueueFollowUp(display, content)
			return
		}
	}
	submission, err := m.app.FollowUpMessage(ctx, content, m.draftAttachments)
	if err != nil {
		m.screen.Editor.SetText(display)
		m.reportCapability(nil, err)
		return
	}
	m.draftAttachments = nil
	m.addPendingUser(display, content, submission.TurnID, ui.PendingUserFollowUp)
}

func (m *model) enqueueFollowUp(display, content string) {
	msg := ui.PendingUserMessage{Display: display, Content: content, Kind: ui.PendingUserFollowUp}
	m.queue = append(m.queue, msg)
	m.pendingUsers = append(m.pendingUsers, msg)
}

func (m *model) sendFirstMessage(ctx context.Context, msg, attachPath string) {
	if attachPath != "" {
		attachment, err := validatedAttachment(attachPath, m.app.Session().WorkingDir)
		if err != nil {
			m.reportCapability(nil, err)
			return
		}
		m.draftAttachments = append(m.draftAttachments, attachment)
	}
	m.submit(ctx, msg, submitOptions{busyMode: busySubmitSteer})
}

func (m *model) startRun(ctx context.Context, message string, attachments []messages.Attachment) {
	submission, err := m.app.FollowUpMessage(ctx, message, attachments)
	if err != nil {
		m.screen.Editor.SetText(message)
		m.draftAttachments = attachments
		m.reportCapability(nil, err)
		return
	}
	m.addPendingUser(message, message, submission.TurnID, ui.PendingUserFollowUp)
}

func (m *model) startCompact(ctx context.Context, additionalPrompt string) {
	if m.app == nil {
		m.addNotice("", "Conversation compaction is not supported for this session", ui.StMuted())
		return
	}
	if err := m.app.CompactSession(ctx, additionalPrompt); err != nil {
		if errors.Is(err, runtime.ErrUnsupported) {
			m.addNotice("", "Conversation compaction is not supported for this session", ui.StMuted())
			return
		}
		m.addNotice("⚠ ", "Could not compact conversation: "+err.Error(), ui.StWarning())
	}
}

func (m *model) startSkillFork(ctx context.Context, skillName, task string) {
	if !m.app.SupportsForkSkills() {
		m.addNotice("", "Forked skills are not supported for this session", ui.StMuted())
		return
	}
	operationID := app.NewSkillOperationID()
	m.ownedSkillOperation = operationID
	err := m.app.StartSkillForkOperation(ctx, operationID, skillName, task)
	if err != nil {
		m.ownedSkillOperation = ""
		m.addNotice("⚠ ", "Could not start fork skill: "+err.Error(), ui.StWarning())
		return
	}
}

func (m *model) refreshCommands(ctx context.Context) {
	cmds := make([]ui.Command, 0)
	for _, cmd := range builtinCommands() {
		if !m.disabledCommands[cmd.Name] {
			cmds = append(cmds, cmd)
		}
	}
	if m.app == nil || m.app.Runtime() == nil {
		m.screen.Autocomplete.SetCommands(cmds)
		return
	}
	for name, c := range m.app.CurrentAgentCommands(ctx) {
		if m.disabledCommands[name] {
			continue
		}
		cmds = append(cmds, ui.Command{Name: name, Desc: c.DisplayText(), Kind: ui.CmdAgent})
	}
	sk, err := m.app.CurrentAgentSkillsContext(ctx)
	if err != nil {
		m.addNotice("⚠ ", "Could not discover skills: "+err.Error(), ui.StWarning())
	}
	for _, sk := range sk {
		if m.disabledCommands[sk.Name] {
			continue
		}
		cmds = append(cmds, ui.Command{Name: sk.Name, Desc: sk.Description, Kind: ui.CmdAgent})
	}
	for name, prompt := range m.app.CurrentMCPPrompts(ctx) {
		if !m.disabledCommands[name] {
			cmds = append(cmds, ui.Command{Name: name, Desc: prompt.Description, Kind: ui.CmdAgent})
		}
	}
	m.screen.Autocomplete.SetCommands(cmds)
}

func (m *model) handleConfirmKey(ctx context.Context, k ui.Key) {
	confirm := m.screen.Confirm
	if confirm == nil {
		return
	}
	if confirm.Rejecting {
		switch k.Typ {
		case ui.KeyEsc:
			confirm.Rejecting = false
			confirm.RejectReason = ""
		case ui.KeyEnter:
			confirm.Rejecting = false
			m.resolveConfirm(ctx, runtime.ResumeReject(confirm.RejectReason))
			confirm.RejectReason = ""
		case ui.KeyBackspace:
			runes := []rune(confirm.RejectReason)
			if len(runes) > 0 {
				confirm.RejectReason = string(runes[:len(runes)-1])
			}
		case ui.KeyRune, ui.KeyPaste:
			confirm.RejectReason += string(k.Runes)
		}
		return
	}
	if k.Typ == ui.KeyEsc {
		m.resolveConfirm(ctx, runtime.ResumeReject("rejected by user"))
		return
	}
	if k.Typ != ui.KeyRune || len(k.Runes) == 0 {
		return
	}
	switch k.Runes[0] {
	case 'y', 'Y':
		m.resolveConfirm(ctx, runtime.ResumeApprove())
	case 'a', 'A':
		m.resolveConfirm(ctx, runtime.ResumeApproveTool(m.screen.Confirm.Tool))
	case 'b', 'B':
		m.resolveConfirm(ctx, runtime.ResumeApproveBalanced())
	case 's', 'S':
		m.resolveConfirm(ctx, runtime.ResumeApproveAutonomous())
	case 'r', 'R':
		confirm.Rejecting = true
	case 'n', 'N':
		m.resolveConfirm(ctx, runtime.ResumeReject("rejected by user"))
	}
}

func (m *model) handleInteractionResponse(ctx context.Context, msg messages.InteractionResponseMsg) {
	response := msg.Response
	if msg.SessionID == "" || response.InteractionID == "" || m.app.SessionRuntime() == nil {
		m.addNotice("⚠ ", "Cannot answer interaction: session correlation is missing", ui.StWarning())
		return
	}
	if response.Kind == runtime.InteractionElicitation && response.ElicitationID == "" {
		m.addNotice("⚠ ", "Cannot answer interaction: elicitation correlation is missing", ui.StWarning())
		return
	}
	handle, err := m.app.SessionRuntime().SessionByID(msg.SessionID)
	if err == nil {
		err = handle.Respond(ctx, response)
	}
	if err != nil {
		m.addNotice("⚠ ", "Could not answer interaction: "+err.Error(), ui.StWarning())
	}
}

func (m *model) resolveConfirm(ctx context.Context, req runtime.ResumeRequest) {
	confirm := m.screen.Confirm
	m.screen.Confirm = nil
	if confirm == nil {
		return
	}
	m.handleInteractionResponse(ctx, messages.InteractionResponseMsg{
		SessionID: confirm.SessionID,
		Response: runtime.InteractionResponse{
			InteractionID: confirm.RequestID,
			Kind:          runtime.InteractionConfirmation,
			Resume:        req,
		},
	})
}

func (m *model) resetConversation() {
	m.clearInterruptIntent()
	m.cancelViewAcquisition()
	m.status.Dormant, m.status.Pending = false, 0
	m.lifecycle = lifecycle.State{}
	m.elicitations, m.maxIterations = nil, nil
	m.subagentSnapshot = nil
	m.screen.Subagents = nil
	m.majorHighWater = [2]uint64{}
	m.majorNoticeVisible = false
	m.screen.Transcript.ClearActive()
	m.queue = nil
	m.pendingUsers = nil
	m.draftAttachments = nil
	m.inputReplay.Reset(nil)
	m.cancelMarkerPending = false
	m.screen.Confirm = nil
	m.usage.Reset()
	m.status.ContextLength = 0
	m.status.ContextLimit = 0
	m.status.CompactionThreshold = 0
	m.status.Compacting = false
	m.status.Tokens = 0
	m.status.Cost = 0
	m.status.CostKnown = false
}

func (m *model) clearScreen() {
	m.r.Repaint()
}

func (m *model) quit() {
	m.quitting = true
}

func (m *model) addUserEcho(text string) {
	m.screen.Transcript.AddBlock(func(w int) []string { return ui.RenderUserLines(text, w) })
}

func (m *model) addPendingUser(display, content, turnID string, kind ui.PendingUserKind) {
	if turnID != "" {
		for i := range m.pendingUsers {
			if m.pendingUsers[i].TurnID == turnID {
				return
			}
		}
	}
	m.pendingUsers = append(m.pendingUsers, ui.PendingUserMessage{Display: display, Content: content, TurnID: turnID, Kind: kind})
}

func (m *model) consumePendingUser(kind ui.PendingUserKind, turnID string) (ui.PendingUserMessage, bool) {
	for i, msg := range m.pendingUsers {
		if msg.Kind != kind || turnID == "" || msg.TurnID != turnID {
			continue
		}
		m.pendingUsers = append(m.pendingUsers[:i], m.pendingUsers[i+1:]...)
		return msg, true
	}
	return ui.PendingUserMessage{}, false
}

func (m *model) addInputEcho(origin session.InputOrigin, mode, senderName, senderID, content string, outcome ...session.ReportOutcome) {
	input := session.UserMessage(content)
	if len(outcome) > 0 {
		input.ReportOutcome = outcome[0]
	}
	input.InputOrigin, input.InputMode = origin, mode
	input.SenderName, input.SenderID = senderName, senderID
	msg := tuitypes.Input(input)
	if m.inputReferences == nil {
		m.inputReferences = subagentindex.New()
	}
	references, parentID := m.inputReferences, m.inputParentSessionID
	m.screen.Transcript.AddInputBlock(func(w int) []string {
		msg.InputReference = references.Resolve(parentID, msg.SenderID, msg.SenderName)
		return ui.RenderInputLines(msg, w)
	})
}

func (m *model) addNotice(prefix, text string, style lipgloss.Style) {
	m.screen.Transcript.AddBlock(func(w int) []string { return ui.RenderNoticeLines(prefix, text, w, style) })
}

func (m *model) commitHelp() {
	m.screen.Transcript.AddBlock(func(width int) []string {
		var lines []string
		heading := func(text string) {
			if len(lines) > 0 {
				lines = append(lines, "")
			}
			lines = append(lines, ui.WrapANSI(ui.StBold().Render(text), width)...)
		}
		entry := func(text string) {
			lines = append(lines, ui.WrapANSI(ui.StMuted().Render("  "+text), width)...)
		}

		heading("Lean terminal help")
		entry("Independent lean app only; not the full TUI's shortcuts.")
		entry("/help: show this help. Type / for the available command list.")
		entry("The list includes enabled built-ins, agent commands, skills, and MCP prompts.")
		for _, command := range builtinCommands() {
			if !m.disabledCommands[command.Name] {
				entry("/" + command.Name + ": " + command.Desc)
			}
		}
		entry("Disabled commands are rejected, including direct slash input.")
		entry("/subagents opens the tree; /subagents on|off saves Use subagents (default ON).")
		entry("Tree: Up/Down select, Left/Right collapse/expand; Tab selects actions; u toggles Use subagents.")
		entry("Enter opens the selected live viewer without sending; Esc closes the tree without cancelling work.")
		entry("Sending targets the visible viewer. /back restores its predecessor without cancelling execution.")
		entry("Restored · paused sessions wait for /resume; related sessions require their own explicit Resume.")
		entry("/attach <path> adds a draft attachment; /drop lists draft and session files.")
		entry("/clear only redraws here; in the full TUI it starts a new session.")

		heading("Composer and sending")
		entry("Enter: send input or draft attachments; while busy, steer (may queue), or queue when send-mode is queue.")
		entry("LF / Ctrl+J and CR / Ctrl+M decode as Enter here, not newline or model selection.")
		entry("Shift+Enter: insert newline (requires terminal keyboard support).")
		entry("Alt+Enter: send; while busy, enqueue an end-of-turn follow-up.")
		entry("Typing / bracketed paste: insert text; pasted CR characters are stripped.")
		entry("! prefix: run a shell command instead of sending (not in read-only sessions).")

		heading("Cursor and history")
		entry("Left / Right: move one character.")
		entry("Alt+B / Alt+F: move one word backward / forward.")
		entry("Ctrl+Left/Right, Alt+Left/Right, Shift+Left/Right: move by word, not select.")
		entry("Home / Ctrl+A: line start. End / Ctrl+E: line end.")
		entry("Up / Down: move visual row; at first / last row, previous / next history.")
		entry("Down past newest history restores the draft. Completion overrides Up / Down.")
		entry("No app-level selection, select-all, or copy-selection bindings.")

		heading("Editing")
		entry("Backspace / Ctrl+H: delete previous character. Delete: delete next character.")
		entry("Ctrl+D: delete next character with a nonempty draft; quit if empty.")
		entry("Ctrl+U / Ctrl+K: delete to line start / end.")
		entry("Ctrl+W / Alt+Backspace: delete previous word.")

		heading("Command and argument completion")
		entry("Type / at the start of a single-line draft (no spaces) to find commands.")
		entry("Up / Down: select previous / next match (stops at the ends).")
		entry("Enter: execute selected completion. Tab: accept it and add a space, without sending.")
		entry("Esc: dismiss completion when idle; typing can reopen it.")
		entry("Tab does nothing without active completion; scoped arguments use the same controls.")

		heading("Response and application controls")
		entry("Alt+Up: withdraw all eligible pending messages and restore their displays as one draft.")
		entry("This replaces the draft; unavailable or already-consumed pending messages stay unchanged.")
		entry("Ctrl+C: interrupt while busy; otherwise clear a nonempty draft; otherwise quit.")
		entry("/settings interrupt-confirmation selects always (Y/N), double-tap (one second), or none.")
		entry("Esc: invoke that same interrupt when busy; follows interrupt-confirmation.")
		entry("Shift+Tab: cycle thinking effort when the current model/runtime supports it.")
		entry("Ctrl+L: clear/redraw the screen, without resetting the conversation.")

		heading("Tool confirmation (when the subagent picker is closed)")
		entry("Y / y: approve once. A / a: always approve this tool.")
		entry("B / b: auto-approve safe tools (balanced). S / s: approve autonomously for the session.")
		entry("N / n / Esc: reject. R / r: reject with a reason; Enter submits it, Esc goes back.")
		entry("Elicitation/OAuth: /respond <request-id> name=value …|<answer>|decline|cancel; fields are typed and validated against the schema.")
		entry("Maximum iterations: /respond <request-id> continue|cancel. Responses keep exact session correlation.")
		entry("Other keys, including Enter, Ctrl+C and Ctrl+D, are ignored at the approval prompt.")

		heading("Terminal key aliases")
		entry("Alt includes Option when the terminal sends Alt sequences.")
		entry("Supported CSI / SS3 cursor and Home/End encodings share the actions above.")
		entry("Supported Kitty encodings share these actions; unsupported controls/Alt keys are ignored.")
		return lines
	})
}

func splitCommand(text string) (name, rest string) {
	text = strings.TrimPrefix(strings.TrimSpace(text), "/")
	name, rest, _ = strings.Cut(text, " ")
	return name, strings.TrimSpace(rest)
}
