package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/google/uuid"

	"github.com/docker/docker-agent/pkg/app/export"
	"github.com/docker/docker-agent/pkg/app/transcript"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/shellpath"
	"github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type Services interface {
	CurrentAgentInfo(ctx context.Context) runtime.CurrentAgentInfo
	CurrentAgentTools(ctx context.Context) ([]tools.Tool, error)
	CurrentAgentToolsetStatuses() []tools.ToolsetStatus
	RestartToolset(ctx context.Context, name string) error
	EmitStartupInfo(ctx context.Context, sess *session.Session, sink runtime.EventSink)
	EmitAgentInfo(ctx context.Context, sink runtime.EventSink)
	ResetStartupInfo()
	SessionStore() session.Store
	PermissionsInfo() *runtime.PermissionsInfo
	CurrentAgentSkillsToolset() *skillstool.ToolSet
	CurrentMCPPrompts(ctx context.Context) map[string]tools.PromptInfo
	ExecuteMCPPrompt(ctx context.Context, name string, arguments map[string]string) (string, error)
	UpdateSessionTitle(ctx context.Context, sess *session.Session, title string) error
	OnToolsChanged(handler func(runtime.Event))
	OnBackgroundEvent(handler func(runtime.Event))
}

type sessionState struct {
	session *session.Session
	handle  runtime.SessionHandle
	binding runtime.SessionBinding
	err     error
}

type App struct {
	commandMetadata commandMetadata
	ctx             func() context.Context

	stateMu                sync.RWMutex // guards currentState, published as one coherent immutable tuple
	currentState           sessionState
	runtime                Services
	sessions               runtime.SessionRuntime
	firstMessage           *string
	firstMessageAttach     string
	queuedMessages         []string
	events                 chan any
	throttleDuration       time.Duration
	cancel                 context.CancelFunc
	currentAgentModel      string      // Tracks the current agent's model ID from AgentInfoEvent
	exitAfterFirstResponse bool        // Exit TUI after first assistant response completes
	resolvedView           bool        // canonical view acquisition already completed; Start must not restore again
	readOnly               bool        // When true, no new messages can be sent to the LLM
	titleGenerating        atomic.Bool // True when title generation is in progress
	titleEnabled           bool
	titleGen               *sessiontitle.Generator     // Title generator for local runtime (nil for remote)
	snapshotController     builtins.SnapshotController // Drives /undo, /snapshots, /reset; nil for runtimes that don't capture snapshots

	// stopBridge tears down the session-event bridge (the hub subscription
	// feeding the App bus); re-set whenever the session changes. hubBridged
	// reports whether a bridge is active: the bus is then the single event
	// source and Run's own channel is drained for flow control only.
	// runCancelled mutes bridged events after the user cancels the in-flight
	// turn (all but the stream stop), mirroring the classic drop-on-cancel.
	bridgeMu         sync.Mutex
	stopBridge       func()
	hubBridged       bool
	bridgeEpoch      atomic.Uint64
	presentation     atomic.Pointer[PresentationState]
	projectionMu     sync.Mutex
	cancelGeneration uint64
	connection       atomic.Uint32 // ConnectionState of the current bridge
	treeMu           sync.Mutex
	treeWatch        *treeWatch
	runCancelled     atomic.Bool
	// lifecycleMu correlates accepted submissions with cancellation and bridged
	// envelopes. A cancelled request only mutes its own tail; a stale stop can
	// never clear presentation state for a newer request.
	lifecycleMu        sync.Mutex
	latestRequestID    string
	projectedRequestID string
	cancelledRequests  map[string]struct{}
	// suppressUserEcho drops the pre-StreamStarted user-message re-emission
	// of a retried run from the bridged stream — the bubble is already on
	// screen. Cleared by the run's StreamStarted.
	suppressUserEcho atomic.Bool

	// attachedSubagent marks this App as a live viewer of an async subagent's
	// sub-session: the runtime's session driver owns runs and delivery; the
	// App only observes events and hands user input to the runtime.
	attachedSubagent *runtime.SubagentAttachInfo

	startOnce      sync.Once
	subsMu         sync.Mutex
	subs           []chan any
	subscriberDone map[chan any]<-chan struct{}
	fanoutOnce     sync.Once
	busMu          sync.Mutex
	busContext     context.Context //nolint:containedctx // observer bus follows App lifetime, not execution
	busCancel      context.CancelFunc
	busDone        chan struct{}
}

// Opt is an option for creating a new App.
type Opt func(*App)

// WithFirstMessage sets the first message to send.
func WithFirstMessage(msg string) Opt {
	return func(a *App) {
		a.firstMessage = &msg
	}
}

// WithFirstMessageAttachment sets the attachment path for the first message.
func WithFirstMessageAttachment(path string) Opt {
	return func(a *App) {
		a.firstMessageAttach = path
	}
}

// WithExitAfterFirstResponse configures the app to exit after the first assistant response.
func WithExitAfterFirstResponse() Opt {
	return func(a *App) {
		a.exitAfterFirstResponse = true
	}
}

// WithQueuedMessages sets messages to be queued after the first message is sent.
// These messages will be delivered to the TUI as SendMsg events, which the
// chat page will queue and process sequentially after each agent response.
func WithQueuedMessages(msgs []string) Opt {
	return func(a *App) {
		a.queuedMessages = msgs
	}
}

// WithAutomaticTitles requests first-turn titles through the canonical owner.
func WithAutomaticTitles() Opt {
	return func(a *App) { a.titleEnabled = true }
}

// WithTitleGenerator overrides title candidates for in-process sessions.
func WithTitleGenerator(gen *sessiontitle.Generator) Opt {
	return func(a *App) {
		a.titleGen = gen
	}
}

// WithReadOnly marks the session as read-only: the conversation history
// is displayed but no new messages can be sent to the LLM.
func WithReadOnly() Opt {
	return func(a *App) {
		a.readOnly = true
	}
}

// WithSnapshotController plumbs in the [builtins.SnapshotController]
// the App uses to drive /undo, /snapshots, /reset. Pass the same
// controller to the runtime via runtime.WithAutoInjector so the
// instance that captures the checkpoints is the one the TUI commands
// drive. Pass nil (or omit the option) for runtimes that don't capture
// snapshots; the App then reports SnapshotsEnabled()==false and the
// related commands silently no-op.
func WithSnapshotController(c builtins.SnapshotController) Opt {
	return func(a *App) {
		a.snapshotController = c
	}
}

// WithRuntimeServices injects legacy non-execution presentation/configuration
// services while handle execution remains exclusively owned by SessionRuntime.
func WithRuntimeServices(services Services) Opt {
	return func(a *App) { a.runtime = services }
}

func New(ctx context.Context, sessions runtime.SessionRuntime, sess *session.Session, binding runtime.SessionBinding, opts ...Opt) *App {
	return newApp(ctx, sessions, sessionState{session: sess, binding: binding}, false, opts...)
}

