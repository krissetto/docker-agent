package runtime

import (
	"cmp"
	"context"
	"log/slog"
	"maps"
	"slices"

	"github.com/docker/docker-agent/pkg/session"
)

// LiveSession is one row of the /context team view: a currently running
// RunStream session (or the idle current root session) with its agent
// identity, session identity and context budget. Token counts come from the
// session's provider-reported cumulative usage; ContextLimit is 0 when the
// effective model's window cannot be resolved (harness-backed agents, models
// absent from the catalogue).
type LiveSession struct {
	SessionID    string `json:"session_id"`
	AgentName    string `json:"agent_name"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	ContextLimit int64  `json:"context_limit"`
	// Current marks the caller's current root session. It is listed even
	// while idle (no active stream), unlike child rows which exist only
	// while their RunStream is live.
	Current bool `json:"current"`

	// CompactionModel is the identity ("provider/model") of the row's agent
	// dedicated compaction model, set only when it actually caps ContextLimit
	// below the primary model's own window. Render-unused in the /context
	// dialog (the header's single cap statement is authoritative there); kept
	// on the struct as part of the JSON/API surface for other consumers.
	CompactionModel string `json:"compaction_model,omitempty"`
	// PrimaryContextLimit is the primary model's own context window, set
	// only alongside CompactionModel. Render-unused today (see CompactionModel).
	PrimaryContextLimit int64 `json:"primary_context_limit,omitempty"`
}

// UsedTokens returns the session's current context occupancy estimate.
func (s LiveSession) UsedTokens() int64 {
	return s.InputTokens + s.OutputTokens
}

// ShortID returns the first 8 characters of the session ID, enough to
// disambiguate concurrent runs of the same agent in UI listings.
func (s LiveSession) ShortID() string {
	if len(s.SessionID) <= 8 {
		return s.SessionID
	}
	return s.SessionID[:8]
}

// liveCompactionRequest is one queued explicit compaction of a live session,
// executed on that session's own run goroutine at a safe iteration boundary.
type liveCompactionRequest struct {
	additionalPrompt string
	events           EventSink
	done             func()
}

// LiveSessions projects immutable canonical owner snapshots. Only the addressed
// session's root tree is visible; settled intermediate parents retain lineage.
// The supplied session contributes identity only, never mutable display state.
func (r *LocalRuntime) LiveSessions(ctx context.Context, current *session.Session) []LiveSession {
	r.sessionDrivers.mu.Lock()
	drivers := maps.Clone(r.sessionDrivers.drivers)
	r.sessionDrivers.mu.Unlock()

	type ownerView struct {
		session *session.Session
		active  bool
	}
	views := make(map[string]ownerView, len(drivers))
	for id, driver := range drivers {
		var view ownerView
		if err := driver.ownerCall(ctx, func() error {
			if driver.sess != nil {
				view.session = driver.sess.OwnSnapshot()
				view.active = driver.running() || driver.starting() || driver.settling()
			}
			return nil
		}); err == nil && view.session != nil {
			views[id] = view
		}
	}
	rootOf := func(id string) string {
		seen := make(map[string]bool)
		for {
			if seen[id] {
				return ""
			}
			seen[id] = true
			view, ok := views[id]
			if !ok {
				return ""
			}
			if view.session.ParentID == "" {
				return id
			}
			id = view.session.ParentID
		}
	}
	var rows []LiveSession
	rootID := ""
	if current != nil {
		view, ok := views[current.ID]
		if !ok {
			return nil
		}
		rootID = rootOf(current.ID)
		if rootID == "" {
			return nil
		}
		rows = append(rows, r.liveSessionRow(r.sessionModelContext(ctx, view.session), view.session, r.sessionAgentName(view.session), true))
	}
	var children []LiveSession
	for id, view := range views {
		if !view.active || (current != nil && (id == current.ID || rootOf(id) != rootID)) {
			continue
		}
		children = append(children, r.liveSessionRow(r.sessionModelContext(ctx, view.session), view.session, r.sessionAgentName(view.session), false))
	}
	slices.SortStableFunc(children, func(a, b LiveSession) int {
		if c := cmp.Compare(a.AgentName, b.AgentName); c != 0 {
			return c
		}
		return cmp.Compare(a.SessionID, b.SessionID)
	})
	return append(rows, children...)
}

func (r *LocalRuntime) sessionModelContext(ctx context.Context, sess *session.Session) context.Context {
	if sess == nil {
		return ctx
	}
	if driver, ok := r.sessionDrivers.Lookup(sess.ID); ok {
		return driver.scopeModels(ctx)
	}
	return ctx
}

// liveSessionRow builds one team-view row from a session and its agent.
func (r *LocalRuntime) liveSessionRow(ctx context.Context, sess *session.Session, agentName string, current bool) LiveSession {
	input, output := sess.Usage()
	row := LiveSession{
		SessionID:    sess.ID,
		AgentName:    agentName,
		InputTokens:  input,
		OutputTokens: output,
		Current:      current,
	}
	if a, err := r.team.Agent(agentName); err == nil && a != nil && !a.HasHarness() {
		modelID := r.getEffectiveModelID(ctx, a)
		row.ContextLimit = r.contextLimitForAgentModel(ctx, a, modelID)
		if a.CompactionModel() != nil {
			compactionModel, primaryLimit, compactionLimit := r.compactionCapAttribution(ctx, a, modelID)
			if compactionCaps(primaryLimit, compactionLimit) {
				row.CompactionModel = compactionModel
				row.PrimaryContextLimit = primaryLimit
			}
		}
	}
	return row
}

// CompactLiveSession is a compatibility façade for addressed owner compaction.
// The owner reserves and drains active-turn requests at a safe boundary.
func (r *LocalRuntime) CompactLiveSession(ctx context.Context, sessionID, additionalPrompt string, events EventSink) error {
	driver, ok := r.sessionDrivers.Lookup(sessionID)
	if !ok {
		return &SessionError{Kind: SessionErrorNotFound, SessionID: sessionID, Operation: SessionOperationCompact}
	}
	return driver.compact(ctx, additionalPrompt, events)
}

// runLiveCompactionRequest performs one queued manual compaction, forwarding
// all resulting events to the request's sink. When no terminal compaction
// event was emitted (a pre_compact or before_compaction hook vetoed the run),
// a completed/skipped event is synthesized, mirroring App.CompactSession's
// behavior for the root /compact path.
func (r *LocalRuntime) runLiveCompactionRequest(ctx context.Context, sess *session.Session, req liveCompactionRequest) {
	if req.done != nil {
		defer req.done()
	}
	// With ctx already cancelled (a teardown drain after Ctrl+C), attempting
	// the compaction model call would fail immediately and emit a noisy
	// started/failed pair. Consume the request and report a single terminal
	// skipped event instead, so the requester still observes a terminal
	// signal without a phantom failure.
	if ctx.Err() != nil {
		slog.InfoContext(ctx, "Skipping explicit compaction for live session: context already cancelled",
			"session_id", sess.ID, "agent", r.sessionAgentName(sess))
		req.events.Emit(SessionCompactionCompleted(sess.ID, CompactionOutcomeSkipped, r.sessionAgentName(sess)))
		return
	}

	slog.InfoContext(ctx, "Running explicit compaction for live session",
		"session_id", sess.ID, "agent", r.sessionAgentName(sess))

	completed := false
	sink := EventSinkFunc(func(event Event) {
		if e, ok := event.(*SessionCompactionEvent); ok && e.Status == "completed" {
			completed = true
		}
		req.events.Emit(event)
	})
	r.compactWithReason(ctx, sess, req.additionalPrompt, compactionReasonManual, sink)
	if !completed {
		req.events.Emit(SessionCompactionCompleted(sess.ID, CompactionOutcomeSkipped, r.sessionAgentName(sess)))
	}
}

// sessionAgentName resolves the display agent name for sess, tolerating a
// nil resolution (which resolveSessionAgent's contract makes unexpected).
func (r *LocalRuntime) sessionAgentName(sess *session.Session) string {
	if a := r.resolveSessionAgent(sess); a != nil {
		return a.Name()
	}
	return ""
}
