package board

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/docker/docker-agent/pkg/api"
)

// Event types the board reacts to on the session event stream. Every other
// runtime event is ignored.
const (
	eventStreamStarted = "stream_started"
	eventStreamStopped = "stream_stopped"
	eventSessionTitle  = "session_title"
	eventSessionExited = "session_exited"
	// eventUserMessage marks a real user prompt entering the session. The
	// runtime emits it only for human-authored turns (sub-agent and skill
	// sub-sessions suppress it), right before the turn's outermost
	// stream_started, which makes it a turn-boundary marker.
	eventUserMessage = "user_message"
	// eventError is emitted when a turn fails (model error, tool failure,
	// hook block…). Unlike stream_stopped it is delivered on the blocking
	// sink and buffered for replay, so it is the reliable failure signal.
	eventError = "error"
	// eventRuntimePaused is emitted when the run loop blocks at an iteration
	// boundary because /pause was toggled on. There is no matching resume
	// event: the loop simply starts emitting events again once resumed.
	eventRuntimePaused = "runtime_paused"
	eventGap           = "gap"
	eventInteraction   = "board_interaction"
	eventBaseline      = "board_baseline"
)

// reasonNormal is the stream_stopped reason for a turn that completed
// cleanly, as opposed to "error", "canceled", "hook_blocked"...
const reasonNormal = "normal"

// streamIdleTimeout is how long StreamEvents tolerates a silent connection
// once the server has proven it sends heartbeats (": ping" SSE comments,
// emitted every 15s). Three missed heartbeats means the transport is hung —
// e.g. the agent's VM was paused — not that the session is quiet, so the
// stream is aborted and the watcher reconnects. Servers that predate
// heartbeats never arm the watchdog, keeping long-lived idle streams working.
var streamIdleTimeout = 45 * time.Second

// errStreamIdle reports a stream aborted by the idle watchdog.
var errStreamIdle = errors.New("event stream idle: heartbeats stopped")

// event is the subset of a runtime event the board cares about.
type event struct {
	Type     string    `json:"type"`
	Baseline *snapshot `json:"-"`
	Title    string    `json:"title"`
	// Reason classifies how a stream ended (stream_stopped only). It is
	// authoritative for the turn's outcome, unlike mid-turn error events
	// which a parent agent may have recovered from.
	Reason string `json:"reason"`
	// Seq is the event's position in the session's buffer, parsed from the
	// SSE "id:" line. It is 0 when the server sent no id. Compared with
	// [snapshot.LastEventSeq] it tells replayed history from live events.
	Seq uint64 `json:"-"`
}

// snapshot is the part of GET /snapshot the board uses to (re)build a card's
// state and find the stream position to resume from.
type snapshot struct {
	Title        string `json:"title"`
	LastEventSeq uint64 `json:"last_event_seq"`
	Epoch        string `json:"epoch,omitempty"`
	State        string `json:"state,omitempty"`
	Paused       bool   `json:"paused,omitempty"`
	LastError    string `json:"last_error,omitempty"`
}

// client drives one session's control plane over its unix socket.
type client struct {
	http    *http.Client
	base    string
	session string
	epoch   string
}

// newClient returns a client that reaches the control plane over the given
// unix socket and targets the given session id.
func newClient(socket, session string) *client {
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &client{
		http:    &http.Client{Transport: transport},
		base:    "http://agent",
		session: session,
	}
}

func (c *client) endpoint(name string) string {
	return c.base + api.SessionAPIPath + "/" + url.PathEscape(c.session) + "/" + name
}

// Snapshot reads the session's state and the stream position it corresponds to.
func (c *client) Snapshot(ctx context.Context) (snapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("snapshot"), http.NoBody)
	if err != nil {
		return snapshot{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return snapshot{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return snapshot{}, fmt.Errorf("snapshot: %s", resp.Status)
	}
	var wire api.SessionSnapshot[string, string, json.RawMessage]
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return snapshot{}, fmt.Errorf("decode snapshot: %w", err)
	}
	c.epoch = wire.Epoch
	snap := snapshot{LastEventSeq: wire.Cursor, Epoch: wire.Epoch, State: wire.Status.State, Paused: wire.Status.Paused || len(wire.Interactions) > 0, LastError: wire.Status.LastError}
	if wire.Session != nil {
		snap.Title = wire.Session.TitleSnapshot()
	}
	return snap, nil
}

