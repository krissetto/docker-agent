package acp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"
	"go.opentelemetry.io/otel"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/host/turn"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/runtime"
	runtimeclient "github.com/docker/docker-agent/pkg/runtime/client"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	loaderdefaults "github.com/docker/docker-agent/pkg/teamloader/defaults"
	"github.com/docker/docker-agent/pkg/version"
)

// Agent implements the ACP Agent interface for docker agent.
type Agent struct {
	agentSource  config.Source
	runConfig    *config.RuntimeConfig
	sessionStore session.Store
	sessions     map[string]*Session

	conn             *acp.AgentSideConnection
	clientFS         acp.FileSystemCapabilities
	team             *team.Team
	providerRegistry *provider.Registry
	mu               sync.Mutex
	admissionClosed  bool
	stopping         bool
	stopped          bool
	closedSessionIDs map[string]struct{}
	stopDone         chan struct{}
}

var _ acp.Agent = (*Agent)(nil)

// Session represents an ACP session.
type Session struct {
	id             string
	sess           *session.Session
	rt             runtime.SessionRuntime
	supervisor     runtime.SessionRuntimeSupervisor
	session        runtime.SessionHandle
	workingDir     string
	additionalDirs []string

	mu sync.Mutex

	turns            chan struct{}
	cancel           context.CancelFunc
	generation       uint64
	closed           bool
	shutdownDone     chan struct{}
	shutdownComplete bool
}

var errSessionClosed = errors.New("ACP session closed")

func (s *Session) cancelTurn() {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Session) close() {
	s.mu.Lock()
	s.closed = true
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// shutdown retains ownership on failure and serializes concurrent close/stop attempts.
func (s *Session) shutdown(ctx context.Context) error {
	s.close()
	for {
		s.mu.Lock()
		if s.shutdownComplete {
			s.mu.Unlock()
			return nil
		}
		if done := s.shutdownDone; done != nil {
			s.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		s.shutdownDone = make(chan struct{})
		s.mu.Unlock()
		var err error
		if s.supervisor != nil {
			err = s.supervisor.Shutdown(ctx)
		}
		s.mu.Lock()
		s.shutdownComplete = err == nil
		close(s.shutdownDone)
		s.shutdownDone = nil
		s.mu.Unlock()
		return err
	}
}

func (s *Session) startTurn(ctx context.Context) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	turnCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cancel()
		return nil, nil, errSessionClosed
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		cancel()
		return nil, nil, err
	}
	if s.turns == nil {
		s.turns = make(chan struct{}, 1)
		s.turns <- struct{}{}
	}
	turns := s.turns
	previous := s.cancel
	s.generation++
	generation := s.generation
	s.cancel = cancel
	s.mu.Unlock()

	if previous != nil {
		previous()
	}

	select {
	case <-turnCtx.Done():
		s.clearTurn(generation, cancel)
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return nil, nil, errSessionClosed
		}
		return nil, nil, turnCtx.Err()
	case <-turns:
	}

	s.mu.Lock()
	closed := s.closed
	current := s.generation == generation
	err := turnCtx.Err()
	s.mu.Unlock()
	if closed || !current || err != nil {
		turns <- struct{}{}
		s.clearTurn(generation, cancel)
		if closed {
			return nil, nil, errSessionClosed
		}
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, context.Canceled
	}

	finish := func() {
		turns <- struct{}{}
		s.clearTurn(generation, cancel)
	}
	return turnCtx, finish, nil
}

func (s *Session) clearTurn(generation uint64, cancel context.CancelFunc) {
	cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.generation == generation {
		s.cancel = nil
	}
}

// NewAgent creates a new ACP agent.
func NewAgent(agentSource config.Source, runConfig *config.RuntimeConfig, sessionStore session.Store) *Agent {
	return &Agent{
		agentSource:      agentSource,
		runConfig:        runConfig,
		sessionStore:     sessionStore,
		sessions:         make(map[string]*Session),
		closedSessionIDs: make(map[string]struct{}),
		stopDone:         make(chan struct{}),
	}
}

// Stop drains all per-session supervisors before stopping shared toolsets.
// Failed runtime drains retain ownership for a later Stop. Toolset stop errors
// are terminal and logged, matching the lifecycle owner cleanup policy.
func (a *Agent) Stop(ctx context.Context) {
	for {
		a.mu.Lock()
		if a.stopped {
			a.mu.Unlock()
			return
		}
		if a.stopping {
			done := a.stopDone
			a.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return
			}
		}
		a.admissionClosed = true
		a.stopping = true
		a.stopDone = make(chan struct{})
		sessions := make([]*Session, 0, len(a.sessions))
		for _, acpSess := range a.sessions {
			acpSess.close()
			sessions = append(sessions, acpSess)
		}
		t := a.team
		a.mu.Unlock()
		drained := true
		for _, acpSess := range sessions {
			if err := acpSess.shutdown(ctx); err != nil {
				drained = false
				slog.ErrorContext(ctx, "Failed to stop ACP session supervisor", "session_id", acpSess.id, "error", err)
			} else {
				a.mu.Lock()
				delete(a.sessions, acpSess.id)
				a.mu.Unlock()
			}
		}
		if drained && t != nil {
			if err := t.StopToolSets(ctx); err != nil {
				slog.ErrorContext(ctx, "Failed to stop tool sets", "error", err)
			}
		}
		a.mu.Lock()
		a.stopped = drained
		a.stopping = false
		close(a.stopDone)
		a.mu.Unlock()
		return
	}
}

