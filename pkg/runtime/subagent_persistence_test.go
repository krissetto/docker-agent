package runtime

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
)

// A subagent's transcript must survive a process restart intact. This is the
// full pipeline: spawn → turns run and persist (AddSubSession fires once per
// finished turn, so it must be an idempotent snapshot upsert — a plain insert
// conflicts with the row created by the previous turn or a title update and
// rolls back the transcript) → fresh runtime adopts → attach shows messages.
func TestSubagentTranscriptSurvivesReload(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "s.db")
	newRuntime := func() (*LocalRuntime, session.Store) {
		t.Helper()
		store, err := sqlitestore.New(t.Context(), dbPath)
		require.NoError(t, err)
		t.Cleanup(func() { store.(*session.SQLiteSessionStore).Close() })
		prov := func() *mockProvider {
			return &mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("done!").AddStopWithUsage(1, 1).Build()}
		}
		tm := team.New(team.WithAgents(
			agent.New("root", "prompt", agent.WithModel(prov()), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "planner"})),
			agent.New("planner", "prompt", agent.WithModel(prov())),
		))
		rt, err := NewLocalRuntime(t.Context(), tm, WithSessionStore(store))
		require.NoError(t, err)
		return rt, store
	}

	waitForSettled := func(rt *LocalRuntime, id subagent.NodeID) {
		t.Helper()
		require.Eventually(t, func() bool {
			n, ok := rt.subagents.tree.Node(id)
			return ok && (n.State == subagent.NodeIdle || n.State == subagent.NodeFailed)
		}, 10*time.Second, 10*time.Millisecond)
	}

	// Process A: spawn, run a turn, then a follow-up turn (second persist of
	// the same sub-session).
	rtA, storeA := newRuntime()
	sess := session.New(session.WithID("parent-sess"))
	require.NoError(t, storeA.AddSession(t.Context(), sess))

	id, err := rtA.subagents.Spawn(sess, "root", subagent.AllowedSubagent{Agent: "planner"}, "do the thing")
	require.NoError(t, err)
	waitForSettled(rtA, id)
	require.True(t, deliverMessageForTest(rtA, t.Context(), mustAttachSession(t, rtA, id), "keep going"))
	require.Eventually(t, func() bool {
		rec, ok := rtA.subagents.Read(id)
		return ok && rec.state != subagent.NodeRunning && rtA.sessionDrivers.Settled(rec.sessionID)
	}, 10*time.Second, 10*time.Millisecond)
	rtA.subagents.Close()
	rtA.sessionDrivers.Close()
	storeA.(*session.SQLiteSessionStore).Close()

	// Process B: restore and attach.
	rtB, storeB := newRuntime()
	loaded, err := storeB.GetSession(t.Context(), "parent-sess")
	require.NoError(t, err)
	_, err = rtB.RestoreSubagentTree(t.Context(), loaded)
	require.NoError(t, err)

	info, ok := rtB.SubagentAttachInfo(id)
	require.True(t, ok, "adopted child must be attachable")
	msgs := info.Session.GetAllMessages()
	require.NotEmpty(t, msgs, "transcript must survive reload")
	assert.Equal(t, chat.MessageRoleUser, msgs[0].Message.Role)
	assert.Equal(t, "do the thing", msgs[0].Message.Content)

	var taskCount, followUpCount int
	for _, m := range msgs {
		switch m.Message.Content {
		case "do the thing":
			taskCount++
		case "keep going":
			followUpCount++
		}
	}
	assert.Equal(t, 1, taskCount, "repeat persists must not duplicate messages")
	assert.Equal(t, 1, followUpCount, "the follow-up turn's transcript persisted too")

	// The parent references the sub-session exactly once, no matter how many
	// turns persisted it.
	refs := 0
	for _, item := range loaded.Messages {
		if item.SubSession != nil && item.SubSession.ID == info.Session.ID {
			refs++
		}
	}
	assert.Equal(t, 1, refs, "one sub-session reference in the parent")
}

func TestStoppedSubagentAttachSurvivesReload(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "s.db")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	newRuntime := func(block bool) (*LocalRuntime, session.Store) {
		t.Helper()
		store, err := sqlitestore.New(t.Context(), dbPath)
		require.NoError(t, err)
		t.Cleanup(func() { _ = store.(*session.SQLiteSessionStore).Close() })
		model := func() provider.Provider {
			if block {
				return &activeRootBlockingProvider{id: "test/mock-model", release: release}
			}
			return &mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("done!").AddStopWithUsage(1, 1).Build()}
		}
		rootOpts := []agent.Opt{agent.WithModel(model()), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "planner"})}
		if block {
			rootOpts = append(rootOpts, agent.WithTitleModel(&mockProvider{id: "test/mock-title-model", stream: newStreamBuilder().AddContent("title").AddStopWithUsage(1, 1).Build()}))
		}
		tm := team.New(team.WithAgents(
			agent.New("root", "prompt", rootOpts...),
			agent.New("planner", "prompt", agent.WithModel(model())),
		))
		rt, err := NewLocalRuntime(t.Context(), tm, WithSessionStore(store))
		require.NoError(t, err)
		return rt, store
	}

	rtA, storeA := newRuntime(true)
	sess := session.New(session.WithID("parent-sess"))
	require.NoError(t, storeA.AddSession(t.Context(), sess))

	id, err := rtA.subagents.Spawn(sess, "root", subagent.AllowedSubagent{Agent: "planner"}, "do the thing")
	require.NoError(t, err)
	infoA, ok := rtA.SubagentAttachInfo(id)
	require.True(t, ok)
	require.Eventually(t, func() bool {
		d, ok := rtA.sessionDrivers.Lookup(infoA.Session.ID)
		if !ok {
			return false
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.running
	}, 10*time.Second, 10*time.Millisecond)
	_, err = rtA.subagents.stopChild(sess.ID, id)
	require.NoError(t, err)
	rtA.subagents.Close()
	rtA.sessionDrivers.Close()
	_ = storeA.(*session.SQLiteSessionStore).Close()

	rtB, storeB := newRuntime(false)
	loaded, err := storeB.GetSession(t.Context(), "parent-sess")
	require.NoError(t, err)
	_, err = rtB.RestoreSubagentTree(t.Context(), loaded)
	require.NoError(t, err)

	infoB, ok := rtB.SubagentAttachInfo(id)
	require.True(t, ok, "stopped subagent must remain attachable after reload")
	require.NotNil(t, infoB.Session)
	assert.Equal(t, infoA.Session.ID, infoB.Session.ID)
	msgs := infoB.Session.GetAllMessages()
	require.NotEmpty(t, msgs, "stopped subagent transcript must be persisted")
	assert.Equal(t, chat.MessageRoleUser, msgs[0].Message.Role)
	assert.Equal(t, "do the thing", msgs[0].Message.Content)

	node, ok := rtB.subagents.tree.Node(id)
	require.True(t, ok)
	assert.Equal(t, subagent.NodeStopped, node.State)
	_, err = rtB.subagents.sendToChild(loaded.ID, id, "hi")
	require.Error(t, err)
	_, err = rtB.subagents.stopChild(loaded.ID, id)
	require.Error(t, err)
}

// mustAttachSession resolves a subagent's session id.
func mustAttachSession(t *testing.T, rt *LocalRuntime, id subagent.NodeID) string {
	t.Helper()
	info, ok := rt.SubagentAttachInfo(id)
	require.True(t, ok)
	return info.Session.ID
}