// NewResolved attaches presentation to an already committed canonical owner.
// It never hydrates, creates, restores or reconciles that owner's model binding.
func NewResolved(ctx context.Context, sessions runtime.SessionRuntime, committed runtime.CommittedSessionView, opts ...Opt) (*App, error) {
	info, handle := committed.Info, committed.SessionHandle
	activeAgent := info.ActiveAgentName
	if activeAgent == "" {
		activeAgent = info.Binding.AgentName
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if handle == nil || info.Session == nil || info.SessionID == "" || handle.ID() != info.SessionID || info.Session.ID != info.SessionID || info.Binding.AgentName == "" || handle.AgentName() != activeAgent || info.WorkingDir != info.Session.WorkingDir {
		return nil, &runtime.SessionError{Kind: runtime.SessionErrorInvalid, SessionID: info.SessionID, Operation: "resolved_view"}
	}
	return newApp(ctx, sessions, sessionState{session: info.Session, handle: handle, binding: info.Binding}, true, opts...), nil
}

// NewResolvedFromTemplate carries only same-owner immutable presentation
// configuration. It never replays queued input, contexts, buses or request state.
func NewResolvedFromTemplate(ctx context.Context, sessions runtime.SessionRuntime, committed runtime.CommittedSessionView, template *App) (*App, error) {
	if template == nil || sessions == nil || reflect.TypeOf(sessions) != reflect.TypeOf(template.sessions) || !reflect.TypeOf(sessions).Comparable() || sessions != template.sessions {
		return nil, &runtime.SessionError{Kind: runtime.SessionErrorWrongSession, SessionID: committed.Info.SessionID, Operation: "resolved_view_template"}
	}
	options := []Opt{
		WithRuntimeServices(template.runtime),
		WithTitleGenerator(template.titleGen),
		func(a *App) { a.titleEnabled = template.titleEnabled },
		WithSnapshotController(template.snapshotController),
	}
	if committed.Info.Attach != nil {
		attach := *committed.Info.Attach
		if attach.Session != nil {
			attach.Session = attach.Session.Clone()
		}
		options = append(options, WithSubagentAttach(attach))
	}
	if template.readOnly {
		options = append(options, WithReadOnly())
	}
	if template.exitAfterFirstResponse {
		options = append(options, WithExitAfterFirstResponse())
	}
	return NewResolved(ctx, sessions, committed, options...)
}

func newApp(ctx context.Context, sessions runtime.SessionRuntime, initial sessionState, resolved bool, opts ...Opt) *App {
	sess := initial.session
	app := &App{
		ctx:              func() context.Context { return context.WithoutCancel(ctx) },
		runtime:          nil,
		sessions:         sessions,
		currentState:     initial,
		resolvedView:     resolved,
		events:           make(chan any, 128),
		throttleDuration: 50 * time.Millisecond,
	}
	app.initBus(ctx)
	for _, opt := range opts {
		opt(app)
	}
	state := app.state()
	switch {
	case resolved:
		state = initial
	case sessions != nil && sess != nil:
		state.binding = app.resolvedSessionBinding(sess, state.binding)
		state.handle, state.binding, state.err = resolveSessionHandle(ctx, sessions, sess, state.binding)
	case sess != nil:
		state.err = &runtime.SessionError{Kind: runtime.SessionErrorUnsupported, SessionID: sess.ID, Operation: "create_session"}
	}
	app.replaceSessionState(state)

	app.cancelledRequests = make(map[string]struct{})

	return app
}

// resolveSessionHandle reuses the registry's immutable handle when a persisted
// session is loaded again. A new handle is registered only for a genuinely
// unknown ID; stopped, closed, stale, and binding errors remain final.
func resolveSessionHandle(ctx context.Context, sessions runtime.SessionRuntime, sess *session.Session, binding runtime.SessionBinding) (runtime.SessionHandle, runtime.SessionBinding, error) {
	handle, err := sessions.SessionByID(sess.ID)
	if err != nil {
		var sessionErr *runtime.SessionError
		if !errors.As(err, &sessionErr) || sessionErr.Kind != runtime.SessionErrorNotFound {
			return nil, binding, err
		}
		handle, err = sessions.CreateSession(ctx, sess, binding)
		if err != nil {
			return nil, binding, err
		}
	}

	if handle == nil {
		return nil, binding, &runtime.SessionError{Kind: runtime.SessionErrorInvalid, SessionID: sess.ID, Operation: "resolve_session"}
	}
	if hydrator, ok := handle.(runtime.Hydrator); ok {
		if err := hydrator.Hydrate(ctx); err != nil {
			return nil, binding, err
		}
	}
	// The handle owns the final/default agent binding. Normalize to it before
	// looking up persisted metadata so an empty requested binding can never
	// write or decorate the empty override key.
	binding.AgentName = handle.AgentName()
	if ref, ok := sess.AgentModelOverrides[binding.AgentName]; ok {
		binding.Model = ref
	}
	if switcher := handle; switcher != nil && switcher.Metadata().Capabilities.ModelSwitching {
		// Reconciliation is deliberate even for the empty ref: multiple sessions
		// can pin the same in-process agent, so loading a default session must
		// clear an override left by the previously active session.
		if err := switcher.SetModel(ctx, binding.Model); err != nil {
			return nil, binding, fmt.Errorf("reconcile session model override: %w", err)
		}
	}
	return handle, binding, nil
}

func (a *App) resolvedSessionBinding(sess *session.Session, binding runtime.SessionBinding) runtime.SessionBinding {
	if binding.AgentName == "" {
		binding.AgentName = sess.AgentName
	}
	if binding.Model == "" && sess.AgentModelOverrides != nil {
		binding.Model = sess.AgentModelOverrides[binding.AgentName]
	}
	return binding
}

func (s sessionState) operationError(operation string) error {
	if s.err != nil {
		return s.err
	}
	sessionID := ""
	if s.session != nil {
		sessionID = s.session.ID
	}
	return &runtime.SessionError{Kind: runtime.SessionErrorInvalid, SessionID: sessionID, Operation: runtime.SessionOperation(operation)}
}

func (a *App) state() sessionState {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.currentState
}

func (a *App) replaceSessionState(state sessionState) {
	a.projectionMu.Lock()
	defer a.projectionMu.Unlock()
	if state.session != nil && state.handle != nil {
		state.session = state.session.Clone()
	}
	a.presentation.Store(nil)
	a.cancelGeneration++
	a.lifecycleMu.Lock()
	a.projectedRequestID, a.latestRequestID = "", ""
	a.cancelledRequests = make(map[string]struct{})
	a.lifecycleMu.Unlock()
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.currentState = state
}

// Start begins App-owned background event producers. Construction stays cheap
// and side-effect free; embedders call Start when the App enters a managed
// lifecycle.
func (a *App) Start(ctx context.Context) {
	a.startOnce.Do(func() {
		a.initBus(ctx)
		context.AfterFunc(ctx, a.stopBus)
		if a.attachedSubagent == nil && !a.resolvedView {
			a.reloadSubagentTree(ctx)
		}
		// One event source for local runtimes: the session's canonical stream
		// feeds the bus, whoever drives a run — this App, the subagent
		// manager, or the runtime's session handle waking the session.
		a.hubBridged = a.startSessionEventBridge(ctx)
		a.startSubagentTreeBridge(a.busLifetime())
		// Emit startup info (agent, team, tools) through the events channel.
		// This runs in the background so the TUI can start immediately while
		// slow operations (like MCP tool loading) complete asynchronously.
		startupSession := a.Session()
		go func(sess *session.Session) {
			startupEvents := make(chan runtime.Event, 10)
			go func() {
				defer close(startupEvents)
				a.runtime.EmitStartupInfo(ctx, sess, runtime.NewChannelSink(startupEvents))
			}()
			for event := range startupEvents {
				select {
				case a.events <- event:
				case <-ctx.Done():
					return
				}
			}
		}(startupSession)

		// Subscribe to tool list changes so the sidebar updates immediately
		// when an MCP server adds or removes tools outside handle turns.
		a.runtime.OnToolsChanged(func(event runtime.Event) {
			select {
			case a.events <- event:
			case <-ctx.Done():
			}
		})

		// Forward events surfaced from detached background work (token usage
		// from background agent tasks) so the sidebar and agent inspector can
		// account for background agents' context usage.
		a.runtime.OnBackgroundEvent(func(event runtime.Event) {
			select {
			case a.events <- event:
			case <-ctx.Done():
			}
		})
	})
}

type EventCommand func() any

func (a *App) InitialEventCommands() []EventCommand {
	if a.firstMessage == nil {
		return nil
	}

	commands := []EventCommand{
		func() any {
			// Use the shared PrepareUserMessage function for consistent attachment handling
			userMsg, attachedPath := a.prepareFirstMessage(a.ctx(), *a.firstMessage, a.firstMessageAttach)
			if userMsg == nil {
				// Agent-only command with no content - agent switched but no message to send
				return nil
			}
			// Inherit the attachment in any sub-session created by this turn.
			if attachedPath != "" {
				if err := a.EditSession(a.ctx(), runtime.SessionEdit{Kind: runtime.SessionEditAttachment, AttachmentPath: attachedPath}); err != nil {
					return runtime.Error("Failed to attach file: " + err.Error())
				}
			}

			// If the message has multi-content (attachments), we need to handle it specially
			if len(userMsg.Message.MultiContent) > 0 {
				return messages.SendAttachmentMsg{
					Content: userMsg,
				}
			}

			return messages.SendMsg{
				Content: userMsg.Message.Content,
			}
		},
	}

	// Queue additional messages to be sent after the first one.
	// The TUI's message queue will hold them until the agent finishes
	// processing the previous message.
	for _, msg := range a.queuedMessages {
		commands = append(commands, func() any {
			return messages.SendMsg{
				Content: msg,
			}
		})
	}

	return commands
}

// CurrentAgentTools returns the tools available to the current agent.
func (a *App) CurrentAgentTools(ctx context.Context) ([]tools.Tool, error) {
	if h := a.SessionHandle(); h != nil && runtime.IsLocalSessionHandle(h) {
		if provider, ok := a.runtime.(interface {
			AgentTools(context.Context, string) ([]tools.Tool, error)
		}); ok {
			return provider.AgentTools(ctx, h.AgentName())
		}
	}
	info, err := a.InspectTools(ctx)
	return info.Tools, err
}

// agentConfigProvider exposes static configuration for legacy local callers.
type agentConfigProvider interface {
	AgentConfigInfo(ctx context.Context, agentName string) runtime.AgentConfigInfo
}

// AgentConfigInfo returns display-only metadata from the active session's
// owning team, without starting toolsets or exposing the source configuration.
func (a *App) AgentConfigInfo(ctx context.Context, agentName string) runtime.AgentConfigInfo {
	if h := a.SessionHandle(); h != nil {
		if reader, ok := h.(runtime.SessionAgentConfigReader); ok {
			info, err := reader.SessionAgentConfig(ctx, agentName)
			if err == nil && a.SessionHandle() == h && info.SessionID == h.ID() && info.AgentName == agentName {
				return info.Info
			}
		}
		return runtime.AgentConfigInfo{}
	}
	cp, ok := a.runtime.(agentConfigProvider)
	if !ok {
		return runtime.AgentConfigInfo{}
	}
	return cp.AgentConfigInfo(ctx, agentName)
}

// CurrentAgentToolsetStatuses returns lifecycle status for each toolset of
// the active agent.
func (a *App) CurrentAgentToolsetStatuses() []tools.ToolsetStatus {
	if a.SessionHandle() != nil {
		a.commandMetadata.mu.RLock()
		defer a.commandMetadata.mu.RUnlock()
		if a.commandMetadata.handle != a.SessionHandle() {
			return nil
		}
		return slices.Clone(a.commandMetadata.statuses)
	}
	return a.runtime.CurrentAgentToolsetStatuses()
}

// RestartToolset triggers a supervisor-driven restart of the named toolset.
func (a *App) RestartToolset(ctx context.Context, name string) error {
	if h := a.SessionHandle(); h != nil {
		if p, ok := h.(runtime.SessionToolsetController); ok && h.Metadata().Capabilities.ToolsetRestart {
			return p.RestartToolset(ctx, name)
		}
		return runtime.UnsupportedSessionOperation(h.ID(), "restart_toolset")
	}
	return a.runtime.RestartToolset(ctx, name)
}

// agentThinkingLevelsProvider is an optional runtime capability: resolving
// the thinking-effort levels the current agent's active model supports.
// Only the local runtime (which holds the agent and its model config)
// implements it; remote runtimes don't, so /effort argument completion
// simply offers no candidates for them.
type agentThinkingLevelsProvider interface {
	CurrentAgentThinkingLevels(ctx context.Context) []effort.Level
}

// CurrentAgentThinkingLevels returns the handle-authoritative thinking-effort
// levels supported by the bound agent's active model. Legacy local runtimes
// retain the provider fallback for embedders that have not adopted sessions.
func (a *App) CurrentAgentThinkingLevels(ctx context.Context) []effort.Level {
	handle := a.SessionHandle()
	if provider := handle; provider != nil {
		return provider.ThinkingLevels(ctx)
	}
	if handle != nil {
		return slices.Clone(handle.Metadata().ThinkingLevels)
	}
	p, ok := a.runtime.(agentThinkingLevelsProvider)
	if !ok {
		return nil
	}
	return p.CurrentAgentThinkingLevels(ctx)
}

func (a *App) CurrentAgentThinkingLevel(ctx context.Context) effort.Level {
	handle := a.SessionHandle()
	if provider := handle; provider != nil {
		return provider.CurrentThinkingLevel(ctx)
	}
	if handle != nil {
		return handle.Metadata().ThinkingLevel
	}
	return ""
}

// SupportsModelSwitching reports whether the bound handle/transport exposes a
// model-switching capability.
func (a *App) SupportsModelSwitching() bool {
	switcher := a.SessionHandle()
	return switcher != nil && switcher.Metadata().Capabilities.ModelSwitching
}

// AvailableModels returns model choices for the bound handle's pinned agent.
func (a *App) AvailableModels(ctx context.Context) []runtime.ModelChoice {
	switcher := a.SessionHandle()
	if switcher == nil || !switcher.Metadata().Capabilities.ModelSwitching {
		return nil
	}
	currentRef := ""
	var customRefs []string
	state := a.state()
	if state.session != nil {
		currentRef = state.session.AgentModelOverrides[state.binding.AgentName]
		customRefs = state.session.CustomModelsUsed
	}
	return runtime.DecorateModelChoices(switcher.AvailableModels(ctx), currentRef, customRefs)
}

func (a *App) unsupportedSessionOperation(operation runtime.SessionOperation) error {
	sessionID := ""
	if sess := a.Session(); sess != nil {
		sessionID = sess.ID
	}
	return runtime.UnsupportedSessionOperation(sessionID, operation)
}

// RefreshModelsCatalog refreshes discovery for the bound handle when supported.
func (a *App) RefreshModelsCatalog(ctx context.Context) error {
	refresher := a.SessionHandle()
	if refresher == nil {
		return a.unsupportedSessionOperation(runtime.SessionOperationRefreshModels)
	}
	return refresher.RefreshModelsCatalog(ctx)
}

func (a *App) SupportsModelCatalogRefresh() bool {
	handle := a.SessionHandle()
	return handle != nil && handle.Metadata().Capabilities.ModelCatalogRefresh
}

func (a *App) SupportsPause() bool {
	handle := a.SessionHandle()
	return handle != nil && handle.Metadata().Capabilities.Pause
}

func (a *App) TogglePause(ctx context.Context) (bool, error) {
	pauser := a.SessionHandle()
	if pauser == nil {
		return false, a.unsupportedSessionOperation(runtime.SessionOperationPause)
	}
	return pauser.TogglePause(ctx)
}

func (a *App) refreshSessionProjection(ctx context.Context) {
	reader := a.SessionHandle()
	if reader == nil {
		return
	}
	snapshot, err := reader.Snapshot(ctx)
	if err != nil || snapshot == nil {
		return
	}
	a.installSessionView(ctx, reader, snapshot)
}

func (a *App) SupportsSessionEditing() bool {
	handle := a.SessionHandle()
	return handle != nil && handle.Metadata().Capabilities.SessionEditing
}

func (a *App) SetCurrentSessionStarred(ctx context.Context, starred bool) error {
	editor := a.SessionHandle()
	if editor == nil {
		return a.unsupportedSessionOperation(runtime.SessionOperationSetStarred)
	}
	if err := editor.SetStarred(ctx, starred); err != nil {
		return err
	}
	a.refreshSessionProjection(ctx)
	return nil
}

// SetCurrentAgentModel changes the bound handle's model. The handle owns the
// override and its persistence, so the App only refreshes its projection of
// the session and the sidebar's agent info afterwards.
func (a *App) SetCurrentAgentModel(ctx context.Context, modelRef string) error {
	switcher := a.SessionHandle()
	if switcher == nil {
		return a.unsupportedSessionOperation(runtime.SessionOperationSetModel)
	}
	if err := switcher.SetModel(ctx, modelRef); err != nil {
		return err
	}
	a.refreshSessionProjection(ctx)
	a.refreshAgentInfo(ctx)
	return nil
}

func (a *App) SupportsThinkingLevels() bool {
	handle := a.SessionHandle()
	return handle != nil && handle.Metadata().Capabilities.ThinkingLevels
}

// CycleAgentThinkingLevel advances the bound session's pinned agent through
// the handle-owned mutation capability.
func (a *App) CycleAgentThinkingLevel(ctx context.Context) (effort.Level, error) {
	controller := a.SessionHandle()
	if controller == nil {
		return "", a.unsupportedSessionOperation(runtime.SessionOperationThinkingLevel)
	}
	level, err := controller.CycleThinkingLevel(ctx)
	if err == nil {
		a.refreshAgentInfo(ctx)
	}
	return level, err
}

// SetAgentThinkingLevel applies level to the bound session's pinned agent
// through the handle-owned mutation capability.
func (a *App) SetAgentThinkingLevel(ctx context.Context, level effort.Level) (effort.Level, error) {
	controller := a.SessionHandle()
	if controller == nil {
		return "", a.unsupportedSessionOperation(runtime.SessionOperationThinkingLevel)
	}
	applied, err := controller.SetThinkingLevel(ctx, level)
	if err == nil {
		a.refreshAgentInfo(ctx)
	}
	return applied, err
}

func (a *App) refreshAgentInfo(ctx context.Context) {
	emitter := a.SessionHandle()
	if emitter == nil {
		return
	}
	a.pumpToEvents(ctx, func(sink runtime.EventSink) {
		emitter.EmitPinnedAgentInfo(ctx, sink)
	})
}

// CurrentAgentCommands returns the commands for the active agent
func (a *App) CurrentAgentCommands(ctx context.Context) types.Commands {
	if a.SessionHandle() != nil {
		a.commandMetadata.mu.RLock()
		defer a.commandMetadata.mu.RUnlock()
		if a.commandMetadata.handle != a.SessionHandle() {
			return nil
		}
		return a.commandMetadata.commands
	}
	return a.runtime.CurrentAgentInfo(ctx).Commands
}

// CurrentAgentSkillsContext returns the available skills if skills are enabled for the current agent.
func (a *App) CurrentAgentSkillsContext(ctx context.Context) ([]skills.Skill, error) {
	if runner := a.SessionHandle(); runner != nil {
		return runner.Skills(ctx)
	}
	st := a.runtime.CurrentAgentSkillsToolset()
	if st == nil {
		return nil, nil
	}
	return st.Skills(), nil
}

// ResolveSkillCommand checks if the input matches a skill slash command (e.g. /skill-name args).
// If matched, it reads the skill content and returns the resolved prompt. Otherwise returns "".
//
// StartSkillForkOperation dispatches fork-mode skills without polluting the parent transcript.
func (a *App) ResolveSkillCommand(ctx context.Context, input string) (string, error) {
	if runner := a.SessionHandle(); runner != nil {
		return runner.ResolveSkillCommand(ctx, input)
	}
	if !strings.HasPrefix(input, "/") {
		return "", nil
	}

	st := a.runtime.CurrentAgentSkillsToolset()
	if st == nil {
		return "", nil
	}

	cmd, arg, _ := strings.Cut(input[1:], " ")
	arg = strings.TrimSpace(arg)

	for _, skill := range st.Skills() {
		if skill.Name != cmd {
			continue
		}

		if skill.IsFork() {
			// Fall through to ResolveCommand for non-chat callers; the
			// chat layers already routed fork-mode skills via
			// SkillCommandForkResult before reaching this point.
			return "", nil
		}

		// NopRuntime refuses the commands the body embeds instead of running
		// them: input resolution is synchronous inside the UI loop, so there is
		// no way to raise an approval prompt here. Each refusal is inlined in
		// the content the agent receives, and the agent can still run the
		// command itself through its own (gated) shell tool.
		content, err := st.ReadSkillContent(ctx, skill.Name, tools.NopRuntime{})
		if err != nil {
			return "", fmt.Errorf("reading skill %q: %w", skill.Name, err)
		}

		if arg != "" {
			return fmt.Sprintf("Use the following skill.\n\nUser's request: %s\n\n<skill name=%q>\n%s\n</skill>", arg, skill.Name, content), nil
		}
		return fmt.Sprintf("Use the following skill.\n\n<skill name=%q>\n%s\n</skill>", skill.Name, content), nil
	}

	return "", nil
}

// SkillCommandForkResult returns (skillName, task, true) when input is a slash
// command for a `context: fork` skill, otherwise (_, _, false). Chat layers
// must call this before ResolveInputOnce and route to StartSkillForkOperation.
// SkillCommandForkResult performs fork-skill discovery and preserves transport
// errors so callers never silently fall through to a normal message.
func (a *App) SkillCommandForkResult(ctx context.Context, input string) (skillName, task string, ok bool, err error) {
	if !strings.HasPrefix(input, "/") {
		return "", "", false, nil
	}
	if runner := a.SessionHandle(); runner != nil {
		cmd, arg, _ := strings.Cut(input[1:], " ")
		availableSkills, err := runner.Skills(ctx)
		if err != nil {
			return "", "", false, err
		}
		for _, skill := range availableSkills {
			if skill.Name == cmd && skill.IsFork() {
				return skill.Name, strings.TrimSpace(arg), true, nil
			}
		}
		return "", "", false, nil
	}
	st := a.runtime.CurrentAgentSkillsToolset()
	if st == nil {
		return "", "", false, nil
	}
	cmd, arg, _ := strings.Cut(input[1:], " ")
	arg = strings.TrimSpace(arg)
	for _, skill := range st.Skills() {
		if skill.Name == cmd && skill.IsFork() {
			return skill.Name, arg, true, nil
		}
	}
	return "", "", false, nil
}

func (a *App) SupportsForkSkills() bool {
	handle := a.SessionHandle()
	return handle != nil && handle.Metadata().Capabilities.ForkSkills
}

func NewSkillOperationID() string { return uuid.NewString() }

func (a *App) StartSkillForkOperation(ctx context.Context, operationID, skillName, task string) error {
	args := skillstool.RunSkillArgs{Name: skillName, Task: task}
	starter := a.SessionHandle()
	if starter == nil {
		return a.unsupportedSessionOperation(runtime.SessionOperationRunSkill)
	}
	if err := starter.StartSkillFork(ctx, operationID, args); err != nil {
		return err
	}
	return nil
}

type ResolvedInput struct {
	Display   string
	Content   string
	SkillName string
	SkillTask string
	ForkSkill bool
}

// ResolveInputOnce classifies and resolves slash input exactly once.
func (a *App) ResolveInputOnce(ctx context.Context, input string) (ResolvedInput, error) {
	out := ResolvedInput{Display: input, Content: input}
	if name, task, ok, err := a.SkillCommandForkResult(ctx, input); err != nil {
		return out, err
	} else if ok {
		out.SkillName, out.SkillTask, out.ForkSkill = name, task, true
		return out, nil
	}
	if resolved, err := a.ResolveSkillCommand(ctx, input); err != nil {
		return out, err
	} else if resolved != "" {
		out.Content = resolved
		return out, nil
	}
	out.Content = a.ResolveCommand(ctx, input)
	return out, nil
}

// CurrentAgentModel returns the model ID for the current agent.
// Returns the tracked model from AgentInfoEvent, or falls back to session overrides.
// Returns empty string if no model information is available (fail-open scenario).
func (a *App) CurrentAgentModel(ctx context.Context) string {
	if a.currentAgentModel != "" {
		return a.currentAgentModel
	}
	// Fallback to session overrides
	state := a.state()
	if state.session != nil && state.session.AgentModelOverrides != nil {
		agentName := ""
		if state.handle != nil {
			agentName = state.handle.AgentName()
		}
		if modelRef, ok := state.session.AgentModelOverrides[agentName]; ok {
			return modelRef
		}
	}
	return ""
}

// TrackCurrentAgentModel updates the tracked model ID for the current agent.
// This is called when AgentInfoEvent is received from the runtime.
func (a *App) TrackCurrentAgentModel(model string) {
	a.currentAgentModel = model
}

// CurrentMCPPrompts returns the available MCP prompts for the active agent
func (a *App) CurrentMCPPrompts(ctx context.Context) map[string]mcptools.PromptInfo {
	if a.SessionHandle() != nil {
		prompts, _ := a.CachedMCPPrompts()
		return prompts
	}
	return a.runtime.CurrentMCPPrompts(ctx)
}

// ExecuteMCPPrompt executes an MCP prompt with provided arguments and returns the content
func (a *App) ExecuteMCPPrompt(ctx context.Context, promptName string, arguments map[string]string) (string, error) {
	if h := a.SessionHandle(); h != nil {
		if p, ok := h.(runtime.SessionMCPPrompts); ok && h.Metadata().Capabilities.MCPPrompts {
			return p.ExecuteMCPPrompt(ctx, promptName, arguments)
		}
		return "", runtime.UnsupportedSessionOperation(h.ID(), "mcp_prompt")
	}
	return a.runtime.ExecuteMCPPrompt(ctx, promptName, arguments)
}

// ResolveCommand converts /command to its prompt text
func (a *App) prepareFirstMessage(ctx context.Context, input, attachPath string) (*session.Message, string) {
	resolved := a.ResolveCommand(ctx, input)
	messageText, commandPath := cli.ParseAttachCommand(resolved)
	return cli.CreateUserMessageWithAttachment(ctx, messageText, cmp.Or(commandPath, attachPath))
}

func (a *App) ResolveCommand(ctx context.Context, userInput string) string {
	command, rest, ok := a.LookupCommand(ctx, userInput)
	if !ok {
		return userInput
	}
	if command.Instruction == "" {
		return rest
	}
	if rest == "" {
		return command.Instruction
	}
	return command.Instruction + " " + rest
}

// LookupCommand parses userInput as a /command invocation and returns the
// matching command, the trailing arguments, and whether a match was found.
// Callers that want to act on command metadata (for example switching to a
// sub-agent declared via the `agent:` field) should call this before
// ResolveCommand to inspect the raw command.
func (a *App) LookupCommand(ctx context.Context, userInput string) (types.Command, string, bool) {
	if !strings.HasPrefix(userInput, "/") {
		return types.Command{}, "", false
	}
	head, rest, _ := strings.Cut(userInput, " ")
	command, ok := a.CurrentAgentCommands(ctx)[strings.TrimPrefix(head, "/")]
	return command, rest, ok
}

// EmitStartupInfo emits initial agent, team, and toolset information to the provided channel
func (a *App) EmitStartupInfo(ctx context.Context, events chan runtime.Event) {
	state := a.state()
	a.runtime.EmitStartupInfo(ctx, state.session, runtime.NewChannelSink(events))
}

// Run one agent loop
func (a *App) Run(ctx context.Context, cancel context.CancelFunc, message string, attachments []messages.Attachment) {
	a.cancel = cancel
	a.runCancelled.Store(false)
	state := a.state()
	a.startCompatibilityTitle(ctx, state, message)
	msg := runtime.TurnInput{Content: message, GenerateTitle: a.titleEnabled || a.titleGen != nil, TitleGenerator: a.titleGen}
	if len(attachments) > 0 {
		msg.MultiContent = a.buildUserMultiContent(ctx, state.session, message, attachments)
	}
	if state.handle == nil {
		a.sendEvent(ctx, runtime.Error(state.operationError("submit").Error()))
		return
	}
	submission, err := state.handle.Submit(ctx, msg)
	if err != nil {
		a.sendEvent(ctx, runtime.Error(err.Error()))
		return
	}
	a.recordSubmission(submission.TurnID)
}

// buildUserMultiContent assembles the MultiContent parts for a user message
// with attachments. It builds a single text string with the user's message
// and inlined text files — keeping everything in one text block ensures the
// model sees file content together with the message, rather than as separate
// content blocks — followed by any binary parts (images, PDFs, …).
func (a *App) buildUserMultiContent(ctx context.Context, _ *session.Session, message string, attachments []messages.Attachment) []chat.MessagePart {
	var textBuilder strings.Builder
	textBuilder.WriteString(message)

	// binaryParts holds non-text file parts (images, PDFs, etc.)
	var binaryParts []chat.MessagePart

	for _, att := range attachments {
		switch {
		case att.FilePath != "":
			// File-reference attachment: read and classify from disk.
			// Only remember the path on the session when the file actually
			// exists as a regular file — we don't want sub-agents to inherit
			// dangling references to directories or missing paths. The editor
			// resolves @-mentions to absolute paths before this point.
			if a.processFileAttachment(ctx, att, &textBuilder, &binaryParts) {
				if err := a.EditSession(ctx, runtime.SessionEdit{Kind: runtime.SessionEditAttachment, AttachmentPath: att.FilePath}); err != nil {
					a.sendEvent(ctx, runtime.Warning("Failed to remember attachment: "+err.Error(), ""))
				}
			}
		case att.Content != "":
			// Inline content attachment (e.g. pasted text).
			a.processInlineAttachment(att, &textBuilder)
		case len(att.Data) > 0:
			// Binary inline content.
			doc, resizeMeta, procErr := chat.ProcessAttachmentWithMetadata(chat.MessagePart{
				Type: chat.MessagePartTypeDocument,
				Document: &chat.Document{
					Name:     att.Name,
					MimeType: att.MimeType,
					Source: chat.DocumentSource{
						InlineData: att.Data,
					},
				},
			})
			if procErr != nil {
				slog.WarnContext(ctx, "skipping inline attachment: processing failed", "name", att.Name, "error", procErr)
				a.sendEvent(ctx, runtime.Warning(fmt.Sprintf("Skipped attachment %s: %s", att.Name, procErr), ""))
				continue
			}
			if resizeMeta != nil {
				if note := chat.FormatDimensionNote(resizeMeta); note != "" {
					textBuilder.WriteString("\n")
					textBuilder.WriteString(note)
				}
			}
			binaryParts = append(binaryParts, chat.MessagePart{
				Type:     chat.MessagePartTypeDocument,
				Document: &doc,
			})
		default:
			slog.DebugContext(ctx, "skipping attachment with no file path, content, or data", "name", att.Name)
		}
	}

	multiContent := []chat.MessagePart{
		{Type: chat.MessagePartTypeText, Text: textBuilder.String()},
	}
	return append(multiContent, binaryParts...)
}

// processFileAttachment reads a file from disk, classifies it, and either
// appends its text content to textBuilder or adds a binary part to binaryParts.
// Returns true when the path resolved to a real, regular file that we attempted
// to surface to the model — even if the content itself was rejected (too
// large, unsupported MIME, transient read error, etc.). The boolean is meant
// for callers that want to record the path on the session for later reuse by
// sub-agents; we don't want those references to point at directories or
// missing files, but we do want them to cover "the agent has bigger tools
// than us" cases.
func (a *App) processFileAttachment(ctx context.Context, att messages.Attachment, textBuilder *strings.Builder, binaryParts *[]chat.MessagePart) bool {
	absPath := att.FilePath

	fi, err := os.Stat(absPath)
	if err != nil {
		var reason string
		switch {
		case os.IsNotExist(err):
			reason = "file does not exist"
		case os.IsPermission(err):
			reason = "permission denied"
		default:
			reason = fmt.Sprintf("cannot access file: %v", err)
		}
		slog.WarnContext(ctx, "skipping attachment", "path", absPath, "reason", reason)
		a.sendEvent(ctx, runtime.Warning(fmt.Sprintf("Skipped attachment %s: %s", att.Name, reason), ""))
		return false
	}

	if !fi.Mode().IsRegular() {
		slog.WarnContext(ctx, "skipping attachment: not a regular file", "path", absPath, "mode", fi.Mode().String())
		a.sendEvent(ctx, runtime.Warning(fmt.Sprintf("Skipped attachment %s: not a regular file", att.Name), ""))
		return false
	}

	const maxAttachmentSize = 100 * 1024 * 1024 // 100MB
	if fi.Size() > maxAttachmentSize {
		slog.WarnContext(ctx, "skipping attachment: file too large", "path", absPath, "size", fi.Size(), "max", maxAttachmentSize)
		a.sendEvent(ctx, runtime.Warning(fmt.Sprintf("Skipped attachment %s: file too large (max 100MB)", att.Name), ""))
		return true
	}

	mimeType := chat.DetectMimeType(absPath)

	switch {
	case chat.IsTextFile(absPath):
		if fi.Size() > chat.MaxInlineFileSize {
			slog.WarnContext(ctx, "skipping attachment: text file too large to inline", "path", absPath, "size", fi.Size(), "max", chat.MaxInlineFileSize)
			a.sendEvent(ctx, runtime.Warning(fmt.Sprintf("Skipped attachment %s: text file too large to inline (max 5MB)", att.Name), ""))
			return true
		}
		content, err := chat.ReadFileForInline(absPath)
		if err != nil {
			slog.WarnContext(ctx, "skipping attachment: failed to read file", "path", absPath, "error", err)
			a.sendEvent(ctx, runtime.Warning(fmt.Sprintf("Skipped attachment %s: failed to read file", att.Name), ""))
			return true
		}
		textBuilder.WriteString("\n\n")
		textBuilder.WriteString(content)

	case chat.IsSupportedMimeType(mimeType):
		// Route through ProcessAttachmentWithMetadata for normalised Document output.
		// For images this also returns resize metadata used to emit a dimension note.
		doc, resizeMeta, procErr := chat.ProcessAttachmentWithMetadata(chat.MessagePart{
			Type: chat.MessagePartTypeFile,
			File: &chat.MessageFile{Path: absPath, MimeType: mimeType},
		})
		if procErr != nil {
			slog.WarnContext(ctx, "skipping attachment: processing failed", "path", absPath, "error", procErr)
			a.sendEvent(ctx, runtime.Warning(fmt.Sprintf("Skipped attachment %s: %s", att.Name, procErr), ""))
			return true
		}
		// For images, emit a dimension note so the model can map coordinates back to the original.
		if resizeMeta != nil {
			if note := chat.FormatDimensionNote(resizeMeta); note != "" {
				textBuilder.WriteString("\n" + note)
			}
		}
		*binaryParts = append(*binaryParts, chat.MessagePart{
			Type:     chat.MessagePartTypeDocument,
			Document: &doc,
		})

	default:
		slog.WarnContext(ctx, "skipping attachment: unsupported file type", "path", absPath, "mime_type", mimeType)
		a.sendEvent(ctx, runtime.Warning(fmt.Sprintf("Skipped attachment %s: unsupported file type", att.Name), ""))
	}

	return true
}

// SendPresentationEvent sends an event to the TUI, respecting context cancellation to
// avoid blocking on the channel when the consumer has stopped reading.
func (a *App) SendPresentationEvent(ctx context.Context, event any) {
	a.sendEvent(ctx, event)
}

func (a *App) sendEvent(ctx context.Context, event any) {
	select {
	case a.events <- event:
	case <-ctx.Done():
	}
}

// processInlineAttachment handles content that is already in memory (e.g. pasted
// text). The content is appended to textBuilder wrapped in an XML tag for context.
func (a *App) processInlineAttachment(att messages.Attachment, textBuilder *strings.Builder) {
	textBuilder.WriteString("\n\n")
	fmt.Fprintf(textBuilder, "<attached_file path=%q>\n%s\n</attached_file>", att.Name, att.Content)
}

// Retry requests an handle-native retry without appending transcript input.
func (a *App) Retry(ctx context.Context, cancel context.CancelFunc) {
	a.cancel = cancel
	a.runCancelled.Store(false)
	a.suppressUserEcho.Store(true)
	state := a.state()
	if state.handle == nil {
		a.sendEvent(ctx, runtime.Error(state.operationError("retry").Error()))
		return
	}
	submission, err := state.handle.Retry(ctx)
	if err != nil {
		a.sendEvent(ctx, runtime.Error(err.Error()))
		return
	}
	a.recordSubmission(submission.TurnID)
}

// RunWithMessage runs the agent loop with a pre-constructed message.
// This is used for special cases like image attachments.
func (a *App) RunWithMessage(ctx context.Context, cancel context.CancelFunc, msg *session.Message) {
	a.cancel = cancel
	a.runCancelled.Store(false)
	state := a.state()
	a.startCompatibilityTitle(ctx, state, msg.Message.Content)
	input := runtime.TurnInput{Content: msg.Message.Content, MultiContent: msg.Message.MultiContent, GenerateTitle: a.titleEnabled || a.titleGen != nil, TitleGenerator: a.titleGen}
	if state.handle == nil {
		a.sendEvent(ctx, runtime.Error(state.operationError("submit").Error()))
		return
	}
	submission, err := state.handle.Submit(ctx, input)
	if err != nil {
		a.sendEvent(ctx, runtime.Error(err.Error()))
		return
	}
	a.recordSubmission(submission.TurnID)
}

func (a *App) RunBangCommand(ctx context.Context, command string) {
	command = strings.TrimSpace(command)
	if command == "" {
		a.events <- runtime.ShellOutput("Error: empty command")
		return
	}

	shell, argsPrefix := shellpath.DetectShell()
	out, err := exec.CommandContext(ctx, shell, append(argsPrefix, command)...).CombinedOutput()
	output := "$ " + command + "\n" + string(out)
	if err != nil && len(out) == 0 {
		output = "$ " + command + "\nError: " + err.Error()
	}
	a.events <- runtime.ShellOutput(output)
}

type SubscribeOptions struct {
	PreserveSessionMetadata bool
	Ready                   chan<- struct{}
}

func (a *App) Subscribe(ctx context.Context, send func(any), options SubscribeOptions) {
	a.initBus(ctx)
	busCtx := a.busLifetime()
	ch := make(chan any, subscriberBufferSize)
	a.subsMu.Lock()
	if a.subscriberDone == nil {
		a.subscriberDone = make(map[chan any]<-chan struct{})
	}
	a.subscriberDone[ch] = ctx.Done()
	a.subs = append(a.subs, ch)
	a.subsMu.Unlock()
	if options.Ready != nil {
		close(options.Ready)
	}
	defer a.removeSubscriber(ch)

	a.fanoutOnce.Do(a.startFanOut)

	for {
		select {
		case <-ctx.Done():
			return
		case <-busCtx.Done():
			return
		case msg := <-ch:
			if bridged, ok := msg.(SessionEventMsg); ok {
				if bridged.Epoch != 0 && (bridged.Epoch != a.bridgeEpoch.Load() || (a.Session() != nil && bridged.OriginSessionID != a.Session().ID)) {
					continue
				}
				if !options.PreserveSessionMetadata {
					msg = bridged.Event
				}
			}
			send(msg)
		}
	}
}

// Deprecated: use Subscribe. It preserves the legacy unwrapped event
// semantics required by older embedders.
func (a *App) SubscribeWith(ctx context.Context, send func(tea.Msg)) {
	a.Subscribe(ctx, func(msg any) { send(msg) }, SubscribeOptions{})
}

const subscriberBufferSize = 1024

func (a *App) removeSubscriber(ch chan any) {
	a.subsMu.Lock()
	defer a.subsMu.Unlock()
	a.subs = slices.DeleteFunc(a.subs, func(c chan any) bool { return c == ch })
	delete(a.subscriberDone, ch)
}

// startFanOut keeps bounded, cancellation-aware delivery through the last fanout.
// A slow consumer backpressures observation; any upstream journal gap then goes
// through the same authoritative Reset path instead of silently losing deltas.
func (a *App) startFanOut() {
	busCtx := a.busLifetime()
	throttled := a.throttleEvents(busCtx, a.events)
	a.busMu.Lock()
	done := a.busDone
	a.busMu.Unlock()
	go func() {
		defer close(done)
		for msg := range throttled {
			a.subsMu.Lock()
			subs := slices.Clone(a.subs)
			cancellations := make([]<-chan struct{}, len(subs))
			for i, ch := range subs {
				cancellations[i] = a.subscriberDone[ch]
			}
			a.subsMu.Unlock()
			for i, ch := range subs {
				canceled := cancellations[i]

				select {
				case ch <- msg:
				case <-canceled:
				case <-busCtx.Done():
					return
				}
			}
		}
	}()
}

// SteerMessage resolves attachments into message parts and queues the result
// for mid-turn injection into the running agent. The runtime appends the
// message to the session (and emits the matching UserMessageEvent) when the
// agent loop drains it.
func (a *App) SteerMessage(ctx context.Context, content string, attachments []messages.Attachment) (runtime.Submission, error) {
	state := a.state()
	if state.handle == nil {
		return runtime.Submission{}, state.operationError("send")
	}
	input := runtime.TurnInput{Content: content}
	if len(attachments) > 0 {
		input.MultiContent = a.buildUserMultiContent(ctx, state.session, content, attachments)
	}
	return state.handle.Steer(ctx, input)
}

// FollowUpMessage resolves attachments and queues a message for a separate turn
// after the current agent turn finishes.
func (a *App) FollowUpMessage(ctx context.Context, content string, attachments []messages.Attachment) (runtime.Submission, error) {
	state := a.state()
	if state.handle == nil {
		return runtime.Submission{}, state.operationError("submit")
	}
	input := runtime.TurnInput{Content: content}
	if len(attachments) > 0 {
		input.MultiContent = a.buildUserMultiContent(ctx, state.session, content, attachments)
	}
	submission, err := state.handle.Submit(ctx, input)
	if err == nil {
		a.recordSubmission(submission.TurnID)
	}
	return submission, err
}

func (a *App) fenceSessionBridge() {
	a.bridgeMu.Lock()
	defer a.bridgeMu.Unlock()
	a.bridgeEpoch.Add(1)
	if a.stopBridge != nil {
		a.stopBridge()
		a.stopBridge = nil
	}
	a.hubBridged = false
}

// SwitchAgentTransactional stages the fresh handle without activating its event
// bridge. The caller fences/persists routing first, then invokes commit.
func (a *App) SwitchAgentTransactional(ctx context.Context, target string) (commit func(), rollback func() error, err error) {
	if a.attachedSubagent != nil {
		return nil, nil, &runtime.SessionError{Kind: runtime.SessionErrorUnsupported, SessionID: a.Session().ID, Operation: "switch_attached_agent"}
	}
	state := a.state()
	sess, handle := state.session, state.handle
	if sess == nil || handle == nil {
		return nil, nil, state.operationError("switch_agent")
	}
	switcher, ok := a.sessions.(runtime.AgentSwitcher)
	if !ok {
		return nil, nil, runtime.ErrUnsupported
	}
	newSession, cloned, err := switcher.SwitchAgent(ctx, sess.ID, target)
	if err != nil {
		return nil, nil, err
	}
	if cloned == nil || newSession == nil {
		return nil, nil, &runtime.SessionError{Kind: runtime.SessionErrorInvalid, SessionID: sess.ID, Operation: "switch_agent"}
	}
	binding := runtime.SessionBinding{AgentName: newSession.AgentName(), Model: newSession.Metadata().Model, Durability: state.binding.Durability}
	a.fenceSessionBridge()
	a.replaceSessionState(sessionState{session: cloned, handle: newSession, binding: binding})
	a.firstMessage = nil
	a.firstMessageAttach = ""
	activate := func() {
		a.hubBridged = a.startSessionEventBridge(ctx)
		a.reEmitStartupInfo(ctx)
	}
	var once sync.Once
	undo := func() error {
		var rollbackErr error
		once.Do(func() {
			a.replaceSessionState(state)
			a.hubBridged = a.startSessionEventBridge(ctx)
			a.reEmitStartupInfo(ctx)
			if a.sessions != nil {
				rollbackErr = a.sessions.DeleteSession(context.WithoutCancel(ctx), cloned.ID)
			}
		})
		return rollbackErr
	}
	return activate, undo, nil
}

func (a *App) NewSession() {
	if a.attachedSubagent != nil {
		return // an attached viewer cannot swap out the subagent's session
	}
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	// Preserve user-controlled session flags
	// so they don't reset to default on /new
	var opts []session.Opt
	var priorPolicy session.SafetyPolicy
	current := a.state()
	if current.session != nil {
		opts = append(opts,
			session.WithToolsApproved(current.session.ToolsApproved),
			session.WithSafetyPolicy(current.session.GetSafetyPolicy()),
			session.WithHideToolResults(current.session.HideToolResults),
			session.WithWorkingDir(current.session.WorkingDir),
		)
		priorPolicy = current.session.GetPriorSafetyPolicy()
	}
	sess := session.New(opts...)
	sess.PriorSafetyPolicy = priorPolicy
	state := sessionState{session: sess, binding: a.resolvedSessionBinding(sess, current.binding)}
	if a.sessions != nil {
		state.handle, state.binding, state.err = resolveSessionHandle(a.ctx(), a.sessions, sess, state.binding)
	}
	a.replaceSessionState(state)
	// Clear first message so it won't be re-sent on re-init
	a.firstMessage = nil
	a.firstMessageAttach = ""
	a.hubBridged = a.startSessionEventBridge(a.ctx())

	// Re-emit startup info so the sidebar shows agent/tools info in the new session
	a.reEmitStartupInfo(a.ctx())
}

// reEmitStartupInfo resets and re-emits startup info (agent, team, tools)
// through the events channel so the sidebar updates.
func (a *App) reEmitStartupInfo(ctx context.Context) {
	if a.runtime == nil {
		return
	}
	a.runtime.ResetStartupInfo()
	state := a.state()
	sess := state.session
	a.pumpToEvents(ctx, func(sink runtime.EventSink) {
		a.runtime.EmitStartupInfo(ctx, sess, sink)
	})
}

// pumpToEvents runs emit (which produces events into the supplied sink) in a
// background goroutine and forwards everything it emits to the app's events
// channel, without blocking the caller.
func (a *App) pumpToEvents(ctx context.Context, emit func(runtime.EventSink)) {
	go func() {
		ch := make(chan runtime.Event, 10)
		go func() {
			defer close(ch)
			emit(runtime.NewChannelSink(ch))
		}()
		for event := range ch {
			select {
			case a.events <- event:
			case <-ctx.Done():
				return
			default:
			}
		}
	}()
}

func (a *App) Session() *session.Session {
	return a.state().session
}

// contextBreakdownProvider is a compatibility capability for runtimes that
// compute context directly rather than exposing session context operations.
type contextBreakdownProvider interface {
	ContextBreakdown(ctx context.Context, sess *session.Session) (*runtime.ContextBreakdown, error)
}

// generatedFileResolver is an optional runtime capability: resolving one
// recorded generated-media reference to its bytes and validated canonical
// path, gated on the generated-media manifest and the owning session's
// workspace (see [runtime.LocalRuntime.ResolveGeneratedFile]). Both
// local and remote session handles implement it, keeping retrieval scoped to
// the canonical viewing session.
type generatedFileResolver interface {
	ResolveGeneratedFile(ctx context.Context, ref runtime.GeneratedFileRef) (*runtime.ResolvedGeneratedFile, error)
}

// CanResolveGeneratedFiles reports whether the runtime can resolve
// generated-media references at all, letting UIs skip resolution work
// entirely on runtimes without the capability.
func (a *App) CanResolveGeneratedFiles() bool {
	if handle := a.SessionHandle(); handle != nil {
		_, ok := handle.(generatedFileResolver)
		return ok
	}
	_, ok := a.runtime.(generatedFileResolver)
	return ok
}

// ResolveGeneratedFile resolves one recorded generated-media reference.
// Returns an error wrapping [runtime.ErrUnsupported] when the runtime does
// not support generated media. Treat errors as "unavailable", never user text.
func (a *App) ResolveGeneratedFile(ctx context.Context, ref runtime.GeneratedFileRef) (*runtime.ResolvedGeneratedFile, error) {
	if handle := a.SessionHandle(); handle != nil {
		if resolver, ok := handle.(generatedFileResolver); ok {
			return resolver.ResolveGeneratedFile(ctx, ref)
		}
		return nil, fmt.Errorf("generated file resolution: %w", runtime.ErrUnsupported)
	}
	resolver, ok := a.runtime.(generatedFileResolver)
	if !ok {
		return nil, fmt.Errorf("generated file resolution: %w", runtime.ErrUnsupported)
	}
	return resolver.ResolveGeneratedFile(ctx, ref)
}

// ContextBreakdown returns the estimated context-window composition for the
// current session. Returns an error wrapping [runtime.ErrUnsupported] when
// the bound handle/runtime does not advertise context inspection.
func (a *App) ContextBreakdown(ctx context.Context) (*runtime.ContextBreakdown, error) {
	if inspector := a.SessionHandle(); inspector != nil {
		return inspector.ContextBreakdown(ctx)
	}
	cp, ok := a.runtime.(contextBreakdownProvider)
	if !ok {
		return nil, fmt.Errorf("context breakdown: %w", runtime.ErrUnsupported)
	}
	return cp.ContextBreakdown(ctx, a.Session())
}

// liveSessionLister is an optional presentation capability for listing the
// current root session plus every live sub-agent session in the team view.
type liveSessionLister interface {
	LiveSessions(ctx context.Context, current *session.Session) []runtime.LiveSession
}

// LiveSessions returns the current root session plus every currently live
// sub-agent session (foreground children and background agent tasks), or nil
// when the runtime does not expose live-session tracking.
func (a *App) LiveSessions(ctx context.Context) []runtime.LiveSession {
	if inspector := a.SessionHandle(); inspector != nil {
		rows, err := inspector.LiveSessions(ctx)
		if err == nil {
			return rows
		}
		return nil
	}
	lister, ok := a.runtime.(liveSessionLister)
	if !ok {
		return nil
	}
	return lister.LiveSessions(ctx, a.Session())
}

// liveSessionCompactor is an optional runtime capability: queueing an
// explicit compaction of one live session onto that session's own run loop.
type liveSessionCompactor interface {
	CompactLiveSession(ctx context.Context, sessionID, additionalPrompt string, events runtime.EventSink) error
}

// CompactLiveSession queues a manual compaction for the identified live
// session and bridges the resulting compaction/usage events into the app's
// event stream. The request executes on the target session's own run loop at
// a safe iteration boundary; neither the root stream nor the target stream
// is cancelled. Returns an error wrapping [runtime.ErrUnsupported] when the
// runtime cannot target live sessions (e.g. remote runtimes), or the
// runtime's rejection for unknown/finished sessions and duplicate requests.
func (a *App) CompactLiveSession(ctx context.Context, sessionID, additionalPrompt string) error {
	sink := runtime.EventSinkFunc(func(event runtime.Event) {
		a.sendEvent(ctx, event)
	})
	if compactor := a.SessionHandle(); compactor != nil {
		return compactor.CompactTarget(ctx, sessionID, additionalPrompt, sink)
	}
	compactor, ok := a.runtime.(liveSessionCompactor)
	if !ok {
		return fmt.Errorf("targeted session compaction: %w", runtime.ErrUnsupported)
	}
	return compactor.CompactLiveSession(ctx, sessionID, additionalPrompt, sink)
}

// CompactSession requests manual compaction from the bound handle and bridges
// its canonical events through the normal app event sink.
func (a *App) CompactSession(ctx context.Context, additionalPrompt string) error {
	compactor := a.SessionHandle()
	if compactor == nil {
		return fmt.Errorf("session compaction: %w", runtime.ErrUnsupported)
	}
	if runtime.IsLocalSessionHandle(compactor) {
		// Local handles journal events themselves; the observation bridge emits
		// them exactly once.
		return compactor.Compact(ctx, additionalPrompt, nil)
	}
	sink := runtime.EventSinkFunc(func(event runtime.Event) { a.sendEvent(ctx, event) })
	return compactor.Compact(ctx, additionalPrompt, sink)
}

// AttachedFiles returns the current session's attached file paths, the same
// inventory DropAttachedFile resolves against. Backs /drop argument
// completion; nil when there is no active session. The canonical owner
// retains attachment inventory for subsequent delegation and prompts.
func (a *App) AttachedFiles() []string {
	sess := a.Session()
	if sess == nil {
		return nil
	}
	return sess.AttachedFilesSnapshot()
}

// DropAttachedFile removes a file from the current session's attached files
// and returns the absolute path that was dropped. The path is resolved
// against the session's attachment list: exact match first, then the
// absolute form of a relative path, then a unique base-name match. Dropping
// stops the file from being propagated to future sub-agent delegations and
// skill prompts; content already inlined in past messages is unaffected.
func (a *App) DropAttachedFile(ctx context.Context, path string) (string, error) {
	sess := a.Session()
	if sess == nil {
		return "", errors.New("no active session")
	}
	resolved, err := resolveAttachedFile(sess.AttachedFilesSnapshot(), path)
	if err != nil {
		return "", err
	}
	if editor := a.SessionHandle(); editor != nil {
		if err := editor.RemoveAttachment(ctx, resolved); err != nil {
			return "", err
		}
		a.refreshSessionProjection(ctx)
		return resolved, nil
	}
	return "", a.unsupportedSessionOperation(runtime.SessionOperationRemoveAttachment)
}

// resolveAttachedFile maps user input to one recorded attachment path.
func resolveAttachedFile(attached []string, path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("no file specified")
	}
	if len(attached) == 0 {
		return "", errors.New("no files are attached to this session")
	}
	if slices.Contains(attached, path) {
		return path, nil
	}
	if abs, err := filepath.Abs(path); err == nil && slices.Contains(attached, abs) {
		return abs, nil
	}
	var matches []string
	for _, candidate := range attached {
		if filepath.Base(candidate) == path {
			matches = append(matches, candidate)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("file is not attached to this session: %s", path)
	default:
		return "", fmt.Errorf("%q matches %d attached files, use the full path", path, len(matches))
	}
}

// Runtime returns presentation-only services. Session execution capabilities are
// exposed separately by SessionRuntime and must never be inferred from Services.
func (a *App) Runtime() Services {
	return a.runtime
}

// SessionHandle returns the current immutable session handle for lifecycle-safe
// ownership transfer. Callers must not infer registry ownership from it.
func (a *App) SessionHandle() runtime.SessionHandle {
	return a.state().handle
}

// SessionRuntime returns the shared handle registry injected at construction.
func (a *App) SessionRuntime() runtime.SessionRuntime { return a.sessions }

// Binding returns the immutable session binding used to create the handle.
func (a *App) Binding() runtime.SessionBinding { return a.state().binding }

// PermissionsInfo returns combined permissions info from team and session.
// Returns nil if no permissions are configured at either level.
func (a *App) PermissionsInfo() *runtime.PermissionsInfo {
	state := a.state()
	// Get team-level permissions from runtime
	teamPerms := a.runtime.PermissionsInfo()

	// Get session-level permissions
	var sessionPerms *runtime.PermissionsInfo
	if state.session != nil && state.session.Permissions != nil {
		if len(state.session.Permissions.Allow) > 0 || len(state.session.Permissions.Ask) > 0 || len(state.session.Permissions.Deny) > 0 {
			sessionPerms = &runtime.PermissionsInfo{
				Allow: state.session.Permissions.Allow,
				Ask:   state.session.Permissions.Ask,
				Deny:  state.session.Permissions.Deny,
			}
		}
	}

	// Return nil if no permissions configured at any level
	if teamPerms == nil && sessionPerms == nil {
		return nil
	}

	// Merge permissions, with session taking priority (listed first)
	result := &runtime.PermissionsInfo{}
	if sessionPerms != nil {
		result.Allow = append(result.Allow, sessionPerms.Allow...)
		result.Ask = append(result.Ask, sessionPerms.Ask...)
		result.Deny = append(result.Deny, sessionPerms.Deny...)
	}
	if teamPerms != nil {
		result.Allow = append(result.Allow, teamPerms.Allow...)
		result.Ask = append(result.Ask, teamPerms.Ask...)
		result.Deny = append(result.Deny, teamPerms.Deny...)
	}

	return result
}

// HasPermissions returns true if any permissions are configured (team or session level).
func (a *App) HasPermissions() bool {
	return a.PermissionsInfo() != nil
}

// ShouldExitAfterFirstResponse returns true if the app is configured to exit
// after the first assistant response completes.
func (a *App) ShouldExitAfterFirstResponse() bool {
	return a.exitAfterFirstResponse
}

// MarkRunCancelled mutes bridged events for the cancelled in-flight turn
// (all but the stream stop) so the UI freezes immediately while the run
// winds down, matching the classic drop-on-cancel behavior. Cleared on the
// next Run.
func (a *App) MarkRunCancelled() {
	a.lifecycleMu.Lock()
	if a.latestRequestID != "" {
		a.cancelledRequests[a.latestRequestID] = struct{}{}
	}
	a.lifecycleMu.Unlock()
	a.runCancelled.Store(true)
}

func (a *App) recordSubmission(requestID string) {
	if requestID == "" {
		return
	}
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	a.latestRequestID = requestID
}

// CancelRun cancels the current run without ending the pinned handle lifetime.
// It returns whether a run was active.
func (a *App) CancelRun() runtime.CancelOutcome {
	return a.CaptureCancelRun()()
}

// CaptureCancelRun captures the current canonical turn and handle together.
// A delayed UI confirmation never retargets a replacement session or turn.
func (a *App) CaptureCancelRun() func() runtime.CancelOutcome {
	a.projectionMu.Lock()
	state := a.state()
	generation := a.cancelGeneration
	turnID := ""
	if projection := a.presentation.Load(); projection != nil {
		turnID = projection.Status.TurnID
	}
	a.projectionMu.Unlock()
	return func() runtime.CancelOutcome {
		return a.cancelTurn(state, turnID, generation)
	}
}

func (a *App) cancelTurn(state sessionState, turnID string, generation uint64) runtime.CancelOutcome {
	a.projectionMu.Lock()
	current := a.state()
	projection := a.presentation.Load()
	valid := a.cancelGeneration == generation && current.handle == state.handle && projection != nil && projection.Status.TurnID == turnID
	a.projectionMu.Unlock()
	if !valid || state.handle == nil || turnID == "" {
		return runtime.CancelNotActive
	}
	result, err := state.handle.Cancel(a.ctx(), turnID)
	if err != nil {
		return runtime.CancelNotActive
	}
	if result.Outcome == runtime.CancelAccepted || result.Outcome == runtime.CancelAlreadyCancelling {
		a.projectionMu.Lock()
		if current := a.state(); current.handle == state.handle {
			a.lifecycleMu.Lock()
			a.cancelledRequests[turnID] = struct{}{}
			a.lifecycleMu.Unlock()
		}
		a.projectionMu.Unlock()
	}
	return result.Outcome
}

// IsReadOnly returns true when the session is in read-only mode and no new
// messages should be sent to the LLM.
func (a *App) IsReadOnly() bool {
	return a.readOnly
}

func (a *App) PlainTextTranscript() string {
	return transcript.PlainText(a.Session())
}

// SessionStore returns the session store for browsing/loading sessions.
// Returns nil if no session store is configured.
func (a *App) SessionStore() session.Store {
	return a.runtime.SessionStore()
}

// ReplaceSession replaces the current session with the given session.
// This is used when loading a past session. It also re-emits startup info
// so the sidebar displays the agent and tool information.
// If the session has stored model overrides, they are applied to the runtime.
func (a *App) ReplaceSession(ctx context.Context, sess *session.Session) {
	if a.attachedSubagent != nil {
		return // an attached viewer cannot swap out the subagent's session
	}
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	current := a.state()
	binding := runtime.SessionBinding{
		AgentName:  sess.AgentName,
		Durability: current.binding.Durability,
	}
	binding.Model = sess.AgentModelOverrides[binding.AgentName]
	var handle runtime.SessionHandle
	var sessionErr error
	if a.sessions != nil {
		handle, binding, sessionErr = resolveSessionHandle(ctx, a.sessions, sess, binding)
	}
	a.replaceSessionState(sessionState{session: sess, handle: handle, binding: binding, err: sessionErr})
	// Clear first message so it won't be re-sent on re-init
	a.firstMessage = nil
	a.firstMessageAttach = ""
	// Hydrate the loaded session's subagent view from the subagent store
	// before the TUI components read it.
	a.reloadSubagentTree(ctx)
	a.hubBridged = a.startSessionEventBridge(ctx)

	// Reset and re-emit startup info so the sidebar shows agent/tools info
	a.reEmitStartupInfo(ctx)

	// If this runtime is still driving subagents for the loaded session (an
	// in-process switch back), push the live swarm snapshot: the sidebar just
	// restored the persisted tree with active nodes marked stopped.
	a.emitLiveSubagentTree(ctx)
}

// throttleEvents buffers and merges rapid events to prevent UI flooding
func (a *App) throttleEvents(ctx context.Context, in <-chan any) <-chan any {
	out := make(chan any, 128)

	go func() {
		defer close(out)

		var buffer []any
		var timerCh <-chan time.Time

		flush := func() {
			for _, msg := range a.mergeEvents(buffer) {
				select {
				case out <- msg:
				case <-ctx.Done():
					return
				}
			}
			buffer = buffer[:0]
			timerCh = nil
		}

		for {
			select {
			case <-ctx.Done():
				return

			case msg, ok := <-in:
				if !ok {
					flush()
					return
				}

				buffer = append(buffer, msg)
				if a.shouldThrottle(msg) {
					if timerCh == nil {
						timerCh = time.After(a.throttleDuration)
					}
				} else {
					flush()
				}

			case <-timerCh:
				flush()
			}
		}
	}()

	return out
}

// shouldThrottle determines if an event should be buffered/throttled
func (a *App) shouldThrottle(msg any) bool {
	if wrapped, ok := msg.(SessionEventMsg); ok {
		msg = wrapped.Event
	}
	switch msg.(type) {
	case *runtime.AgentChoiceEvent:
		return true
	case *runtime.AgentChoiceReasoningEvent:
		return true
	case *runtime.PartialToolCallEvent:
		return true
	case *runtime.ToolCallOutputEvent:
		return true
	default:
		return false
	}
}

// mergeEvents merges consecutive similar events to reduce UI updates.
//
// Each merge group is built with a single strings.Builder so concatenating N
// chunks costs O(N) instead of the O(N^2) the naive `merged.Content + next.Content`
// pattern produces. This matters during fast LLM streams where dozens of
// chunks land per throttle window.
func (a *App) mergeEvents(events []any) []any {
	if len(events) == 0 {
		return events
	}

	result := make([]any, 0, len(events))

	for i := 0; i < len(events); i++ {
		if first, ok := events[i].(SessionEventMsg); ok {
			unwrapped := []any{first.Event}
			for _, msg := range events[i+1:] {
				next, ok := msg.(SessionEventMsg)
				if !ok || next.OriginSessionID != first.OriginSessionID || next.TurnID != first.TurnID || next.Epoch != first.Epoch || next.Seed != first.Seed {
					break
				}
				unwrapped = append(unwrapped, next.Event)
			}
			// Canonical cursor/gap handling and App projection precede this
			// presentation-only merge. Preserve the newest projected head.
			for j := 0; j < len(unwrapped); j++ {
				merged, consumed := mergeEventRun(unwrapped[j], unwrapped[j+1:])
				last := events[i+j+consumed].(SessionEventMsg)
				last.Event, _ = merged.(runtime.Event)
				result = append(result, last)
				j += consumed
			}
			i += len(unwrapped) - 1
			continue
		}
		merged, consumed := mergeEventRun(events[i], events[i+1:])
		result = append(result, merged)
		i += consumed
	}

	return result
}

func mergeEventRun(first any, rest []any) (any, int) {
	switch ev := first.(type) {
	case *runtime.AgentChoiceEvent:
		return mergeAgentChoiceRun(ev, rest)
	case *runtime.AgentChoiceReasoningEvent:
		return mergeAgentChoiceReasoningRun(ev, rest)
	case *runtime.PartialToolCallEvent:
		return mergePartialToolCallRun(ev, rest)
	case *runtime.ToolCallOutputEvent:
		return mergeToolCallOutputRun(ev, rest)
	default:
		return first, 0
	}
}

// mergeAgentChoiceRun merges first with any directly-following AgentChoiceEvents
// for the same agent. It returns the merged event and the number of follow-up
// events that were consumed.
func mergeAgentChoiceRun(first *runtime.AgentChoiceEvent, rest []any) (*runtime.AgentChoiceEvent, int) {
	n := 0
	total := len(first.Content)
	for _, msg := range rest {
		next, ok := msg.(*runtime.AgentChoiceEvent)
		if !ok || next.Type != first.Type || next.AgentName != first.AgentName || next.SessionID != first.SessionID {
			break
		}
		total += len(next.Content)
		n++
	}
	if n == 0 {
		return first, 0
	}

	var b strings.Builder
	b.Grow(total)
	b.WriteString(first.Content)
	for _, msg := range rest[:n] {
		b.WriteString(msg.(*runtime.AgentChoiceEvent).Content)
	}
	return &runtime.AgentChoiceEvent{
		Type:         first.Type,
		Content:      b.String(),
		SessionID:    first.SessionID,
		AgentContext: first.AgentContext,
	}, n
}

// mergeAgentChoiceReasoningRun is the AgentChoiceReasoningEvent counterpart of
// mergeAgentChoiceRun.
func mergeAgentChoiceReasoningRun(first *runtime.AgentChoiceReasoningEvent, rest []any) (*runtime.AgentChoiceReasoningEvent, int) {
	n := 0
	total := len(first.Content)
	for _, msg := range rest {
		next, ok := msg.(*runtime.AgentChoiceReasoningEvent)
		if !ok || next.Type != first.Type || next.AgentName != first.AgentName || next.SessionID != first.SessionID {
			break
		}
		total += len(next.Content)
		n++
	}
	if n == 0 {
		return first, 0
	}

	var b strings.Builder
	b.Grow(total)
	b.WriteString(first.Content)
	for _, msg := range rest[:n] {
		b.WriteString(msg.(*runtime.AgentChoiceReasoningEvent).Content)
	}
	return &runtime.AgentChoiceReasoningEvent{
		Type:         first.Type,
		Content:      b.String(),
		SessionID:    first.SessionID,
		AgentContext: first.AgentContext,
	}, n
}

// mergeToolCallOutputRun merges output chunks across consecutive
// ToolCallOutputEvents that share the same tool call ID.
func mergeToolCallOutputRun(first *runtime.ToolCallOutputEvent, rest []any) (*runtime.ToolCallOutputEvent, int) {
	n := 0
	total := len(first.Output)
	for _, msg := range rest {
		next, ok := msg.(*runtime.ToolCallOutputEvent)
		if !ok || next.Type != first.Type || next.AgentName != first.AgentName || next.ToolCallID != first.ToolCallID {
			break
		}
		total += len(next.Output)
		n++
	}
	if n == 0 {
		return first, 0
	}

	var b strings.Builder
	b.Grow(total)
	b.WriteString(first.Output)
	for _, msg := range rest[:n] {
		b.WriteString(msg.(*runtime.ToolCallOutputEvent).Output)
	}
	return &runtime.ToolCallOutputEvent{
		Type:           first.Type,
		ToolCallID:     first.ToolCallID,
		ToolDefinition: first.ToolDefinition,
		Output:         b.String(),
		AgentContext:   first.AgentContext,
	}, n
}

// mergePartialToolCallRun merges argument deltas across consecutive
// PartialToolCallEvents that share the same tool call ID.
func mergePartialToolCallRun(first *runtime.PartialToolCallEvent, rest []any) (*runtime.PartialToolCallEvent, int) {
	n := 0
	total := len(first.ToolCall.Function.Arguments)
	for _, msg := range rest {
		next, ok := msg.(*runtime.PartialToolCallEvent)
		if !ok || next.Type != first.Type || next.AgentName != first.AgentName || next.ToolCall.ID != first.ToolCall.ID || next.ToolCall.Type != first.ToolCall.Type {
			break
		}
		total += len(next.ToolCall.Function.Arguments)
		n++
	}
	if n == 0 {
		return first, 0
	}

	var b strings.Builder
	b.Grow(total)
	b.WriteString(first.ToolCall.Function.Arguments)

	name := first.ToolCall.Function.Name
	toolDef := first.ToolDefinition
	for _, msg := range rest[:n] {
		next := msg.(*runtime.PartialToolCallEvent)
		b.WriteString(next.ToolCall.Function.Arguments)
		// The function name is sometimes only present on later deltas; keep the
		// first non-empty value we observe across the run.
		name = cmp.Or(name, next.ToolCall.Function.Name)
		toolDef = cmp.Or(toolDef, next.ToolDefinition)
	}
	return &runtime.PartialToolCallEvent{
		Type: first.Type,
		ToolCall: tools.ToolCall{
			ID:   first.ToolCall.ID,
			Type: first.ToolCall.Type,
			Function: tools.FunctionCall{
				Name:      name,
				Arguments: b.String(),
			},
		},
		ToolDefinition: toolDef,
		AgentContext:   first.AgentContext,
	}, n
}

// ExportHTML exports the current session as a standalone HTML file.
// If filename is empty, a default name based on the session title and timestamp is used.
func (a *App) ExportHTML(ctx context.Context, filename string) (string, error) {
	state := a.state()
	var description string
	if state.handle != nil {
		if provider, ok := state.handle.(runtime.SessionAgentInfoProvider); ok {
			info, err := provider.SessionAgentInfo(ctx)
			if err != nil {
				return "", err
			}
			if info.Agent != nil && info.Agent.AgentName == state.handle.AgentName() {
				description = info.Agent.Description
			}
		}
	} else if a.runtime != nil {
		description = a.runtime.CurrentAgentInfo(ctx).Description
	}
	return export.SessionToFile(state.session, description, filename)
}

// ErrTitleGenerating is returned when attempting to set a title while generation is in progress.
var ErrTitleGenerating = errors.New("title generation in progress, please wait")

// UpdateSessionTitle updates the current session's title and persists it.
// It works with both local and remote runtimes.
func (a *App) UpdateSessionTitle(ctx context.Context, title string) error {
	state := a.state()
	if state.session == nil {
		return errors.New("no active session")
	}

	// Prevent manual title edits while generation is in progress
	if a.titleGenerating.Load() {
		return ErrTitleGenerating
	}

	// Route the intent through the handle so persistence, replay, and all
	// observers see one canonical title event.
	if state.handle == nil {
		return state.operationError("update_title")
	}
	if err := state.handle.UpdateTitle(ctx, title); err != nil {
		return fmt.Errorf("failed to update session title: %w", err)
	}
	return nil
}

// IsTitleGenerating returns true if title generation is currently in progress.
func (a *App) IsTitleGenerating() bool {
	return a.titleGenerating.Load()
}

// generateTitle generates a title using the local title generator.
// This method always clears the titleGenerating flag when done (success or failure).
// It should be called in a goroutine.
func (a *App) startCompatibilityTitle(ctx context.Context, state sessionState, message string) {
	if _, owned := state.handle.(runtime.SessionTitleGenerator); owned {
		return
	}
	if state.session != nil && state.session.TitleSnapshot() == "" && a.titleGen != nil && a.titleGenerating.CompareAndSwap(false, true) {
		go a.generateTitle(ctx, state, []string{message})
	}
}

func (a *App) generateTitle(ctx context.Context, state sessionState, userMessages []string) {
	// Always clear the flag when done, whether success or failure
	defer a.titleGenerating.Store(false)

	if a.titleGen == nil || state.handle == nil {
		slog.DebugContext(ctx, "No title generator available, skipping title generation")
		return
	}

	if state.session == nil || state.handle == nil {
		return
	}
	title, err := a.titleGen.Generate(ctx, state.session.ID, userMessages)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to generate session title", "session_id", state.session.ID, "error", err)
		return
	}

	if title == "" {
		return
	}

	if err := state.handle.UpdateTitle(ctx, title); err != nil {
		slog.ErrorContext(ctx, "Failed to update handle title", "session_id", state.session.ID, "error", err)
	}
}

