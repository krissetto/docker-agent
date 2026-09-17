package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/docker/docker-agent/pkg/runtime"
)

// legacySessionEvents projects the same journal consumed by /api/v2 onto the
// historical raw SSE protocol. A turn stopping is not a session exiting.
func (s *Server) legacySessionEvents(c echo.Context) error {
	handle := s.legacyAttached(c.Param("id"))
	if handle == nil {
		return echo.NewHTTPError(http.StatusNotFound, "no event source for session")
	}
	raw := c.QueryParam("since")
	if raw == "" {
		raw = c.Request().Header.Get("Last-Event-ID")
	}
	// The old event log replayed its retained history for absent/invalid
	// cursors. Observe(nil) instead means snapshot plus tail, so request zero.
	since, parseErr := strconv.ParseUint(raw, 10, 64)
	if parseErr != nil {
		since = 0
	}
	observation, err := handle.Observe(c.Request().Context(), runtime.ObserveOptions{Since: &since})
	if err != nil {
		return sessionHTTPError(err)
	}
	return s.legacyStream(c, handle, observation, "", false)
}

// legacyRunStream is called only after admission succeeds, with an observation
// registered before admission. Only the admitting request owns its exact turn.
func (s *Server) legacyRunStream(c echo.Context, handle runtime.SessionHandle, observation runtime.Observation, turnID string, owned bool) error {
	return s.legacyStream(c, handle, observation, turnID, owned)
}

