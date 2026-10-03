package runtime

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// multiRunProvider hands out a fresh scripted stream per model call, so a
// session can run multiple turns (initial run + session wakes).
type multiRunProvider struct {
	*mockProvider

	build func() chat.MessageStream
}

func (p *multiRunProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return p.build(), nil
}

type blockingMockStream struct {
	responses []chat.MessageStreamResponse
	idx       int
	release   <-chan struct{}
}

func (s *blockingMockStream) Recv() (chat.MessageStreamResponse, error) {
	if s.idx == len(s.responses) {
		s.idx++
		<-s.release
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Index: 0, FinishReason: chat.FinishReasonStop}}, Usage: &chat.Usage{InputTokens: 1, OutputTokens: 1}}, nil
	}
	if s.idx > len(s.responses) {
		return chat.MessageStreamResponse{}, io.EOF
	}
	resp := s.responses[s.idx]
	s.idx++
	return resp, nil
}

func (s *blockingMockStream) Close() {}

func newSessionFixture(t *testing.T) (*LocalRuntime, *session.Session) {
	t.Helper()
	prov := &multiRunProvider{
		mockProvider: &mockProvider{id: "test/mock-model"},
		build: func() chat.MessageStream {
			return newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()
		},
	}
	tm := team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(prov))))
	rt, err := NewLocalRuntime(t.Context(), tm)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })
	return rt, session.New(session.WithID(t.Name() + "/session"))
}

func TestSessionWakesIdleSessionOnNote(t *testing.T) {
	t.Parallel()

	rt, sess := newSessionFixture(t)

	sess.AddMessage(session.UserMessage("hi"))
	for range rt.runExecution(t.Context(), sess) {
	}
	turnOne := len(sess.Messages)

	_, events, cancel := subscribeSessionEventsForTest(rt, sess.ID)
	defer cancel()

	rt.deliverOrBuffer(t.Context(), sess.ID, "<system_info>subagent report</system_info>")

	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-events:
			if _, ok := e.(*StreamStoppedEvent); ok {
				assert.Greater(t, len(sess.Messages), turnOne,
					"the wake run must add the note and a fresh assistant reply")
				last := sess.Messages[len(sess.Messages)-1]
				require.NotNil(t, last.Message)
				assert.Equal(t, chat.MessageRoleAssistant, last.Message.Message.Role)
				assert.Eventually(t, func() bool { return rt.sessionDrivers.Settled(sess.ID) },
					2*time.Second, 10*time.Millisecond)
				return
			}
		case <-deadline:
			t.Fatal("no wake run happened for the buffered note")
		}
	}
}

func TestSessionBuffersUnknownSessionUntilSeen(t *testing.T) {
	t.Parallel()

	rt, sess := newSessionFixture(t)

	rt.deliverOrBuffer(t.Context(), sess.ID, "early note")
	assert.False(t, rt.sessionDrivers.Settled(sess.ID), "note must stay buffered")

	sess.AddMessage(session.UserMessage("hi"))
	for range rt.runExecution(t.Context(), sess) {
	}

	assert.Eventually(t, func() bool {
		return rt.sessionDrivers.Settled(sess.ID) && len(sess.Messages) >= 3
	}, 5*time.Second, 10*time.Millisecond)
	assert.NotContains(t, sess.Messages[0].Message.Message.Content, "early note", "detached runtime note is steering context, not the initial user turn")
}

func TestDriveDrivesFreeSession(t *testing.T) {
	t.Parallel()

	rt, sess := newSessionFixture(t)

	sess.AddMessage(session.UserMessage("hello"))
	var sawStop bool
	for e := range rt.Drive(t.Context(), sess) {
		if _, ok := e.(*StreamStoppedEvent); ok {
			sawStop = true
		}
	}
	assert.True(t, sawStop)
	require.GreaterOrEqual(t, len(sess.Messages), 2)
	require.NotNil(t, sess.Messages[0].Message)
	assert.Equal(t, "hello", sess.Messages[0].Message.Message.Content)
	assert.Equal(t, chat.MessageRoleUser, sess.Messages[0].Message.Message.Role)
	assert.Eventually(t, func() bool { return rt.sessionDrivers.Settled(sess.ID) },
		2*time.Second, 10*time.Millisecond)
}

func TestCloseStopsAttachedDriveCallers(t *testing.T) {
	release := make(chan struct{})
	prov := &activeRootBlockingProvider{id: "test/blocking-model", release: release}
	tm := team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(prov))))
	rt, err := NewLocalRuntime(t.Context(), tm)
	require.NoError(t, err)

	sess := session.New(session.WithID("close-attach"))
	first := rt.Drive(t.Context(), sess)
	second := rt.Drive(t.Context(), sess)
	require.Eventually(t, func() bool {
		d, ok := rt.sessionDrivers.Lookup(sess.ID)
		return ok && !d.Settled()
	}, 2*time.Second, 5*time.Millisecond)

	require.NoError(t, rt.Close())
	for _, stream := range []<-chan Event{first, second} {
		select {
		case _, ok := <-stream:
			for ok {
				_, ok = <-stream
			}
		case <-time.After(2 * time.Second):
			t.Fatal("attached Drive stream did not close")
		}
	}
	close(release)
}
