// Package embeddedchat provides a small headless chat wrapper around the
// docker-agent runtime for embedders that want to drive an agent from their
// own UI instead of running docker-agent's Bubble Tea application.
package embeddedchat

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	dagentcfg "github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/host/lifecycle"
	"github.com/docker/docker-agent/pkg/host/turn"
	dagentruntime "github.com/docker/docker-agent/pkg/runtime"
	runtimeclient "github.com/docker/docker-agent/pkg/runtime/client"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
)

const defaultEventBuffer = 32

var (
	// ErrAgentSourceRequired is returned when New is called without an agent
	// source or a pre-built team.
	ErrAgentSourceRequired = errors.New("embeddedchat: an agent source or a team is required")
	// ErrRegistriesRequired is returned when an AgentSource is loaded without
	// load options: the loader needs toolset and provider registries. Pass
	// embeddedchat/defaults.Opts() for docker-agent's full registries, or
	// your own via LoadOpts.
	ErrRegistriesRequired = errors.New("embeddedchat: LoadOpts is required with AgentSource (e.g. embeddedchat/defaults.Opts())")
	// ErrNotInitialized is returned when a Session has no runtime or conversation.
	ErrNotInitialized = errors.New("embeddedchat: session is not initialized")
	// ErrRunActive is returned when Send is called while a previous run is still active.
	ErrRunActive = errors.New("embeddedchat: a run is already active")
	// ErrClosed is returned when an operation is attempted after Close.
	ErrClosed = errors.New("embeddedchat: session is closed")
)

// Config describes an embedded agent session.
type Config struct {
	// Team is a pre-built team. Embedders that assemble their agents in code
	// use it instead of AgentSource: no YAML loading happens, and — the point
	// — docker-agent's full toolset and provider registries are never linked
	// into their binary. Takes precedence over AgentSource.
	Team *team.Team
	// AgentSource is the agent/team definition to load. Bytes sources are a
	// good fit for embedders that ship a pinned agent in their binary.
	// Requires LoadOpts (see ErrRegistriesRequired).
	AgentSource dagentcfg.Source
	// RuntimeConfig is passed to the team loader. When nil, a zero runtime
	// config is used.
	RuntimeConfig *dagentcfg.RuntimeConfig
	// LoadOpts configures the team loader for the AgentSource path, most
	// importantly the toolset and provider registries.
	// embeddedchat/defaults.Opts() provides docker-agent's full registries.
	LoadOpts []teamloader.Opt
	// ToolsetRegistry resolves toolsets declared by AgentSource; appended to
	// LoadOpts as a convenience.
	ToolsetRegistry teamloader.ToolsetRegistry
	// RuntimeOptions are appended when constructing the runtime.
	RuntimeOptions []dagentruntime.Opt
	// SessionOptions are appended when constructing each conversation session.
	SessionOptions []session.Opt
	// InitialSession, when set, is used as the first conversation instead of
	// a fresh one — e.g. a conversation restored from a session store.
	// Restart still replaces it with a fresh session.
	InitialSession *session.Session
	// EventBuffer controls the size of the channel returned by Send. When zero,
	// a small default buffer is used.
	EventBuffer int
}

// Event is the UI-friendly form of one runtime stream event.
type Event struct {
	// Text is an assistant text delta.
	Text string
	// Tool describes a tool call starting, awaiting confirmation, or finishing.
	Tool *ToolActivity
	// Err is a user-facing runtime error.
	Err error
	// Done marks a clean end of the reply stream.
	Done bool
	// RuntimeEvent is the original docker-agent runtime event for projected
	// events. Not every runtime event is forwarded by this compact API.
	RuntimeEvent dagentruntime.Event
}

// ToolActivity describes one tool call surfaced by the runtime.
type ToolActivity struct {
	Call tools.ToolCall
	Def  tools.Tool
	// Output is an incremental output line streamed by the running call
	// (empty for lifecycle events).
	Output   string
	Finished bool
	IsError  bool
	// Response is the finished call's textual result.
	Response string
	// Result is the finished call's structured result (nil when unavailable).
	Result *tools.ToolCallResult
	// NeedsConfirmation is true when the runtime is blocked until Confirm is
	// called with the user's decision.
	NeedsConfirmation bool
	// RequestID correlates a confirmation with Confirm's ResumeRequest.RequestID.
	// Prefer passing this token explicitly, especially across Restart.
	RequestID string
}

// Session owns one embedded runtime supervisor and one immutable conversation session.
type Session struct {
	cfg Config

	supervisor   dagentruntime.SessionRuntimeSupervisor
	rt           dagentruntime.SessionRuntime
	handle       dagentruntime.SessionHandle
	conversation *session.Session
	welcome      string
	// workingDir captures the workspace provenance once at initialization.
	workingDir string

	mu                  sync.Mutex
	activeCancel        context.CancelFunc
	activeRun           int
	drainErr            error
	closed              bool
	pendingConfirmation string
	pendingHandle       dagentruntime.SessionHandle
}

