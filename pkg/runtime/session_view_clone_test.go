package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
)

func sessionViewCloneFixture() *session.Session {
	sess := session.New(session.WithID("child"), session.WithParentID("root"), session.WithTitle("original"), session.WithAttributes(map[string]string{SessionAgentAttribute: "worker", SessionParentAgentAttribute: "root"}))
	sess.AgentName = "worker"
	msg := session.UserMessage("original message")
	msg.Message.ToolDefinitions = []tools.Tool{{Name: "read", Parameters: map[string]any{"properties": map[string]any{"path": map[string]any{"type": "string"}}}}}
	sess.AddMessage(msg)
	return sess
}

func TestSessionViewCommittedCloneIsolation(t *testing.T) {
	for _, resident := range []bool{false, true} {
		name := "publication"
		if resident {
			name = "resident"
		}
		t.Run(name, func(t *testing.T) {
			store := session.NewInMemorySessionStore()
			root := session.New(session.WithID("root"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
			child := sessionViewCloneFixture()
			require.NoError(t, store.AddSession(t.Context(), root))
			require.NoError(t, store.AddSession(t.Context(), child))
			rt, owner := coordinationRuntime(t, store, coordinationReply("root"), coordinationReply("child"))
			rootNode := subagent.SessionRootID(root.ID)
			tree := subagent.Snapshot{Version: subagent.SnapshotVersion, Root: rootNode, Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: rootNode, Agent: "root"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child-node", SessionID: child.ID, Parent: rootNode, Agent: "worker", State: subagent.NodeIdle}}}}}}
			require.NoError(t, rt.subagentStore.SaveTree(t.Context(), root.ID, tree))
			preparer := owner.Runtime().(SessionViewPreparer)
			if resident {
				warmup, err := preparer.PrepareSessionView(t.Context(), child.ID)
				require.NoError(t, err)
				_, err = warmup.Commit(t.Context())
				require.NoError(t, err)
			}
			prepared, err := preparer.PrepareSessionView(t.Context(), child.ID)
			require.NoError(t, err)
			p := prepared.(*preparedSessionView)
			assert.Equal(t, resident, len(p.reservations) == 0)
			before := prepared.Info()
			result, err := prepared.Commit(t.Context())
			require.NoError(t, err)
			require.NotNil(t, result.Info.Attach)
			committedBefore := cloneSessionViewInfo(result.Info)
			assert.NotSame(t, p.info.Attach, result.Info.Attach)
			assert.NotSame(t, result.Info.Session, result.Info.Attach.Session)
			driver := result.SessionHandle.(*sessionHandle).driver
			canonical := driver.session().OwnSnapshot()
			assert.Equal(t, canonical, result.Info.Session.OwnSnapshot())

			mutate := func(sess *session.Session, value string) {
				sess.SetTitle(value)
				sess.SetAttribute("mutation", value)
				sess.Messages[0].Message.Message.Content = value
				params := sess.Messages[0].Message.Message.ToolDefinitions[0].Parameters.(map[string]any)
				params["properties"].(map[string]any)["path"].(map[string]any)["type"] = value
			}
			mutate(result.Info.Session, "session mutation")
			assert.Equal(t, canonical, result.Info.Attach.Session.OwnSnapshot())
			mutate(result.Info.Attach.Session, "attach mutation")
			assert.Equal(t, "session mutation", result.Info.Session.TitleSnapshot())
			result.Info.Attach.Name = "changed name"
			result.Info.Attach.Agent = "changed agent"
			assert.Equal(t, before, prepared.Info(), "caller mutations must not reach prepared metadata or either prepared session")
			assert.Equal(t, canonical, driver.session().OwnSnapshot(), "caller mutations must not reach the canonical session")
			again, err := prepared.Commit(t.Context())
			require.NoError(t, err)
			assert.Same(t, result.SessionHandle, again.SessionHandle)
			assert.Equal(t, committedBefore, again.Info, "first result mutations must not reach the cached committed result")
			mutate(again.Info.Session, "second result mutation")
			mutate(again.Info.Attach.Session, "second attach mutation")
			again.Info.Attach.Name = "second name"
			third, err := prepared.Commit(t.Context())
			require.NoError(t, err)
			assert.Equal(t, committedBefore, third.Info, "repeat results must also be detached from the cache")
		})
	}
}

// legacySessionViewCanonicalResult preserves the old clone-then-overwrite shape
// solely for an allocation comparison with canonicalResult.
func legacySessionViewCanonicalResult(p *preparedSessionView) (CommittedSessionView, error) {
	handle, err := p.r.SessionByID(p.info.SessionID)
	if err != nil {
		return CommittedSessionView{}, err
	}
	driver := handle.(*sessionHandle).driver
	driver.mu.Lock()
	info := cloneSessionViewInfo(p.info)
	info.Session = driver.sess.Clone()
	if tree, ok := snapshotForRoot(p.r.subagents.tree.Snapshot(), subagent.SessionRootID(p.root.ID)); ok && info.SessionID == p.root.ID {
		info.Session.SetSubagentTree(&tree)
	}
	info.Binding.AgentName, info.Binding.Model = driver.AgentNameLocked(), driver.modelRef
	info.WorkingDir = driver.sess.WorkingDir
	driver.mu.Unlock()
	if info.Attach != nil {
		info.Attach.Session = info.Session.Clone()
		info.Attach.Agent = info.Binding.AgentName
	}
	return CommittedSessionView{SessionHandle: handle, Info: info}, nil
}

func BenchmarkSessionViewCanonicalResult(b *testing.B) {
	for _, attached := range []bool{false, true} {
		name := "root"
		if attached {
			name = "attached"
		}
		b.Run(name, func(b *testing.B) {
			sess := sessionViewCloneFixture()
			for range 63 {
				sess.AddMessage(sess.Messages[0].Message)
			}
			rt := &LocalRuntime{subagents: &subagentManager{tree: subagent.NewTree()}}
			rt.sessionDrivers = newSessionDriverRegistry(rt)
			rt.sessionDrivers.drivers[sess.ID] = &sessionDriver{sess: sess}
			p := &preparedSessionView{r: rt, root: sess, info: PreparedSessionViewInfo{SessionID: sess.ID, Session: sess.Clone()}}
			if attached {
				p.root = session.New(session.WithID("root"))
				p.info.Attach = &SubagentAttachInfo{Session: sess.Clone(), Agent: "worker"}
			}
			for _, impl := range []struct {
				name string
				run  func(*preparedSessionView) (CommittedSessionView, error)
			}{{"legacy", legacySessionViewCanonicalResult}, {"optimized", (*preparedSessionView).canonicalResult}, {"optimized_defensive_return", func(p *preparedSessionView) (CommittedSessionView, error) {
				result, err := p.canonicalResult()
				if err == nil {
					result.Info = cloneSessionViewInfo(result.Info)
				}
				return result, err
			}}} {
				b.Run(impl.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						result, err := impl.run(p)
						if err != nil || result.Info.Session == nil {
							b.Fatal("missing canonical result", err)
						}
					}
				})
			}
		})
	}
}
