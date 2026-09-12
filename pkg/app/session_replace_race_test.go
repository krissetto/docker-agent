package app

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestStartAndReplaceSessionSnapshotStartupSession(t *testing.T) {
	services := &blockingStartupServices{mockRuntime: mockRuntime{}, entered: make(chan *session.Session, 1), release: make(chan struct{})}
	first := session.New(session.WithID("first"))
	second := session.New(session.WithID("second"))
	a := New(t.Context(), nil, first, runtime.SessionBinding{}, WithRuntimeServices(services))

	var wg sync.WaitGroup
	wg.Go(func() { a.Start(t.Context()) })
	started := <-services.entered
	a.ReplaceSession(t.Context(), second)
	close(services.release)
	wg.Wait()

	if started != first {
		t.Fatalf("startup used replacement session: got %s want %s", started.ID, first.ID)
	}
	if got := a.Session(); got != second {
		t.Fatalf("replacement not published: got %s want %s", got.ID, second.ID)
	}
}

func TestRunAndRetryDispatchCoherentCapturedHandleDuringReplace(t *testing.T) {
	t.Parallel()
	first := session.New(session.WithID("first"))
	second := session.New(session.WithID("second"))
	firstHandle := &projectionSession{id: first.ID}
	secondHandle := &projectionSession{id: second.ID}
	a := &App{currentState: sessionState{session: first, handle: firstHandle}, events: make(chan any, 8)}

	for _, dispatch := range []func(){
		func() { a.Run(t.Context(), func() {}, "run", nil) },
		func() { a.Retry(t.Context(), func() {}) },
	} {
		a.replaceSessionState(sessionState{session: first, handle: firstHandle})
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			dispatch()
		})
		wg.Go(func() {
			<-start
			a.replaceSessionState(sessionState{session: second, handle: secondHandle})
		})
		close(start)
		wg.Wait()
	}

	firstHandle.mu.Lock()
	firstCalls := firstHandle.next
	firstHandle.mu.Unlock()
	secondHandle.mu.Lock()
	secondCalls := secondHandle.next
	secondHandle.mu.Unlock()
	assert.Equal(t, 2, firstCalls+secondCalls)
}

func TestRunTitleGenerationStaysWithCapturedSessionState(t *testing.T) {
	t.Parallel()
	provider := &blockingTitleProvider{entered: make(chan struct{}), release: make(chan struct{})}
	first := session.New(session.WithID("first"))
	second := session.New(session.WithID("second"))
	firstHandle := &titleCaptureSession{projectionSession: projectionSession{id: first.ID}, titles: make(chan string, 1)}
	secondHandle := &titleCaptureSession{projectionSession: projectionSession{id: second.ID}, titles: make(chan string, 1)}
	a := &App{
		ctx:          t.Context,
		currentState: sessionState{session: first, handle: firstHandle},
		titleGen:     sessiontitle.New(provider),
		events:       make(chan any, 8),
	}

	a.Run(t.Context(), func() {}, "describe this", nil)
	<-provider.entered
	a.replaceSessionState(sessionState{session: second, handle: secondHandle})
	close(provider.release)

	assert.Equal(t, "captured title", <-firstHandle.titles)
	select {
	case title := <-secondHandle.titles:
		t.Fatalf("replacement handle received stale title %q", title)
	default:
	}
}

type titleCaptureSession struct {
	projectionSession
	titles chan string
}

func (s *titleCaptureSession) UpdateTitle(_ context.Context, title string) error {
	s.titles <- title
	return nil
}

type blockingTitleProvider struct {
	stubProvider
	entered chan struct{}
	release chan struct{}
}

func (p *blockingTitleProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	close(p.entered)
	<-p.release
	return &titleStream{}, nil
}

type titleStream struct{ sent bool }

func (s *titleStream) Recv() (chat.MessageStreamResponse, error) {
	if s.sent {
		return chat.MessageStreamResponse{}, io.EOF
	}
	s.sent = true
	return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "captured title"}}}}, nil
}
func (*titleStream) Close() {}

func TestConcurrentReplacePublishesCoherentSessionState(t *testing.T) {
	t.Parallel()
	first := session.New(session.WithID("first"), session.WithAgentName("root"))
	second := session.New(session.WithID("second"), session.WithAgentName("root"))
	firstHandle := &projectionSession{id: first.ID}
	secondHandle := &projectionSession{id: second.ID}
	a := &App{currentState: sessionState{session: first, handle: firstHandle, binding: runtime.SessionBinding{Model: first.ID}}}

	for range 1000 {
		a.replaceSessionState(sessionState{session: first, handle: firstHandle, binding: runtime.SessionBinding{Model: first.ID}})
		start := make(chan struct{})
		done := make(chan struct{})
		go func() {
			<-start
			a.replaceSessionState(sessionState{session: second, handle: secondHandle, binding: runtime.SessionBinding{Model: second.ID}})
			close(done)
		}()
		close(start)
		for {
			state := a.state()
			require.NotNil(t, state.session)
			require.NotNil(t, state.handle)
			assert.Equal(t, state.session.ID, state.handle.ID())
			assert.Equal(t, state.session.ID, state.binding.Model)
			select {
			case <-done:
				goto replaced
			default:
			}
		}
	replaced:
		state := a.state()
		assert.Equal(t, second.ID, state.session.ID)
		assert.Equal(t, second.ID, state.handle.ID())
	}
}

type blockingStartupServices struct {
	mockRuntime

	entered chan *session.Session
	release chan struct{}
}

func (s *blockingStartupServices) EmitStartupInfo(_ context.Context, sess *session.Session, _ runtime.EventSink) {
	s.entered <- sess
	<-s.release
}