// New builds the runtime for the configured team (or loads AgentSource) and
// creates the first conversation session.
func New(ctx context.Context, cfg Config) (*Session, error) {
	if cfg.Team == nil && cfg.AgentSource == nil {
		return nil, ErrAgentSourceRequired
	}
	runConfig := cfg.RuntimeConfig
	if runConfig == nil {
		runConfig = &dagentcfg.RuntimeConfig{}
	}

	tm := cfg.Team
	var runtimeOpts []dagentruntime.Opt
	if tm == nil {
		loadOpts := append([]teamloader.Opt(nil), cfg.LoadOpts...)
		if cfg.ToolsetRegistry != nil {
			loadOpts = append(loadOpts, teamloader.WithToolsetRegistry(cfg.ToolsetRegistry))
		}
		if len(loadOpts) == 0 {
			return nil, ErrRegistriesRequired
		}
		loaded, err := teamloader.LoadWithConfig(ctx, cfg.AgentSource, runConfig, loadOpts...)
		if err != nil {
			return nil, fmt.Errorf("embeddedchat: load agent: %w", err)
		}
		tm = loaded.Team

		modelSwitcher := &dagentruntime.ModelSwitcherConfig{
			Models:             loaded.Models,
			Providers:          loaded.Providers,
			ModelsGateway:      runConfig.ModelsGateway,
			EncryptedConfig:    loaded.EncryptedConfig,
			EnvProvider:        runConfig.EnvProvider(),
			ProviderRegistry:   loaded.ProviderRegistry,
			AgentDefaultModels: loaded.AgentDefaultModels,
		}
		// Reuse the models.dev store the team loader already warmed so model-
		// metadata lookups don't re-pay the cold catalog parse.
		if store, storeErr := runConfig.ModelsDevStore(); storeErr == nil {
			modelSwitcher.ModelsStore = store
		}
		runtimeOpts = append(runtimeOpts, dagentruntime.WithModelSwitcherConfig(modelSwitcher))
	}

	runtimeOpts = append(runtimeOpts,
		dagentruntime.WithWorkingDir(runConfig.WorkingDir),
		dagentruntime.WithSessionStore(session.NewInMemorySessionStore()),
	)
	runtimeOpts = append(runtimeOpts, cfg.RuntimeOptions...)
	rt, err := dagentruntime.New(ctx, tm, runtimeOpts...)
	if err != nil {
		if cfg.Team == nil {
			_ = tm.StopToolSets(context.WithoutCancel(ctx))
		}
		return nil, fmt.Errorf("embeddedchat: create runtime: %w", err)
	}

	supervisor := dagentruntime.NewSessionRuntimeSupervisor(rt)
	if cfg.Team == nil {
		supervisor = lifecycle.OwnRuntime(supervisor, tm)
	}
	s := &Session{cfg: cfg, supervisor: supervisor, rt: supervisor.Runtime()}
	// Capture the embedder's workspace root once, at initialization: a later
	// process chdir must not change which workspace owns the conversations.
	s.workingDir, err = session.CaptureLocalWorkingDir(runConfig.WorkingDir)
	if err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("embeddedchat: capture working dir: %w", err)
	}
	if root, err := tm.DefaultAgent(); err == nil {
		s.welcome = root.WelcomeMessage()
	}
	if cfg.InitialSession != nil {
		s.conversation = cfg.InitialSession.Clone()
	} else {
		s.resetConversationLocked()
	}
	if s.conversation.AgentName == "" {
		s.conversation.AgentName = s.welcomeAgent(tm)
	}
	if err := s.bindConversationLocked(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Session) welcomeAgent(tm *team.Team) string {
	root, err := tm.DefaultAgent()
	if err != nil {
		return ""
	}
	return root.Name()
}

func (s *Session) bindConversationLocked(ctx context.Context) error {
	handle, err := s.rt.CreateSession(ctx, s.conversation, dagentruntime.SessionBinding{AgentName: s.conversation.AgentName})
	if err != nil {
		return fmt.Errorf("embeddedchat: bind session: %w", err)
	}
	s.handle = handle
	return nil
}

// WelcomeMessage returns the loaded agent's welcome message.
func (s *Session) WelcomeMessage() string { return s.welcome }

// SessionRuntime returns the underlying session registry for advanced embedders.
// It does not expose a second local stream execution path.
func (s *Session) SessionRuntime() dagentruntime.SessionRuntime {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rt
}

