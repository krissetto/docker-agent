package board

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveUnix runs an HTTP handler on a unix socket and returns the socket path.
func serveUnix(t *testing.T, handler http.Handler) string {
	t.Helper()
	// Not t.TempDir(): its per-test path is long enough to overflow the
	// ~104-byte unix sun_path limit under long test names.
	dir, err := os.MkdirTemp("", "board-client") //nolint:forbidigo,usetesting // see above
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	socket := filepath.Join(dir, "cp.sock")
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "unix", socket)
	require.NoError(t, err)
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return socket
}

// Heartbeat comments are invisible to the event callback, and once one has
// been seen, a stream that goes silent is aborted with an error (instead of
// blocking forever on a hung transport) so the watcher reconnects.
func TestStreamEventsIdleWatchdogAbortsSilentStream(t *testing.T) {
	old := streamIdleTimeout
	streamIdleTimeout = 50 * time.Millisecond
	t.Cleanup(func() { streamIdleTimeout = old })

	socket := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f := w.(http.Flusher)
		fmt.Fprint(w, ": ping\n\n")
		fmt.Fprint(w, "data: {\"type\":\"stream_started\"}\n\n")
		f.Flush()
		// Then hang without closing, like a wedged transport.
		<-r.Context().Done()
	}))

	c := newClient(socket, "sess-1")
	var got []event
	err := c.StreamEvents(t.Context(), 0, func(ev event) bool {
		got = append(got, ev)
		return true
	})
	require.ErrorIs(t, err, errStreamIdle)
	require.Len(t, got, 1, "heartbeat comments must not reach the callback")
	assert.Equal(t, eventStreamStarted, got[0].Type)
}

func TestStreamEventsHeartbeatLinesResetWatchdog(t *testing.T) {
	old := streamIdleTimeout
	streamIdleTimeout = 250 * time.Millisecond
	t.Cleanup(func() { streamIdleTimeout = old })

	socket := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f := w.(http.Flusher)
		for range 6 {
			fmt.Fprint(w, ": ping\n")
			f.Flush()
			time.Sleep(50 * time.Millisecond) //nolint:forbidigo // heartbeat cadence is under test
		}
		fmt.Fprint(w, "data: {\"type\":\"stream_stopped\"}\n\n")
		f.Flush()
		<-r.Context().Done()
	}))

	c := newClient(socket, "sess-1")
	err := c.StreamEvents(t.Context(), 0, func(ev event) bool {
		assert.Equal(t, eventStreamStopped, ev.Type)
		return false
	})
	require.NoError(t, err)
}

// Without any heartbeat from the server (an older docker-agent), the watchdog
// stays unarmed: a quiet stream is left alone and events keep flowing.
func TestStreamEventsNoHeartbeatNoWatchdog(t *testing.T) {
	old := streamIdleTimeout
	streamIdleTimeout = 50 * time.Millisecond
	t.Cleanup(func() { streamIdleTimeout = old })

	socket := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"type\":\"stream_started\"}\n\n")
		f.Flush()
		time.Sleep(120 * time.Millisecond) //nolint:forbidigo // real quiet time is the thing under test
		fmt.Fprint(w, "data: {\"type\":\"stream_stopped\"}\n\n")
	}))

	c := newClient(socket, "sess-1")
	var got []event
	err := c.StreamEvents(t.Context(), 0, func(ev event) bool {
		got = append(got, ev)
		return len(got) < 2
	})
	require.NoError(t, err)
	require.Len(t, got, 2)
}