func (s *Server) legacyStream(c echo.Context, handle runtime.SessionHandle, observation runtime.Observation, turnID string, owned bool) error {
	if observation.Cancel != nil {
		defer observation.Cancel()
	}
	ctx := c.Request().Context()
	writeFailed := false
	// Preserve the historical POST request lifetime without canceling whatever
	// happens to be active now. Admission, not observation, grants this token.
	if owned && turnID != "" {
		defer func() {
			if ctx.Err() != nil || writeFailed {
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				_, cancelErr := handle.Cancel(cleanupCtx, turnID)
				awaitErr := handle.AwaitTurn(cleanupCtx, turnID)
				if err := errors.Join(cancelErr, awaitErr); err != nil {
					slog.WarnContext(cleanupCtx, "Failed to drain disconnected legacy turn", "session_id", handle.ID(), "turn_id", turnID, "error", err)
				}
			}
		}()
	}
	response := c.Response()
	response.Header().Set(echo.HeaderContentType, "text/event-stream")
	response.Header().Set(echo.HeaderCacheControl, "no-cache")
	response.Header().Set("Connection", "keep-alive")
	response.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(response.Writer)
	flush := func(frame string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Bound a slow peer's write without coupling its lifetime to a turn.
		// Recorders and other writers without deadline support remain usable.
		_ = controller.SetWriteDeadline(time.Now().Add(15 * time.Second))
		if _, err := fmt.Fprint(response, frame); err != nil {
			writeFailed = true
			return err
		}
		err := controller.Flush()
		writeFailed = err != nil
		return err
	}
	defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	write := func(sequence uint64, event any) error {
		data, err := json.Marshal(event)
		if err != nil {
			return err
		}
		frame := ""
		if turnID == "" && sequence > 0 {
			frame = fmt.Sprintf("id: %d\n", sequence)
		}
		return flush(frame + "data: " + string(data) + "\n\n")
	}
	if err := flush(""); err != nil {
		return nil
	}
	streamError := func(code, message string) {
		_ = write(0, runtime.ErrorWithCodeForSession(handle.ID(), code, message))
	}
	exited := func(sequence uint64, reason string) {
		_ = write(sequence, struct {
			Type   string `json:"type"`
			Reason string `json:"reason,omitempty"`
		}{Type: "session_exited", Reason: reason})
	}
	seenInteractions := make(map[string]bool)
	if turnID != "" {
		if snapshot := observation.Primary(); snapshot.Session != nil && snapshot.Session.Title != "" {
			if err := write(0, runtime.SessionTitle(handle.ID(), snapshot.Session.Title)); err != nil {
				return nil
			}
		}
	}
	consume := func(envelope runtime.SessionEvent) bool {
		if envelope.Gap {
			_ = write(0, struct {
				Type string `json:"type"`
			}{Type: "gap"})
			if turnID != "" {
				streamError("observation_gap", "Run event history was lost. Read the session snapshot and reconnect to its events; the accepted turn has not been canceled.")
			}
			// Gap is a resnapshot barrier. Never apply the stale tail, nor
			// wait for a terminal that may have been evicted with the gap.
			return false
		}
		if envelope.SessionID != "" && envelope.SessionID != handle.ID() {
			return true
		}
		if turnID != "" && envelope.TurnID != turnID {
			return true
		}
		if envelope.Event == nil {
			return true
		}
		if envelope.InteractionID != "" {
			seenInteractions[envelope.InteractionID] = true
		}
		if stopped, ok := envelope.Event.(*runtime.StreamStoppedEvent); ok && turnID == "" && stopped.Reason == "deleted" {
			exited(envelope.Sequence, "deleted")
			return false
		}
		if _, stopped := envelope.Event.(*runtime.StreamStoppedEvent); stopped && turnID != "" {
			// StreamStopped precedes durable settlement. Do not expose the
			// old terminal (or EOF) as completion until the canonical exact
			// turn barrier succeeds. Cancellation still uses admission ownership.
			settlementCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := handle.AwaitTurn(settlementCtx, turnID)
			cancel()
			if err != nil {
				slog.WarnContext(ctx, "Failed to await legacy turn settlement", "session_id", handle.ID(), "turn_id", turnID, "error", err)
				streamError("settlement_failed", "The turn stopped but its durable settlement could not be confirmed. Wait for this turn through the session API before treating it as complete: "+err.Error())
				return false
			}
		}
		if err := write(envelope.Sequence, envelope.Event); err != nil {
			return false
		}
		_, stopped := envelope.Event.(*runtime.StreamStoppedEvent)
		return turnID == "" || !stopped
	}
	for _, envelope := range observation.Replay {
		if !consume(envelope) {
			return nil
		}
	}
	if turnID == "" {
		// A reconnect cursor can be newer than a still-outstanding prompt.
		// Reseed the original canonical payload, not a manufactured request.
		for _, snapshot := range observation.Initial {
			for _, interaction := range snapshot.Interactions {
				if interaction.SessionID != "" && interaction.SessionID != handle.ID() {
					continue
				}
				if interaction.Event != nil && !seenInteractions[interaction.InteractionID] {
					if err := write(0, interaction.Event); err != nil {
						return nil
					}
				}
			}
		}
	}
	interval := s.heartbeatInterval
	if interval <= 0 {
		interval = defaultEventsHeartbeatInterval
	}
	heartbeat := time.NewTicker(interval)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err, ok := <-observation.Errors:
			if !ok {
				observation.Errors = nil
				continue
			}
			if err != nil {
				streamError("observation_failed", "Session observation failed; reconnect to the session events: "+err.Error())
				return nil
			}
		case envelope, ok := <-observation.Events:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				if turnID != "" {
					streamError("observation_ended", "Run observation ended before its terminal event. Read the session snapshot and reconnect to its events; the accepted turn has not been canceled.")
				} else if legacySessionOwnerEnded(ctx, handle) {
					// Deletion after a completed turn closes the canonical
					// channel without duplicating StreamStopped. No sequence
					// is invented for this per-connection terminal projection.
					exited(0, "")
				}
				return nil
			}
			if !consume(envelope) {
				return nil
			}
		case <-heartbeat.C:
			if err := flush(": ping\n\n"); err != nil {
				return nil
			}
		}
	}
}

// Channel closure alone is not owner termination: remote transports and slow
// observations can also close. Ask the canonical owner, without retaining a
// replacement subscription or an unbounded wait.
func legacySessionOwnerEnded(ctx context.Context, handle runtime.SessionHandle) bool {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	probe, err := handle.Observe(ctx, runtime.ObserveOptions{})
	if probe.Cancel != nil {
		probe.Cancel()
	}
	var sessionErr *runtime.SessionError
	return errors.As(err, &sessionErr) && (sessionErr.Kind == runtime.SessionErrorStopped || sessionErr.Kind == runtime.SessionErrorClosed || sessionErr.Kind == runtime.SessionErrorNotFound)
}
