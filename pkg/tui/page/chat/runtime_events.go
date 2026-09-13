package chat

import (
	"fmt"
	"log/slog"
	"slices"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/app/lifecycle"
	chatmsg "github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sound"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/types"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// Runtime Event Handling
//
// This file maps runtime events to UI updates, following the Elm Architecture
// pattern of explicit event-to-update mappings. Events are organized by category:
//
// Stream Lifecycle:
//   - StreamStartedEvent  → Start spinners, set pending response
//   - StreamStoppedEvent  → Stop spinners, process queue, maybe exit
//
// Content Events:
//   - AgentChoiceEvent         → Append text to message
//   - AgentChoiceReasoningEvent → Append reasoning block
//   - UserMessageEvent         → Replace loading with user message
//   - MessageAddedEvent        → Render generated media (local runs only)
//
// Tool Events:
//   - PartialToolCallEvent      → Show tool call in progress
//   - ToolCallEvent             → Tool execution started
//   - ToolCallConfirmationEvent → Show confirmation dialog
//   - ToolCallOutputEvent       → Append live tool output
//   - ToolCallResponseEvent     → Show tool result
//
// Sidebar Updates (forwarded):
//   - TokenUsageEvent, AgentInfoEvent, TeamInfoEvent, etc.
//   - BudgetUsageEvent          → Live run-budget reading
//
// Warnings:
//   - BudgetExceededEvent       → Run stopped by its budget
//
// Dialogs:
//   - MaxIterationsReachedEvent → Show max iterations dialog
//   - ElicitationRequestEvent   → Show elicitation/OAuth dialog

// handleRuntimeEvent processes runtime events and returns the appropriate command.
// Returns (handled, cmd) where handled indicates if the event was processed.
//
// The switch is organized by event category for clarity.
func (p *chatPage) handleRuntimeEvent(msg tea.Msg) (bool, tea.Cmd) {
	seed := false
	if bridged, ok := msg.(msgtypes.SessionRuntimeEventMsg); ok {
		msg = bridged.Event
		seed = bridged.Seed
		if bridged.Projection != nil {
			p.lifecycle = bridged.Projection.Lifecycle
			p.sharedProjection = true
			defer func() { p.sharedProjection = false }()
		}
	}
	switch msg := msg.(type) {
	case *app.SessionViewEvent:
		p.sessionState.SetYoloMode(msg.Session.IsToolsApproved())
		p.sessionState.SetSessionTitle(msg.Session.TitleSnapshot())
		return true, nil
	case *app.SessionResetEvent:
		return true, p.resetProjection(msg.Snapshot)
	case *runtime.InteractionResolvedEvent:
		return true, nil
	// ===== Error and Warning Events =====
	case *runtime.SkillOperationEvent:
		if msg.OperationID != p.ownedSkillOperation {
			return true, nil
		}
		switch msg.Status {
		case "completed", "failed":
			p.ownedSkillOperation = ""
			started := p.ownedSkillStream
			p.ownedSkillStream = false
			var errCmd tea.Cmd
			if msg.Status == "failed" && msg.Error != "" {
				errCmd = p.messages.AddErrorMessage(msg.Error)
			}
			if !started && p.lifecycle.Depth() == 0 {
				return true, tea.Batch(errCmd, p.finishSkillOperation())
			}
			return true, errCmd
		}
	case *runtime.ErrorEvent:
		if userconfig.Get().GetSound() {
			sound.Play(p.ctx(), sound.Failure)
		}
		return true, p.messages.AddErrorMessage(msg.Error)

	case *runtime.WarningEvent:
		return true, notification.WarningCmd(msg.Message)

	case *runtime.ModelFallbackEvent:
		// Update sidebar with the fallback model immediately so it reflects the switch
		sidebarCmd := p.sidebar.SetAgentInfo(msg.AgentName, msg.FallbackModel, "", 0, "", 0)
		// Notify user when switching to a fallback model, include the reason
		fallbackMsg := fmt.Sprintf("Model %s failed (%s), switching to %s", msg.FailedModel, msg.Reason, msg.FallbackModel)
		return true, tea.Batch(sidebarCmd, notification.WarningCmd(fallbackMsg))

	// ===== Stream Lifecycle Events =====
	case *runtime.StreamStartedEvent:
		return true, p.handleStreamStarted(msg, seed)

	case *runtime.StreamStoppedEvent:
		return true, p.handleStreamStopped(msg)

	// ===== Content Events =====
	case *runtime.PendingUserMessageAcceptedEvent:
		p.applyLifecycle(msg)
		if !lifecycle.IsUserInput(msg.InputOrigin) {
			return true, nil
		}
		for _, queued := range p.messageQueue {
			if queued.turnID == msg.TurnID {
				return true, nil
			}
		}
		p.messageQueue = append(p.messageQueue, queuedMessage{turnID: msg.TurnID, content: msg.Message})
		return true, p.syncQueueToSidebar()

	case *runtime.PendingUserMessageEditedEvent:
		for i := range p.messageQueue {
			if p.messageQueue[i].turnID == msg.TurnID {
				p.messageQueue[i].content = msg.Message
				return true, p.syncQueueToSidebar()
			}
		}
		return true, nil

	case *runtime.PendingUserMessageCanceledEvent:
		if !p.sharedProjection {
			p.lifecycle.Pending = slices.DeleteFunc(slices.Clone(p.lifecycle.Pending), func(id string) bool { return id == msg.TurnID })
		}
		p.inputReplay.Withdraw(msg.SessionPosition)
		p.messages.RemovePendingSessionPosition(msg.SessionPosition)
		if msg.SessionPosition >= 0 && msg.SessionPosition < p.snapshotEnd {
			p.snapshotEnd--
		}
		p.messageQueue = slices.DeleteFunc(p.messageQueue, func(queued queuedMessage) bool { return queued.turnID == msg.TurnID })
		return true, p.syncQueueToSidebar()

	case *runtime.PendingUserMessagePromotedEvent:
		p.applyLifecycle(msg)
		p.messageQueue = slices.DeleteFunc(p.messageQueue, func(queued queuedMessage) bool { return queued.turnID == msg.TurnID })
		queueCmd := p.syncQueueToSidebar()
		if !p.inputReplay.Consume(msg.TurnID, msg.SessionPosition) {
			return true, queueCmd
		}
		input := session.UserMessage(msg.Message, msg.MultiContent...)
		input.InputOrigin, input.InputMode, input.SenderID, input.SenderName = msg.InputOrigin, msg.InputMode, msg.SenderID, msg.SenderName
		if lifecycle.VisibleTranscriptMessage(input) {
			p.showStartupBanner = false
		}
		return true, tea.Batch(queueCmd, p.messages.AddInputMessage(input, msg.SessionPosition))

	case *runtime.UserMessageEvent:
		// Attach protocol: a position inside the transcript snapshot is already
		// on screen — the buffered event would duplicate it.
		if msg.TurnID == "" && p.snapshotEnd > 0 && msg.SessionPosition >= 0 && msg.SessionPosition < p.snapshotEnd {
			return true, nil
		}
		if !p.inputReplay.Consume(msg.TurnID, msg.SessionPosition) {
			return true, nil
		}
		input := session.UserMessage(msg.Message, msg.MultiContent...)
		input.InputOrigin, input.InputMode, input.SenderID, input.SenderName = msg.InputOrigin, msg.InputMode, msg.SenderID, msg.SenderName
		if lifecycle.VisibleTranscriptMessage(input) {
			p.showStartupBanner = false
		}
		return true, p.messages.AddInputMessage(input, msg.SessionPosition)

	case *runtime.MessageAddedEvent:
		if msg.CommittedRole() != chatmsg.MessageRoleAssistant {
			return true, nil
		}
		// Snapshot-covered commits must not settle the current, newer live tail.
		if msg.SessionPosition >= 0 && msg.SessionPosition < p.snapshotEnd {
			return true, nil
		}
		finalized := p.messages.CompleteAssistant(msg)
		return true, tea.Batch(finalized, p.handleMessageAdded(msg))

	case *runtime.AgentChoiceEvent:
		return true, p.handleAgentChoice(msg)

	case *runtime.AgentChoiceReasoningEvent:
		return true, p.handleAgentChoiceReasoning(msg)

	case *runtime.ShellOutputEvent:
		return true, p.messages.AddShellOutputMessage(msg.Output)

	// ===== Tool Events =====
	case *runtime.PartialToolCallEvent:
		return true, p.handlePartialToolCall(msg)

	case *runtime.ToolCallEvent:
		return true, p.handleToolCall(msg)

	case *runtime.ToolCallConfirmationEvent:
		return true, p.handleToolCallConfirmation(msg)

	case *runtime.ToolCallOutputEvent:
		return true, p.handleToolCallOutput(msg)

	case *runtime.ToolCallResponseEvent:
		return true, p.handleToolCallResponse(msg)

	// ===== Sidebar Info Events (forwarded) =====
	case *runtime.TokenUsageEvent:
		p.handleTokenUsage(msg)
		return true, nil

	case *runtime.AgentInfoEvent:
		sidebarCmd := p.sidebar.SetAgentInfo(msg.AgentName, msg.Model, msg.Description, msg.ContextLimit, msg.CompactionModel, msg.PrimaryContextLimit)
		if msg.WelcomeMessage != "" {
			p.showStartupBanner = false
		}
		p.messages.AddWelcomeMessage(msg.WelcomeMessage)
		return true, sidebarCmd

	case *runtime.TeamInfoEvent:
		p.sidebar.SetTeamInfo(msg.AvailableAgents)
		// The appModel already updated the agent-color registry from this event.
		// A transcript restored before the event (session reload) was rendered
		// and cached with fallback colors; re-render it once the roster changes.
		names := make([]string, len(msg.AvailableAgents))
		for i, a := range msg.AvailableAgents {
			names[i] = a.Name
		}
		if !slices.Equal(names, p.teamAgentNames) {
			p.teamAgentNames = names
			p.messages.InvalidateRenderCaches()
		}
		return true, nil

	case *runtime.AgentSwitchingEvent:
		return true, p.handleAgentSwitching(msg)

	case *runtime.ToolsetInfoEvent:
		if skills, err := p.app.CurrentAgentSkillsContext(p.ctx()); err == nil {
			p.sidebar.SetSkillsInfo(len(skills))
			return true, p.forwardToSidebar(msg)
		} else {
			return true, tea.Batch(p.forwardToSidebar(msg), notification.ErrorCmd("Failed to discover skills: "+err.Error()))
		}

	case *runtime.SessionTitleEvent:
		return true, p.forwardToSidebar(msg)

	case *runtime.SubagentTreeEvent:
		// Keep the id → name index fresh so subagent tool calls can be
		// attributed by name while still running.
		p.subagents.Reset(msg.Snapshot)
		p.messages.RefreshInputReferences()
		return true, p.forwardToSidebar(msg)

	case *runtime.SessionCompactionEvent:
		p.applyLifecycle(msg)
		// The sidebar tracks the started/completed pair to drive its
		// "compacting…" gauge state (it ignores sessions it is not
		// currently displaying).
		sidebarCmd := p.forwardToSidebar(msg)
		if p.isSubSessionEvent(msg.SessionID) {
			// A sub-agent session compacting (threshold-triggered on a
			// foreground child, or an explicit /context request on a live
			// session) must not touch the root chat's work state: no
			// clearing of msgCancel, no spinner flip, no queue processing.
			return true, tea.Batch(sidebarCmd, subSessionCompactionNotice(msg))
		}
		if msg.Status == "started" && p.lifecycle.Depth() == 0 {
			return true, tea.Batch(sidebarCmd, p.setWorking(true), p.setPendingResponse(true))
		}
		if msg.Status == "completed" {
			noticeCmd := rootCompactionNotice(msg)
			if p.lifecycle.Depth() > 0 {
				// Automatic compaction nested in a live stream (the threshold
				// fires after StreamStarted): update presentation only.
				// Clearing msgCancel/working/queue here would break Esc and
				// start queued messages against the live run (#3872);
				// StreamStopped owns the cleanup.
				return true, tea.Batch(sidebarCmd, noticeCmd)
			}
			// Standalone /compact runs outside any stream (Summarize emits no
			// StreamStarted): this terminal event cleans up the work state.
			p.msgCancel = nil
			return true, tea.Batch(
				sidebarCmd,
				p.setWorking(false),
				p.setPendingResponse(false),
				p.messages.ScrollToBottom(),
				noticeCmd,
			)
		}
		return true, sidebarCmd

	// ===== RAG Indexing Events (forwarded to sidebar) =====
	case *runtime.RAGIndexingStartedEvent,
		*runtime.RAGIndexingProgressEvent,
		*runtime.RAGIndexingCompletedEvent:
		return true, p.forwardToSidebar(msg)

	// ===== Budget Events =====
	// budget_usage feeds the sidebar's live spend reading. budget_exceeded
	// is rendered as a warning rather than a dialog: the run is already
	// stopping and there is nothing to ask the user, unlike the iteration
	// cap, which offers to continue.
	case *runtime.BudgetUsageEvent:
		return true, p.forwardToSidebar(msg)

	case *runtime.BudgetExceededEvent:
		return true, p.handleBudgetExceeded(msg)

	// ===== Dialog Events =====
	case *runtime.MaxIterationsReachedEvent:
		return true, p.handleMaxIterationsReached(msg)

	case *runtime.ElicitationRequestEvent:
		return true, p.handleElicitationRequest(msg)
	}

	return false, nil
}

// forwardToSidebar forwards a message to the sidebar and returns the resulting command.
func (p *chatPage) forwardToSidebar(msg tea.Msg) tea.Cmd {
	slog.Debug("Forwarding event to sidebar", "event_type", fmt.Sprintf("%T", msg))
	model, cmd := p.sidebar.Update(msg)
	p.sidebar = model.(sidebar.Model)
	return cmd
}

// isSubSessionEvent reports whether sessionID identifies a session other
// than the page's root session. Events without a session ID are treated as
// root events for backward compatibility.
func (p *chatPage) isSubSessionEvent(sessionID string) bool {
	if sessionID == "" || p.app == nil {
		return false
	}
	sess := p.app.Session()
	return sess != nil && sessionID != sess.ID
}

// rootCompactionNotice builds the root session's terminal compaction
// feedback. Success is only announced when a summary was actually applied:
// "failed" already surfaced an ErrorEvent; "skipped" means the compaction
// was a no-op (nothing to compact, hook veto, empty model output). Empty
// outcome (older servers) keeps the legacy success toast.
func rootCompactionNotice(msg *runtime.SessionCompactionEvent) tea.Cmd {
	switch msg.Outcome {
	case "", runtime.CompactionOutcomeApplied:
		return notification.SuccessCmd("Session compacted successfully.")
	case runtime.CompactionOutcomeSkipped:
		return notification.InfoCmd("Session compaction skipped.")
	}
	return nil
}

// subSessionCompactionNotice builds the agent-scoped feedback for a
// sub-agent session's compaction: success, skipped or failed on the terminal
// event, silence while it runs (the request itself was already announced).
func subSessionCompactionNotice(msg *runtime.SessionCompactionEvent) tea.Cmd {
	if msg.Status != "completed" {
		return nil
	}
	agentLabel := msg.AgentName
	if agentLabel == "" {
		agentLabel = "sub-agent"
	}
	switch msg.Outcome {
	case "", runtime.CompactionOutcomeApplied:
		return notification.SuccessCmd(fmt.Sprintf("Compacted %s's session.", agentLabel))
	case runtime.CompactionOutcomeSkipped:
		return notification.InfoCmd(fmt.Sprintf("Compaction skipped for %s's session.", agentLabel))
	case runtime.CompactionOutcomeFailed:
		return notification.ErrorCmd(fmt.Sprintf("Compaction failed for %s's session.", agentLabel))
	}
	return nil
}

// handleTokenUsage updates sidebar and session with token usage data.
// This handler performs side effects only and returns no command.
func (p *chatPage) handleTokenUsage(msg *runtime.TokenUsageEvent) {
	p.sidebar.SetTokenUsage(msg)
}

func (p *chatPage) handleStreamStarted(msg *runtime.StreamStartedEvent, seed bool) tea.Cmd {
	p.ownedSkillStream = p.ownedSkillOperation != ""

	slog.Debug("handleStreamStarted called", "agent", msg.AgentName, "session_id", msg.SessionID)
	p.streamCancelled = false
	p.applyLifecycle(msg)
	p.streamStartTime = time.Now()
	spinnerCmd := p.setWorking(true)
	var pendingCmd tea.Cmd
	if p.app == nil || p.app.AttachedSubagent() == nil || !seed {
		pendingCmd = p.setPendingResponse(true)
	}
	sidebarCmd := p.forwardToSidebar(msg)
	return tea.Batch(pendingCmd, spinnerCmd, sidebarCmd)
}

func (p *chatPage) handleAgentChoice(msg *runtime.AgentChoiceEvent) tea.Cmd {
	if p.streamCancelled {
		return nil
	}
	// Track that we've received assistant content
	p.hasReceivedAssistantContent = true
	// Clear pending response indicator - first chunk has arrived
	p.setPendingResponse(false)
	// Content is useful activity: acknowledge the sidebar's outbound transfer
	// box when this agent is a delegation target.
	activityCmd := p.sidebar.SetAgentActivity(msg.AgentName)
	return tea.Batch(activityCmd, p.messages.AppendToLastMessage(msg.AgentName, msg.Content))
}

func (p *chatPage) handleAgentChoiceReasoning(msg *runtime.AgentChoiceReasoningEvent) tea.Cmd {
	if p.streamCancelled {
		return nil
	}
	p.setPendingResponse(false)
	activityCmd := p.sidebar.SetAgentActivity(msg.AgentName)
	return tea.Batch(activityCmd, p.messages.AppendReasoning(msg.AgentName, msg.Content))
}

// handleAgentSwitching forwards transfer_task hop boundaries to the sidebar
// and schedules the presentation timers it arms, routed back to this page so
// they expire on their owner even after a tab switch. A stop the sidebar
// accepted (it closed the exact inverse hop of a recorded start) additionally
// adds the delegation-return transition to the chat ("child returned control
// to parent"); a stale stop — no matching hop — stays silent everywhere, and
// a start adds nothing to the chat: the transfer_task tool call already
// narrates the outbound direction. Boundaries arriving after the user
// cancelled the stream are dropped entirely: the delegation they belong to
// was already torn down visually.
func (p *chatPage) handleAgentSwitching(msg *runtime.AgentSwitchingEvent) tea.Cmd {
	if p.streamCancelled {
		return nil
	}
	res := p.sidebar.SetAgentSwitching(msg.Switching, msg.FromAgent, msg.ToAgent)
	cmd := tea.Batch(res.Cmd, p.scheduleTransferTimers(res.Timers))
	if msg.Switching || !res.Accepted {
		return cmd
	}
	returnCmd := p.messages.AddAgentReturn(msg.FromAgent, msg.ToAgent)
	return tea.Batch(cmd, returnCmd, p.messages.ScrollToBottom())
}

func (p *chatPage) finishSkillOperation() tea.Cmd {
	p.msgCancel = nil
	p.streamCancelled = false
	p.setPendingResponse(false)
	return tea.Batch(p.setWorking(false), p.messages.ScrollToBottom())
}

func (p *chatPage) handleStreamStopped(msg *runtime.StreamStoppedEvent) tea.Cmd {
	slog.Debug("handleStreamStopped called",
		"agent", msg.AgentName,
		"session_id", msg.SessionID,
		"reason", msg.Reason,
		"should_exit", p.app != nil && p.app.ShouldExitAfterFirstResponse(),
		"has_content", p.hasReceivedAssistantContent,
		"stream_depth", p.lifecycle.Depth())

	p.applyLifecycle(msg)

	sidebarCmd := p.forwardToSidebar(msg)

	// Sub-agent stream stopped — the parent is still running, so only
	// forward to the sidebar and keep the working/cancel state intact.
	// Without this guard, pressing Esc after a sub-agent completes but
	// while the parent continues would have no effect.
	// Also clear the now-stale "parent → child" labeled spinner and
	// replace it with a plain parent spinner so the UI reflects the
	// updated delegation state.
	if p.lifecycle.Depth() > 0 {
		p.setPendingResponse(false)
		return tea.Batch(p.messages.ScrollToBottom(), sidebarCmd, p.setPendingResponse(true))
	}

	// Outermost stream stopped — fully clean up. This is the exact-content
	// boundary for the active root response; nested stops leave the parent's
	// deferred tail intact until the parent itself stops or the user re-enters it.
	finalizeCmd := p.messages.FinalizeStream()
	// Only play the success sound when the stream completed normally.
	// Errors already trigger a failure sound via ErrorEvent, and
	// user-initiated cancels don't warrant a chime.
	if userconfig.Get().GetSound() && isSuccessfulStop(msg.Reason) {
		duration := time.Since(p.streamStartTime)
		threshold := time.Duration(userconfig.Get().GetSoundThreshold()) * time.Second
		if duration >= threshold {
			sound.Play(p.ctx(), sound.Success)
		}
	}
	p.msgCancel = nil
	p.streamCancelled = false
	spinnerCmd := p.setWorking(false)
	p.setPendingResponse(false)
	var exitCmd tea.Cmd
	if p.app.ShouldExitAfterFirstResponse() && p.hasReceivedAssistantContent {
		slog.Debug("Exit after first response triggered, scheduling delayed exit")
		exitCmd = tea.Tick(50*time.Millisecond, func(time.Time) tea.Msg {
			return msgtypes.ExitAfterFirstResponseMsg{}
		})
	}

	return tea.Batch(finalizeCmd, p.messages.ScrollToBottom(), spinnerCmd, sidebarCmd, exitCmd)
}

// handlePartialToolCall processes partial tool call events by rendering each
// tool call as it streams in. The tool call appears with its name and a static
// "pending" indicator (not animated) to show it's receiving data.
func (p *chatPage) handlePartialToolCall(msg *runtime.PartialToolCallEvent) tea.Cmd {
	p.setPendingResponse(false)
	var toolDef tools.Tool
	if msg.ToolDefinition != nil {
		toolDef = *msg.ToolDefinition
	}
	// A tool call is useful activity even without any streamed text:
	// acknowledge the sidebar's outbound transfer box.
	activityCmd := p.sidebar.SetAgentActivity(msg.AgentName)
	toolCmd := p.messages.AddOrUpdateToolCall(msg.AgentName, msg.ToolCall, toolDef, types.ToolStatusPending)
	return tea.Batch(activityCmd, toolCmd, p.messages.ScrollToBottom())
}

func (p *chatPage) handleToolCallConfirmation(msg *runtime.ToolCallConfirmationEvent) tea.Cmd {
	spinnerCmd := p.setWorking(false)
	toolCmd := p.messages.AddOrUpdateToolCall(msg.AgentName, msg.ToolCall, msg.ToolDefinition, types.ToolStatusConfirmation)
	dialogCmd := core.CmdHandler(dialog.OpenDialogMsg{
		Model:            dialog.NewToolConfirmationDialog(p.ar, msg, p.sessionState),
		OriginatingEvent: msg,
	})
	return tea.Batch(toolCmd, p.messages.ScrollToBottom(), spinnerCmd, dialogCmd)
}

func (p *chatPage) handleToolCall(msg *runtime.ToolCallEvent) tea.Cmd {
	p.setPendingResponse(false)
	spinnerCmd := p.setWorking(true)
	sidebarCmd := p.forwardToSidebar(msg)
	toolCmd := p.messages.AddOrUpdateToolCall(msg.AgentName, msg.ToolCall, msg.ToolDefinition, types.ToolStatusRunning)
	return tea.Batch(toolCmd, p.messages.ScrollToBottom(), spinnerCmd, sidebarCmd)
}

func (p *chatPage) handleToolCallOutput(msg *runtime.ToolCallOutputEvent) tea.Cmd {
	return tea.Batch(p.messages.AppendToolOutput(msg), p.messages.ScrollToBottom())
}

func (p *chatPage) handleToolCallResponse(msg *runtime.ToolCallResponseEvent) tea.Cmd {
	spinnerCmd := p.setWorking(true)
	sidebarCmd := p.forwardToSidebar(msg)

	status := types.ToolStatusCompleted
	if msg.Result.IsError {
		status = types.ToolStatusError
	}
	toolCmd := p.messages.AddToolResult(msg, status)

	// Update todo sidebar if this is a todo tool
	if msg.ToolDefinition.Category == "todo" && !msg.Result.IsError {
		_ = p.sidebar.SetTodos(msg.Result)
	}

	return tea.Batch(toolCmd, p.messages.ScrollToBottom(), spinnerCmd, sidebarCmd)
}

// handleBudgetExceeded reports a run stopped by its budget. It stops the
// spinner and raises a warning naming the limit that tripped, alongside
// the assistant stop message the runtime already appended.
//
// Deliberately not a dialog: unlike the iteration cap there is nothing to
// ask. A budget is a ceiling the operator set on purpose, so raising it
// means editing the config rather than answering a prompt.
func (p *chatPage) handleBudgetExceeded(msg *runtime.BudgetExceededEvent) tea.Cmd {
	spinnerCmd := p.setWorking(false)
	warnCmd := notification.WarningCmd(fmt.Sprintf(
		"Run stopped by %s — used %s of %s.", msg.ConfigPath, msg.Used, msg.Max,
	))
	return tea.Batch(spinnerCmd, warnCmd)
}

func (p *chatPage) handleMaxIterationsReached(msg *runtime.MaxIterationsReachedEvent) tea.Cmd {
	spinnerCmd := p.setWorking(false)
	dialogCmd := core.CmdHandler(dialog.OpenDialogMsg{
		Model:            dialog.NewMaxIterationsDialog(msg.MaxIterations, msg.SessionID, msg.RequestID),
		OriginatingEvent: msg,
	})
	return tea.Batch(spinnerCmd, dialogCmd)
}

func (p *chatPage) handleElicitationRequest(msg *runtime.ElicitationRequestEvent) tea.Cmd {
	spinnerCmd := p.setWorking(false)

	// Check if this is an OAuth flow by looking at the meta type
	// Guard against nil Meta map to prevent panic
	if msg.Meta != nil {
		if elicitationType, ok := msg.Meta["docker-agent/type"].(string); ok && elicitationType == "oauth_flow" {
			// OAuth flow - show the OAuth authorization dialog
			var serverURL string
			if url, ok := msg.Meta["docker-agent/server_url"].(string); ok {
				serverURL = url
			}
			dialogCmd := core.CmdHandler(dialog.OpenDialogMsg{
				Model:            dialog.NewOAuthAuthorizationDialog(serverURL, dialog.ElicitationRefFor(msg)),
				OriginatingEvent: msg,
			})
			return tea.Batch(spinnerCmd, dialogCmd)
		}
	}

	// Check elicitation mode
	switch msg.Mode {
	case "url":
		// URL-based elicitation - show URL dialog
		dialogCmd := core.CmdHandler(dialog.OpenDialogMsg{
			Model:            dialog.NewURLElicitationDialog(p.ctx(), msg.Message, msg.URL, dialog.ElicitationRefFor(msg)),
			OriginatingEvent: msg,
		})
		return tea.Batch(spinnerCmd, dialogCmd)

	default:
		// Form-based elicitation (default) - show form dialog
		dialogCmd := core.CmdHandler(dialog.OpenDialogMsg{
			Model:            dialog.NewElicitationDialog(msg.Message, msg.Schema, msg.Meta, dialog.ElicitationRefFor(msg)),
			OriginatingEvent: msg,
		})
		return tea.Batch(spinnerCmd, dialogCmd)
	}
}

// isSuccessfulStop returns true when the stream reason indicates a
// normal completion that warrants the success sound. Empty reason
// (e.g. cache hits, early exits before a turn runs) is treated as
// success to preserve backward compatibility.
func isSuccessfulStop(reason string) bool {
	switch reason {
	case "", "normal", "continue", "steered":
		return true
	default:
		return false
	}
}

func (p *chatPage) applyLifecycle(event runtime.Event) {
	if !p.sharedProjection {
		p.lifecycle, _ = p.lifecycle.Apply(event)
	}
}

func (p *chatPage) resetProjection(snapshot runtime.SessionSnapshot) tea.Cmd {
	p.lifecycle = lifecycle.FromSnapshot(snapshot)
	p.inputReplay.Reset(snapshot.Session)
	p.messageQueue = nil
	for _, input := range snapshot.PendingInputs {
		if !lifecycle.IsUserInput(input.InputOrigin) {
			continue
		}
		p.messageQueue = append(p.messageQueue, queuedMessage{turnID: input.TurnID, content: input.Content})
	}
	cmds := []tea.Cmd{p.syncQueueToSidebar()}
	if snapshot.Session != nil {
		p.sessionState.SetYoloMode(snapshot.Session.IsToolsApproved())
		p.sessionState.SetSessionTitle(snapshot.Session.TitleSnapshot())
		restoredMedia, mediaRequests := p.collectRestoredGeneratedMedia(snapshot.Session)
		if resetter, ok := p.messages.(interface {
			ResetFromSession(sess *session.Session, media map[int][]types.AssistantMedia) tea.Cmd
		}); ok {
			cmds = append(cmds, resetter.ResetFromSession(snapshot.Session, restoredMedia))
		} else {
			cmds = append(cmds, p.messages.LoadFromSession(snapshot.Session, restoredMedia))
		}
		cmds = append(cmds, p.hydrateSidebarSession(snapshot.Session))
		p.snapshotEnd = snapshot.TranscriptPosition
		if snapshot.Session.MessageCount() > 0 {
			p.showStartupBanner = false
		}
		cmds = append(cmds, p.resolveGeneratedMediaCmd(mediaRequests))
	}
	running := snapshot.Status.State == runtime.SessionStateRunning || snapshot.Status.State == runtime.SessionStateQueued || snapshot.Status.State == runtime.SessionStateCancelling
	p.streamCancelled = false
	cmds = append(cmds, p.setWorking(running))
	if !running {
		p.messages.RemoveSpinner()
	}
	return tea.Batch(cmds...)
}
