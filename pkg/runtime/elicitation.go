package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/docker/docker-agent/pkg/telemetry/genai"
	"github.com/docker/docker-agent/pkg/tools"
)

// ElicitationResult represents the result of an elicitation request.
//
// Returned by the embedder via ResumeElicitation when the user responds to a
// schema-driven prompt that an MCP server (or the runtime) requested.
type ElicitationResult struct {
	Action  tools.ElicitationAction
	Content map[string]any // The submitted form data (only present when action is "accept")
}

// ElicitationError represents a declined or cancelled elicitation, exposed
// to callers that prefer error-style propagation over an Action value.
type ElicitationError struct {
	Action  string
	Message string
}

func (e *ElicitationError) Error() string {
	return fmt.Sprintf("elicitation %s: %s", e.Action, e.Message)
}

// ElicitationRequestHandler is the callback signature an embedder can supply
// to handle inbound elicitation requests directly (e.g. an HTTP server).
type ElicitationRequestHandler func(ctx context.Context, message string, schema map[string]any) (map[string]any, error)

// waiterState is the terminal-state machine for a single elicitationWaiter.
// Exactly one of resolve/cancel ever wins the transition out of pending,
// closing the #3584 cancellation-vs-response race: a resolve that already
// flipped the state keeps its value in the channel for the ctx.Done() branch
// to drain, instead of the handler discarding a response ResumeElicitation
// already reported as delivered.
type waiterState int32

const (
	waiterPending waiterState = iota
	waiterResolved
	waiterCanceled
)

// elicitationWaiter is one pending elicitation request's response slot.
type elicitationWaiter struct {
	ch       chan ElicitationResult
	canceled chan struct{}
	state    atomic.Int32
}

func newElicitationWaiter() *elicitationWaiter {
	return &elicitationWaiter{ch: make(chan ElicitationResult, 1), canceled: make(chan struct{})}
}

// tryResolve attempts to deliver result, winning the terminal-state race
// only if the waiter is still pending. Returns false without sending when
// the waiter was already resolved or cancelled.
func (w *elicitationWaiter) tryResolve(result ElicitationResult) bool {
	if !w.state.CompareAndSwap(int32(waiterPending), int32(waiterResolved)) {
		return false
	}
	w.ch <- result
	return true
}

// tryCancel attempts to mark the waiter cancelled, winning the terminal-state
// race only if it is still pending. Returns false when resolve already won —
// the caller must then receive from ch instead of treating this as a
// cancellation, since a value is already there (or is about to land).
func (w *elicitationWaiter) tryCancel() bool {
	if !w.state.CompareAndSwap(int32(waiterPending), int32(waiterCanceled)) {
		return false
	}
	close(w.canceled)
	return true
}

// OnElicitationRequest installs a transport subscription. Requests carry the
// resolved session identity; responses are accepted only by that session owner.
func (r *LocalRuntime) OnElicitationRequest(handler func(Event)) {
	r.elicitationSinkMu.Lock()
	defer r.elicitationSinkMu.Unlock()
	r.onElicitationRequest = handler
}

type sessionElicitationSink struct {
	id      uint64
	parent  string
	handler func(Event) bool

	mu       sync.Mutex
	idle     *sync.Cond
	inFlight int
}

func newSessionElicitationSink(id uint64, parent string, handler func(Event) bool) *sessionElicitationSink {
	sink := &sessionElicitationSink{id: id, parent: parent, handler: handler}
	sink.idle = sync.NewCond(&sink.mu)
	return sink
}

func (s *sessionElicitationSink) acquire() func(Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight++
	return s.handler
}

func (s *sessionElicitationSink) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight--
	if s.inFlight == 0 {
		s.idle.Broadcast()
	}
}

func (s *sessionElicitationSink) wait() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.inFlight > 0 {
		s.idle.Wait()
	}
}