// RegenerateSessionTitle triggers AI-based title regeneration for the current session.
// Returns ErrTitleGenerating if a title generation is already in progress.
func (a *App) RegenerateSessionTitle(ctx context.Context) error {
	state := a.state()
	if state.session == nil {
		return errors.New("no active session")
	}

	// Check if title generation is already in progress
	if a.titleGenerating.Load() {
		return ErrTitleGenerating
	}

	if owner, ok := state.handle.(runtime.SessionTitleGenerator); ok {
		var userMessages []string
		for _, msg := range state.session.GetAllMessages() {
			if msg.Message.Role == chat.MessageRoleUser {
				userMessages = append(userMessages, msg.Message.Content)
			}
		}
		if len(userMessages) == 0 {
			return errors.New("no user messages for title generation")
		}
		if err := owner.GenerateSessionTitle(ctx, a.titleGen, userMessages, true); err != nil {
			if errors.Is(err, runtime.ErrSessionCapacity) {
				return ErrTitleGenerating
			}
			return err
		}
		return nil
	}
	// For local runtime with title generator, use it directly
	if a.titleGen != nil {
		a.titleGenerating.Store(true)

		// Collect user messages for title generation
		var userMessages []string
		for _, msg := range state.session.GetAllMessages() {
			if msg.Message.Role == chat.MessageRoleUser {
				userMessages = append(userMessages, msg.Message.Content)
			}
		}

		go a.generateTitle(ctx, state, userMessages)
		return nil
	}

	// For remote runtime, title regeneration is not yet supported
	// (the server would need to implement this)
	slog.DebugContext(ctx, "Title regeneration not available for remote runtime", "session_id", state.session.ID)
	return errors.New("title regeneration not available")
}

// CancelPendingMessage withdraws a canonical pending input without cancelling an active turn.
// False means the input was already promoted or the handle does not support withdrawal.
func (a *App) CancelPendingMessage(ctx context.Context, turnID string) (bool, error) {
	state := a.state()
	canceler, ok := state.handle.(runtime.PendingMessageCanceler)
	if !ok || turnID == "" {
		return false, nil
	}
	return canceler.CancelPendingMessage(ctx, turnID)
}
