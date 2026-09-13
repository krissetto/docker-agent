package server

import (
	"cmp"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"time"
	"unicode"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/echolog"
	"github.com/docker/docker-agent/pkg/httpsec"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools/mcp"
	"github.com/docker/docker-agent/pkg/upstream"
)

type Server struct {
	e         *echo.Echo
	sm        *SessionManager
	authToken string
	// heartbeatInterval is how often an idle /events stream emits an SSE
	// comment (": ping") so clients can tell a quiet session from a dead
	// transport. SSE comments are invisible to EventSource clients and carry
	// no id, so they never interfere with the sequenced stream or its replay.
	heartbeatInterval time.Duration
}

func New(ctx context.Context, sessionStore session.Store, runConfig *config.RuntimeConfig, refreshInterval time.Duration, agentSources config.Sources, authToken string, maxRequestBytes int64, opts ...SessionManagerOpt) (*Server, error) {
	return newServer(ctx, sessionStore, runConfig, refreshInterval, agentSources, authToken, maxRequestBytes, "", opts...)
}

// NewWithCORS constructs an API server with strict opt-in browser access.
func NewWithCORS(ctx context.Context, sessionStore session.Store, runConfig *config.RuntimeConfig, refreshInterval time.Duration, agentSources config.Sources, authToken string, maxRequestBytes int64, corsOrigin string, opts ...SessionManagerOpt) (*Server, error) {
	return newServer(ctx, sessionStore, runConfig, refreshInterval, agentSources, authToken, maxRequestBytes, corsOrigin, opts...)
}

func newServer(ctx context.Context, sessionStore session.Store, runConfig *config.RuntimeConfig, refreshInterval time.Duration, agentSources config.Sources, authToken string, maxRequestBytes int64, corsOrigin string, opts ...SessionManagerOpt) (*Server, error) {
	return NewWithManager(NewSessionManager(ctx, agentSources, sessionStore, refreshInterval, runConfig, opts...), authToken, WithMaxRequestBytes(maxRequestBytes), WithCORSOrigin(corsOrigin)), nil
}

const defaultMaxRequestBytes int64 = 1 << 20 // 1 MiB

// Option configures a [Server] at construction time.
type Option func(*serverOptions)

type serverOptions struct {
	maxRequestBytes int64
	corsOrigin      string
}

// WithMaxRequestBytes sets the maximum request body size in bytes. Requests
// whose body exceeds the limit are rejected with HTTP 413. Zero or negative
// values fall back to the default (1 MiB).
func WithMaxRequestBytes(n int64) Option {
	return func(o *serverOptions) { o.maxRequestBytes = n }
}

// WithCORSOrigin enables browser access for the exact, explicitly configured
// HTTP(S) origin. Empty leaves CORS disabled.
func WithCORSOrigin(origin string) Option {
	return func(o *serverOptions) { o.corsOrigin = origin }
}

// NewWithManager builds a Server around an already-constructed SessionManager.
// Useful when the runtime is owned by another component (e.g. the TUI) and
// only needs to be exposed over HTTP.
func NewWithManager(sm *SessionManager, authToken string, opts ...Option) *Server {
	var o serverOptions
	for _, opt := range opts {
		opt(&o)
	}
	maxBytes := o.maxRequestBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxRequestBytes
	}

	e := echo.New()
	e.Use(echolog.RedactedRequestLogger())
	e.Use(middleware.BodyLimit(strconv.FormatInt(maxBytes, 10)))
	e.Use(echo.WrapMiddleware(upstream.Handler))
	if o.corsOrigin != "" {
		e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
			AllowOrigins: []string{o.corsOrigin},
			AllowMethods: []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete, http.MethodOptions},
			AllowHeaders: []string{"Authorization", "Content-Type", "Accept"},
			MaxAge:       86400,
		}))
	}

	// Add bearer token middleware if token is configured
	if authToken != "" {
		e.Use(BearerTokenMiddleware(authToken))
	}

	s := &Server{e: e, sm: sm, authToken: authToken, heartbeatInterval: defaultEventsHeartbeatInterval}
	s.registerRoutes()
	return s
}