func (a *Agent) admissionErrorLocked() error {
	if a.admissionClosed || a.stopping || a.stopped {
		return errors.New("ACP agent is stopping")
	}
	return nil
}

// SetAgentConnection sets the ACP connection.
func (a *Agent) SetAgentConnection(conn *acp.AgentSideConnection) {
	a.conn = conn
}

// Initialize implements [acp.Agent].
func (a *Agent) Initialize(ctx context.Context, params acp.InitializeRequest) (acp.InitializeResponse, error) {
	slog.DebugContext(ctx, "ACP Initialize called", "client_version", params.ProtocolVersion)

	a.mu.Lock()
	a.clientFS = params.ClientCapabilities.Fs
	defer a.mu.Unlock()
	loadOpts := append(loaderdefaults.Opts(), teamloader.WithToolsetRegistry(createToolsetRegistry(a)))
	loadResult, err := teamloader.LoadWithConfig(ctx, a.agentSource, a.runConfig, loadOpts...)
	if err != nil {
		return acp.InitializeResponse{}, fmt.Errorf("failed to load teams: %w", err)
	}
	t := loadResult.Team
	a.team = t
	a.providerRegistry = loadResult.ProviderRegistry
	slog.DebugContext(ctx, "Teams loaded successfully", "source", a.agentSource.Name(), "agent_count", t.Size())

	agentTitle := "docker agent"
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		AgentInfo: &acp.Implementation{
			Name:    "docker agent",
			Version: version.Version,
			Title:   &agentTitle,
		},
		AgentCapabilities: acp.AgentCapabilities{
			LoadSession: false,
			SessionCapabilities: acp.SessionCapabilities{
				AdditionalDirectories: &acp.SessionAdditionalDirectoriesCapabilities{},
				Close:                 &acp.SessionCloseCapabilities{},
				List:                  &acp.SessionListCapabilities{},
				Resume:                &acp.SessionResumeCapabilities{},
			},
			PromptCapabilities: acp.PromptCapabilities{
				EmbeddedContext: true,
				Image:           true,
				Audio:           false, // Not yet supported
			},
			McpCapabilities: acp.McpCapabilities{
				Http: false, // MCP servers from client not yet supported
				Sse:  false, // MCP servers from client not yet supported
			},
		},
	}, nil
}

// newRuntime creates a new runtime using the default agent.
func (a *Agent) newRuntime(ctx context.Context, workingDir string) (runtime.SessionRuntimeSupervisor, *agent.Agent, error) {
	if a.team == nil {
		return nil, nil, errors.New("agent not initialized")
	}

	defaultAgent, err := a.team.DefaultAgent()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to resolve default agent: %w", err)
	}

	opts := []runtime.Opt{
		runtime.WithSessionStore(a.sessionStore),
		runtime.WithProviderRegistry(a.providerRegistry),
		// Match the CLI tracer scope; without this the ACP-mode
		// runtime's `startSpan` is a no-op for every runtime.* span.
		runtime.WithTracer(otel.Tracer(version.AppName)),
	}
	if workingDir != "" {
		opts = append(opts, runtime.WithWorkingDir(workingDir))
	}

	rt, err := runtime.NewLocalRuntime(ctx, a.team, opts...)
	if err != nil {
		return nil, nil, err
	}
	return runtime.NewSessionRuntimeSupervisor(rt), defaultAgent, nil
}

// registerSessionIfAbsent stores acpSess only if no session with the same id
// is already registered. It returns the session that ended up in the map
// (either the existing one or acpSess) and a boolean indicating whether
// acpSess was the one stored. This avoids a TOCTOU race between checking
// a.sessions and registering a new session.
type registrationOutcome uint8

const (
	registrationStored registrationOutcome = iota
	registrationDuplicate
	registrationStopping
	registrationClosed
)

func (a *Agent) registerSessionIfAbsent(acpSess *Session) registrationOutcome {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.admissionClosed || a.stopping || a.stopped {
		return registrationStopping
	}
	if _, closed := a.closedSessionIDs[acpSess.id]; closed {
		return registrationClosed
	}
	if _, ok := a.sessions[acpSess.id]; ok {
		return registrationDuplicate
	}
	a.sessions[acpSess.id] = acpSess
	return registrationStored
}

