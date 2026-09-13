package leantui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

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
	if m.screen.Confirm != nil {
		m.handleConfirmKey(ctx, k)
		return
	}

	switch k.Typ {
	case ui.KeyCtrlC:
		m.handleInterrupt()
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
		if m.busy || m.runCancel != nil {
			m.handleInterrupt()
		} else {
			m.screen.Autocomplete.Dismiss()
		}
	case ui.KeyCtrlL:
		m.clearScreen()
	case ui.KeyRune, ui.KeyPaste:
		m.screen.Editor.Insert(k.Runes)
	}

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

func (m *model) handleInterrupt() {
	switch {
	case m.busy:
		outcome := runtime.CancelNotActive
		if m.app != nil {
			outcome = m.app.CancelRun()
		}
		if outcome == runtime.CancelNotActive && m.runCancel == nil {
			m.addNotice("⚠ ", "Could not cancel current response", ui.StWarning())
		}
		if m.runCancel != nil {
			m.runCancel()
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
	m.status.Thinking = level.String()
}

// handleSetThinkingLevel applies the /effort command: it sets the current
// model's reasoning-effort level to the requested value.
func (m *model) handleSetThinkingLevel(ctx context.Context, level string) {
	if !m.thinkingLevelChangeable() {
		return
	}
	if level == "" {
		m.addNotice("", "Usage: /effort <none|minimal|low|medium|high|xhigh|max>", ui.StMuted())
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
	m.status.Thinking = applied.String()
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
	m.submitEditorMode(ctx, text, busySubmitSteer)
}

func (m *model) submitEditorMode(ctx context.Context, text string, mode busySubmitMode) {
	m.submit(ctx, text, submitOptions{fromEditor: true, busyMode: mode})
}

func (m *model) submitFollowUp(ctx context.Context, text string) {
	m.submit(ctx, text, submitOptions{busyMode: busySubmitQueue})
}

func (m *model) submit(ctx context.Context, text string, opts submitOptions) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
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
	switch name {
	case "exit", "quit":
		m.quit()
		return true
	case "new":
		m.app.NewSession()
		m.resetConversation()
		m.addNotice("", "Started a new session.", ui.StMuted())
		m.refreshCommands(ctx)
		return true
	case "clear":
		m.clearScreen()
		return true
	case "copy":
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
	if m.busy {
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
	loader, ok := m.app.SessionRuntime().(runtime.SessionLoader)
	if !ok {
		m.addNotice("✗ ", "Session loading is not supported", ui.StError())
		return
	}
	_, sess, err := loader.LoadSession(ctx, sessionID)
	if err != nil {
		m.addNotice("✗ ", "Failed to load session: "+err.Error(), ui.StError())
		return
	}
	if cleanDirectory(sess.WorkingDir) != cleanDirectory(m.app.Session().WorkingDir) {
		m.addNotice("✗ ", "Session is not from the current directory", ui.StError())
		return
	}
	m.app.ReplaceSession(ctx, sess)
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
			m.addInputEcho(msg.InputOrigin, msg.InputMode, msg.SenderName, msg.SenderID, content)
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
			desc := choice.Name
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
				Name:  choice.Ref,
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
	if m.app.IsReadOnly() {
		m.addUserEcho(display)
		m.addNotice("⚠ ", "This session is read-only.", ui.StWarning())
		return
	}
	if m.busy {
		switch mode {
		case busySubmitSteer:
			submission, err := m.app.SteerMessage(ctx, content, nil)
			if err != nil {
				m.addNotice("⚠ ", "Could not steer current response: "+err.Error(), ui.StWarning())
				return
			}
			kind := ui.PendingUserSteer
			if submission.Disposition == runtime.SubmissionDispositionQueued {
				kind = ui.PendingUserFollowUp
			}
			m.addPendingUser(display, content, submission.TurnID, kind)
			return
		case busySubmitFollowUp:
			submission, err := m.app.FollowUpMessage(ctx, content, nil)
			if err != nil {
				m.addNotice("⚠ ", "Could not enqueue follow-up: "+err.Error(), ui.StWarning())
				return
			}
			m.addPendingUser(display, content, submission.TurnID, ui.PendingUserFollowUp)
			return
		default:
			m.enqueueFollowUp(display, content)
			return
		}
	}
	m.startRun(ctx, content, nil)
}

func (m *model) enqueueFollowUp(display, content string) {
	msg := ui.PendingUserMessage{Display: display, Content: content, Kind: ui.PendingUserFollowUp}
	m.queue = append(m.queue, msg)
	m.pendingUsers = append(m.pendingUsers, msg)
}

func (m *model) sendFirstMessage(ctx context.Context, msg, attachPath string) {
	var atts []messages.Attachment
	if attachPath != "" {
		if abs, err := filepath.Abs(attachPath); err == nil {
			atts = append(atts, messages.Attachment{Name: filepath.Base(abs), FilePath: abs})
		}
	}

	trimmed := strings.TrimSpace(msg)
	if command, ok := strings.CutPrefix(trimmed, "!"); ok {
		m.runBangCommand(ctx, command)
		return
	}

	content := msg
	if strings.HasPrefix(trimmed, "/") {
		if resolved, err := m.app.ResolveInputOnce(ctx, trimmed); err != nil {
			m.addNotice("⚠ ", "Could not resolve input: "+err.Error(), ui.StWarning())
			return
		} else if resolved.Content != "" {
			content = resolved.Content
		}
	}

	switch {
	case trimmed != "":
	case len(atts) > 0:
		m.addNotice("", "(attached "+atts[0].Name+")", ui.StMuted())
	default:
		return
	}
	m.startRun(ctx, content, atts)
}

// beginRun marks the model busy and returns a cancelable context for a new
// run, storing its cancel func so it can be interrupted.
func (m *model) beginRun(ctx context.Context) (context.Context, context.CancelFunc) {
	runCtx, cancel := context.WithCancel(ctx)
	m.runCancel = cancel
	m.busy = true
	m.cancelMarkerPending = false
	return runCtx, cancel
}

func (m *model) startRun(ctx context.Context, message string, attachments []messages.Attachment) {
	runCtx, cancel := m.beginRun(ctx)
	m.app.Run(runCtx, cancel, message, attachments)
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
	_, cancel := m.beginRun(ctx)
	_ = cancel
	operationID := app.NewSkillOperationID()
	m.ownedSkillOperation = operationID
	err := m.app.StartSkillForkOperation(ctx, operationID, skillName, task)
	if err != nil {
		m.ownedSkillOperation = ""
		m.addNotice("⚠ ", "Could not start fork skill: "+err.Error(), ui.StWarning())
		m.finishBusy(ctx)
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
		cmds = append(cmds, ui.Command{Name: sk.Name, Desc: sk.Description, Kind: ui.CmdAgent})
	}
	m.screen.Autocomplete.SetCommands(cmds)
}

func (m *model) handleConfirmKey(ctx context.Context, k ui.Key) {
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
	if m.runCancel != nil {
		m.runCancel()
		m.runCancel = nil
	}
	m.screen.Transcript.ClearActive()
	m.queue = nil
	m.pendingUsers = nil
	m.inputReplay.Reset(nil)
	m.busy = false
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
	if m.runCancel != nil {
		m.runCancel()
	}
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

func (m *model) addInputEcho(origin session.InputOrigin, mode, senderName, senderID, content string) {
	input := session.UserMessage(content)
	input.InputOrigin, input.InputMode = origin, mode
	input.SenderName, input.SenderID = senderName, senderID
	msg := tuitypes.Input(input)
	m.screen.Transcript.AddInputBlock(func(w int) []string {
		msg.InputReference = m.inputReferences.Resolve(m.inputParentSessionID, msg.SenderID, msg.SenderName)
		return ui.RenderInputLines(msg, w)
	})
}

func (m *model) addNotice(prefix, text string, style lipgloss.Style) {
	m.screen.Transcript.AddBlock(func(w int) []string { return ui.RenderNoticeLines(prefix, text, w, style) })
}

func (m *model) commitHelp() {
	m.screen.Transcript.AddBlock(func(int) []string {
		return []string{
			ui.StBold().Render("Commands"),
			ui.StMuted().Render("  /new       start a new session"),
			ui.StMuted().Render("  /sessions  resume a session from this directory"),
			ui.StMuted().Render("  /compact   summarize and compact the conversation"),
			ui.StMuted().Render("  /model     change the model for the current agent"),
			ui.StMuted().Render("  /effort    set the model's reasoning effort (e.g. /effort high)"),
			ui.StMuted().Render("  /copy      copy the last assistant response"),
			ui.StMuted().Render("  /clear     clear the screen"),
			ui.StMuted().Render("  /help      show this help"),
			ui.StMuted().Render("  /exit      quit"),
			"",
			ui.StBold().Render("Shortcuts"),
			ui.StMuted().Render("  Enter      send             Shift+Enter insert newline"),
			ui.StMuted().Render("  Alt+Enter  follow up        Up/Down     history"),
			ui.StMuted().Render("  Tab        complete command Shift+Tab   cycle thinking"),
			ui.StMuted().Render("  Option+Up  edit all pending messages"),
			ui.StMuted().Render("  Esc        interrupt         Ctrl+C     cancel / quit"),
			ui.StMuted().Render("  Ctrl+W     delete previous word"),
		}
	})
}

func splitCommand(text string) (name, rest string) {
	text = strings.TrimPrefix(strings.TrimSpace(text), "/")
	name, rest, _ = strings.Cut(text, " ")
	return name, strings.TrimSpace(rest)
}