func (s *Server) registerRoutes() {
	// Health and readiness endpoints (not under /api)
	s.e.GET("/health", s.health)
	s.e.GET("/ready", s.ready)

	group := s.e.Group("/api")

	group.GET("/agents", s.getAgents)
	group.GET("/agents/:id", s.getAgentConfig)

	group.POST("/sessions/:id/tools/toggle", s.toggleSessionYolo)
	group.PATCH("/sessions/:id/safety-policy", s.updateSessionSafetyPolicy)
	group.PATCH("/sessions/:id/permissions", s.updateSessionPermissions)
	group.PATCH("/sessions/:id/tokens", s.updateSessionTokens)
	group.POST("/sessions/:id/fork", s.forkSession)
	group.PATCH("/sessions/:id/messages/:msg_id", s.updateMessage)
	group.POST("/sessions/:id/summaries", s.addSummary)
	group.GET("/sessions/:id/recovery", s.getSessionRecoveryData)
	group.POST("/sessions/batch/delete", s.batchDeleteSessions)
	group.POST("/sessions/batch/export", s.batchExportSessions)

	s.registerCanonicalSessionRoutes(group.Group("/sessions"))

	group.GET("/agents/:id/:agent_name/tools/count", s.getAgentToolCount)

	group.POST("/mcp-oauth/callback", s.mcpOAuthCallback)

	group.GET("/ping", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})
	group.GET("/ready", s.sessionsReady)
}

func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	// Wrap the Echo handler with otelhttp so the configured W3C
	// propagator extracts `traceparent` / `tracestate` / `baggage`
	// from incoming API requests. Without this the API server's
	// runtime spans (already wired via `WithTracer` in the session
	// manager) start fresh trace ids per request rather than
	// chaining onto the calling client's trace.
	srv := http.Server{
		Handler:           otelhttp.NewHandler(s.e, "agent-api"),
		ReadHeaderTimeout: 10 * time.Second,
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && ctx.Err() == nil {
		slog.ErrorContext(ctx, "Failed to start server", "error", err)
		return err
	}

	if ctx.Err() != nil {
		<-shutdownDone
	}

	// Session runtimes built for this server outlive individual requests; stop
	// them once no request can reach them anymore.
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := s.sm.Shutdown(stopCtx); err != nil {
		slog.ErrorContext(ctx, "Failed to shut down session runtimes", "error", err)
	}
	return nil
}

const maxAPITimeout = 5 * time.Minute

// ready blocks until at least one session is registered. The caller
// may supply a ?timeout=<duration> query parameter (default 30s, max 5m).
func (s *Server) sessionsReady(c echo.Context) error {
	timeout := 30 * time.Second
	if v := c.QueryParam("timeout"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid timeout: %v", err))
		}
		timeout = min(d, maxAPITimeout)
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), timeout)
	defer cancel()

	if err := s.sm.WaitReady(ctx); err != nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "no sessions registered within timeout")
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) getAgents(c echo.Context) error {
	// A failing source must not hide healthy agents from callers.
	agents := []api.Agent{}
	for k, agentSource := range s.sm.Sources {
		slog.Debug("API source", "source", agentSource.Name())

		cfg, err := config.Load(c.Request().Context(), agentSource)
		if err != nil {
			slog.Error("Failed to load config from API source", "key", k, "error", err)
			continue
		}

		agent, ok := agentsAPIEntry(k, cfg)
		if !ok {
			slog.Warn("No agents found in config from API source", "key", k)
			continue
		}
		agents = append(agents, agent)
	}

	slices.SortFunc(agents, func(a, b api.Agent) int {
		return cmp.Compare(a.Name, b.Name)
	})

	return c.JSON(http.StatusOK, agents)
}

// agentsAPIEntry summarizes a loaded config into the api.Agent listing
// entry for /api/agents. The len(cfg.Agents)==0 check MUST run before any
// access to cfg.Agents (e.g. First()), which panics on an empty slice: this
// guards the handler even though validateConfig already rejects agent-less
// configs at load time (defense in depth against a bypass or future
// regression in that check).
func agentsAPIEntry(name string, cfg *latest.Config) (api.Agent, bool) {
	if len(cfg.Agents) == 0 {
		return api.Agent{}, false
	}
	root := cfg.Agents.First()
	var commands []string
	if len(root.Commands) > 0 {
		commands = make([]string, 0, len(root.Commands))
		for k := range root.Commands {
			commands = append(commands, k)
		}
		slices.Sort(commands)
	}
	return api.Agent{
		Name:        name,
		Multi:       len(cfg.Agents) > 1,
		Description: root.Description,
		Commands:    commands,
	}, true
}