// NewSession implements [acp.Agent].
func (a *Agent) NewSession(ctx context.Context, params acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	slog.DebugContext(ctx, "ACP NewSession called", "cwd", params.Cwd)

	a.mu.Lock()
	if err := a.admissionErrorLocked(); err != nil {
		a.mu.Unlock()
		return acp.NewSessionResponse{}, err
	}
	a.mu.Unlock()

	if len(params.McpServers) > 0 {
		slog.WarnContext(ctx, "MCP servers provided by client are not yet supported", "count", len(params.McpServers))
	}

	workingDir, err := resolveWorkingDir(params.Cwd)
	if err != nil {
		return acp.NewSessionResponse{}, err
	}

	// An empty cwd is allowed: clients (e.g. zed) may not always supply a
	// working directory at session creation. We persist it as empty and
	// later prompts/tools fall back to the agent's default working dir.
	// The persisted WorkingDir stays empty too: workspace provenance must
	// come from the client, never be inferred from the server's process cwd.
	if err := validateWorkingDir(workingDir); err != nil {
		return acp.NewSessionResponse{}, err
	}

	additionalDirs, err := resolveAdditionalDirectories(params.AdditionalDirectories)
	if err != nil {
		return acp.NewSessionResponse{}, err
	}

	supervisor, defaultAgent, err := a.newRuntime(ctx, workingDir)
	if err != nil {
		return acp.NewSessionResponse{}, err
	}

	rt := supervisor.Runtime()
	sess := session.New(
		session.WithMaxIterations(defaultAgent.MaxIterations()),
		session.WithMaxConsecutiveToolCalls(defaultAgent.MaxConsecutiveToolCalls()),
		session.WithMaxOldToolCallTokens(defaultAgent.MaxOldToolCallTokens()),
		session.WithMaxToolResultTokens(defaultAgent.MaxToolResultTokens()),
		session.WithWorkingDir(workingDir),
	)
	sess.SetTitle("ACP Session " + sess.ID)

	if err := a.sessionStore.AddSession(ctx, sess); err != nil {
		_ = supervisor.Shutdown(ctx)
		return acp.NewSessionResponse{}, fmt.Errorf("failed to persist session: %w", err)
	}

	slog.DebugContext(ctx, "ACP session created", "session_id", sess.ID)

	handle, err := rt.CreateSession(ctx, sess, runtime.SessionBinding{AgentName: defaultAgent.Name()})
	if err != nil {
		_ = a.sessionStore.DeleteSession(context.WithoutCancel(ctx), sess.ID)
		_ = supervisor.Shutdown(ctx)
		return acp.NewSessionResponse{}, fmt.Errorf("bind ACP session: %w", err)
	}

	candidate := &Session{
		id:             sess.ID,
		sess:           sess,
		rt:             rt,
		supervisor:     supervisor,
		session:        handle,
		workingDir:     workingDir,
		additionalDirs: additionalDirs,
	}
	outcome := a.registerSessionIfAbsent(candidate)
	if outcome != registrationStored {
		_ = supervisor.Shutdown(ctx)
		_ = a.sessionStore.DeleteSession(context.WithoutCancel(ctx), sess.ID)
		return acp.NewSessionResponse{}, fmt.Errorf("ACP session registration rejected: %v", outcome)
	}

	return acp.NewSessionResponse{SessionId: acp.SessionId(sess.ID)}, nil
}

// Authenticate implements [acp.Agent].
func (a *Agent) Authenticate(ctx context.Context, _ acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	slog.DebugContext(ctx, "ACP Authenticate called")
	return acp.AuthenticateResponse{}, nil
}

// Logout implements [acp.Agent] (optional, not supported).
func (a *Agent) Logout(ctx context.Context, _ acp.LogoutRequest) (acp.LogoutResponse, error) {
	slog.DebugContext(ctx, "ACP Logout called (not supported)")
	return acp.LogoutResponse{}, acp.NewMethodNotFound(acp.AgentMethodLogout)
}

// LoadSession implements [acp.AgentLoader] (optional, not supported).
func (a *Agent) LoadSession(ctx context.Context, _ acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	slog.DebugContext(ctx, "ACP LoadSession called (not supported)")
	return acp.LoadSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionLoad)
}

// CloseSession implements [acp.Agent].
func (a *Agent) CloseSession(ctx context.Context, params acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	sid := string(params.SessionId)
	slog.DebugContext(ctx, "ACP CloseSession called", "session_id", sid)

	a.mu.Lock()
	if a.closedSessionIDs == nil {
		a.closedSessionIDs = make(map[string]struct{})
	}
	a.closedSessionIDs[sid] = struct{}{}
	acpSess, ok := a.sessions[sid]
	a.mu.Unlock()

	if ok && acpSess != nil {
		if err := acpSess.shutdown(ctx); err != nil {
			return acp.CloseSessionResponse{}, err
		}
		a.mu.Lock()
		delete(a.sessions, sid)
		a.mu.Unlock()
	}

	return acp.CloseSessionResponse{}, nil
}

