package server

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

type serverSession struct {
	runtime.UnsupportedSessionHandle
	id        string
	agent     string
	state     runtime.SessionState
	sent      []runtime.TurnInput
	submit    []runtime.TurnInput
	responses []runtime.InteractionResponse
	stops     int
}

func (a *serverSession) ID() string        { return a.id }
func (a *serverSession) AgentName() string { return a.agent }
func (a *serverSession) Metadata() runtime.SessionMetadata {
	return runtime.SessionMetadata{SessionID: a.id, AgentName: a.agent}
}

func (a *serverSession) Submit(_ context.Context, in runtime.TurnInput) (runtime.Submission, error) {
	a.submit = append(a.submit, in)
	return runtime.Submission{SessionID: a.id, TurnID: "accepted"}, nil
}

func (a *serverSession) Retry(context.Context) (runtime.Submission, error) {
	return runtime.Submission{SessionID: a.id, TurnID: "retry"}, nil
}

func (a *serverSession) Steer(_ context.Context, in runtime.TurnInput) (runtime.Submission, error) {
	a.sent = append(a.sent, in)
	return runtime.Submission{SessionID: a.id, TurnID: "accepted"}, nil
}

func (a *serverSession) Observe(ctx context.Context, _ runtime.ObserveOptions) (runtime.Observation, error) {
	ch := make(chan runtime.SessionEvent)
	obsCtx, cancel := context.WithCancel(ctx)
	go func() { <-obsCtx.Done(); close(ch) }()
	return runtime.Observation{Events: ch, Cancel: cancel}, nil
}

func (a *serverSession) Status(context.Context) (runtime.SessionStatus, error) {
	return runtime.SessionStatus{SessionID: a.id, AgentName: a.agent, State: a.state}, nil
}

func (a *serverSession) Respond(_ context.Context, response runtime.InteractionResponse) error {
	if response.InteractionID == "" {
		return errors.New("missing request")
	}
	a.responses = append(a.responses, response)
	return nil
}

func (a *serverSession) Cancel(context.Context, string) (runtime.CancelResult, error) {
	a.stops++
	a.state = runtime.SessionStateSettled
	return runtime.CancelResult{SessionID: a.id, Outcome: runtime.CancelAccepted}, nil
}

type serverSessionRegistry struct {
	session *serverSession
	closed  chan struct{}
}

func (r *serverSessionRegistry) CreateSession(_ context.Context, sess *session.Session, binding runtime.SessionBinding) (runtime.SessionHandle, error) {
	r.session.id, r.session.agent = sess.ID, binding.AgentName
	return r.session, nil
}

func (r *serverSessionRegistry) SessionByID(string) (runtime.SessionHandle, error) {
	return r.session, nil
}

func (r *serverSessionRegistry) DeleteSession(ctx context.Context, _ string) error {
	return r.session.Release(ctx)
}

func TestBorrowedSessionRuntimeOutlivesServerShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	closed := make(chan struct{})
	registry := &serverSessionRegistry{session: &serverSession{}, closed: closed}
	sm := NewSessionManager(ctx, config.Sources{}, session.NewInMemorySessionStore(), 0, &config.RuntimeConfig{}, WithSessionRuntime(registry))
	srv := NewWithManager(sm, "")
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("server did not shut down within bound")
	}
	select {
	case <-closed:
		t.Fatal("server shutdown closed its borrowed session runtime")
	default:
	}
}

func TestSessionObserverDisconnectDoesNotStopSession(t *testing.T) {
	handle := &serverSession{id: "s", agent: "root", state: runtime.SessionStateRunning}
	obsCtx, cancel := context.WithCancel(t.Context())
	obs, err := handle.Observe(obsCtx, runtime.ObserveOptions{})
	require.NoError(t, err)
	cancel()
	<-obs.Events
	assert.Zero(t, handle.stops)
	status, err := handle.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, runtime.SessionStateRunning, status.State)
}

func (a *serverSession) Release(context.Context) error { return nil }

func (a *serverSession) UpdateTitle(context.Context, string) error { return nil }