func (s *Server) getAgentConfig(c echo.Context) error {
	cfg, err := s.sm.LoadAgentConfig(c.Request().Context(), c.Param("id"))
	if err != nil {
		return agentSourceHTTPError("failed to load agent source", err)
	}
	return c.JSON(http.StatusOK, cfg)
}

func agentSourceHTTPError(operation string, err error) error {
	switch {
	case errors.Is(err, ErrAgentNotFound):
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	case errors.Is(err, ErrAgentSourceUnavailable):
		return echo.NewHTTPError(http.StatusBadGateway, fmt.Sprintf("%s: %v", operation, err))
	default:
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("%s: %v", operation, err))
	}
}

// forkSession creates a new session whose history is a deep copy of
// an existing session up to (but excluding) the Nth user message. The
// new session uses a fork-numbered title and starts with no runtime
// attached.
func (s *Server) forkSession(c echo.Context) error {
	var req api.ForkSessionRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}

	forked, err := s.sm.ForkSession(c.Request().Context(), c.Param("id"), req.UserMessageIndex)
	if err != nil {
		switch {
		case errors.Is(err, ErrForkOutOfRange),
			errors.Is(err, ErrForkInSubSession):
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to fork session: %v", err))
	}

	return c.JSON(http.StatusOK, api.SessionResponse{
		ID:            forked.ID,
		Title:         forked.Title,
		CreatedAt:     forked.CreatedAt,
		Messages:      forked.GetAllMessages(),
		ToolsApproved: forked.ToolsApproved,
		SafetyPolicy:  forked.SafetyPolicy,
		InputTokens:   forked.InputTokens,
		OutputTokens:  forked.OutputTokens,
		WorkingDir:    forked.WorkingDir,
		Permissions:   forked.ClonePermissions(),
	})
}

func (s *Server) toggleSessionYolo(c echo.Context) error {
	if err := s.sm.ToggleToolApproval(c.Request().Context(), c.Param("id")); err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, nil)
}

func (s *Server) updateSessionSafetyPolicy(c echo.Context) error {
	var req api.UpdateSessionSafetyPolicyRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}
	if err := s.sm.SetSessionSafetyPolicy(c.Request().Context(), c.Param("id"), req.SafetyPolicy); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]string{"message": "session safety policy updated"})
}

func (s *Server) getAgentToolCount(c echo.Context) error {
	count, err := s.sm.GetAgentToolCount(c.Request().Context(), c.Param("id"), c.Param("agent_name"))
	if err != nil {
		return agentSourceHTTPError("failed to get agent tool count", err)
	}

	return c.JSON(http.StatusOK, map[string]int{"available_tools": count})
}

func (s *Server) updateSessionPermissions(c echo.Context) error {
	sessionID := c.Param("id")
	var req api.UpdateSessionPermissionsRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}

	if err := s.sm.UpdateSessionPermissions(c.Request().Context(), sessionID, req.Permissions); err != nil {
		return sessionHTTPError(err)
	}

	return c.JSON(http.StatusOK, map[string]string{"message": "session permissions updated"})
}

