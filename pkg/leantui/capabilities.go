package leantui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/xeipuuv/gojsonschema"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/evaluation"
	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// reportCapability preserves runtime capability errors instead of silently
// turning an unavailable operation into ordinary agent input.
func (m *model) reportCapability(value any, err error) {
	if err != nil {
		m.addNotice("⚠ ", err.Error(), ui.StWarning())
		return
	}
	if text, ok := value.(string); ok {
		m.addNotice("", text, ui.StMuted())
		return
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		m.addNotice("⚠ ", err.Error(), ui.StWarning())
		return
	}
	m.addNotice("", string(data), ui.StMuted())
	if result, ok := value.(plans.ListResult); ok && len(result.Plans) > 0 && m.screen.Editor.IsEmpty() {
		var choices []ui.Command
		for _, entry := range result.Plans {
			choices = append(choices, ui.Command{Name: entry.Name, Desc: entry.Title + " · " + entry.Status, Value: "view " + entry.Name})
		}
		m.completeArgument("plans", choices)
	}
}

func (m *model) handleCapabilityCommand(ctx context.Context, name, arg string, mode busySubmitMode) bool {
	if m.app != nil && m.app.Runtime() == nil {
		switch name {
		case "tools", "skills", "toolset-restart", "permissions":
			m.reportCapability("This runtime does not expose the requested host capability.", nil)
			return true
		}
	}
	if m.app != nil && m.app.IsReadOnly() {
		switch name {
		case "attach", "drop", "yolo", "title", "undo", "pause", "toolset-restart":
			m.reportCapability("This session is read-only.", nil)
			return true
		}
	}
	switch name {
	case "resume":
		if m.app.IsReadOnly() {
			m.reportCapability("This session is read-only.", nil)
		} else {
			m.reportCapability("Resume requested for this session.", m.app.EditSession(ctx, runtime.SessionEdit{Kind: runtime.SessionEditResume}))
		}
	case "speak":
		m.startSpeech(ctx)
	case "respond":
		m.respondInteraction(ctx, arg)
	case "plans":
		m.handlePlans(ctx, arg)
	case "settings":
		m.handleSettings(arg)
	case "fork":
		m.spawnViewer(ctx, "", true)
	case "attach":
		m.attachDraft(arg)
	case "drop":
		m.dropAttachment(ctx, arg)
	case "context":
		a := m.app
		m.capabilityJob(ctx, func(ctx context.Context) (any, error) { return a.ContextBreakdown(ctx) })
		m.reportCapability(m.draftAttachments, nil)
	case "cost":
		if m.budgetUsage != nil {
			m.reportCapability(m.budgetUsage, nil)
		}
		input, output, cost := m.app.Session().TokensAndCost()
		m.reportCapability(map[string]any{"input_tokens": input, "output_tokens": output, "cost": cost, "own_cost": m.app.Session().OwnCost(), "total_cost": m.app.Session().TotalCost(), "by_model": sessionModelCosts(m.app.Session())}, nil)
	case "eval":
		m.reportCapability(evaluation.Save(m.app.Session().Clone(), arg))
	case "copy-session":
		text := m.app.PlainTextTranscript()
		if m.term != nil {
			m.term.SetClipboard(text)
		}
		m.reportCapability("Conversation copied.", writeClipboard(text))
	case "pause":
		paused, err := m.app.TogglePause(ctx)
		m.reportCapability(fmt.Sprintf("Pause armed: %t", paused), err)
	case "permissions":
		if arg == "" {
			m.reportCapability(m.app.PermissionsInfo(), nil)
		} else {
			if m.app.IsReadOnly() {
				m.reportCapability("This session is read-only.", nil)
				break
			}
			payload, confirmed := strings.CutPrefix(arg, "confirm ")
			if !confirmed {
				m.reportCapability("Replace session permission rules: /permissions confirm <JSON object with allow/ask/deny arrays>. This changes which tools may run without asking.", nil)
				break
			}
			var permissions session.PermissionsConfig
			if err := json.Unmarshal([]byte(payload), &permissions); err != nil {
				m.reportCapability(nil, err)
			} else {
				m.reportCapability("Updated session permission rules.", m.app.EditSession(ctx, runtime.SessionEdit{Kind: runtime.SessionEditPermissions, Permissions: &permissions}))
			}
		}
	case "yolo":
		if arg != "confirm" && arg != "off" {
			m.reportCapability("Automatic approval can execute tools without asking. Use /yolo confirm to approve this change, or /yolo off to disable it.", nil)
			break
		}
		approved := arg == "confirm"
		m.reportCapability("Updated automatic approval.", m.app.EditSession(ctx, runtime.SessionEdit{Kind: runtime.SessionEditPolicy, ToolsApproved: &approved}))
	case "title":
		if arg == "" {
			m.reportCapability("Regenerating title.", m.app.RegenerateSessionTitle(ctx))
		} else {
			m.reportCapability("Updated title.", m.app.UpdateSessionTitle(ctx, arg))
		}
	case "export":
		a := m.app
		m.capabilityJob(ctx, func(ctx context.Context) (any, error) { return a.ExportHTML(ctx, arg) })
	case "tools":
		m.reportCapability(m.app.CurrentAgentToolsetStatuses(), nil)
		a := m.app
		m.capabilityJob(ctx, func(ctx context.Context) (any, error) { return a.CurrentAgentTools(ctx) })
	case "skills":
		a := m.app
		m.capabilityJob(ctx, func(ctx context.Context) (any, error) { return a.CurrentAgentSkillsContext(ctx) })
	case "toolset-restart":
		if arg == "" {
			var choices []ui.Command
			for _, status := range m.app.CurrentAgentToolsetStatuses() {
				if status.Restartable {
					choices = append(choices, ui.Command{Name: status.Name, Desc: status.State.String(), Value: status.Name})
				}
			}
			m.completeArgument("toolset-restart", choices)
		} else {
			a := m.app
			m.capabilityJob(ctx, func(ctx context.Context) (any, error) { return "Restart requested.", a.RestartToolset(ctx, arg) })
		}
	case "snapshots":
		if arg == "" {
			m.reportCapability(m.app.ListSnapshots(), nil)
			m.reportCapability("Reset to a checkpoint: /snapshots <index> confirm (0 restores the original workspace).", nil)
		} else {
			index, confirmation, _ := strings.Cut(arg, " ")
			keep, err := strconv.Atoi(index)
			switch {
			case err != nil || keep < 0 || confirmation != "confirm":
				m.reportCapability("Usage: /snapshots <nonnegative index> confirm. This restores workspace files.", nil)
			case m.busy() || m.app.IsReadOnly():
				m.reportCapability("Cannot restore files while the session is active or read-only.", nil)
			default:
				m.reportCapability(m.app.ResetSnapshot(ctx, keep))
			}
		}
	case "undo":
		if m.busy() {
			m.reportCapability("Wait for the current response to finish before restoring files.", nil)
			break
		}
		if arg != "confirm" {
			m.reportCapability("Restores workspace files from the latest snapshot. Use /undo confirm to proceed.", nil)
		} else {
			m.reportCapability(m.app.UndoLastSnapshot(ctx))
		}
	case "shell":
		if arg == "" {
			switch {
			case m.app.IsReadOnly():
				m.reportCapability("This session is read-only.", nil)
			case m.runExternal == nil:
				m.reportCapability("Interactive shell requires a terminal handoff.", nil)
			default:
				shell := os.Getenv("SHELL")
				if shell == "" {
					shell = "/bin/sh"
				}
				command := exec.CommandContext(ctx, shell)
				command.Dir = m.app.Session().WorkingDir
				m.reportCapability("Shell exited.", m.runExternal(command))
			}
		} else {
			m.runBangCommand(ctx, arg)
		}
	case "subagents", "subagent-view", "subagent-attach", "back":
		m.handleViewerCommand(ctx, name, arg)
	case "panes":
		m.reportCapability("Pane layouts require the full TUI and are unavailable in standalone lean. Use /subagents and /back to navigate session viewers.", nil)
	case "getting-started", "tour":
		m.reportCapability("The interactive visual tour requires the full TUI. /help documents lean workflows.", nil)
	default:
		for _, builtin := range builtinCommands() {
			if builtin.Name == name {
				return false
			}
		}
		if m.app == nil || m.app.Runtime() == nil {
			return false
		}
		prompt, ok := m.app.CurrentMCPPrompts(ctx)[name]
		if !ok {
			return false
		}
		args := make(map[string]string)
		if strings.HasPrefix(arg, "{") {
			if err := json.Unmarshal([]byte(arg), &args); err != nil {
				m.reportCapability(nil, err)
				return true
			}
		} else if arg != "" && len(prompt.Arguments) > 0 {
			args[prompt.Arguments[0].Name] = arg
		}
		for _, parameter := range prompt.Arguments {
			if parameter.Required && args[parameter.Name] == "" {
				m.reportCapability("Missing required prompt argument "+parameter.Name+". Supply the first argument as text, or all arguments as a JSON object.", nil)
				return true
			}
		}
		content, err := m.app.ExecuteMCPPrompt(ctx, name, args)
		if err != nil {
			m.reportCapability(nil, err)
		} else {
			m.dispatchUserMessage(ctx, "/"+name+" "+arg, content, mode)
		}
	}
	return true
}

