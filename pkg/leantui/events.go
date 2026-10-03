package leantui

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sound"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/components/messagebar"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/subagentindex"
	tuitypes "github.com/docker/docker-agent/pkg/tui/types"
)

// handleEvent applies a single runtime event emitted by the App to the model,
// updating the conversation, tool state, status footer, or busy state.
func (m *model) handleEvent(ctx context.Context, ev any) {
	sequence := uint64(0)
	seed := false
	shared := false
	if bridged, ok := ev.(app.SessionEventMsg); ok {
		if m.app != nil && !m.app.IsCurrentSessionEvent(bridged) {
			return
		}
		if m.app != nil && m.app.Session() != nil && bridged.OriginSessionID != "" && bridged.OriginSessionID != m.app.Session().ID {
			return
		}
		sequence = bridged.Sequence
		seed = bridged.Seed
		ev = bridged.Event
		if bridged.Projection != nil {
			m.lifecycle = bridged.Projection.Lifecycle
			m.status.Dormant = bridged.Projection.Status.Dormant
			m.status.Pending = bridged.Projection.Status.Pending
			m.status.Interrupted = bridged.Projection.Status.InterruptedTurns
			m.status.Connection = connectionLabel(bridged.Projection.Connection)
			shared = true
			if confirm := m.screen.Confirm; confirm != nil && !bridged.Projection.HasInteraction(app.InteractionKey{SessionID: confirm.SessionID, InteractionID: confirm.RequestID}) {
				m.screen.Confirm = nil
			}
		}
	}
	switch e := ev.(type) {
	case *app.ConnectionStateEvent:
		m.status.Connection = connectionLabel(e.State)
		switch e.State {
		case app.ConnectionReconnecting:
			m.addNotice("⚠ ", "Session connection lost; reconnecting. Accepted work continues on the server.", ui.StWarning())
		case app.ConnectionConnected:
			m.addNotice("✓ ", "Session connection restored.", ui.StMuted())
		case app.ConnectionDisconnected:
			m.addNotice("⚠ ", "Session connection closed; accepted work continues on the server. Reopen the session to reattach.", ui.StWarning())
		}
	case *runtime.DormancyChangedEvent:
		if m.app != nil && m.app.Session() != nil && e.SessionID == m.app.Session().ID {
			m.status.Dormant = e.Dormant
		}
	case capabilityResult:
		if m.app != nil && m.app.Session() != nil && e.sessionID == m.app.Session().ID && m.app.IsCurrentSessionEvent(e.identity) {
			m.applyCapabilityResult(e.value, e.err)
		}
	case viewerBranch:
		m.status.Branch = string(e)
	case *runtime.TurnSettledEvent:
		if !seed && sequence > m.soundSequence {
			m.soundSequence = sequence
			if m.soundEnabled && m.playSound != nil {
				switch e.Outcome {
				case runtime.TurnCompleted:
					if !m.streamStarted.IsZero() && time.Since(m.streamStarted) >= m.soundThreshold {
						m.playSound(ctx, sound.Success)
					}
				case runtime.TurnFailed:
					m.playSound(ctx, sound.Failure)
				}
			}
			m.streamStarted = time.Time{}
		}
		if e.Outcome == runtime.TurnCompleted {
			m.addMajorEvent(messagebar.Event{Owner: e.SessionID, ID: e.TurnID, Kind: messagebar.CompletedTurn, Sequence: sequence, Replay: seed})
		}
	case *runtime.SubagentCreatedEvent:
		m.addMajorEvent(messagebar.Event{Owner: e.SessionID, ID: string(e.NodeID) + "/" + e.ChildSessionID + "/" + e.CreatedAt.Format(time.RFC3339Nano), Kind: messagebar.SpawnedSubagent, Sequence: sequence, Replay: seed})

	case speechDelta:
		if e.generation == m.speechGeneration {
			m.screen.Editor.Insert([]rune(e.text))
		}
	case *runtime.SubagentTreeEvent:
		snapshot := e.Snapshot
		m.subagentSnapshot = &snapshot
		m.observeTreeAttention(snapshot)
		if m.screen.Subagents != nil {
			m.screen.Subagents.Update(snapshot)
		}
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
		m.status.Dormant = e.Snapshot.Status.Dormant
		m.status.Pending = e.Snapshot.Status.Pending
		m.status.Interrupted = e.Snapshot.Status.InterruptedTurns
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
		if !shared && m.screen.Confirm != nil {
			projection := app.PresentationState{Interactions: e.Snapshot.Interactions}
			if !projection.HasInteraction(app.InteractionKey{SessionID: m.screen.Confirm.SessionID, InteractionID: m.screen.Confirm.RequestID}) {
				m.screen.Confirm = nil
			}
		}
		m.elicitations = nil
		m.maxIterations = nil
		for _, interaction := range e.Snapshot.Interactions {
			if interaction.Kind != runtime.InteractionConfirmation {
				m.handleEvent(ctx, interaction.Event)
			}
			if confirmation, ok := interaction.Event.(*runtime.ToolCallConfirmationEvent); ok {
				if current := m.screen.Confirm; current == nil || current.SessionID != confirmation.SessionID || current.RequestID != confirmation.RequestID {
					m.handleEvent(ctx, confirmation)
				}
			}
		}
		if n := e.Snapshot.Status.InterruptedTurns; n > 0 {
			m.addNotice("⚠ ", lifecycle.InterruptedTurnsNotice(n), ui.StWarning())
		}
	case *runtime.InteractionResolvedEvent:
		delete(m.elicitations, e.InteractionID)
		delete(m.maxIterations, e.InteractionID)
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
		m.trackStreamStarted(e.SessionID)
		if !seed {
			m.streamStarted = time.Now()
		}
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
		m.handleInputEcho(e.InputOrigin, e.InputMode, e.SenderName, e.SenderID, e.Message, e.TurnID, e.SessionPosition, e.ReportOutcome)
	case *runtime.PendingUserMessageCanceledEvent:
		m.inputReplay.Withdraw(e.SessionPosition)
		if !shared {
			m.lifecycle.Pending = slices.DeleteFunc(slices.Clone(m.lifecycle.Pending), func(id string) bool { return id == e.TurnID })
		}
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
			if e.Status == "failed" && e.Error != "" {
				m.addNotice("✗ ", e.Error, ui.StError())
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
		if current := m.screen.Confirm; current != nil && current.SessionID == e.SessionID && current.RequestID == e.RequestID {
			break // replay of the same interaction preserves its in-progress draft
		}
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
		if m.status.Agent != e.AgentName || (e.Model != "" && e.Model != m.status.Model) {
			m.status.Thinking = ""
			m.status.PrimaryThinking = nil
			m.status.ModelName = ""
			m.status.ThinkingMode, m.status.ThinkingLevel = "", ""
			m.status.ThinkingLevels = nil
			m.status.CanCycleThinking = false
		}
		m.status.Agent = e.AgentName
		if m.sessionState != nil {
			m.sessionState.SetCurrentAgentName(e.AgentName)
		}
		if e.Model != "" {
			m.status.Model = e.Model
			m.status.Provider, _, _ = strings.Cut(e.Model, "/")
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
		m.priorityNoticeUntil = time.Now().Add(messagebar.DefaultLifetime)
		m.screen.Transcript.FlushPending()
		m.addNotice("✗ ", e.Error, ui.StError())
	case *runtime.BudgetExceededEvent:
		m.priorityNoticeUntil = time.Now().Add(messagebar.DefaultLifetime)
		m.reportCapability(e, nil)
	case *runtime.BudgetUsageEvent:
		m.budgetUsage = e
	case *runtime.WarningEvent:
		m.priorityNoticeUntil = time.Now().Add(messagebar.DefaultLifetime)
		m.addNotice("⚠ ", e.Message, ui.StWarning())
	case *runtime.ShellOutputEvent:
		output := e.Output
		m.screen.Transcript.AddBlock(func(w int) []string { return ui.RenderToolOutput(output, w) })
	case *runtime.AgentSwitchingEvent:
		if e.Switching && e.ToAgent != "" {
			m.addNotice("→ ", "Switching to "+e.ToAgent, ui.StMuted())
		}
	case *runtime.ElicitationRequestEvent:
		if m.elicitations == nil {
			m.elicitations = make(map[string]*runtime.ElicitationRequestEvent)
		}
		m.elicitations[e.RequestID] = e
		m.reportCapability(e.Message, nil)
		if e.URL != "" {
			m.reportCapability("Open this authorization URL only if you trust the requesting server: "+e.URL, nil)
		}
		if fields := elicitationFieldLines(e.Schema); len(fields) > 0 {
			m.reportCapability("Fields:\n"+strings.Join(fields, "\n"), nil)
			m.reportCapability("Reply: /respond "+e.RequestID+" name=value … (quote values with spaces; omitted fields use their shown default) · /respond "+e.RequestID+" decline|cancel", nil)
		} else {
			m.reportCapability("Reply: /respond "+e.RequestID+" <your answer> · /respond "+e.RequestID+" decline|cancel", nil)
		}
	case *runtime.MaxIterationsReachedEvent:
		if m.maxIterations == nil {
			m.maxIterations = make(map[string]*runtime.MaxIterationsReachedEvent)
		}
		m.maxIterations[e.RequestID] = e
		m.reportCapability("Maximum iterations reached. /respond "+e.RequestID+" continue|cancel", nil)
	case *runtime.ModelFallbackEvent:
		m.addNotice("⚠ ", "Model "+e.FailedModel+" failed, falling back to "+e.FallbackModel+".", ui.StWarning())
	}
}

func (m *model) handleUserMessageEvent(e *runtime.UserMessageEvent, turnID string) {
	m.handleInputEcho(e.InputOrigin, e.InputMode, e.SenderName, e.SenderID, e.Message, turnID, e.SessionPosition, e.ReportOutcome)
}

func (m *model) handleInputEcho(origin session.InputOrigin, mode, senderName, senderID, content, turnID string, position int, outcome ...session.ReportOutcome) {
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
	m.addInputEcho(origin, mode, senderName, senderID, display, outcome...)
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
		m.status.Compacting = true
	case "completed":
		m.status.Compacting = false
		if m.lifecycle.Depth() == 0 {
			m.finishBusy(ctx)
		}
	}
}

// finishBusy finalizes presentation and submits the next queued message, if
// any. Only subsequent canonical events can mark that submission as running.
func (m *model) finishBusy(ctx context.Context) bool {
	m.screen.Transcript.FlushPending()
	if m.cancelMarkerPending {
		m.screen.Transcript.AddBlock(func(int) []string { return []string{ui.StWarning().Render("⏹ Cancelled")} })
		m.cancelMarkerPending = false
	}

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
		if a.ModelID != "" {
			m.status.Model = a.ModelID
			if a.Provider != "" && !strings.HasPrefix(a.ModelID, a.Provider+"/") {
				m.status.Model = a.Provider + "/" + a.ModelID
			}
		}
		m.status.ModelName = a.ModelName
		m.status.Provider = a.Provider
		m.status.Thinking = a.Thinking
		m.status.ThinkingMode = a.ThinkingMode
		m.status.ThinkingLevel = a.ThinkingLevel
		m.status.ThinkingLevels = slices.Clone(a.ThinkingLevels)
		m.status.CanCycleThinking = a.CanCycleThinking
		m.status.PrimaryThinking = nil
		if a.PrimaryThinking != nil {
			m.status.PrimaryThinking = &ui.ThinkingStatus{
				Mode:  a.PrimaryThinking.Mode,
				Level: a.PrimaryThinking.Level,
			}
		}
	}
	m.refreshCommands(ctx)
}

func (m *model) addMajorEvent(event messagebar.Event) {
	if event.Sequence == 0 || event.Owner == "" || event.ID == "" {
		return
	}
	if m.app == nil || m.app.Session() == nil || event.Owner != m.app.Session().ID {
		return
	}
	index := int(event.Kind - 1)
	if index < 0 || index >= len(m.majorHighWater) || event.Sequence <= m.majorHighWater[index] {
		return
	}
	m.majorHighWater[index] = event.Sequence
	if event.Replay {
		return
	}
	if m.majorEvents == nil {
		m.majorEvents = &messagebar.Aggregator{}
	}
	m.majorEvents.Add(event, time.Now())
}

func (m *model) hasMajorNotice(now time.Time) bool {
	if m.majorEvents == nil || m.app == nil || m.app.Session() == nil {
		return false
	}
	_, active := m.majorEvents.Current(m.app.Session().ID, now)
	return active
}