// SubscribeSessionElicitations registers one App-owned, session-scoped reliable
// elicitation route without replacing the Runtime interface's embedder-wide
// fallback sink. Exact-session ownership wins. Otherwise parent links captured
// from known session drivers are followed under the registry lock, so a
// detached/background descendant is delivered to its nearest open ancestor
// App. Traversal is bounded by the number of known drivers and cycles abort to
// the fallback sink. Re-registering a session deterministically replaces its
// prior App subscriber; the returned cancel only removes its own generation.
func (r *LocalRuntime) SubscribeSessionElicitations(sessionID, parentID string, handler func(Event) bool) (func(), bool) {
	r.elicitationSinkMu.Lock()
	if r.elicitationSessionSinks == nil {
		r.elicitationSessionSinks = make(map[string]*sessionElicitationSink)
	}
	r.nextElicitationSessionSink++
	id := r.nextElicitationSessionSink
	sink := newSessionElicitationSink(id, parentID, handler)
	r.elicitationSessionSinks[sessionID] = sink
	r.elicitationSinkMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.elicitationSinkMu.Lock()
			if current, ok := r.elicitationSessionSinks[sessionID]; ok && current.id == id {
				delete(r.elicitationSessionSinks, sessionID)
			}
			r.elicitationSinkMu.Unlock()
			sink.wait()
		})
	}, true
}

// MirrorsElicitationOnRunStream tells embedders that transport callbacks and
// the addressed execution stream project the same interaction. An embedder
// using the callback must not forward the stream copy a second time.
func (r *LocalRuntime) MirrorsElicitationOnRunStream() {}

// elicitationRoute follows canonical immutable ancestry for transport routing.
func (r *LocalRuntime) elicitationRoute(sessionID string) []string {
	parents := make(map[string]string)
	if r.sessionDrivers != nil {
		driverRoute := r.sessionDrivers.ElicitationRoute(sessionID)
		for i, id := range driverRoute {
			parent := ""
			if i+1 < len(driverRoute) {
				parent = driverRoute[i+1]
			}
			parents[id] = parent
		}
	}

	r.elicitationSinkMu.RLock()
	for id, sink := range r.elicitationSessionSinks {
		if _, known := parents[id]; !known {
			parents[id] = sink.parent
		}
	}
	r.elicitationSinkMu.RUnlock()

	route := make([]string, 0, len(parents)+1)
	seen := make(map[string]struct{}, len(parents)+1)
	for current := sessionID; current != "" && len(route) <= len(parents); current = parents[current] {
		if _, duplicate := seen[current]; duplicate {
			break
		}
		seen[current] = struct{}{}
		route = append(route, current)
	}
	return route
}

func (r *LocalRuntime) emitElicitationRequest(event Event) bool {
	var route []string
	if scoped, ok := event.(SessionScoped); ok {
		route = r.elicitationRoute(scoped.GetSessionID())
	}
	r.elicitationSinkMu.RLock()
	var (
		scopedSink    *sessionElicitationSink
		scopedHandler func(Event) bool
		fallback      func(Event)
	)
	for _, sessionID := range route {
		if sink, exists := r.elicitationSessionSinks[sessionID]; exists {
			scopedSink = sink
			scopedHandler = sink.acquire()
			break
		}
	}
	if scopedHandler == nil {
		fallback = r.onElicitationRequest
	}
	r.elicitationSinkMu.RUnlock()
	if scopedHandler != nil {
		defer scopedSink.release()
		return scopedHandler(event)
	}
	if fallback == nil {
		return false
	}
	fallback(event)
	return true
}

// EmitElicitationRequestForTesting invokes whatever OnElicitationRequest sink
// is currently registered, exactly as elicitationHandler would, but without
// the real MCP elicitation handshake. elicitationHandler is unexported, so
// callers outside this package (e.g. pkg/server) cannot drive it directly to
// prove a *specific* runtime instance has the expected sink wired; this
// gives them a seam to do that instead of reconstructing the sink separately
// and invoking it in isolation, which would pass even if the runtime under
// test was never actually wired up (#3584 re-review should-fix 1).
func (r *LocalRuntime) EmitElicitationRequestForTesting(event Event) {
	r.emitElicitationRequest(event)
}

// hasElicitationSink reports whether the requested session has an exact or
// ancestor App subscriber, or the embedder-wide fallback sink. It uses the
// same route resolution as delivery so one unrelated open App cannot make a
// headless background session wait for a response nobody can receive.
func (r *LocalRuntime) hasElicitationSink(sessionID string) bool {
	route := r.elicitationRoute(sessionID)
	r.elicitationSinkMu.RLock()
	defer r.elicitationSinkMu.RUnlock()
	if r.onElicitationRequest != nil {
		return true
	}
	for _, id := range route {
		if _, ok := r.elicitationSessionSinks[id]; ok {
			return true
		}
	}
	return false
}