// ListSessions implements [acp.Agent].
func (a *Agent) ListSessions(ctx context.Context, _ acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	slog.DebugContext(ctx, "ACP ListSessions called")

	summaries, err := a.sessionStore.GetSessionSummaries(ctx)
	if err != nil {
		return acp.ListSessionsResponse{}, fmt.Errorf("failed to list sessions: %w", err)
	}

	sessions := make([]acp.SessionInfo, 0, len(summaries))
	for _, s := range summaries {
		cwd, additionalDirs := a.sessionListPaths(ctx, s.ID)
		info := acp.SessionInfo{
			SessionId:             acp.SessionId(s.ID),
			Title:                 &s.Title,
			Cwd:                   cwd,
			AdditionalDirectories: additionalDirs,
		}
		if !s.CreatedAt.IsZero() {
			// We don't track session updates yet, so report CreatedAt in
			// the ACP UpdatedAt field as our best-effort timestamp.
			createdAt := s.CreatedAt.UTC().Format(time.RFC3339)
			info.UpdatedAt = &createdAt
		}
		sessions = append(sessions, info)
	}

	return acp.ListSessionsResponse{Sessions: sessions}, nil
}

// ResumeSession implements [acp.Agent].
func (a *Agent) ResumeSession(ctx context.Context, params acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	sid := string(params.SessionId)
	slog.DebugContext(ctx, "ACP ResumeSession called", "session_id", sid)

	a.mu.Lock()
	if err := a.admissionErrorLocked(); err != nil {
		a.mu.Unlock()
		return acp.ResumeSessionResponse{}, err
	}
	if _, closed := a.closedSessionIDs[sid]; closed {
		a.mu.Unlock()
		return acp.ResumeSessionResponse{}, fmt.Errorf("session %s is closed", sid)
	}
	_, alreadyRegistered := a.sessions[sid]
	a.mu.Unlock()
	if alreadyRegistered {
		return acp.ResumeSessionResponse{}, nil
	}

	sess, err := a.sessionStore.GetSession(ctx, sid)
	if err != nil {
		return acp.ResumeSessionResponse{}, fmt.Errorf("failed to load session %s: %w", sid, err)
	}

	workingDir, err := resolveWorkingDir(params.Cwd)
	if err != nil {
		return acp.ResumeSessionResponse{}, err
	}
	if err := validateWorkingDir(workingDir); err != nil {
		return acp.ResumeSessionResponse{}, err
	}
	if workingDir != "" {
		sess.WorkingDir = workingDir
	}

	additionalDirs, err := resolveAdditionalDirectories(params.AdditionalDirectories)
	if err != nil {
		return acp.ResumeSessionResponse{}, err
	}

	supervisor, defaultAgent, err := a.newRuntime(ctx, sess.WorkingDir)
	if err != nil {
		return acp.ResumeSessionResponse{}, err
	}

	rt := supervisor.Runtime()
	// Re-adopt any persisted subagent swarm so the resumed session's
	// send_message / read_subagent keep working.
	if restorer, ok := rt.(runtime.TreeRestorer); ok {
		if err := restorer.RestoreSessionTree(ctx, sess); err != nil {
			_ = supervisor.Shutdown(ctx)
			return acp.ResumeSessionResponse{}, fmt.Errorf("restore subagent tree for session %s: %w", sid, err)
		}
	}

	handle, err := rt.CreateSession(ctx, sess, runtime.SessionBinding{AgentName: defaultAgent.Name()})
	if err != nil {
		_ = supervisor.Shutdown(ctx)
		return acp.ResumeSessionResponse{}, fmt.Errorf("bind ACP session: %w", err)
	}
	// the same session id between our initial check and now, drop the
	// runtime we just built and reuse the existing registration.
	outcome := a.registerSessionIfAbsent(&Session{
		id:             sid,
		sess:           sess,
		rt:             rt,
		supervisor:     supervisor,
		session:        handle,
		workingDir:     sess.WorkingDir,
		additionalDirs: additionalDirs,
	})
	switch outcome {
	case registrationStored:
		slog.DebugContext(ctx, "ACP session resumed", "session_id", sid)
		return acp.ResumeSessionResponse{}, nil
	case registrationDuplicate:
		_ = supervisor.Shutdown(ctx)
		return acp.ResumeSessionResponse{}, nil
	default:
		_ = supervisor.Shutdown(ctx)
		return acp.ResumeSessionResponse{}, fmt.Errorf("ACP session resume rejected: %v", outcome)
	}
}

// SetSessionConfigOption implements [acp.Agent] (optional, not advertised in capabilities).
func (a *Agent) SetSessionConfigOption(ctx context.Context, _ acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	slog.DebugContext(ctx, "ACP SetSessionConfigOption called (not supported)")
	return acp.SetSessionConfigOptionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetConfigOption)
}

// Cancel implements [acp.Agent].
func (a *Agent) Cancel(_ context.Context, params acp.CancelNotification) error {
	sid := string(params.SessionId)
	slog.Debug("ACP Cancel called", "session_id", sid)

	a.mu.Lock()
	acpSess, ok := a.sessions[sid]
	a.mu.Unlock()

	if ok && acpSess != nil && acpSess.session != nil {
		acpSess.cancelTurn()
	}

	return nil
}