// mcpOAuthCallback is the out-of-band entry point used by embedders
// that receive an OAuth deeplink (e.g. a system-wide URL-scheme handler
// or an OS-integrated launcher) and want to forward the resulting
// {code, state} to docker-agent without going through the session-keyed
// session interaction response path.
//
// The state value is opaque, high-entropy and was generated in-process by
// docker-agent's unmanaged OAuth flow (see GenerateState in
// pkg/tools/mcp). Looking it up in the pending-oauth registry IS the
// per-request authorization: docker-agent only accepts callbacks for
// states it is currently awaiting. An unknown state returns 404 (which
// is the expected outcome for replays and any state value the agent did
// not itself generate).
//
// Threat model:
//
//   - The registry is the primary defence. State values are >=128-bit
//     opaque tokens from GenerateState; an attacker that has not
//     observed a live state cannot brute-force one in a useful window.
//   - State values DO appear in transit: in the elicitation Meta on the
//     session SSE stream (visible only to the connected client), and in
//     the authorize URL the user opens (visible to the user's browser
//     and the authorization server). They are also written to debug
//     logs when --debug is on.
//   - If an attacker DOES observe a live state (e.g. via leaked debug
//     logs or a compromised host), they could POST here with an
//     attacker-controlled code; the resulting token would be bound to
//     the attacker's account, not the user's. Setting --auth-token
//     blocks this regardless of state leakage, because the route then
//     also requires bearer auth.
//   - Operators running docker-agent on a network-reachable interface
//     SHOULD configure --auth-token. Defaults to localhost-only via the
//     existing socket binding when not overridden.
//
// The handler never blocks: it hands the callback to the buffered
// channel of the waiting flow and returns immediately. The token
// exchange and storage happen inside that flow's goroutine, which then
// emits the existing authorization_event on the session SSE stream.
func (s *Server) mcpOAuthCallback(c echo.Context) error {
	q := c.QueryParams()
	state := q.Get("state")
	if state == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "missing state query parameter")
	}
	code := q.Get("code")
	errStr := q.Get("error")
	errDesc := q.Get("error_description")
	if code == "" && errStr == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "missing both code and error query parameters")
	}

	err := mcp.DeliverPendingOAuthCallback(state, mcp.PendingOAuthCallback{
		Code:    code,
		Error:   errStr,
		ErrDesc: errDesc,
	})
	if errors.Is(err, mcp.ErrPendingOAuthNoWaiter) {
		return echo.NewHTTPError(http.StatusNotFound, "no pending OAuth flow for the given state")
	}
	if err != nil {
		slog.WarnContext(c.Request().Context(), "Failed to deliver pending oauth callback", "error", err)
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to deliver pending oauth callback: %v", err))
	}
	return c.JSON(http.StatusOK, nil)
}

// defaultEventsHeartbeatInterval is the default for [Server.heartbeatInterval].
const defaultEventsHeartbeatInterval = 15 * time.Second

// parseSinceParam resolves the resume point for an /events stream from the
// ?since=<seq> query parameter, falling back to the Last-Event-ID header that
// SSE clients replay automatically on reconnect. Returns nil when neither is
// present or parseable, meaning "replay the current buffer, then tail".
func (s *Server) updateMessage(c echo.Context) error {
	sessionID := c.Param("id")
	msgID := c.Param("msg_id")
	var req api.UpdateMessageRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}

	if req.Message == nil {
		return echo.NewHTTPError(http.StatusBadRequest, "message is required")
	}

	if err := s.sm.UpdateMessage(c.Request().Context(), sessionID, msgID, req.Message); err != nil {
		if errors.Is(err, ErrSessionBusy) {
			return echo.NewHTTPError(http.StatusConflict, err.Error())
		}
		return sessionHTTPError(err)
	}

	return c.JSON(http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) addSummary(c echo.Context) error {
	sessionID := c.Param("id")
	var req api.AddSummaryRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}

	if req.Summary == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "summary is required")
	}
	if err := validateSummaryAttribution(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	item := session.Item{
		Summary: req.Summary,
		// Older clients send first_kept_entry under the legacy "tokens" name.
		FirstKeptEntry: cmp.Or(req.FirstKeptEntry, req.Tokens),
		Cost:           req.Cost,
		Model:          req.Model,
		Usage:          req.Usage,
	}
	if err := s.sm.AddSummary(c.Request().Context(), sessionID, item); err != nil {
		return sessionHTTPError(err)
	}

	return c.JSON(http.StatusCreated, map[string]string{"status": "added"})
}

// maxSummaryModelNameLen bounds the model attribution accepted on a summary
// so a hostile client can't persist arbitrarily large names into the store.
const maxSummaryModelNameLen = 256

// validateSummaryAttribution sanity-checks the cost-attribution metadata of
// an AddSummary request and normalizes an all-zero usage to nil so remote
// summaries look identical to locally generated ones (which only record
// usage when an LLM call actually ran).
func validateSummaryAttribution(req *api.AddSummaryRequest) error {
	if len(req.Model) > maxSummaryModelNameLen {
		return fmt.Errorf("model name exceeds %d characters", maxSummaryModelNameLen)
	}
	for _, r := range req.Model {
		if unicode.IsControl(r) {
			return errors.New("model name must not contain control characters")
		}
	}
	if u := req.Usage; u != nil {
		if u.InputTokens < 0 || u.OutputTokens < 0 || u.CachedInputTokens < 0 || u.CacheWriteTokens < 0 || u.ReasoningTokens < 0 {
			return errors.New("usage token counts must not be negative")
		}
		if *u == (chat.Usage{}) {
			req.Usage = nil
		}
	}
	return nil
}

