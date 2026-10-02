package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTransportSnapshot(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":{\"session\":{\"id\":\"s\"},\"status\":{\"session_id\":\"s\"},\"cursor\":0}}\n\n")
	fmt.Fprint(w, "data: {\"version\":2,\"type\":\"ready\",\"cursor\":0}\n\n")
	w.(http.Flusher).Flush()
}

func TestSessionLongLivedRequestsPreserveClientPolicy(t *testing.T) {
	// The opt-in run exercises the real former 30-second boundary as well as the
	// fast deterministic configured-timeout regression in ordinary test runs.
	delay, timeout := 150*time.Millisecond, 30*time.Millisecond
	if os.Getenv("DOCKER_AGENT_TEST_LONG_OBSERVATION") == "1" {
		delay, timeout = 31*time.Second, 30*time.Second
	}
	var mutations atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
		cookie, err := r.Cookie("transport")
		if assert.NoError(t, err) {
			assert.Equal(t, "preserved", cookie.Value)
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/events"):
			writeTransportSnapshot(w)
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
			fmt.Fprint(w, "data: {\"version\":2,\"type\":\"event\",\"envelope\":{\"version\":2,\"session_id\":\"s\",\"sequence\":1,\"event\":{\"type\":\"stream_started\",\"session_id\":\"s\"}}}\n\n")
		default:
			if strings.HasSuffix(r.URL.Path, "/messages") {
				mutations.Add(1)
			}
			select {
			case <-time.After(delay):
				w.WriteHeader(http.StatusNoContent)
			case <-r.Context().Done():
			}
		}
	}))
	defer server.Close()
	injected := server.Client()
	injected.Timeout = timeout
	injected.Jar, _ = cookiejar.New(nil)
	client, err := NewClient(server.URL, WithHTTPClient(injected), WithAuthToken("secret"))
	require.NoError(t, err)
	injected.Jar.SetCookies(client.baseURL, []*http.Cookie{{Name: "transport", Value: "preserved"}})
	redirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	injected.CheckRedirect = redirect
	clone := client.longLivedHTTPClient()
	assert.Same(t, injected.Transport, clone.Transport)
	assert.Same(t, injected.Jar, clone.Jar)
	assert.Equal(t, http.ErrUseLastResponse, clone.CheckRedirect(nil, nil))
	assert.Zero(t, clone.Timeout)
	assert.Equal(t, timeout, injected.Timeout)
	transport, err := NewSessionTransport(client)
	require.NoError(t, err)
	handle, err := transport.SessionByID("s")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), delay+5*time.Second)
	defer cancel()
	observation, err := handle.Observe(ctx, ObserveOptions{})
	require.NoError(t, err)
	defer observation.Cancel()
	waiting := make(chan error, 1)
	go func() { waiting <- handle.(*remoteSession).AwaitTurn(ctx, "turn") }()
	select {
	case event, ok := <-observation.Events:
		require.True(t, ok, "stream ended at ordinary timeout")
		assert.Equal(t, uint64(1), event.Sequence)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.NoError(t, <-waiting)
	// Ordinary mutations still time out and are never retried. Use a short
	// timeout even during the optional real-boundary run.
	ordinary := *injected
	ordinary.Timeout = 30 * time.Millisecond
	crudClient, err := NewClient(server.URL, WithHTTPClient(&ordinary), WithAuthToken("secret"))
	require.NoError(t, err)
	err = crudClient.sessionJSON(t.Context(), http.MethodPost, "/messages", nil, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, int32(1), mutations.Load())
}

func TestSessionLongLivedRequestsHonorCallerDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	client, err := NewClient(server.URL)
	require.NoError(t, err)
	for _, stream := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		if stream {
			_, err = client.attachSession(ctx, "s", ObserveOptions{})
		} else {
			err = client.sessionWaitJSON(ctx, http.MethodPost, "/wait", nil, nil)
		}
		cancel()
		require.ErrorIs(t, err, context.DeadlineExceeded)
	}
}

func TestSessionObservationFailureClassification(t *testing.T) {
	for _, status := range []int{401, 403, 400, 404, 408, 429, 500, 502, 503, 504, 501} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
			defer server.Close()
			client, err := NewClient(server.URL)
			require.NoError(t, err)
			_, err = client.attachSession(t.Context(), "s", ObserveOptions{})
			var classified interface{ Retryable() bool }
			require.ErrorAs(t, err, &classified)
			assert.Equal(t, status == 408 || status == 429 || status == 500 || status == 502 || status == 503 || status == 504, classified.Retryable())
			var structured *SessionError
			require.ErrorAs(t, err, &structured)
		})
	}
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprintf("protocol_live_%v", live), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if live {
					writeTransportSnapshot(w)
				}
				fmt.Fprint(w, "data: {\"version\":999,\"type\":\"invalid\"}\n\n")
			}))
			defer server.Close()
			client, err := NewClient(server.URL)
			require.NoError(t, err)
			observation, err := client.attachSession(t.Context(), "s", ObserveOptions{})
			if live {
				require.NoError(t, err)
				defer observation.Cancel()
				err = <-observation.Errors
			}
			var classified interface{ Retryable() bool }
			require.True(t, errors.As(err, &classified), "%v", err)
			assert.False(t, classified.Retryable())
		})
	}
}