// backgroundElicitationDeclinedNote returns a model-readable explanation for
// an elicitation that was auto-declined because it originated from a
// background (non-interactive) session with no UI available to answer it.
func backgroundElicitationDeclinedNote(message string) string {
	return fmt.Sprintf(
		"Note: a tool requested user input (%q) while running as a background task. "+
			"Background tasks have no interactive UI to answer such requests, so it was "+
			"automatically declined. If the tool truly needs user input, ask the user to "+
			"run this task in the foreground instead.",
		message,
	)
}

// elicitationSpec carries an MCP request through the shared waiter registry.
type elicitationSpec struct {
	message string
	mode    string
	schema  any
	url     string
	// serverElicitationID is the originating MCP server's wire ID, if any.
	// Informational only — never a routing key (#3584 review item 2a).
	serverElicitationID string
	meta                map[string]any
	// agentName and sessionID override the runtime-derived defaults (the
	// owner-pinned agent and the ctx conversation ID) when the caller
	// knows the owning agent/session more precisely.
	agentName string
	sessionID string
}

type nonInteractiveSessionKey struct{}

func withNonInteractiveSession(ctx context.Context) context.Context {
	return context.WithValue(ctx, nonInteractiveSessionKey{}, true)
}

func isNonInteractiveSession(ctx context.Context) bool {
	value, _ := ctx.Value(nonInteractiveSessionKey{}).(bool)
	return value
}

// elicitationHandler is the MCP-toolset-side hook that turns an inbound
// elicitation request from a server into an ElicitationRequest event and
// waits for the embedder's response, correlated by elicitation ID.
func (r *LocalRuntime) elicitationHandler(ctx context.Context, req *mcp.ElicitParams) (tools.ElicitationResult, error) {
	slog.DebugContext(ctx, "Elicitation request received from MCP server", "message", req.Message)
	return r.requestElicitation(ctx, elicitationSpec{
		message:             req.Message,
		mode:                req.Mode,
		schema:              req.RequestedSchema,
		url:                 req.URL,
		serverElicitationID: req.ElicitationID,
		meta:                req.Meta,
	})
}

