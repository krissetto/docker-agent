package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
)

func TestSecondAppTurnWithStoredHistoryUsesSameHandle(t *testing.T) {
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.(*session.SQLiteSessionStore).Close()) })
	for i := range 64 {
		require.NoError(t, store.AddSession(t.Context(), session.New(session.WithID(fmt.Sprintf("historical-%d", i)))))
	}
	sess := session.New(session.WithID("active"), session.WithAgentName("root"))
	for i := range 64 {
		sess.AddMessage(session.UserMessage(fmt.Sprintf("committed history %d", i)))
	}
	require.NoError(t, store.AddSession(t.Context(), sess))
	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "reply briefly", agent.WithModel(replyProvider{})))), runtime.WithSessionStore(store))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	a := New(t.Context(), owner.Runtime(), loaded, runtime.SessionBinding{AgentName: "root"}, WithRuntimeServices(rt))
	t.Cleanup(a.Close)
	handle := a.SessionHandle()
	require.NotNil(t, handle)
	events := make(chan any, 128)
	ready := make(chan struct{})
	go a.Subscribe(t.Context(), func(event any) { events <- event }, SubscribeOptions{Ready: ready})
	<-ready
	a.Start(t.Context())
	var priorCancel context.CancelFunc
	defer func() {
		if priorCancel != nil {
			priorCancel()
		}
	}()
	for _, content := range []string{"first live message", "second live message"} {
		if priorCancel != nil {
			priorCancel()
		}
		turnCtx, cancel := context.WithCancel(t.Context())
		priorCancel = cancel
		a.Run(turnCtx, cancel, content, nil)
		timeout := time.NewTimer(5 * time.Second)
		stopped := false
		var answer strings.Builder
		for !stopped {
			select {
			case event := <-events:
				switch event := event.(type) {
				case *runtime.ErrorEvent:
					t.Fatalf("%s failed: %s", content, event.Error)
				case *runtime.AgentChoiceEvent:
					answer.WriteString(event.Content)
				case *runtime.StreamStoppedEvent:
					stopped = event.SessionID == sess.ID
				}
			case <-timeout.C:
				t.Fatalf("%s never stopped", content)
			}
		}
		timeout.Stop()
		assert.Equal(t, "ok", answer.String(), "each turn must deliver its full output")
		assert.Same(t, handle, a.SessionHandle(), "subsequent turns must reuse canonical admission")
	}
	priorCancel()
	snapshot, err := handle.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "second live message", snapshot.GetLastUserMessageContent())
	assert.Equal(t, "ok", snapshot.GetLastAssistantMessageContent())
	assert.GreaterOrEqual(t, snapshot.MessageCount(), 68, "committed history must remain intact")
	replies := 0
	for _, item := range snapshot.ItemsSnapshot() {
		if item.Message != nil && item.Message.Message.Role == chat.MessageRoleAssistant && item.Message.Message.Content == "ok" {
			replies++
		}
	}
	assert.Equal(t, 2, replies, "both assistant responses must be committed")
	for {
		select {
		case event := <-events:
			if failure, ok := event.(*runtime.ErrorEvent); ok {
				t.Fatalf("late admission error: %s", failure.Error)
			}
		default:
			return
		}
	}
}
