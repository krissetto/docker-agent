package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func TestMessageExtensionsIsolateFailureAndPolicy(t *testing.T) {
	a := agent.New("root", "instructions", agent.WithModel(&mockProvider{id: "test/model"}))
	sess := session.New()
	original := []chat.Message{{Role: chat.MessageRoleUser, Content: "secret", MultiContent: []chat.MessagePart{{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "original"}}}}}
	r := &LocalRuntime{}
	WithMessageTransform("broken", func(_ context.Context, in *hooks.Input, msgs []chat.Message) ([]chat.Message, error) {
		in.AgentName = "hijacked"
		msgs[0].MultiContent[0].ImageURL.URL = "mutated"
		return nil, errors.New("projection unavailable")
	})(r)
	WithMessageTransform("project", func(_ context.Context, in *hooks.Input, msgs []chat.Message) ([]chat.Message, error) {
		require.Equal(t, "root", in.AgentName)
		require.Equal(t, "original", msgs[0].MultiContent[0].ImageURL.URL)
		msgs[0].Content = "projected"
		return msgs, nil
	})(r)
	WithMessagePolicy("redact", func(_ context.Context, in *hooks.Input, msgs []chat.Message) ([]chat.Message, error) {
		require.Equal(t, sess.ID, in.SessionID)
		require.Equal(t, "test/model", in.ModelID)
		require.Equal(t, "projected", msgs[0].Content)
		msgs[0].Content = "safe"
		return msgs, nil
	})(r)
	projected := r.applyBeforeLLMCallTransforms(t.Context(), sess, a, "test/model", nil, original)
	out, err := r.applyMessagePolicies(t.Context(), sess, a, "test/model", nil, projected)
	require.NoError(t, err)
	require.Equal(t, "safe", out[0].Content)
	require.Equal(t, "secret", original[0].Content)
	require.Equal(t, "original", original[0].MultiContent[0].ImageURL.URL)
	WithMessagePolicy("deny", func(context.Context, *hooks.Input, []chat.Message) ([]chat.Message, error) { panic("policy failed") })(r)
	out, err = r.applyMessagePolicies(t.Context(), sess, a, "test/model", nil, projected)
	require.ErrorContains(t, err, "policy failed")
	require.Nil(t, out)
}

func TestRuntimeRegistryIsPrivateAndRejectsReservedName(t *testing.T) {
	source := hooks.NewRegistry()
	a := agent.New("root", "instructions", agent.WithModel(&mockProvider{id: "test/model"}))
	tm := team.New(team.WithAgents(a))
	first, err := NewLocalRuntime(t.Context(), tm, WithHooksRegistry(source), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	second, err := NewLocalRuntime(t.Context(), tm, WithHooksRegistry(source), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	require.NotSame(t, source, first.hooksRegistry)
	require.NotSame(t, first.hooksRegistry, second.hooksRegistry)
	_, exists := source.LookupBuiltin(BuiltinCacheResponse)
	require.False(t, exists)
	require.NoError(t, source.RegisterBuiltin(BuiltinCacheResponse, func(context.Context, *hooks.Input, []string) (*hooks.Output, error) { return nil, nil }))
	_, err = NewLocalRuntime(t.Context(), tm, WithHooksRegistry(source), WithModelStore(mockModelStore{}))
	require.ErrorContains(t, err, "reserved")
}

func TestObserverSnapshotsAndPanicsCannotMutateDelivery(t *testing.T) {
	sess := session.New()
	event := &UserMessageEvent{Message: "original", MultiContent: []chat.MessagePart{{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "original"}}}}
	r := &LocalRuntime{sessionDrivers: newSessionDriverRegistry(nil), observers: []EventObserver{&fnObserver{onEvent: func(_ context.Context, snapshot *session.Session, e Event) {
		snapshot.ID = "changed"
		detached := e.(*UserMessageEvent)
		detached.Message = "changed"
		detached.MultiContent[0].ImageURL.URL = "changed"
		panic("telemetry failed")
	}}, &fnObserver{onEvent: func(_ context.Context, snapshot *session.Session, e Event) {
		require.Equal(t, sess.ID, snapshot.ID)
		require.Equal(t, "original", e.(*UserMessageEvent).Message)
	}}}}
	inner := make(chan Event, 1)
	inner <- event
	close(inner)
	for got := range r.observe(t.Context(), sess, inner) {
		require.Same(t, event, got)
	}
	require.Equal(t, "original", event.MultiContent[0].ImageURL.URL)
}

func TestOutboundPolicyPreventsProviderDelivery(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   MessageTransform
	}{
		{"error", func(context.Context, *hooks.Input, []chat.Message) ([]chat.Message, error) {
			return nil, errors.New("denied")
		}},
		{"panic", func(context.Context, *hooks.Input, []chat.Message) ([]chat.Message, error) { panic("denied") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			primary := &recordingMsgProvider{mockProvider: mockProvider{id: "test/primary", stream: &mockStream{}}}
			fallback := &recordingMsgProvider{mockProvider: mockProvider{id: "test/fallback", stream: &mockStream{}}}
			a := agent.New("root", "instructions", agent.WithModel(primary), agent.WithFallbackModel(fallback))
			r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(a)), WithModelStore(mockModelStore{}), WithSessionCompaction(false), WithMessagePolicy("deny", tc.fn))
			require.NoError(t, err)
			sess := session.New(session.WithUserMessage("secret"))
			for range r.runExecution(t.Context(), sess) {
			}
			require.Empty(t, primary.got, "policy failure must never reach provider")
			require.Empty(t, fallback.got, "policy failure must not try fallback")
		})
	}
}

func TestMessagePolicyCancellationRetainsWorkerUntilHandlerReturns(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	primary := &recordingMsgProvider{mockProvider: mockProvider{id: "test/primary", stream: &mockStream{}}}
	a := agent.New("root", "instructions", agent.WithModel(primary))
	r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(a)), WithModelStore(mockModelStore{}), WithSessionCompaction(false), WithMessagePolicy("blocked", func(context.Context, *hooks.Input, []chat.Message) ([]chat.Message, error) {
		close(started)
		<-release
		return []chat.Message{{Role: chat.MessageRoleUser, Content: "safe"}}, nil
	}))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	stream := r.runExecution(ctx, session.New(session.WithUserMessage("secret")))
	<-started
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer shutdownCancel()
	require.ErrorIs(t, r.shutdownSessions(shutdownCtx), context.DeadlineExceeded, "shutdown must retain the owned callback worker")
	releaseOnce.Do(func() { close(release) })
	for range stream {
	}
	require.NoError(t, r.shutdownSessions(t.Context()))
	require.Empty(t, primary.got, "canceled policy must not deliver its output")
}

func TestObserverMessageAddedSnapshotPreservesPrivatePayload(t *testing.T) {
	message := &session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "original", MultiContent: []chat.MessagePart{{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "original"}}}}}
	event := MessageAdded("session", message, "root").(*MessageAddedEvent)
	event.boundaryOnly = true
	snapshot, err := observerEventSnapshot(event)
	require.NoError(t, err)
	copy := snapshot.(*MessageAddedEvent)
	require.Equal(t, event, copy)
	copy.Message.Message.MultiContent[0].ImageURL.URL = "changed"
	require.Equal(t, "original", message.Message.MultiContent[0].ImageURL.URL)
}