func TestCanonicalSnapshotAndEpochObservation(t *testing.T) {
	socket := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/sessions/s/snapshot":
			fmt.Fprint(w, `{"session":{"id":"s","title":"Current"},"status":{"state":"running"},"epoch":"epoch-one","cursor":9,"interactions":[{"interaction_id":"ask","kind":"confirmation"}]}`)
		case "/api/v2/sessions/s/events":
			assert.Equal(t, "9", r.URL.Query().Get("since"))
			assert.Equal(t, "epoch-one", r.URL.Query().Get("since_epoch"))
			fmt.Fprint(w, "data: {\"type\":\"snapshot\",\"snapshot\":{\"epoch\":\"epoch-one\",\"cursor\":9,\"status\":{\"state\":\"running\"}}}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"event\",\"envelope\":{\"epoch\":\"epoch-two\",\"sequence\":1,\"event\":{\"type\":\"stream_started\"}}}\n\n")
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
	}))
	client := newClient(socket, "s")
	snap, err := client.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "Current", snap.Title)
	assert.False(t, snap.Paused)
	assert.Equal(t, []string{"ask"}, snap.InteractionIDs)
	err = client.StreamEvents(t.Context(), snap.LastEventSeq, func(ev event) bool {
		if ev.Type == eventBaseline {
			require.NotNil(t, ev.Baseline)
			assert.Equal(t, "running", ev.Baseline.State)
			return true
		}
		assert.Equal(t, eventGap, ev.Type)
		return false
	})
	require.NoError(t, err)
}

func TestCanonicalFollowupKeepsIdempotency(t *testing.T) {
	socket := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v2/sessions/s/messages", r.URL.Path)
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.JSONEq(t, `{"content":"follow up","request_id":"key"}`, string(body))
		w.WriteHeader(http.StatusAccepted)
	}))
	require.NoError(t, newClient(socket, "s").Followup(t.Context(), "key", "follow up"))
}

func TestCanonicalInteractionSnapshotAndTail(t *testing.T) {
	t.Parallel()
	socket := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/snapshot") {
			fmt.Fprint(w, `{"session":{"id":"sess-1","title":"Task"},"status":{"session_id":"sess-1","state":"running"},"epoch":"one","cursor":7,"interactions":[{"session_id":"sess-1","interaction_id":"first","kind":"confirmation"},{"session_id":"sess-1","interaction_id":"second","kind":"elicitation"}]}`)
			return
		}
		assert.Equal(t, "7", r.URL.Query().Get("since"))
		assert.Equal(t, "one", r.URL.Query().Get("since_epoch"))
		for i, kind := range []string{"tool_call_confirmation", "elicitation_request", "max_iterations_reached", "interaction_resolved"} {
			fmt.Fprintf(w, "data: {\"version\":2,\"type\":\"event\",\"envelope\":{\"version\":2,\"session_id\":\"sess-1\",\"epoch\":\"one\",\"sequence\":%d,\"interaction_id\":\"request-%d\",\"event\":{\"type\":%q}}}\n\n", 8+i, i, kind)
		}
	}))
	c := newClient(socket, "sess-1")
	snap, err := c.Snapshot(t.Context())
	require.NoError(t, err)
	assert.False(t, snap.Paused, "explicit pause is separate from attention")
	assert.Equal(t, []string{"first", "second"}, snap.InteractionIDs)
	var events []event
	require.NoError(t, c.StreamEvents(t.Context(), snap.LastEventSeq, func(ev event) bool { events = append(events, ev); return len(events) < 4 }))
	for i, ev := range events {
		assert.Equal(t, fmt.Sprintf("request-%d", i), ev.InteractionID)
		if i < 3 {
			assert.Equal(t, eventInteraction, ev.Type)
		} else {
			assert.Equal(t, eventInteractionResolved, ev.Type)
		}
	}
}

func TestInteractionResolutionPayloadTokenWithoutEnvelopeToken(t *testing.T) {
	t.Parallel()
	socket := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "data: {\"version\":2,\"type\":\"event\",\"envelope\":{\"version\":2,\"session_id\":\"s\",\"epoch\":\"one\",\"sequence\":8,\"event\":{\"type\":\"interaction_resolved\",\"session_id\":\"s\",\"interaction_id\":\"request\",\"reason\":\"responded\"}}}\n\n")
	}))
	c := newClient(socket, "s")
	require.NoError(t, c.StreamEvents(t.Context(), 0, func(ev event) bool {
		assert.Equal(t, eventInteractionResolved, ev.Type)
		assert.Equal(t, "request", ev.InteractionID)
		return false
	}))
}