func (s *Server) updateSessionTokens(c echo.Context) error {
	sessionID := c.Param("id")
	var req api.UpdateSessionTokensRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}

	if err := s.sm.UpdateSessionTokens(c.Request().Context(), sessionID, req.InputTokens, req.OutputTokens, req.Cost); err != nil {
		return sessionHTTPError(err)
	}

	return c.JSON(http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) batchDeleteSessions(c echo.Context) error {
	var req api.BatchDeleteSessionsRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}

	if len(req.SessionIDs) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "session_ids cannot be empty")
	}

	deleted, failed := s.sm.BatchDeleteSessions(c.Request().Context(), req.SessionIDs)

	return c.JSON(http.StatusOK, api.BatchDeleteSessionsResponse{
		DeletedCount: deleted,
		FailedCount:  len(failed),
		FailedIDs:    failed,
	})
}

func (s *Server) batchExportSessions(c echo.Context) error {
	var req api.BatchExportSessionsRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}

	if len(req.SessionIDs) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "session_ids cannot be empty")
	}

	export, err := s.sm.BatchExportSessions(c.Request().Context(), req.SessionIDs)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to export sessions: %v", err))
	}

	return c.JSON(http.StatusOK, export)
}

func (s *Server) health(c echo.Context) error {
	return c.JSON(http.StatusOK, api.HealthResponse{
		Status: "ok",
	})
}

func (s *Server) ready(c echo.Context) error {
	// Check if session store is accessible (quick connectivity check)
	ctx, cancel := context.WithTimeout(c.Request().Context(), 100*time.Millisecond)
	defer cancel()

	sessions, err := s.sm.GetSessions(ctx)
	var storeConnected bool
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		// We assume store is connected if we can query it or we hit a timeout
		// (timeout is still better than a hard connection failure)
		storeConnected = true
	}

	activeSessions := 0
	if sessions != nil {
		activeSessions = len(sessions)
	}

	var toolsetHealth string
	var latestError string

	// Determine overall readiness
	ready := storeConnected
	if !ready {
		latestError = "store disconnected"
	}

	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}

	if !ready {
		toolsetHealth = "unavailable"
	} else {
		toolsetHealth = "ok"
	}

	return c.JSON(status, api.ReadyResponse{
		Ready:          ready,
		ActiveSessions: activeSessions,
		StoreConnected: storeConnected,
		ToolsetHealth:  toolsetHealth,
		LatestError:    latestError,
	})
}

func (s *Server) getSessionRecoveryData(c echo.Context) error {
	sessionID := c.Param("id")
	data, err := s.sm.ExportSessionForRecovery(c.Request().Context(), sessionID)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to export session: %v", err))
	}

	return c.JSON(http.StatusOK, data)
}

// ValidateCORSOrigin validates the strict single origin accepted by serve api.
// Wildcards, regexes, and opaque origins (including "null") are intentionally
// rejected; operators must name one HTTP(S) browser origin exactly.
func ValidateCORSOrigin(origin string) error {
	matcher, err := httpsec.ParseOrigins(origin)
	if err != nil {
		return err
	}
	if matcher.HasPatterns() || len(matcher.Literals()) != 1 || matcher.Literals()[0] == "*" {
		return errors.New("must be one exact http(s) origin")
	}
	return nil
}

// BearerTokenMiddleware validates bearer token authentication
func BearerTokenMiddleware(expectedToken string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			// The demo document is inert and intentionally public; its API calls
			// still pass through bearer authentication.
			if c.Request().Method == http.MethodOptions || c.Path() == "/health" || c.Path() == "/ready" {
				return next(c)
			}

			auth := c.Request().Header.Get("Authorization")
			if auth == "" {
				return echo.NewHTTPError(http.StatusUnauthorized, "missing Authorization header")
			}

			// Extract Bearer token
			const prefix = "Bearer "
			if len(auth) < len(prefix) || auth[:len(prefix)] != prefix {
				return echo.NewHTTPError(http.StatusUnauthorized, "invalid Authorization header format")
			}

			token := auth[len(prefix):]
			if subtle.ConstantTimeCompare([]byte(token), []byte(expectedToken)) != 1 {
				return echo.NewHTTPError(http.StatusUnauthorized, "invalid token")
			}

			return next(c)
		}
	}
}