// Conversation returns a detached snapshot of the canonical conversation.
// Mutating the snapshot does not modify the runtime-owned session.
func (s *Session) Conversation(ctx context.Context) (*session.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if s.handle == nil {
		return nil, ErrNotInitialized
	}
	return s.handle.Snapshot(ctx)
}

// Restart cancels any active run and replaces the conversation with a fresh
// session, preserving the runtime and loaded agent.
func (s *Session) Restart() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.cancelActiveLocked()
	// Restart has no caller context; cleanup must outlive the cancelled Send.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) //rubocop:disable Lint/ContextConnectivity
	defer cancel()
	if s.handle != nil {
		if err := s.handle.Release(ctx); err != nil {
			return fmt.Errorf("embeddedchat: release previous session: %w", err)
		}
	}
	s.drainErr = nil
	s.pendingConfirmation, s.pendingHandle = "", nil
	s.resetConversationLocked()
	return s.bindConversationLocked(ctx)
}

// Close cancels any active run and releases runtime resources.
func (s *Session) Close() error {
	s.mu.Lock()
	s.closed = true
	s.cancelActiveLocked()
	supervisor := s.supervisor
	s.mu.Unlock()
	if supervisor != nil {
		// Close has no caller context; it must drain even after Send cancellation.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) //rubocop:disable Lint/ContextConnectivity
		defer cancel()
		return supervisor.Shutdown(ctx)
	}
	return nil
}

func (s *Session) resetConversationLocked() {
	// The captured root goes first so an embedder-supplied WithWorkingDir in
	// SessionOptions still wins.
	opts := append([]session.Opt{session.WithWorkingDir(s.workingDir)}, s.cfg.SessionOptions...)
	s.conversation = session.New(opts...)
}

func (s *Session) cancelActiveLocked() {
	if s.activeCancel != nil {
		s.activeCancel()
	}
}

// Send appends prompt to the conversation and streams the assistant reply.
// The returned channel closes when the runtime stream stops. A clean stream
// emits a final Done event first; a stream that reports an ErrorEvent emits one
// Err event, suppresses later projected events, then keeps draining until the
// runtime stops.
// If ctx is cancelled, Send drains the runtime stream until it stops, but no
// further events are delivered to the caller.
func (s *Session) Send(ctx context.Context, prompt string) (<-chan Event, error) {
	s.mu.Lock()
	if s.rt == nil || s.handle == nil || s.conversation == nil {
		s.mu.Unlock()
		return nil, ErrNotInitialized
	}
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	if s.drainErr != nil {
		s.mu.Unlock()
		return nil, s.drainErr
	}
	if s.activeCancel != nil {
		s.mu.Unlock()
		return nil, ErrRunActive
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.activeCancel = cancel
	s.activeRun++
	runID := s.activeRun
	ownedTurn, err := turn.Start(runCtx, s.handle, dagentruntime.TurnInput{Content: prompt})
	if err != nil {
		s.activeCancel = nil
		cancel()
		s.mu.Unlock()
		return nil, fmt.Errorf("embeddedchat: start message: %w", err)
	}
	sessionHandle := s.handle
	s.mu.Unlock()

	out := make(chan Event, eventBufferSize(s.cfg.EventBuffer))
	go s.forwardEvents(runCtx, ownedTurn, sessionHandle, out, cancel, runID)
	return out, nil
}

// Confirm answers the pending tool confirmation, if any. Set req.RequestID to
// ToolActivity.RequestID to reject stale replies. For compatibility, an omitted
// ID answers the current pending confirmation; it cannot identify a stale UI reply.
func (s *Session) Confirm(ctx context.Context, req dagentruntime.ResumeRequest) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	handle := s.handle
	if req.RequestID == "" {
		req.RequestID = s.pendingConfirmation
	}
	if req.RequestID != "" && req.RequestID == s.pendingConfirmation {
		handle = s.pendingHandle
	}
	s.mu.Unlock()
	if handle == nil {
		return ErrNotInitialized
	}
	err := handle.Respond(ctx, dagentruntime.InteractionResponse{
		InteractionID: req.RequestID,
		Kind:          dagentruntime.InteractionConfirmation,
		Resume:        req,
	})
	if err == nil {
		s.mu.Lock()
		if s.pendingHandle == handle && s.pendingConfirmation == req.RequestID {
			s.pendingConfirmation, s.pendingHandle = "", nil
		}
		s.mu.Unlock()
	}
	return err
}

