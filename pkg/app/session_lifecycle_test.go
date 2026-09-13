package app

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
)

// sessionCaptureHandle records submission at the session ownership boundary.
type sessionCaptureHandle struct {
	projectionSession

	sess   *session.Session
	inputs []runtime.TurnInput
}

func (h *sessionCaptureHandle) Submit(ctx context.Context, input runtime.TurnInput) (runtime.Submission, error) {
	if err := ctx.Err(); err != nil {
		return runtime.Submission{}, err
	}
	h.inputs = append(h.inputs, input)
	h.sess.AddMessage(session.UserMessage(input.Content))
	return runtime.Submission{SessionID: h.sess.ID, TurnID: "turn"}, nil
}

// backgroundSessionCaptureRuntime records the session handed to the runtime
// entry points App drives from background goroutines, and holds each call
// until release is closed so a concurrent ReplaceSession can be interleaved.
type backgroundSessionCaptureRuntime struct {
	mockRuntime

	started chan *session.Session
	release chan struct{}
}

func (r *backgroundSessionCaptureRuntime) EmitStartupInfo(_ context.Context, sess *session.Session, _ runtime.EventSink) {
	r.started <- sess
	<-r.release
}

type backgroundSkillHandle struct {
	projectionSession

	runtime *backgroundSessionCaptureRuntime
	sess    *session.Session
}

func (*backgroundSkillHandle) Skills(context.Context) ([]skills.Skill, error) { return nil, nil }
func (*backgroundSkillHandle) ResolveSkillCommand(_ context.Context, input string) (string, error) {
	return input, nil
}

func (h *backgroundSkillHandle) StartSkillFork(_ context.Context, _ string, _ skillstool.RunSkillArgs) error {
	go func() {
		h.runtime.started <- h.sess
		<-h.runtime.release
	}()
	return nil
}

// TestAppBackgroundWorkSnapshotsSessionBeforeReplace covers every App entry
// point that hands a.session to a background goroutine: the goroutine must
// receive the session that was current when it was spawned, and must not
// read the field itself, which races with ReplaceSession (#4229). Under
// -race a field read from the goroutine fails this test deterministically.
func TestAppBackgroundWorkSnapshotsSessionBeforeReplace(t *testing.T) {
	for _, entryPoint := range []struct {
		name string
		run  func(*App, context.Context, context.CancelFunc)
	}{
		{name: "Start", run: func(app *App, ctx context.Context, _ context.CancelFunc) {
			app.Start(ctx)
		}},
		{name: "reEmitStartupInfo", run: func(app *App, ctx context.Context, _ context.CancelFunc) {
			app.reEmitStartupInfo(ctx)
		}},
		{name: "RunSkillFork", run: func(app *App, ctx context.Context, cancel context.CancelFunc) {
			require.NoError(t, app.StartSkillForkOperation(ctx, "operation", "skill", "task"))
		}},
	} {
		t.Run(entryPoint.name, func(t *testing.T) {
			oldSession := session.New()
			rt := &backgroundSessionCaptureRuntime{
				started: make(chan *session.Session, 4),
				release: make(chan struct{}),
			}
			app := &App{
				runtime:      rt,
				currentState: sessionState{handle: &backgroundSkillHandle{runtime: rt, sess: oldSession}, session: oldSession},
				events:       make(chan any, 16),
			}
			ctx, cancel := context.WithCancel(t.Context())
			entryPoint.run(app, ctx, cancel)

			// Replace the session right away, before the background goroutine
			// has necessarily started running; ReplaceSession re-emits startup
			// info for the new session, so two sessions reach the runtime.
			newSession := session.New()
			app.ReplaceSession(t.Context(), newSession)
			first, second := <-rt.started, <-rt.started
			close(rt.release)

			assert.ElementsMatch(t, []*session.Session{oldSession, newSession}, []*session.Session{first, second},
				"background work must run against the session current when it was spawned")
		})
	}
}

func TestAppRunKeepsWorkScopedToOriginalSession(t *testing.T) {
	for _, withMessage := range []bool{false, true} {
		t.Run(map[bool]string{false: "Run", true: "RunWithMessage"}[withMessage], func(t *testing.T) {
			oldSession := session.New()
			handle := &sessionCaptureHandle{sess: oldSession}
			a := &App{ctx: t.Context, runtime: &mockRuntime{}, currentState: sessionState{session: oldSession, handle: handle}, events: make(chan any, 4)}
			if withMessage {
				a.RunWithMessage(t.Context(), func() {}, session.UserMessage("hello"))
			} else {
				a.Run(t.Context(), func() {}, "hello", nil)
			}
			newSession := session.New()
			a.ReplaceSession(t.Context(), newSession)
			require.Len(t, handle.inputs, 1)
			assert.Equal(t, "hello", handle.inputs[0].Content)
			assert.Equal(t, 1, oldSession.MessageCount())
			assert.Zero(t, newSession.MessageCount())
		})
	}
}

// Admission cancellation is enforced by the handle; App no longer owns a stream guard.
func TestAppRunDoesNotAdmitCanceledInput(t *testing.T) {
	for _, withMessage := range []bool{false, true} {
		t.Run(map[bool]string{false: "Run", true: "RunWithMessage"}[withMessage], func(t *testing.T) {
			sess := session.New()
			handle := &sessionCaptureHandle{sess: sess}
			a := &App{ctx: t.Context, runtime: &mockRuntime{}, currentState: sessionState{session: sess, handle: handle}, events: make(chan any, 4)}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if withMessage {
				a.RunWithMessage(ctx, cancel, session.UserMessage("hello"))
			} else {
				a.Run(ctx, cancel, "hello", nil)
			}
			assert.Empty(t, handle.inputs)
			assert.Zero(t, sess.MessageCount())
		})
	}
}

type lifecycleBlockingTitleProvider struct {
	started chan struct{}
	release chan struct{}
}

func (p *lifecycleBlockingTitleProvider) ID() modelsdev.ID {
	return modelsdev.NewID("test", "title")
}

func (p *lifecycleBlockingTitleProvider) BaseConfig() base.Config { return base.Config{} }

func (p *lifecycleBlockingTitleProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	close(p.started)
	<-p.release
	return &singleTitleStream{}, nil
}

type singleTitleStream struct {
	done bool
}

func (s *singleTitleStream) Recv() (chat.MessageStreamResponse, error) {
	if s.done {
		return chat.MessageStreamResponse{}, io.EOF
	}
	s.done = true
	return chat.MessageStreamResponse{
		Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "Original title"}}},
	}, nil
}

func (*singleTitleStream) Close() {}

func TestGenerateTitleKeepsOriginalSession(t *testing.T) {
	provider := &lifecycleBlockingTitleProvider{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	oldSession := session.New()
	app := &App{
		runtime:      &mockRuntime{},
		currentState: sessionState{session: oldSession, handle: &titleSession{sess: oldSession}},
		events:       make(chan any, 1),
		titleGen:     sessiontitle.New(provider),
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		app.generateTitle(t.Context(), app.state(), []string{"hello"})
	}()
	<-provider.started
	newSession := session.New()
	app.ReplaceSession(t.Context(), newSession)
	close(provider.release)
	<-done

	assert.Equal(t, "Original title", oldSession.TitleSnapshot())
	assert.Empty(t, newSession.TitleSnapshot())
	assert.Empty(t, app.events, "title events belong to the canonical handle observation, not a second App emission")
}
