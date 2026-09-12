package runtime

import (
	"context"
	"errors"
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

// errNoElicitationChannel is returned when the bridge has no channel
// configured (no RunStream is active).
var errNoElicitationChannel = errors.New("no events channel available for elicitation")

// elicitationBridge owns the events channel that the runtime's MCP
// elicitation handler sends requests to. Each RunStream call swaps in its
// own channel on entry and the previous one back on exit, so nested
// sub-session streams don't lose the parent's elicitation pipe.
//
// The bridge encapsulates a non-trivial concurrency contract: while a
// caller holds a reference to the current channel and is in the middle
// of sending an elicitation request, stream teardown must not race with
// close(channel) on the inner stream. We achieve this by serializing
// send, swap, and close with an RWMutex held across the channel
// operation. Pushing this into a small standalone type keeps the
// contract testable in isolation (with the race detector) without
// spinning up a runtime, and keeps LocalRuntime free of the two raw
// fields it used to expose.
//
// Concurrent (non-nested) RunStreams — most notably background jobs
// started via run_background_agent — can swap this single slot out from
// under each other; see elicitationWaiters and OnElicitationRequest for
// the routing/delivery fix (#3584). The bridge itself is kept only as a
// best-effort secondary delivery path for remote/SSE consumers that read
// events directly off a RunStream channel (see remote_runtime.go). It is
// never allowed to hold up the reliable sink or response processing: send
// is bounded by the caller's ctx (see elicitationHandler), and callers
// invoke it from a detached goroutine so a wedged or abandoned channel
// cannot block the request/response path at all (#3584 review item 1).
type elicitationBridge struct {
	mu sync.RWMutex
	ch chan Event
}

// swap atomically replaces the bridge's channel and returns the previous
// value. RunStream calls swap(events) on entry and swap(prev) on exit.
func (b *elicitationBridge) swap(ch chan Event) chan Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	prev := b.ch
	b.ch = ch
	return prev
}

// send delivers ev to the current channel, holding the read lock across
// the send so a concurrent restoreAndClose cannot close the channel out
// from under an in-flight send without going through recover() below. The
// send itself is bounded by ctx: if ctx is done before the channel accepts
// the event, send returns ctx.Err() instead of blocking forever. Combined
// with callers invoking send from a detached goroutine (see
// elicitationHandler), a full or abandoned channel can no longer delay —
// let alone indefinitely block — the reliable sink delivery or response
// handling that used to be sequenced before this call (#3584 item 1).
//
// Returns errNoElicitationChannel when no channel is configured or when a
// defensive recover catches an externally closed channel.
func (b *elicitationBridge) send(ctx context.Context, ev Event) (err error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	defer func() {
		if recover() != nil {
			err = errNoElicitationChannel
		}
	}()
	if b.ch == nil {
		return errNoElicitationChannel
	}
	select {
	case b.ch <- ev:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// restoreAndClose restores the previous stream channel and closes the current
// stream channel under the bridge write lock, so the close is mutually
// exclusive with an in-flight send. This is the #3069 fix: close can no longer
// race a parked sender and panic with "send on closed channel".
//
// Accepted trade-off (do not "fix" by dropping the lock): holding the write
// lock makes restoreAndClose wait for any in-flight send to finish, because
// send holds the read lock across "b.ch <- ev". If the stream consumer has
// gone away and current is full (or unbuffered), that parked send never
// drains until its own ctx is done, so this call blocks on Lock until then. A
// bounded wait is the deliberate, accepted alternative to crashing the whole
// process with a send-on-closed-channel panic; #3584 bounded the wait (send
// used to have no ctx at all and could block indefinitely) and moved the
// caller onto a detached goroutine so this can never stall the request path.
func (b *elicitationBridge) restoreAndClose(current, previous chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ch = previous
	close(current)
}

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
	ch    chan ElicitationResult
	state atomic.Int32
}

func newElicitationWaiter() *elicitationWaiter {
	return &elicitationWaiter{ch: make(chan ElicitationResult, 1)}
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
	return w.state.CompareAndSwap(int32(waiterPending), int32(waiterCanceled))
}

// elicitationWaiters routes an elicitation response to the specific request
// that is waiting for it, keyed by a correlation ID that is unique per
// request (see elicitationHandler). This replaces the single shared
// elicitationRequestCh, which could only ever have one request in flight:
// with concurrent (background-job) elicitations, a response arriving on
// that shared channel could be delivered to an arbitrary waiter, and
// ResumeElicitation had no way to tell "no request in flight" from "the
// request hasn't parked on the channel yet" (a TOCTOU race).
//
// Each waiter is registered BEFORE the corresponding request event is
// emitted, so a response that arrives immediately after — even before the
// handler reaches its receive — is never lost. The registry key is always an
// internally-generated ID (never the MCP wire ElicitationID, which two
// different MCP servers can coincidentally reuse): see elicitationHandler.
type elicitationWaiters struct {
	mu      sync.Mutex
	pending map[string]*elicitationWaiter
}

// register creates a waiter for id and stores it. The channel is buffered
// (capacity 1), so resolve never blocks even if the registrant hasn't
// reached its receive yet.
func (w *elicitationWaiters) register(id string) *elicitationWaiter {
	wt := newElicitationWaiter()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pending == nil {
		w.pending = make(map[string]*elicitationWaiter)
	}
	w.pending[id] = wt
	return wt
}

// abandon removes id's waiter from the registry, if it is still the one
// registered (defends against a hypothetical ID reuse racing a fresh
// register call), without touching its terminal state. Called once a waiter
// is done being awaited via any path, so a later resolve() for a reused ID
// cannot be confused with this one.
func (w *elicitationWaiters) abandon(id string, wt *elicitationWaiter) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pending[id] == wt {
		delete(w.pending, id)
	}
}

