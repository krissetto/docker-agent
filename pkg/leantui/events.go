package leantui

import (
	"context"
	"slices"
	"time"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/subagentindex"
	tuitypes "github.com/docker/docker-agent/pkg/tui/types"
)

// handleEvent applies a single runtime event emitted by the App to the model,
// updating the conversation, tool state, status footer, or busy state.
func (m *model) handleEvent(ctx context.Context, ev any) {
	seed := false
	shared := false
	if bridged, ok := ev.(app.SessionEventMsg); ok {
		seed = bridged.Seed
		ev = bridged.Event
		if bridged.Projection != nil {
			m.lifecycle = bridged.Projection.Lifecycle
			shared = true
			if confirm := m.screen.Confirm; confirm != nil && !bridged.Projection.HasInteraction(app.InteractionKey{SessionID: confirm.SessionID, InteractionID: confirm.RequestID}) {
				m.screen.Confirm = nil
			}
		}
	}
	switch e := ev.(type) {
	case *runtime.SubagentTreeEvent:
		if m.inputReferences == nil {
			m.inputReferences = subagentindex.New()
		}
		m.inputReferences.Reset(e.Snapshot)
		m.screen.Transcript.InvalidateInputBlocks()
	case *app.SessionViewEvent:
		if m.sessionState != nil {
			m.sessionState.SetYoloMode(e.Session.IsToolsApproved())
			m.sessionState.SetSessionTitle(e.Session.TitleSnapshot())
		}
	case *app.SessionResetEvent:
		m.lifecycle = lifecycle.FromSnapshot(e.Snapshot)
		m.screen.Transcript.Clear()
		m.inputReplay.Reset(e.Snapshot.Session)
		m.pendingUsers = nil
		m.queue = nil
		if e.Snapshot.Session != nil {
			m.loadSessionTranscript(e.Snapshot.Session)
		}
		m.pendingUsers = nil
		for _, input := range e.Snapshot.PendingInputs {
			if !lifecycle.IsUserInput(input.InputOrigin) {
				continue
			}
			kind := ui.PendingUserFollowUp
			if input.InputMode == "steer" {
				kind = ui.PendingUserSteer
			}
			m.addPendingUser(input.Content, input.Content, input.TurnID, kind)
		}
		m.busy = e.Snapshot.Status.State == runtime.SessionStateRunning || e.Snapshot.Status.State == runtime.SessionStateQueued || e.Snapshot.Status.State == runtime.SessionStateCancelling
		if m.sessionState != nil {
			pause := service.PauseNone
			if e.Snapshot.Status.PauseArmed {
				pause = service.PausePausing
			}
			if e.Snapshot.Status.Paused {
				pause = service.PausePaused
			}
			m.sessionState.SetPauseState(pause)
		}
		for _, interaction := range e.Snapshot.Interactions {
			if confirmation, ok := interaction.Event.(*runtime.ToolCallConfirmationEvent); ok {
				if current := m.screen.Confirm; current == nil || current.SessionID != confirmation.SessionID || current.RequestID != confirmation.RequestID {
					m.handleEvent(ctx, confirmation)
				}
			}
		}
	case *runtime.InteractionResolvedEvent:
		if current := m.screen.Confirm; current != nil && current.SessionID == e.SessionID && current.RequestID == e.InteractionID {
			m.screen.Confirm = nil
		}

	case msgtypes.SendMsg:
		if e.BypassQueue {
			m.submit(ctx, e.Content, submitOptions{busyMode: busySubmitSteer})
		} else {
			m.submitFollowUp(ctx, e.Content)
		}
	case *runtime.StreamStartedEvent:
		if !shared {
			m.lifecycle, _ = m.lifecycle.Apply(e)
		}
		m.ownedSkillStream = m.ownedSkillOperation != ""
		m.busy = true
		m.trackStreamStarted(e.SessionID)
	case *runtime.PendingUserMessageAcceptedEvent:
		if !shared {
			m.lifecycle, _ = m.lifecycle.Apply(e)
		}
		if lifecycle.IsUserInput(e.InputOrigin) {
			kind := ui.PendingUserFollowUp
			if e.InputMode == "steer" {
				kind = ui.PendingUserSteer
			}
			m.addPendingUser(e.Message, e.Message, e.TurnID, kind)
		}
	case *runtime.PendingUserMessagePromotedEvent:
		if !shared {
			m.lifecycle, _ = m.lifecycle.Apply(e)
		}
		m.handleInputEcho(e.InputOrigin, e.InputMode, e.SenderName, e.SenderID, e.Message, e.TurnID, e.SessionPosition)
	case *runtime.PendingUserMessageCanceledEvent:
		m.inputReplay.Withdraw(e.SessionPosition)
		m.lifecycle.Pending = slices.DeleteFunc(slices.Clone(m.lifecycle.Pending), func(id string) bool { return id == e.TurnID })
		m.pendingUsers = slices.DeleteFunc(m.pendingUsers, func(pending ui.PendingUserMessage) bool { return pending.TurnID == e.TurnID })
		m.queue = slices.DeleteFunc(m.queue, func(pending ui.PendingUserMessage) bool { return pending.TurnID == e.TurnID })
	case *runtime.UserMessageEvent:
		m.handleUserMessageEvent(e, e.TurnID)
	case *runtime.StreamStoppedEvent:
		if !shared {
			m.lifecycle, _ = m.lifecycle.Apply(e)
		}
		m.trackStreamStopped()
		if m.lifecycle.Depth() == 0 {
			m.handleStreamStopped(ctx)
		}
	case *runtime.SkillOperationEvent:
		if e.OperationID != m.ownedSkillOperation {
			break
		}
		switch e.Status {
		case "completed", "failed":
			m.ownedSkillOperation = ""
			started := m.ownedSkillStream
			m.ownedSkillStream = false
			if e.Status == "failed" && e.Error != "" {
				m.addNotice("✗ ", e.Error, ui.StError())
			}
			if !started && m.lifecycle.Depth() == 0 {
				m.finishBusy(ctx)
			}
		}
	case *runtime.PauseChangedEvent:
		if m.sessionState != nil {
			if e.Paused {
				m.sessionState.SetPauseState(service.PausePausing)
				if !seed {
					m.addNotice("⏸ ", "Pausing after the current request", ui.StMuted())
				}
			} else {
				m.sessionState.SetPauseState(service.PauseNone)
				if !seed {
					m.addNotice("▶ ", "Resumed", ui.StMuted())
				}
			}
		}
	case *runtime.PausedEvent:
		if m.sessionState != nil {
			m.sessionState.SetPauseState(service.PausePaused)
		}
		if !seed {
			m.addNotice("⏸ ", "Paused", ui.StMuted())
		}
	case *runtime.MessageAddedEvent:
		if e.CommittedRole() == chat.MessageRoleAssistant {
			m.screen.Transcript.CommitAssistant(e.GetAgentName(), e.CommittedToolCallIDs())
		}
	case *runtime.AgentChoiceReasoningEvent:
		m.screen.Transcript.AppendReasoning(e.Content)
	case *runtime.AgentChoiceEvent:
		m.screen.Transcript.AppendAssistant(e.Content)
	case *runtime.PartialToolCallEvent:
		m.screen.Transcript.FlushPending()
		toolDef := tools.Tool{Name: e.ToolCall.Function.Name}
		if e.ToolDefinition != nil {
			toolDef = *e.ToolDefinition
		}
		m.screen.Transcript.UpsertTool(e.GetAgentName(), e.ToolCall, toolDef, tuitypes.ToolStatusPending)
	case *runtime.ToolCallEvent:
		m.screen.Transcript.FlushPending()
		m.screen.Transcript.UpsertTool(e.GetAgentName(), e.ToolCall, e.ToolDefinition, tuitypes.ToolStatusRunning)
	case *runtime.ToolCallOutputEvent:
		if tv := m.screen.Transcript.Tool(e.ToolCallID); tv != nil && tv.Message() != nil {
			tv.Message().AppendToolOutput(e.Output)
			if tv.Message().ToolStatus == tuitypes.ToolStatusPending {
				tv.Message().ToolStatus = tuitypes.ToolStatusRunning
				if tv.Message().StartedAt == nil {
					now := time.Now()
					tv.Message().StartedAt = &now
				}
			}
		}
	case *runtime.ToolCallResponseEvent:
		var images []ui.InlineImage
		if m.renderImages {
			images = inlineImagesFromToolResult(e.Result)
		}
		m.screen.Transcript.FinishTool(e.ToolCallID, ui.ToolResult{Response: e.Response, Result: e.Result, AgentName: e.GetAgentName(), ToolDefinition: e.ToolDefinition, Images: images}, m.sessionState)
	case *runtime.ToolCallConfirmationEvent:
		m.screen.Transcript.RemoveTool(ui.ToolViewID(e.ToolCall))
		toolDef := ui.EnsureToolDefinition(e.ToolCall, e.ToolDefinition)
		m.screen.Confirm = &ui.ConfirmModel{
			Tool:      toolDef.Name,
			View:      *ui.NewToolView(e.GetAgentName(), e.ToolCall, toolDef, tuitypes.ToolStatusConfirmation),
			SessionID: e.SessionID,
			RequestID: e.RequestID,
		}
	case *runtime.TokenUsageEvent:
		m.setTokenUsage(e.SessionID, e.Usage)
	case *runtime.AgentInfoEvent:
		m.status.Agent = e.AgentName
		if m.sessionState != nil {
			m.sessionState.SetCurrentAgentName(e.AgentName)
		}
		if e.Model != "" {
			m.status.Model = e.Model
		}
		if e.ContextLimit > 0 {
			m.status.ContextLimit = e.ContextLimit
		}
	case *runtime.TeamInfoEvent:
		m.applyTeamInfo(ctx, e)
	case *runtime.SessionCompactionEvent:
		if !shared {
			m.lifecycle, _ = m.lifecycle.Apply(e)
		}
		m.handleSessionCompaction(ctx, e)
	case *runtime.ErrorEvent:
		m.screen.Transcript.FlushPending()
		m.addNotice("✗ ", e.Error, ui.StError())
	case *runtime.WarningEvent:
		m.addNotice("⚠ ", e.Message, ui.StWarning())
	case *runtime.ShellOutputEvent:
		output := e.Output
		m.screen.Transcript.AddBlock(func(w int) []string { return ui.RenderToolOutput(output, w) })
	case *runtime.AgentSwitchingEvent:
		if e.Switching && e.ToAgent != "" {
			m.addNotice("→ ", "Switching to "+e.ToAgent, ui.StMuted())
		}
	case *runtime.MaxIterationsReachedEvent:
		m.addNotice("⚠ ", "Maximum iterations reached.", ui.StWarning())
	case *runtime.ModelFallbackEvent:
		m.addNotice("⚠ ", "Model "+e.FailedModel+" failed, falling back to "+e.FallbackModel+".", ui.StWarning())
	}
}

