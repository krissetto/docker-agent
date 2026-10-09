package runtime

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

func TestDelegationScalarStateUsesCanonicalOwner(t *testing.T) {
	t.Parallel()
	root := allocationHistoryFixture("root", "", 2)
	root.SetAttribute(SessionDelegationAttribute, "true")
	stale := root.Clone()
	child := session.New(session.WithID("child"), session.WithParentID(root.ID), session.WithAttributes(map[string]string{SessionDelegationAttribute: "true"}))
	r := &LocalRuntime{}
	r.sessionDrivers = newSessionDriverRegistry(r)
	driver := &sessionDriver{sess: root}
	r.sessionDrivers.drivers[root.ID] = driver
	r.sessionDrivers.drivers[child.ID] = &sessionDriver{sess: child}
	h := &sessionHandle{runtime: r, driver: r.sessionDrivers.drivers[child.ID]}
	root.SetAttribute(SessionDelegationAttribute, "false")
	require.False(t, r.sessionDelegationEnabled(stale))
	enabled, err := h.DelegationPolicy(t.Context())
	require.NoError(t, err)
	require.False(t, enabled, "child attributes must not bypass canonical root policy")
	for _, value := range []string{"invalid", "", "false"} {
		root.SetAttribute(SessionDelegationAttribute, value)
		require.False(t, r.sessionDelegationEnabled(child))
	}
	root.DeleteAttribute(SessionDelegationAttribute)
	require.True(t, r.sessionDelegationEnabled(child))
	r.SetUseSubagents(false)
	require.False(t, r.sessionDelegationEnabled(child))
	root.SetAttribute(SessionDelegationAttribute, "true")
	require.True(t, r.sessionDelegationEnabled(child))
	replacement := root.Clone()
	replacement.SetAttribute(SessionDelegationAttribute, "false")
	driver.mu.Lock()
	driver.sess = replacement
	driver.mu.Unlock()
	require.False(t, r.sessionDelegationEnabled(root), "replacement must not leave a cached policy")
	require.Equal(t, "true", root.AttributesSnapshot()[SessionDelegationAttribute])

	var wg sync.WaitGroup
	wg.Go(func() {
		for range 100 {
			replacement.SetAttribute(SessionDelegationAttribute, "true")
			replacement.SetAttribute(SessionDelegationAttribute, "false")
			driver.mu.Lock()
			driver.sess = root
			driver.mu.Unlock()
			driver.mu.Lock()
			driver.sess = replacement
			driver.mu.Unlock()
		}
	})
	for range 100 {
		_, err := h.DelegationPolicy(t.Context())
		require.NoError(t, err)
	}
	wg.Wait()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = h.DelegationPolicy(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

func TestDelegationScalarStateAncestorFallbackAndCycle(t *testing.T) {
	t.Parallel()
	root := session.New(session.WithID("stored-root"), session.WithAttributes(map[string]string{SessionDelegationAttribute: "false"}))
	child := session.New(session.WithID("child"), session.WithParentID(root.ID))
	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), root))
	r := &LocalRuntime{sessionStore: store}
	r.sessionDrivers = newSessionDriverRegistry(r)
	require.False(t, r.sessionDelegationEnabled(child))
	root.SetAttribute(SessionDelegationAttribute, "true")
	r.subagents = &subagentManager{sessions: map[string]*sessionSubagents{root.ID: {sess: root}}}
	require.True(t, r.sessionDelegationEnabled(child), "tracked ancestor wins over stored snapshot")
	root.ParentID = child.ID
	r.sessionDrivers.drivers[child.ID] = &sessionDriver{sess: child}
	require.False(t, r.sessionDelegationEnabled(child), "cyclic ancestry must fail closed")
}

func TestDelegationScalarStateMissingCanonicalSessionFailsClosed(t *testing.T) {
	t.Parallel()
	sess := session.New(session.WithID("missing-canonical"))
	r := &LocalRuntime{}
	r.sessionDrivers = newSessionDriverRegistry(r)
	driver := &sessionDriver{}
	r.sessionDrivers.drivers[sess.ID] = driver
	require.False(t, r.sessionDelegationEnabled(sess))
	h := &sessionHandle{runtime: r, driver: driver}
	_, err := h.DelegationPolicy(t.Context())
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	require.Equal(t, SessionErrorNotFound, sessionErr.Kind)
}

func TestRecordAssistantMessageDebugCountMatchesTranscript(t *testing.T) {
	for _, level := range []slog.Level{slog.LevelInfo, slog.LevelDebug} {
		t.Run(level.String(), func(t *testing.T) {
			var output bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: level})))
			t.Cleanup(func() { slog.SetDefault(previous) })
			sess := session.New(session.WithUserMessage("root request"))
			child := session.New(session.WithUserMessage("child request"))
			sess.AddLiveSubSession(child)
			sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleSystem, Content: "not counted"}})
			sess.AddError(&session.Error{Message: "not counted"})
			r := &LocalRuntime{now: time.Now}
			r.recordAssistantMessage(t.Context(), sess, agent.New("root", "prompt"), streamResult{Content: "answer"}, nil, "test/fixture", nil, EventSinkFunc(func(Event) {}))
			require.Len(t, sess.GetAllMessages(), 3)
			if level == slog.LevelDebug {
				require.Contains(t, output.String(), "total_messages=3")
			} else {
				require.Empty(t, output.String())
			}
		})
	}
}