// Prompt implements [acp.Agent].
func (a *Agent) Prompt(ctx context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	sid := string(params.SessionId)
	slog.DebugContext(ctx, "ACP Prompt called", "session_id", sid)

	a.mu.Lock()
	acpSess, ok := a.sessions[sid]
	a.mu.Unlock()

	if !ok {
		return acp.PromptResponse{}, fmt.Errorf("session %s not found", sid)
	}

	turnCtx, finish, err := acpSess.startTurn(ctx)
	if err != nil {
		if errors.Is(err, errSessionClosed) {
			return acp.PromptResponse{}, fmt.Errorf("session %s not found", sid)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
		}
		return acp.PromptResponse{}, err
	}
	defer finish()

	userMsg := a.buildUserMessage(turnCtx, sid, params.Prompt)
	input := runtime.TurnInput{}
	if userMsg != nil {
		input.Content = userMsg.Message.Content
		input.MultiContent = userMsg.Message.MultiContent
	}

	if err := a.runAgent(turnCtx, acpSess, input); err != nil {
		if turnCtx.Err() != nil {
			return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
		}
		return acp.PromptResponse{}, err
	}

	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (a *Agent) buildUserMessage(ctx context.Context, sessionID string, prompt []acp.ContentBlock) *session.Message {
	var (
		parts          []string
		multiContent   []chat.MessagePart
		hasRichContent bool
	)

	appendText := func(text string) {
		if text == "" {
			return
		}
		parts = append(parts, text)
		multiContent = append(multiContent, chat.MessagePart{Type: chat.MessagePartTypeText, Text: text})
	}

	for _, content := range prompt {
		switch {
		case content.Text != nil:
			appendText(content.Text.Text)

		case content.ResourceLink != nil:
			rl := content.ResourceLink
			slog.DebugContext(ctx, "Processing resource link", "uri", rl.Uri, "name", rl.Name)

			if fileContent, ok := a.readResourceLink(ctx, sessionID, rl); ok {
				appendText(fmt.Sprintf("\n\n--- File: %s ---\n%s\n--- End File ---\n", resourceLinkName(rl), fileContent))
			} else {
				appendText(fmt.Sprintf("\n[Referenced file: %s (content unavailable)]\n", resourceLinkName(rl)))
			}

		case content.Resource != nil:
			res := content.Resource.Resource
			if res.TextResourceContents != nil {
				slog.DebugContext(ctx, "Processing embedded text resource", "uri", res.TextResourceContents.Uri)
				appendText(fmt.Sprintf("\n\n--- Resource: %s ---\n%s\n--- End Resource ---\n",
					res.TextResourceContents.Uri, res.TextResourceContents.Text))
			} else if res.BlobResourceContents != nil {
				slog.DebugContext(ctx, "Processing embedded blob resource", "uri", res.BlobResourceContents.Uri)
				appendText(fmt.Sprintf("\n[Binary resource: %s (type: %s)]\n",
					res.BlobResourceContents.Uri, stringOrDefault(res.BlobResourceContents.MimeType, "unknown")))
			}

		case content.Image != nil:
			img := content.Image
			slog.DebugContext(ctx, "Processing image content", "mime_type", img.MimeType)
			hasRichContent = true
			multiContent = append(multiContent, chat.MessagePart{
				Type: chat.MessagePartTypeImageURL,
				ImageURL: &chat.MessageImageURL{
					URL:    imageDataURL(img.MimeType, img.Data),
					Detail: chat.ImageURLDetailAuto,
				},
			})

		case content.Audio != nil:
			slog.DebugContext(ctx, "Audio content received but not yet supported")
			appendText("[Audio content provided]")
		}
	}

	content := strings.Join(parts, "")
	if !hasRichContent {
		return session.UserMessage(content)
	}
	return session.UserMessage(content, multiContent...)
}

// readResourceLink attempts to read a text file referenced by an ACP resource link.
func (a *Agent) readResourceLink(ctx context.Context, sessionID string, rl *acp.ContentBlockResourceLink) (string, bool) {
	if !a.supportsClientReadTextFile() {
		slog.DebugContext(ctx, "ACP client does not support reading resource links")
		return "", false
	}

	path, ok := resourceLinkPath(rl.Uri)
	if !ok {
		slog.DebugContext(ctx, "Unsupported ACP resource link URI", "uri", rl.Uri)
		return "", false
	}

	resolvedPath, err := a.resolveSessionPath(sessionID, path)
	if err != nil {
		slog.WarnContext(ctx, "Blocked unsafe file resource link", "path", path, "error", err)
		return "", false
	}

	resp, err := a.conn.ReadTextFile(ctx, acp.ReadTextFileRequest{
		SessionId: acp.SessionId(sessionID),
		Path:      resolvedPath,
	})
	if err != nil {
		slog.DebugContext(ctx, "Failed to read resource link", "path", resolvedPath, "error", err)
		return "", false
	}

	return resp.Content, true
}

func resourceLinkName(rl *acp.ContentBlockResourceLink) string {
	if name := chat.SanitizeDisplayName(rl.Name); name != "" {
		return name
	}
	if path, ok := resourceLinkPath(rl.Uri); ok {
		if base := chat.SanitizeDisplayName(filepath.Base(path)); base != "" && base != "." && base != string(filepath.Separator) {
			return base
		}
	}
	return "resource"
}

func resourceLinkPath(rawURI string) (string, bool) {
	u, err := url.Parse(rawURI)
	if err != nil || u.Scheme == "" {
		return rawURI, rawURI != ""
	}
	if u.Scheme != "file" {
		return "", false
	}
	if u.Host != "" && u.Host != "localhost" {
		return "", false
	}
	path, err := url.PathUnescape(u.Path)
	if err != nil {
		return "", false
	}
	return path, path != ""
}

func imageDataURL(mimeType, data string) string {
	if strings.HasPrefix(data, "data:") {
		return data
	}
	return fmt.Sprintf("data:%s;base64,%s", mimeType, data)
}

func stringOrDefault(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}

// SetSessionMode implements acp.Agent (optional).
func (a *Agent) SetSessionMode(ctx context.Context, _ acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	slog.DebugContext(ctx, "ACP SetSessionMode called (not supported)")
	return acp.SetSessionModeResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetMode)
}