func (m *model) handleUserMessageEvent(e *runtime.UserMessageEvent, turnID string) {
	m.handleInputEcho(e.InputOrigin, e.InputMode, e.SenderName, e.SenderID, e.Message, turnID, e.SessionPosition)
}

func (m *model) handleInputEcho(origin session.InputOrigin, mode, senderName, senderID, content, turnID string, position int) {
	display := content
	for _, kind := range []ui.PendingUserKind{ui.PendingUserSteer, ui.PendingUserFollowUp} {
		if pending, ok := m.consumePendingUser(kind, turnID); ok {
			display = pending.Display
		}
	}
	if !m.inputReplay.Consume(turnID, position) {
		return
	}
	m.screen.Transcript.FlushPending()
	m.addInputEcho(origin, mode, senderName, senderID, display)
}

func (m *model) handleStreamStopped(ctx context.Context) {
	if m.finishBusy(ctx) {
		return
	}

	if m.app != nil && m.app.ShouldExitAfterFirstResponse() {
		m.quit()
	}
}

func (m *model) handleSessionCompaction(ctx context.Context, e *runtime.SessionCompactionEvent) {
	switch e.Status {
	case "started":
		m.busy = true
		m.status.Compacting = true
	case "completed":
		m.status.Compacting = false
		if m.lifecycle.Depth() == 0 {
			m.finishBusy(ctx)
		}
	}
}

