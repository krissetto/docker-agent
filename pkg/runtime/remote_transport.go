package runtime

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// longLivedHTTPClient preserves the configured transport (including TLS), jar,
// and redirect policy without imposing the ordinary request's whole-body
// timeout on an observation or wait. Their caller context owns their lifetime.
// Do not mutate the borrowed client: it may also be serving ordinary requests.
func (c *Client) longLivedHTTPClient() *http.Client {
	client := *c.httpClient
	client.Timeout = 0
	return &client
}

func (c *Client) sessionWaitJSON(ctx context.Context, method, endpoint string, body, result any) error {
	return c.sessionJSONWithClient(ctx, c.longLivedHTTPClient(), method, endpoint, body, result)
}

// sessionHTTPError retains structured SessionError matching while exposing a
// retry decision to observation adapters. This never retries a request itself.
type sessionHTTPError struct {
	status int
	err    error
}

func (e *sessionHTTPError) Error() string {
	return fmt.Sprintf("session HTTP %d: %v", e.status, e.err)
}
func (e *sessionHTTPError) Unwrap() error { return e.err }
func (e *sessionHTTPError) Retryable() bool {
	return e.status == http.StatusRequestTimeout || e.status == http.StatusTooManyRequests ||
		e.status == http.StatusInternalServerError || e.status == http.StatusBadGateway ||
		e.status == http.StatusServiceUnavailable || e.status == http.StatusGatewayTimeout
}

type observationProtocolError struct{ err error }

func (e *observationProtocolError) Error() string { return e.err.Error() }
func (e *observationProtocolError) Unwrap() error { return e.err }
func (*observationProtocolError) Retryable() bool { return false }

func protocolError(err error) error {
	if err == nil {
		return nil
	}
	return &observationProtocolError{err: err}
}

// A truncated/disconnected stream is recoverable, malformed wire data is not.
func sessionStreamReadError(err error) error {
	if err == nil {
		return nil
	}
	var retryable interface{ Retryable() bool }
	var network net.Error
	if errors.As(err, &retryable) || errors.As(err, &network) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return protocolError(err)
}

const defaultSessionRequestTimeout = 30 * time.Second

// One timer bounds the snapshot handshake, then becomes the idle watchdog.
// Raw reads include SSE comments/heartbeats, so quiet healthy streams remain
// attached. The first-party server heartbeat is 15s; tolerate three intervals.
const sessionStreamIdleTimeout = 45 * time.Second

type sessionStreamWatchdog struct {
	mu       sync.Mutex
	timer    *time.Timer
	ready    bool
	stopped  bool
	expired  bool
	idle     time.Duration
	deadline time.Time
}

func newSessionStreamWatchdog(cancel context.CancelFunc, handshake, idle time.Duration) *sessionStreamWatchdog {
	w := &sessionStreamWatchdog{idle: idle, deadline: time.Now().Add(handshake)}
	w.timer = time.AfterFunc(handshake, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		if !w.stopped {
			if remaining := time.Until(w.deadline); remaining > 0 {
				w.timer.Reset(remaining)
				return
			}
			w.expired = true
			cancel()
		}
	})
	return w
}
func (w *sessionStreamWatchdog) touch(ready bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped || w.expired {
		return
	}
	w.ready = w.ready || ready
	if w.ready {
		w.deadline = time.Now().Add(w.idle)
		w.timer.Reset(w.idle)
	}
}
func (w *sessionStreamWatchdog) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	w.timer.Stop()
}
func (w *sessionStreamWatchdog) timedOut() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.expired
}

type sessionStreamReader struct {
	io.Reader
	watchdog *sessionStreamWatchdog
}

func (r sessionStreamReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 {
		r.watchdog.touch(false)
	}
	return n, err
}

// scanSessionLines never exposes an unterminated final token to the JSON
// decoder. ScanLines normally returns that token before reporting EOF (or a
// read failure), turning a network cutoff into a terminal syntax error. SSE
// data lines require a newline; replay can safely recover the truncated tail.
func scanSessionLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) > 0 && bytes.IndexByte(data, '\n') < 0 {
		return 0, nil, io.ErrUnexpectedEOF
	}
	return bufio.ScanLines(data, atEOF)
}