// sendUpdate sends a session update notification to the ACP client.
func (a *Agent) sendUpdate(ctx context.Context, sessionID string, update acp.SessionUpdate) error {
	return a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: acp.SessionId(sessionID),
		Update:    update,
	})
}

// runAgent runs a single agent loop and streams updates to the ACP client.
func (a *Agent) runAgent(ctx context.Context, acpSess *Session, inputs ...runtime.TurnInput) error {
	var input runtime.TurnInput
	if len(inputs) > 0 {
		input = inputs[0]
	}
	slog.DebugContext(ctx, "Running agent turn", "session_id", acpSess.id)

	ctx = withSessionID(ctx, acpSess.id)

	if err := a.emitAvailableCommands(ctx, acpSess); err != nil {
		slog.DebugContext(ctx, "Failed to emit available commands", "error", err)
	}

	ownedTurn, err := turn.Start(ctx, acpSess.session, input)
	if err != nil {
		return fmt.Errorf("start ACP prompt: %w", err)
	}
	toolCallArgs := map[string]string{}
	termination := ownedTurn.Consume(ctx, func(ctx context.Context, envelope runtime.SessionEvent) (runtimeclient.TurnDecision, error) {
		event := envelope.Event

		switch e := event.(type) {
		case *runtime.AgentChoiceEvent:
			if err := a.sendUpdate(ctx, acpSess.id, acp.UpdateAgentMessageText(e.Content)); err != nil {
				return runtimeclient.TurnTerminate, err
			}

		case *runtime.AgentChoiceReasoningEvent:
			if err := a.sendUpdate(ctx, acpSess.id, acp.UpdateAgentThoughtText(e.Content)); err != nil {
				return runtimeclient.TurnTerminate, err
			}

		case *runtime.ToolCallConfirmationEvent:
			// Rejected calls produce a response without an execution-start event.
			toolCallArgs[e.ToolCall.ID] = e.ToolCall.Function.Arguments
			if err := a.handleToolCallConfirmation(ctx, acpSess, envelope.InteractionID, e); err != nil {
				return runtimeclient.TurnTerminate, err
			}

		case *runtime.ToolCallEvent:
			toolCallArgs[e.ToolCall.ID] = e.ToolCall.Function.Arguments
			if err := a.sendUpdate(ctx, acpSess.id, buildToolCallStart(e.ToolCall, e.ToolDefinition)); err != nil {
				return runtimeclient.TurnTerminate, err
			}

		case *runtime.ToolCallResponseEvent:
			args, ok := toolCallArgs[e.ToolCallID]
			if !ok {
				return runtimeclient.TurnTerminate, fmt.Errorf("missing tool call arguments for tool call ID %s", e.ToolCallID)
			}
			delete(toolCallArgs, e.ToolCallID)

			if err := a.sendUpdate(ctx, acpSess.id, buildToolCallComplete(args, e)); err != nil {
				return runtimeclient.TurnTerminate, err
			}

			if isTodoTool(e.ToolDefinition.Name) && e.Result != nil && e.Result.Meta != nil {
				if planUpdate := buildPlanUpdateFromTodos(e.Result.Meta); planUpdate != nil {
					if err := a.sendUpdate(ctx, acpSess.id, *planUpdate); err != nil {
						return runtimeclient.TurnTerminate, err
					}
				}
			}

		case *runtime.ErrorEvent:
			if err := a.sendUpdate(ctx, acpSess.id, acp.UpdateAgentMessageText(fmt.Sprintf("\n\nError: %s\n", e.Error))); err != nil {
				return runtimeclient.TurnTerminate, err
			}

		case *runtime.WarningEvent:
			if err := a.sendUpdate(ctx, acpSess.id, acp.UpdateAgentMessageText(fmt.Sprintf("\nWarning: %s\n", e.Message))); err != nil {
				return runtimeclient.TurnTerminate, err
			}

		case *runtime.SessionTitleEvent:
			if err := a.sendUpdate(ctx, acpSess.id, acp.SessionUpdate{
				SessionInfoUpdate: &acp.SessionSessionInfoUpdate{
					SessionUpdate: "session_info_update",
					Title:         &e.Title,
				},
			}); err != nil {
				return runtimeclient.TurnTerminate, err
			}

		case *runtime.TokenUsageEvent:
			if e.Usage != nil {
				usageUpdate := acp.SessionUsageUpdate{
					SessionUpdate: "usage_update",
					Size:          int(e.Usage.ContextLimit),
					Used:          int(e.Usage.ContextLength),
				}
				if e.Usage.Cost > 0 {
					usageUpdate.Cost = &acp.Cost{
						Amount:   e.Usage.Cost,
						Currency: "USD",
					}
				}
				if err := a.sendUpdate(ctx, acpSess.id, acp.SessionUpdate{UsageUpdate: &usageUpdate}); err != nil {
					return runtimeclient.TurnTerminate, err
				}
			}

		case *runtime.ModelFallbackEvent:
			if err := a.sendUpdate(ctx, acpSess.id, acp.UpdateAgentMessageText(
				fmt.Sprintf("\nModel %s failed, falling back to %s (%s)\n", e.FailedModel, e.FallbackModel, e.Reason),
			)); err != nil {
				return runtimeclient.TurnTerminate, err
			}

		case *runtime.MaxIterationsReachedEvent:
			if err := a.handleMaxIterationsReached(ctx, acpSess, envelope.InteractionID, e); err != nil {
				return runtimeclient.TurnTerminate, err
			}
		}
		return runtimeclient.TurnContinue, nil
	})
	var drainErr *turn.DrainError
	if errors.As(termination.Err, &drainErr) {
		acpSess.close()
	}

	if termination.Err != nil {
		if termination.ObservationError {
			return fmt.Errorf("observe ACP session: %w", termination.Err)
		}
		return termination.Err
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// handleToolCallConfirmation handles tool call permission requests.
func (a *Agent) handleToolCallConfirmation(ctx context.Context, acpSess *Session, requestID string, e *runtime.ToolCallConfirmationEvent) error {
	toolCallUpdate := buildToolCallUpdate(e.ToolCall, e.ToolDefinition, acp.ToolCallStatusPending)

	permResp, err := a.conn.RequestPermission(ctx, acp.RequestPermissionRequest{
		SessionId: acp.SessionId(acpSess.id),
		ToolCall:  toolCallUpdate,
		Options: []acp.PermissionOption{
			{
				Kind:     acp.PermissionOptionKindAllowOnce,
				Name:     "Allow this action",
				OptionId: "allow",
			},
			{
				Kind:     acp.PermissionOptionKindAllowAlways,
				Name:     "Allow and remember my choice",
				OptionId: "allow-always",
			},
			{
				Kind:     acp.PermissionOptionKindRejectOnce,
				Name:     "Skip this action",
				OptionId: "reject",
			},
		},
	})
	if err != nil {
		return err
	}

	response := runtime.InteractionResponse{InteractionID: requestID, Kind: runtime.InteractionConfirmation}
	if permResp.Outcome.Cancelled != nil {
		response.Resume = runtime.ResumeRequest{Type: runtime.ResumeTypeReject, RequestID: e.RequestID}
		return acpSess.session.Respond(ctx, response)
	}

	if permResp.Outcome.Selected == nil {
		return errors.New("unexpected permission outcome")
	}

	switch string(permResp.Outcome.Selected.OptionId) {
	case "allow":
		response.Resume = runtime.ResumeRequest{Type: runtime.ResumeTypeApprove, RequestID: e.RequestID}
	case "allow-always":
		response.Resume = runtime.ResumeRequest{Type: runtime.ResumeTypeApproveAutonomous, RequestID: e.RequestID}
	case "reject":
		response.Resume = runtime.ResumeRequest{Type: runtime.ResumeTypeReject, RequestID: e.RequestID}
	default:
		return fmt.Errorf("unexpected permission option: %s", permResp.Outcome.Selected.OptionId)
	}

	return acpSess.session.Respond(ctx, response)
}

// handleMaxIterationsReached handles max iterations events.
func (a *Agent) handleMaxIterationsReached(ctx context.Context, acpSess *Session, requestID string, e *runtime.MaxIterationsReachedEvent) error {
	title := fmt.Sprintf("Maximum iterations (%d) reached", e.MaxIterations)
	permResp, err := a.conn.RequestPermission(ctx, acp.RequestPermissionRequest{
		SessionId: acp.SessionId(acpSess.id),
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: "max_iterations",
			Title:      &title,
			Kind:       acp.Ptr(acp.ToolKindExecute),
			Status:     acp.Ptr(acp.ToolCallStatusPending),
		},
		Options: []acp.PermissionOption{
			{
				Kind:     acp.PermissionOptionKindAllowOnce,
				Name:     "Continue",
				OptionId: "continue",
			},
			{
				Kind:     acp.PermissionOptionKindRejectOnce,
				Name:     "Stop",
				OptionId: "stop",
			},
		},
	})
	if err != nil {
		return err
	}

	response := runtime.InteractionResponse{InteractionID: requestID, Kind: runtime.InteractionMaxIterations}
	if permResp.Outcome.Cancelled != nil || permResp.Outcome.Selected == nil ||
		string(permResp.Outcome.Selected.OptionId) == "stop" {
		response.Resume = runtime.ResumeRequest{Type: runtime.ResumeTypeReject, RequestID: e.RequestID}
	} else {
		response.Resume = runtime.ResumeRequest{Type: runtime.ResumeTypeApprove, RequestID: e.RequestID}
	}

	return acpSess.session.Respond(ctx, response)
}

// emitAvailableCommands sends the list of available slash commands to the client.
func (a *Agent) emitAvailableCommands(ctx context.Context, acpSess *Session) error {
	return a.sendUpdate(ctx, acpSess.id, acp.SessionUpdate{
		AvailableCommandsUpdate: &acp.SessionAvailableCommandsUpdate{
			SessionUpdate: "available_commands_update",
			AvailableCommands: []acp.AvailableCommand{
				{Name: "new", Description: "Clear session history and start fresh"},
				{Name: "compact", Description: "Generate summary and compact session history"},
				{Name: "usage", Description: "Display token usage statistics"},
			},
		},
	})
}

func (a *Agent) supportsClientReadTextFile() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.clientFS.ReadTextFile && a.conn != nil
}