func (m *model) completeArgument(command string, choices []ui.Command) {
	if len(choices) == 0 {
		m.reportCapability("No available choices for /"+command+".", nil)
		return
	}
	m.screen.Autocomplete.SetScopedCommands(command+" ", choices)
	m.screen.Editor.SetText("/" + command + " ")
	m.screen.Autocomplete.Sync(m.screen.Editor.Text())
}

func validatedAttachment(path, workingDir string) (messages.Attachment, error) {
	if path == "" {
		return messages.Attachment{}, errors.New("usage: /attach <path>")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workingDir, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return messages.Attachment{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return messages.Attachment{}, err
	}
	if !info.Mode().IsRegular() {
		return messages.Attachment{}, fmt.Errorf("attachment must be a regular file: %s", abs)
	}
	file, err := os.Open(abs)
	if err != nil {
		return messages.Attachment{}, err
	}
	_ = file.Close()
	return messages.Attachment{Name: filepath.Base(abs), FilePath: abs}, nil
}

func (m *model) attachDraft(path string) {
	attachment, err := validatedAttachment(path, m.app.Session().WorkingDir)
	if err != nil {
		m.reportCapability(nil, err)
		return
	}
	for _, existing := range m.draftAttachments {
		if existing.FilePath == attachment.FilePath {
			m.reportCapability("Already attached: "+attachment.Name, nil)
			return
		}
	}
	m.draftAttachments = append(m.draftAttachments, attachment)
	m.reportCapability("Attached to draft: "+attachment.Name, nil)
}

func (m *model) dropAttachment(ctx context.Context, path string) {
	if path == "" {
		var choices []ui.Command
		for _, attachment := range m.draftAttachments {
			choices = append(choices, ui.Command{Name: attachment.FilePath, Desc: "draft attachment", Value: attachment.FilePath})
		}
		for _, attached := range m.app.AttachedFiles() {
			choices = append(choices, ui.Command{Name: attached, Desc: "session attachment", Value: attached})
		}
		m.completeArgument("drop", choices)
		return
	}
	draftPath := path
	if !filepath.IsAbs(draftPath) {
		draftPath = filepath.Join(m.app.Session().WorkingDir, draftPath)
	}
	for i, attachment := range m.draftAttachments {
		if attachment.FilePath == draftPath {
			m.draftAttachments = append(m.draftAttachments[:i], m.draftAttachments[i+1:]...)
			m.reportCapability("Dropped draft attachment: "+path, nil)
			return
		}
	}
	m.reportCapability(m.app.DropAttachedFile(ctx, path))
}

func (m *model) respondInteraction(ctx context.Context, arg string) {
	id, answer, _ := strings.Cut(arg, " ")
	response := runtime.InteractionResponse{InteractionID: id}
	var sessionID string
	if event := m.elicitations[id]; event != nil {
		sessionID = event.SessionID
		response.Kind = runtime.InteractionElicitation
		response.ElicitationID = event.ElicitationID
		switch answer {
		case "cancel":
			response.Elicitation.Action = tools.ElicitationActionCancel
		case "decline":
			response.Elicitation.Action = tools.ElicitationActionDecline
		default:
			var content map[string]any
			if err := json.Unmarshal([]byte(answer), &content); err != nil {
				m.reportCapability(nil, err)
				return
			}
			if event.Schema != nil {
				result, err := gojsonschema.Validate(gojsonschema.NewGoLoader(event.Schema), gojsonschema.NewGoLoader(content))
				if err != nil {
					m.reportCapability(nil, err)
					return
				}
				if !result.Valid() {
					m.reportCapability(fmt.Sprint(result.Errors()), nil)
					return
				}
			}
			response.Elicitation = runtime.ElicitationResult{Action: tools.ElicitationActionAccept, Content: content}
		}
	} else if event := m.maxIterations[id]; event != nil {
		sessionID = event.SessionID
		response.Kind = runtime.InteractionMaxIterations
		switch answer {
		case "continue":
			response.Resume = runtime.ResumeApprove()
		case "cancel":
			response.Resume = runtime.ResumeReject("rejected by user")
		default:
			m.reportCapability("Use continue or cancel.", nil)
			return
		}
	} else {
		m.reportCapability("No outstanding interaction with that exact request ID.", nil)
		return
	}
	if sessionID == "" || id == "" || (response.Kind == runtime.InteractionElicitation && response.ElicitationID == "") {
		m.reportCapability("Cannot answer interaction: correlation is missing.", nil)
		return
	}
	handle, err := m.app.SessionRuntime().SessionByID(sessionID)
	if err == nil {
		err = handle.Respond(ctx, response)
	}
	if err != nil {
		m.reportCapability(nil, err)
		return
	}
	delete(m.elicitations, id)
	delete(m.maxIterations, id)
	m.reportCapability("Response sent.", nil)
}

type speechDelta struct {
	generation uint64
	text       string
}

func (m *model) startSpeech(ctx context.Context) {
	if m.transcriber == nil || !m.transcriber.IsSupported() {
		m.reportCapability("Speech-to-text is supported only by the macOS audio build.", nil)
		return
	}
	if m.transcriber.IsRunning() {
		return
	}
	if m.viewers == nil {
		m.reportCapability("Speech requires the running lean event loop.", nil)
		return
	}
	m.speechGeneration++
	generation, origin, host := m.speechGeneration, m.app, m.viewers
	err := m.transcriber.Start(ctx, func(delta string) {
		select {
		case host.events <- viewerEvent{origin: origin, event: speechDelta{generation: generation, text: delta}}:
		case <-host.ctx().Done():
		}
	})
	m.reportCapability("Listening; Enter stops and sends, Esc stops without sending.", err)
}

func (m *model) stopSpeech() {
	m.speechGeneration++
	if m.transcriber != nil && m.transcriber.IsRunning() {
		m.transcriber.Stop()
	}
}

func sessionModelCosts(sess *session.Session) map[string]float64 {
	costs := make(map[string]float64)
	var walk func(*session.Session)
	walk = func(current *session.Session) {
		for _, item := range current.Clone().Messages {
			if item.Message != nil {
				costs[item.Message.Message.Model] += item.Message.Message.Cost
			}
			if item.Cost != 0 {
				costs[item.Model] += item.Cost
			}
			if item.SubSession != nil {
				walk(item.SubSession)
			}
		}
	}
	walk(sess)
	return costs
}

// capabilityResult is presentation-only. Runtime work captures its App and
// session before leaving the event loop; a later focus change cannot retarget it.
type capabilityResult struct {
	identity  app.SessionEventMsg
	sessionID string
	value     any
	err       error
}

func (m *model) capabilityJob(ctx context.Context, work func(context.Context) (any, error)) {
	if m.viewers == nil {
		m.reportCapability(work(ctx))
		return
	}
	origin, sessionID, host := m.app, m.app.Session().ID, m.viewers
	identity := m.app.CurrentSessionEventIdentity()
	go func(origin *app.App) {
		value, err := work(ctx)
		select {
		case host.events <- viewerEvent{origin: origin, event: capabilityResult{identity: identity, sessionID: sessionID, value: value, err: err}}:
		case <-host.ctx().Done():
		}
	}(origin)
}