// finishBusy clears the busy state at the end of a run and starts the next
// queued message, if any. It reports whether a queued run was started.
func (m *model) finishBusy(ctx context.Context) bool {
	m.screen.Transcript.FlushPending()
	if m.cancelMarkerPending {
		m.screen.Transcript.AddBlock(func(int) []string { return []string{ui.StWarning().Render("⏹ Cancelled")} })
		m.cancelMarkerPending = false
	}
	m.busy = false
	m.runCancel = nil

	if len(m.queue) > 0 {
		next := m.queue[0]
		m.queue[0] = ui.PendingUserMessage{}
		m.queue = m.queue[1:]
		if next.TurnID != "" {
			m.consumePendingUser(ui.PendingUserFollowUp, next.TurnID)
		} else {
			for i, pending := range m.pendingUsers {
				if pending.TurnID == "" && pending.Kind == ui.PendingUserFollowUp {
					m.pendingUsers = slices.Delete(m.pendingUsers, i, i+1)
					break
				}
			}
		}
		m.startRun(ctx, next.Content, nil)
		return true
	}
	return false
}

func (m *model) applyTeamInfo(ctx context.Context, e *runtime.TeamInfoEvent) {
	if m.sessionState != nil {
		m.sessionState.SetAvailableAgents(e.AvailableAgents)
		m.sessionState.SetCurrentAgentName(e.CurrentAgent)
	}
	for _, a := range e.AvailableAgents {
		if a.Name != e.CurrentAgent {
			continue
		}
		m.status.Agent = a.Name
		switch {
		case a.Provider != "" && a.Model != "":
			m.status.Model = a.Provider + "/" + a.Model
		case a.Model != "":
			m.status.Model = a.Model
		}
		m.status.Thinking = a.Thinking
	}
	m.refreshCommands(ctx)
}