func (a *Agent) supportsClientWriteTextFile() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.clientFS.WriteTextFile && a.conn != nil
}

func (a *Agent) resolveSessionPath(sessionID, userPath string) (string, error) {
	a.mu.Lock()
	acpSess := a.sessions[sessionID]
	a.mu.Unlock()
	if acpSess == nil {
		return "", fmt.Errorf("session %s not found", sessionID)
	}

	workingDir, roots := acpSess.pathRoots(a.defaultWorkingDir())
	return resolvePathInRoots(userPath, workingDir, roots)
}

func (a *Agent) sessionListPaths(ctx context.Context, sessionID string) (string, []string) {
	a.mu.Lock()
	acpSess := a.sessions[sessionID]
	a.mu.Unlock()
	if acpSess != nil {
		cwd, _ := acpSess.pathRoots(a.defaultWorkingDir())
		return cwd, append([]string(nil), acpSess.additionalDirs...)
	}

	cwd := a.defaultWorkingDir()
	if a.sessionStore != nil {
		if sess, err := a.sessionStore.GetSession(ctx, sessionID); err == nil && sess.WorkingDir != "" {
			cwd = sess.WorkingDir
		}
	}
	return cwd, nil
}

func (a *Agent) defaultWorkingDir() string {
	if a.runConfig != nil && a.runConfig.WorkingDir != "" {
		if wd, err := resolveWorkingDir(a.runConfig.WorkingDir); err == nil {
			return wd
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	wd, err := resolveWorkingDir(cwd)
	if err != nil {
		return cwd
	}
	return wd
}

func (s *Session) pathRoots(fallbackWorkingDir string) (string, []string) {
	workingDir := s.workingDir
	if workingDir == "" && s.sess != nil {
		workingDir = s.sess.WorkingDir
	}
	if workingDir == "" {
		workingDir = fallbackWorkingDir
	}

	roots := make([]string, 0, 1+len(s.additionalDirs))
	if workingDir != "" {
		roots = append(roots, workingDir)
	}
	roots = append(roots, s.additionalDirs...)
	return workingDir, dedupePaths(roots)
}

func dedupePaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		key := normalizePathForComparison(filepath.Clean(path))
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, filepath.Clean(path))
	}
	return result
}

// resolveWorkingDir normalizes a working directory path.
func resolveWorkingDir(cwd string) (string, error) {
	wd := strings.TrimSpace(cwd)
	if wd == "" {
		return "", nil
	}
	absWd, err := filepath.Abs(wd)
	if err != nil {
		return "", fmt.Errorf("invalid working directory: %w", err)
	}
	return filepath.Clean(absWd), nil
}

func validateWorkingDir(workingDir string) error {
	if workingDir == "" {
		return nil
	}
	info, err := os.Stat(workingDir)
	if err != nil {
		return fmt.Errorf("working directory does not exist: %w", err)
	}
	if !info.IsDir() {
		return errors.New("working directory must be a directory")
	}
	return nil
}

func resolveAdditionalDirectories(dirs []string) ([]string, error) {
	resolved := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		if !filepath.IsAbs(dir) {
			return nil, fmt.Errorf("additional directory must be absolute: %s", dir)
		}
		absDir, err := resolveWorkingDir(dir)
		if err != nil {
			return nil, err
		}
		if err := validateWorkingDir(absDir); err != nil {
			return nil, fmt.Errorf("invalid additional directory %q: %w", dir, err)
		}
		resolved = append(resolved, absDir)
	}
	return dedupePaths(resolved), nil
}