// requestElicitation emits spec as an ElicitationRequest event and waits for
// the embedder's response, correlated by elicitation ID.
func (r *LocalRuntime) requestElicitation(ctx context.Context, spec elicitationSpec) (tools.ElicitationResult, error) {
	// In non-interactive mode (e.g., MCP serve), there is no user to respond
	// to elicitation requests. Decline immediately instead of blocking forever.
	if r.nonInteractive {
		slog.DebugContext(ctx, "Declining elicitation in non-interactive mode", "message", spec.message)
		return tools.ElicitationResult{
			Action: tools.ElicitationActionDecline,
		}, nil
	}

	sessionID := spec.sessionID
	if sessionID == "" {
		sessionID = genai.ConversationIDFromContext(ctx)
	}

	// A background session (run_background_agent) marks its context so
	// toolset Start() OAuth fails fast instead of eliciting (#3200). Mid-call
	// elicitations reach here regardless of that marker, so extend the same
	// fast-fail idea: if this call is running in such a context AND no
	// embedder has registered a sink to surface it (headless use — e.g. the
	// --exec CLI path, which never registers OnElicitationRequest), nobody at
	// all can answer this request. Decline immediately with a model-readable
	// note instead of parking a goroutine forever (#3584).
	backgroundWithoutPrompts := isNonInteractiveSession(ctx) || !tools.InteractivePromptsAllowed(ctx)
	if backgroundWithoutPrompts && !r.hasElicitationSink(sessionID) {
		slog.WarnContext(ctx, "Declining elicitation: background session has no UI to answer it", "message", spec.message)
		r.recordElicitationDecline(ctx, sessionID, spec.message)
		return tools.ElicitationResult{Action: tools.ElicitationActionDecline}, nil
	}

	owner, canonical := r.sessionDrivers.Lookup(sessionID)
	if !canonical && sessionID == "" {
		var err error
		owner, err = r.compatibilityInputTarget(ctx)
		if err != nil {
			return tools.ElicitationResult{}, err
		}
		sessionID, canonical = owner.identityID, true
	}
	if !canonical {
		return tools.ElicitationResult{}, &SessionError{Kind: SessionErrorNotFound, SessionID: sessionID, Operation: "elicitation_route"}
	}
	agentName := spec.agentName
	if agentName == "" {
		agentName = owner.AgentName()
	}

	r.executeOnUserInputHooks(ctx, r.resolveSessionAgent(owner.session()), sessionID, "elicitation")

	// The registry key (and the ElicitationID surfaced to clients for
	// ResumeElicitation routing) is always a freshly generated, internal
	// ID — never the MCP wire elicitation ID. The wire value is only
	// ever set for URL-mode elicitations and is chosen by the originating
	// MCP server; two independent servers (e.g. two background jobs each
	// talking to their own MCP process) can legitimately reuse the same
	// value. Trusting it as the registry key would let the second
	// request's register() silently evict the first request's waiter,
	// orphaning it (#3584 review item 2a). The wire ID is preserved
	// separately on the event (ServerElicitationID) for callers that want
	// to correlate with server-side logs; it is never used for routing.
	correlationID := uuid.NewString()

	// Register the waiter BEFORE emitting the request event. This is the
	// #3584 TOCTOU fix: previously a response that arrived before the
	// handler reached its receive on the shared channel was lost because
	// there was nothing to receive it into yet.
	wt := newElicitationWaiter()
	defer owner.abandonElicitation(correlationID, wt)

	slog.DebugContext(ctx, "Sending elicitation request event to client",
		"message", spec.message,
		"mode", spec.mode,
		"requested_schema", spec.schema,
		"url", spec.url,
		"elicitation_id", correlationID,
		"server_elicitation_id", spec.serverElicitationID)
	slog.DebugContext(ctx, "Elicitation request meta", "meta", spec.meta)

	ev := ElicitationRequest(spec.message, spec.mode, spec.schema, spec.url, correlationID, spec.serverElicitationID, sessionID, spec.meta, agentName)
	if elicitation, ok := ev.(*ElicitationRequestEvent); ok {
		elicitation.RequestID = correlationID
		if err := owner.registerElicitation(ctx, correlationID, elicitation, wt); err != nil {
			return tools.ElicitationResult{}, err
		}
		elicitation.ownerPublished = true
	}

	// Publish before invoking a route that may synchronously resolve the prompt.
	if sink, ok := ctx.Value(executionEventSinkKey{}).(EventSink); ok {
		sink.Emit(ev)
		if !waitForObserverDelivery(ctx, sink) {
			owner.abandonElicitation(correlationID, wt)
			if waiterState(wt.state.Load()) == waiterCanceled {
				return tools.ElicitationResult{}, ctx.Err()
			}
			result := <-wt.ch
			return tools.ElicitationResult{Action: result.Action, Content: result.Content}, nil
		}
	}
	delivered := r.emitElicitationRequest(cloneSessionEvent(ev))
	if backgroundWithoutPrompts && !delivered {
		r.recordElicitationDecline(ctx, sessionID, spec.message)
		return tools.ElicitationResult{Action: tools.ElicitationActionDecline}, nil
	}

	// Wait for the response addressed to this specific request. The
	// ctx.Done() branch cannot simply return ctx.Err(): resolve() may have
	// already won the terminal-state race an instant earlier and be about
	// to (or have already) delivered into wt.ch, in which case
	// ResumeElicitation already reported success to its caller and this
	// handler must not silently discard that response (#3584 review item
	// 2b). The owner claims cancellation and journals it atomically.
	select {
	case result := <-wt.ch:
		return tools.ElicitationResult{
			Action:  result.Action,
			Content: result.Content,
		}, nil
	case <-wt.canceled:
		return tools.ElicitationResult{}, context.Canceled
	case <-ctx.Done():
		slog.DebugContext(ctx, "Context cancelled while waiting for elicitation response")
		owner.abandonElicitation(correlationID, wt)
		if waiterState(wt.state.Load()) == waiterCanceled {
			return tools.ElicitationResult{}, ctx.Err()
		}
		result := <-wt.ch
		return tools.ElicitationResult{
			Action:  result.Action,
			Content: result.Content,
		}, nil
	}
}