// cancel marks wt cancelled if it is still pending and removes it from the
// registry. Returns true when this call won the cancel-vs-resolve race — the
// caller (elicitationHandler's ctx.Done() branch) should then return ctx.Err().
// Returns false when resolve() already won: the caller must receive from
// wt.ch instead, since a result is already there (or is about to land — the
// buffered send in tryResolve never blocks).
func (w *elicitationWaiters) cancel(id string, wt *elicitationWaiter) bool {
	won := wt.tryCancel()
	if won {
		w.abandon(id, wt)
	}
	return won
}

// resolve delivers result to the waiter registered for id and returns true.
// Returns false without side effects when no waiter is currently registered
// for that ID, or when it was already resolved/cancelled — already
// answered, timed out, or unknown.
func (w *elicitationWaiters) resolve(id string, result ElicitationResult) bool {
	w.mu.Lock()
	wt, ok := w.pending[id]
	if ok {
		delete(w.pending, id)
	}
	w.mu.Unlock()
	if !ok {
		return false
	}
	return wt.tryResolve(result)
}

// OnElicitationRequest registers a handler invoked whenever an MCP toolset
// raises an elicitation request. This is the reliable route for
// background-job elicitations (run_background_agent): their RunStream runs
// on a detached goroutine and can race concurrent streams for the bridge's
// single channel slot (#3584), so elicitationHandler calls this sink
// directly, synchronously, and unconditionally — before it ever touches the
// best-effort bridge — as the single, exactly-once delivery point. Embedders
// (e.g. the TUI's App, or the API server for session-scoped SSE delivery)
// register a handler here that forwards the event to their UI/transport.
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

// MirrorsElicitationOnRunStream marks LocalRuntime as a runtime whose
// OnElicitationRequest sink is the single, exactly-once delivery point for
// an elicitation request even though elicitationHandler ALSO best-effort-
// sends the very same event on the RunStream channel, for the benefit of
// out-of-process consumers reading RunStream directly (see
// elicitationBridge). Embedders that forward RunStream events verbatim into
// their own event bus (e.g. pkg/app.App) use this marker — via an optional
// capability check, since it is not part of the Runtime interface — to know
// they must skip *ElicitationRequestEvent to avoid delivering the same
// request twice.
//
// RemoteRuntime deliberately does NOT implement this: its
// OnElicitationRequest is a no-op, so the copy on its RunStream is the ONLY
// delivery and callers must not skip it (#3584 review — an earlier fix
// skipped unconditionally and silently dropped every remote elicitation).
func (r *LocalRuntime) MirrorsElicitationOnRunStream() {}

// emitElicitationRequest forwards an elicitation request event to the
// registered sink, if any. Besides [LocalRuntime.EmitElicitationRequestForTesting],
// this is the ONLY call site that invokes the sink (see elicitationHandler):
// production callers must not add a second delivery path (e.g. re-forwarding
// an event observed on a RunStream channel), or the exactly-once guarantee
// this type documents no longer holds (#3584 item 5 — dual delivery
// previously required a stateful App-side dedupe to paper over).
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
	r.liveSessionsMu.Lock()
	for id, entry := range r.liveSessions {
		if entry != nil && entry.sess != nil {
			parents[id] = entry.sess.ParentID
		}
	}
	r.liveSessionsMu.Unlock()

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