func (s *Session) forwardEvents(ctx context.Context, ownedTurn *turn.Turn, sessionHandle dagentruntime.SessionHandle, out chan<- Event, cancel context.CancelFunc, runID int) {
	defer close(out)
	defer cancel()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.activeRun == runID {
			if s.pendingHandle == sessionHandle {
				s.pendingConfirmation, s.pendingHandle = "", nil
			}
			s.activeCancel = nil
		}
	}()

	emit := func(e Event) bool {
		select {
		case out <- e:
			return true
		case <-ctx.Done():
			return false
		}
	}

	errSent := false
	handle := func(envelope dagentruntime.SessionEvent) bool {
		event := envelope.Event
		switch e := event.(type) {
		case *dagentruntime.ToolCallConfirmationEvent:
			if errSent {
				_ = sessionHandle.Respond(ctx, dagentruntime.InteractionResponse{InteractionID: envelope.InteractionID, Kind: dagentruntime.InteractionConfirmation, Resume: dagentruntime.ResumeReject("The run was aborted.")})
				return true
			}
			s.mu.Lock()
			if s.handle == sessionHandle && !s.closed {
				s.pendingConfirmation, s.pendingHandle = envelope.InteractionID, sessionHandle
			}
			s.mu.Unlock()
			if !emit(Event{RuntimeEvent: event, Tool: &ToolActivity{Call: e.ToolCall, Def: e.ToolDefinition, NeedsConfirmation: true, RequestID: envelope.InteractionID}}) {
				_ = sessionHandle.Respond(ctx, dagentruntime.InteractionResponse{InteractionID: envelope.InteractionID, Kind: dagentruntime.InteractionConfirmation, Resume: dagentruntime.ResumeReject("The run was aborted.")})
			}
		case *dagentruntime.ElicitationRequestEvent:
			_ = sessionHandle.Respond(ctx, dagentruntime.InteractionResponse{InteractionID: envelope.InteractionID, Kind: dagentruntime.InteractionElicitation, ElicitationID: e.ElicitationID, Elicitation: dagentruntime.ElicitationResult{Action: tools.ElicitationActionDecline}})
		case *dagentruntime.MaxIterationsReachedEvent:
			_ = sessionHandle.Respond(ctx, dagentruntime.InteractionResponse{InteractionID: envelope.InteractionID, Kind: dagentruntime.InteractionMaxIterations, Resume: dagentruntime.ResumeReject("")})
		case *dagentruntime.ErrorEvent:
			if errSent {
				return true
			}
			if !emit(Event{RuntimeEvent: event, Err: errors.New(e.Error)}) {
				return false
			}
			errSent = true
		case *dagentruntime.StreamStoppedEvent:
			return false
		default:
			if errSent {
				return true
			}
			if translated, ok := TranslateRuntimeEvent(event); ok && !emit(translated) {
				return false
			}
		}
		return true
	}

	termination := ownedTurn.Consume(ctx, func(ctx context.Context, envelope dagentruntime.SessionEvent) (runtimeclient.TurnDecision, error) {
		if ctx.Err() != nil {
			return runtimeclient.TurnContinue, nil
		}
		if !handle(envelope) {
			return runtimeclient.TurnTerminate, nil
		}
		return runtimeclient.TurnContinue, nil
	})
	var drainErr *turn.DrainError
	if errors.As(termination.Err, &drainErr) {
		s.mu.Lock()
		if s.handle == sessionHandle {
			s.drainErr = drainErr
		}
		s.mu.Unlock()
	}

	if termination.Err != nil && ctx.Err() == nil {
		emit(Event{Err: termination.Err, Done: true})
		errSent = true
	}
	if termination.Terminated {
		return
	}
	if !errSent && ctx.Err() == nil {
		emit(Event{Done: true})
	}
}

func eventBufferSize(configured int) int {
	if configured > 0 {
		return configured
	}
	return defaultEventBuffer
}

// TranslateRuntimeEvent translates content-bearing runtime events into the
// compact Event shape used by embedded chat UIs.
func TranslateRuntimeEvent(event dagentruntime.Event) (Event, bool) {
	switch e := event.(type) {
	case *dagentruntime.AgentChoiceEvent:
		if e.Content == "" {
			return Event{}, false
		}
		return Event{RuntimeEvent: event, Text: e.Content}, true
	case *dagentruntime.ToolCallEvent:
		return Event{RuntimeEvent: event, Tool: &ToolActivity{Call: e.ToolCall, Def: e.ToolDefinition}}, true
	case *dagentruntime.ToolCallOutputEvent:
		if e.Output == "" {
			return Event{}, false
		}
		return Event{RuntimeEvent: event, Tool: &ToolActivity{
			Call:   tools.ToolCall{ID: e.ToolCallID},
			Def:    e.ToolDefinition,
			Output: e.Output,
		}}, true
	case *dagentruntime.ToolCallResponseEvent:
		return Event{RuntimeEvent: event, Tool: &ToolActivity{
			Call:     tools.ToolCall{ID: e.ToolCallID},
			Def:      e.ToolDefinition,
			Finished: true,
			IsError:  e.Result != nil && e.Result.IsError,
			Response: e.Response,
			Result:   e.Result,
		}}, true
	}
	return Event{}, false
}