// Followup enqueues a message to run after the current turn. A non-empty
// idempotencyKey makes the call safe to retry.
func (c *client) Followup(ctx context.Context, idempotencyKey, message string) error {
	body, err := json.Marshal(api.SessionInputRequest{Content: message, RequestID: idempotencyKey})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("messages"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("followup: %s", resp.Status)
	}
	// Drain the small response body before closing it.
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// StreamEvents tails the session event stream starting after `since` (0 from
// the beginning of the buffer). onEvent is called for every event; returning
// false stops the stream cleanly. It returns nil on a clean stop and an
// error when the connection fails.
func (c *client) StreamEvents(ctx context.Context, since uint64, onEvent func(event) bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	u := c.endpoint("events")
	if c.epoch != "" || since > 0 {
		q := url.Values{"since": {strconv.FormatUint(since, 10)}}
		if c.epoch != "" {
			q.Set("since_epoch", c.epoch)
		}
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("events: %s", resp.Status)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	// The idle watchdog is armed by the first heartbeat and re-armed by any
	// subsequent line; when it fires it cancels the request, failing the read.
	var idle atomic.Bool
	var watchdog *time.Timer
	defer func() {
		if watchdog != nil {
			watchdog.Stop()
		}
	}()
	resetWatchdog := func() {
		if watchdog != nil {
			watchdog.Reset(streamIdleTimeout)
		}
	}

	var seq uint64
	var chunks []byte
	var assembling bool
	for scanner.Scan() {
		line := scanner.Text()
		resetWatchdog()
		if strings.HasPrefix(line, ":") {
			// Heartbeat comment: the server sends them, so silence now means
			// a hung transport. Arm the watchdog on the first one.
			if watchdog == nil {
				watchdog = time.AfterFunc(streamIdleTimeout, func() {
					idle.Store(true)
					cancel()
				})
			}
			continue
		}
		if id, ok := strings.CutPrefix(line, "id:"); ok {
			seq, _ = strconv.ParseUint(strings.TrimSpace(id), 10, 64)
			continue
		}
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		var ev event
		payload := []byte(strings.TrimSpace(data))
		var message api.SessionStreamMessage[string, string, json.RawMessage]
		if json.Unmarshal(payload, &message) != nil {
			continue
		}
		switch {
		case message.Envelope != nil:
			envelope := message.Envelope
			if envelope.Gap || (c.epoch != "" && envelope.Epoch != "" && envelope.Epoch != c.epoch) {
				ev.Type = eventGap
			} else if json.Unmarshal(envelope.Event, &ev) != nil {
				continue
			}
			if envelope.InteractionID != "" && (ev.Type == "tool_call_confirmation" || ev.Type == "elicitation_request") {
				ev.Type = eventInteraction
			}
			seq = envelope.Sequence
		case message.Type == "snapshot_begin":
			chunks = nil
			assembling = true
			continue
		case message.Type == "snapshot_chunk":
			if !assembling || len(chunks)+len(message.Chunk) > 64<<20 {
				return errors.New("invalid chunked snapshot")
			}
			chunks = append(chunks, message.Chunk...)
			continue
		case message.Type == "snapshot_end" || message.Type == "snapshot":
			wire := message.Snapshot
			if message.Type == "snapshot_end" {
				if !assembling {
					return errors.New("snapshot ended without beginning")
				}
				wire = new(api.SessionSnapshot[string, string, json.RawMessage])
				if err := json.Unmarshal(chunks, wire); err != nil {
					return err
				}
				chunks = nil
				assembling = false
			}
			if wire == nil {
				return errors.New("missing snapshot")
			}
			if c.epoch != "" && wire.Epoch != c.epoch {
				ev.Type = eventGap
			} else {
				ev.Type = eventBaseline
				ev.Baseline = &snapshot{Epoch: wire.Epoch, LastEventSeq: wire.Cursor, State: wire.Status.State, Paused: wire.Status.Paused || len(wire.Interactions) > 0, LastError: wire.Status.LastError}
				if wire.Session != nil {
					ev.Baseline.Title = wire.Session.TitleSnapshot()
				}
			}
		case message.Type == "ready":
			continue
		case json.Unmarshal(payload, &ev) != nil:
			continue
		}
		ev.Seq = seq
		seq = 0
		if !onEvent(ev) {
			return nil
		}
	}
	if idle.Load() {
		return errStreamIdle
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return errors.New("event stream closed")
}

func (c *client) StopSubtree(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("stop-subtree"), http.NoBody)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("stop subtree: %s", resp.Status)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}