// elicitationDeclineNotes accumulates model-readable notes for elicitations
// that were auto-declined because a background session had no UI available
// to answer them (see elicitationHandler). runCollecting drains these after
// the sub-session completes and prepends them to the tool result, mirroring
// backgroundAuthRequiredNote's #3200 pattern for OAuth-at-Start failures.
type elicitationDeclineNotes struct {
	mu        sync.Mutex
	bySession map[string][]string
}

// record appends note under sessionID. No-op when either is empty.
func (n *elicitationDeclineNotes) record(sessionID, note string) {
	if sessionID == "" || note == "" {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.bySession == nil {
		n.bySession = make(map[string][]string)
	}
	n.bySession[sessionID] = append(n.bySession[sessionID], note)
}

// drain returns and clears the notes recorded for sessionID.
func (n *elicitationDeclineNotes) drain(sessionID string) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	notes := n.bySession[sessionID]
	delete(n.bySession, sessionID)
	return notes
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
	// shared current-agent slot and the ctx conversation ID) when the caller
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
		r.elicitationDeclines.record(sessionID, backgroundElicitationDeclinedNote(spec.message))
		return tools.ElicitationResult{Action: tools.ElicitationActionDecline}, nil
	}

	agentName := spec.agentName
	if agentName == "" {
		agentName = r.currentAgentName()
	}

	// No *agent.Agent is threaded into MCP handler callbacks, so fall back
	// to the current agent here. The session ID is the conversation ID the
	// run loop seeded into ctx (empty for elicitations outside a run, e.g.
	// startup OAuth probes).
	r.executeOnUserInputHooks(ctx, r.agentForContext(ctx), genai.ConversationIDFromContext(ctx), "elicitation")

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
	wt := r.elicitationWaiters.register(correlationID)
	defer r.elicitationWaiters.abandon(correlationID, wt)

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
		if d, exists := r.sessionDrivers.Lookup(sessionID); exists {
			// Each elicitation is independently addressable; the turn remains
			// recorded separately in sessionInteraction.turnID.
			elicitation.RequestID = correlationID
			if elicitation.RequestID == "" {
				elicitation.RequestID = spec.serverElicitationID
			}
			if elicitation.RequestID != "" {
				d.RegisterInteraction(elicitation.RequestID, InteractionElicitation, elicitation)
			}
		}
	}

	// Acquire and invoke a reliable route as one operation. For background /
	// prompt-disabled sessions, failure means the last subscriber disappeared
	// before delivery; abandon the waiter and fast-decline instead of waiting
	// forever. Interactive streams still retain their best-effort bridge path.
	delivered := r.emitElicitationRequest(ev)
	if backgroundWithoutPrompts && !delivered {
		slog.WarnContext(ctx, "Declining elicitation: background session has no UI to answer it", "message", spec.message)
		r.elicitationDeclines.record(sessionID, backgroundElicitationDeclinedNote(spec.message))
		return tools.ElicitationResult{Action: tools.ElicitationActionDecline}, nil
	}

	// Best-effort secondary delivery on the owning stream's events channel,
	// kept for remote/SSE consumers that read directly off RunStream
	// (remote_runtime.go depends on it). Dispatched on a detached goroutine,
	// bounded by ctx, so a wedged or abandoned bridge channel (concurrent
	// RunStreams racing the swap-based single slot, or a dead consumer) can
	// never delay — let alone block — sink delivery or the response wait
	// below (#3584 review item 1). runCollecting no longer treats a bridge
	// delivery as a second source of truth (#3584 review item 5): this send
	// exists solely for out-of-process consumers.
	go func() {
		if err := r.elicitation.send(ctx, ev); err != nil {
			slog.DebugContext(ctx, "Elicitation bridge send failed or abandoned; relying on the registered sink", "error", err)
		}
	}()

	// Wait for the response addressed to this specific request. The
	// ctx.Done() branch cannot simply return ctx.Err(): resolve() may have
	// already won the terminal-state race an instant earlier and be about
	// to (or have already) delivered into wt.ch, in which case
	// ResumeElicitation already reported success to its caller and this
	// handler must not silently discard that response (#3584 review item
	// 2b). cancel() decides the winner atomically.
	select {
	case result := <-wt.ch:
		return tools.ElicitationResult{
			Action:  result.Action,
			Content: result.Content,
		}, nil
	case <-ctx.Done():
		slog.DebugContext(ctx, "Context cancelled while waiting for elicitation response")
		if r.elicitationWaiters.cancel(correlationID, wt) {
			return tools.ElicitationResult{}, ctx.Err()
		}
		result := <-wt.ch
		return tools.ElicitationResult{
			Action:  result.Action,
			Content: result.Content,
		}, nil
	}
}
