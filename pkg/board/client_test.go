package board

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
	assert.True(t, snap.Paused)
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