func TestSessionObservationDetachDoesNotCancelAcceptedWork(t *testing.T) {
	accepted, finish, completed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var cancellations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages"):
			close(accepted)
			go func() { <-finish; close(completed) }()
			fmt.Fprint(w, `{"session_id":"s","turn_id":"turn"}`)
		case strings.HasSuffix(r.URL.Path, "/events"):
			writeTransportSnapshot(w)
			<-r.Context().Done()
		case strings.HasSuffix(r.URL.Path, "/cancel"):
			cancellations.Add(1)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL)
	require.NoError(t, err)
	transport, err := NewSessionTransport(client)
	require.NoError(t, err)
	handle, err := transport.SessionByID("s")
	require.NoError(t, err)
	observation, err := handle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	_, err = handle.Submit(t.Context(), TurnInput{Content: "work"})
	require.NoError(t, err)
	<-accepted
	observation.Cancel()
	close(finish)
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("accepted work did not finish")
	}
	assert.Zero(t, cancellations.Load())
}

func TestSessionStreamWatchdogHandshakeAndHeartbeat(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(fmt.Sprint(ready), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			watchdog := newSessionStreamWatchdog(cancel, 30*time.Millisecond, 80*time.Millisecond)
			defer watchdog.stop()
			if ready {
				watchdog.touch(true)
			}
			for range 4 {
				watchdog.touch(false) // Raw comment-only reads keep ready streams healthy.
				time.Sleep(15 * time.Millisecond)
			}
			if ready {
				require.NoError(t, ctx.Err())
			} else {
				require.ErrorIs(t, ctx.Err(), context.Canceled)
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("idle watchdog failed")
			}
			assert.True(t, watchdog.timedOut())
		})
	}
}

func TestSessionObservationTruncatedFrameRetriesButCompleteCorruptionDoesNot(t *testing.T) {
	for _, tc := range []struct {
		name, ending string
		retryable    bool
	}{
		{name: "partial_json", ending: `data: {"version":2,"type":"event","envelope":`, retryable: true},
		{name: "partial_chunk", ending: `data: {"version":2,"type":"snapshot_chunk","chunk":"abc`, retryable: true},
		{name: "complete_corrupt_line", ending: "data: {\"version\":2,invalid}\n\n", retryable: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeTransportSnapshot(w); fmt.Fprint(w, tc.ending) }))
			defer server.Close()
			client, err := NewClient(server.URL)
			require.NoError(t, err)
			observation, err := client.attachSession(t.Context(), "s", ObserveOptions{})
			require.NoError(t, err)
			defer observation.Cancel()
			err = <-observation.Errors
			require.Error(t, err)
			var classified interface{ Retryable() bool }
			retryable := true
			if errors.As(err, &classified) {
				retryable = classified.Retryable()
			}
			assert.Equal(t, tc.retryable, retryable, "%v", err)
		})
	}
}

func TestSessionChunkedSnapshotEndingPreservesTransportFailure(t *testing.T) {
	for _, tc := range []struct {
		name, ending string
		extra        bool
		retryable    bool
	}{
		{name: "missing_end", retryable: true},
		{name: "cut_end", ending: `data: {"version":2,"type":"snapshot_end","cursor":`, retryable: true},
		{name: "complete_extra_json", extra: true, retryable: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"version\":2,\"type\":\"snapshot_begin\",\"cursor\":0}\n\n")
				snapshot := []byte(`{"session":{"id":"s"},"status":{"session_id":"s"},"cursor":0}`)
				if tc.extra {
					snapshot = append(snapshot, []byte(` {}`)...)
				}
				frame, err := json.Marshal(remoteSessionStreamMessage{Version: sessionWireVersion, Type: "snapshot_chunk", Chunk: snapshot})
				if !assert.NoError(t, err) {
					return
				}
				fmt.Fprintf(w, "data: %s\n\n", frame)
				fmt.Fprint(w, tc.ending)
			}))
			defer server.Close()
			client, err := NewClient(server.URL)
			require.NoError(t, err)
			_, err = client.attachSession(t.Context(), "s", ObserveOptions{})
			require.Error(t, err)
			var classified interface{ Retryable() bool }
			retryable := true
			if errors.As(err, &classified) {
				retryable = classified.Retryable()
			}
			assert.Equal(t, tc.retryable, retryable, "%v", err)
			if tc.retryable {
				require.ErrorIs(t, err, io.ErrUnexpectedEOF)
			}
		})
	}
}
