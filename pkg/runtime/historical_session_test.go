package runtime

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamdef"
)

type historicalReadCounts struct {
	*session.InMemorySessionStore
	mu       sync.Mutex
	full     []string
	own      []string
	metadata []string
}

func (s *historicalReadCounts) GetSession(ctx context.Context, id string) (*session.Session, error) {
	s.mu.Lock()
	s.full = append(s.full, id)
	s.mu.Unlock()
	return s.InMemorySessionStore.GetSession(ctx, id)
}
func (s *historicalReadCounts) GetSessionMetadata(ctx context.Context, id string) (*session.Session, error) {
	s.mu.Lock()
	s.metadata = append(s.metadata, id)
	s.mu.Unlock()
	return s.InMemorySessionStore.GetSessionMetadata(ctx, id)
}
func (s *historicalReadCounts) GetSessionOwnHistory(ctx context.Context, id string) (*session.Session, error) {
	s.mu.Lock()
	s.own = append(s.own, id)
	s.mu.Unlock()
	return s.InMemorySessionStore.GetSessionOwnHistory(ctx, id)
}

func TestHistoricalRemovedChildViewDoesNotRestoreOrHydrateSiblings(t *testing.T) {
	for _, withDefinition := range []bool{true, false} {
		name := "definition-not-recorded"
		if withDefinition {
			name = "known-safe-definition"
		}
		t.Run(name, func(t *testing.T) {
			base := session.NewInMemorySessionStore().(*session.InMemorySessionStore)
			store := &historicalReadCounts{InMemorySessionStore: base}
			const source = "team.yaml"
			root := session.New(session.WithID("root"), session.WithAgentName("root"), session.WithWorkingDir("/workspace"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root", "docker-agent.actor.source": source}))
			require.NoError(t, store.AddSession(t.Context(), root))
			child := session.New(session.WithID("old-child"), session.WithAgentName("worker"), session.WithParentID(root.ID), session.WithWorkingDir(root.WorkingDir), session.WithAttributes(map[string]string{SessionAgentAttribute: "worker", SessionParentAgentAttribute: "root", "docker-agent.actor.source": source}), session.WithUserMessage("recorded child prompt"))
			child.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: "recorded child answer"}))
			child.SetTurnOutcome("finished", string(TurnCompleted))
			nodeID := subagent.NodeID("child-node")
			record := session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: subagent.Node{ID: nodeID, Parent: subagent.SessionRootID(root.ID), SessionID: child.ID, Agent: "worker", Name: "research", State: subagent.NodeStopped}}
			require.NoError(t, store.AdmitChild(t.Context(), session.ChildAdmission{Child: child, Record: record}))
			if withDefinition {
				defs, err := teamdef.NewDefinitionSnapshot("old-team", []teamdef.AgentDefinition{
					{TeamID: "old-team", AgentID: "root-id", AgentKey: "root", Name: "root", Description: "Old root"},
					{TeamID: "old-team", AgentID: "worker-id", AgentKey: "worker", Name: "worker", Description: "Old researcher", Instruction: "Authored instructions", SubagentAccess: &latest.SubagentAccessConfig{ListAgents: latest.SubagentAccessTree, SendMessage: latest.SubagentAccessTree}, Unavailable: []string{"models"}},
				})
				require.NoError(t, err)
				entry, _ := defs.AgentDefinition("worker")
				revision := session.TeamRevision{SessionID: child.ID, TeamID: defs.TeamID, TeamDigest: defs.TeamDigest, AgentDigest: entry.Digest, AgentKey: "worker", AgentID: "worker-id", AgentName: "worker", Origin: source}
				payloads := make([]session.TeamDefinitionPayload, 0, len(defs.Entries()))
				for _, payload := range defs.Entries() {
					payloads = append(payloads, session.TeamDefinitionPayload{Digest: payload.Digest, Kind: payload.Kind, Payload: payload.Payload})
				}
				_, err = store.StoreTeamRevision(t.Context(), revision, payloads)
				require.NoError(t, err)
			}
			treeStore := subagent.NewInMemoryStore()
			rootNode := subagent.Node{ID: subagent.SessionRootID(root.ID), Agent: "root", State: subagent.NodeIdle}
			tree := subagent.Snapshot{Version: subagent.SnapshotVersion, Root: rootNode.ID, Nodes: []subagent.NodeSnapshot{{Node: rootNode, Children: []subagent.NodeSnapshot{{Node: record.Node}}}}}
			require.NoError(t, treeStore.SaveTree(t.Context(), root.ID, tree))
			currentRoot := agent.New("root", "current root", agent.WithModel(coordinationReply("unused")))
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(currentRoot)), WithSessionStore(store), WithSubagentStore(treeStore), WithWorkingDir(root.WorkingDir), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, rt.Close()) })
			preparer := &localSessionRuntimeView{runtime: rt}
			prepared, err := preparer.PrepareSessionView(t.Context(), child.ID)
			require.NoError(t, err)
			committed, err := prepared.Commit(t.Context())
			require.NoError(t, err)
			handle := committed.SessionHandle
			require.True(t, handle.Metadata().Historical)
			require.False(t, handle.Metadata().Capabilities.ExecutionAvailable)
			require.Equal(t, "worker", handle.AgentName())
			require.NotNil(t, committed.Info.Attach)
			require.Equal(t, nodeID, committed.Info.Attach.NodeID)
			snapshot, err := handle.Snapshot(t.Context())
			require.NoError(t, err)
			require.Len(t, snapshot.MessagesSnapshot(), 2)
			require.Equal(t, "recorded child answer", snapshot.GetLastAssistantMessageContent())
			obs, err := handle.Observe(t.Context(), ObserveOptions{})
			require.NoError(t, err)
			require.Len(t, obs.Initial, 1)
			require.Equal(t, "old-child", obs.Primary().Session.ID)
			select {
			case <-obs.Events:
				t.Fatal("historical observation emitted a live event before detach")
			default:
			}
			obs.Cancel()
			select {
			case _, open := <-obs.Events:
				require.False(t, open, "detach closes the terminal historical stream")
			default:
				require.Fail(t, "detach must release the historical observer")
			}

			_, err = handle.Submit(t.Context(), TurnInput{Content: "must not execute"})
			var unsupported *SessionError
			require.ErrorAs(t, err, &unsupported)
			require.Equal(t, SessionErrorHistorical, unsupported.Kind)
			_, err = handle.Cancel(t.Context(), "finished")
			var historic *SessionError
			require.ErrorAs(t, err, &historic)
			require.Equal(t, SessionErrorHistorical, historic.Kind)
			reader := handle.(SessionAgentConfigReader)
			config, err := reader.SessionAgentConfig(t.Context(), "worker")
			require.NoError(t, err)
			require.True(t, config.Historical)
			require.False(t, config.ExecutionAvailable)
			assert.Equal(t, withDefinition, config.DefinitionKnown)
			if withDefinition {
				require.NotNil(t, config.Definition)
				assert.Equal(t, "Authored instructions", config.Definition.Instruction)
				require.NotNil(t, config.Definition.SubagentAccess)
				assert.Equal(t, latest.SubagentAccessTree, config.Definition.SubagentAccess.ListAgents)
				assert.Equal(t, latest.SubagentAccessTree, config.Definition.SubagentAccess.SendMessage)
				assert.Equal(t, "partial_definition: models", config.HistoryReason)
			} else {
				require.Nil(t, config.Definition)
			}
			store.mu.Lock()
			own, full := append([]string(nil), store.own...), append([]string(nil), store.full...)
			store.mu.Unlock()
			require.Equal(t, []string{child.ID}, own, "only selected child history is loaded")
			require.Empty(t, full, "no root or sibling transcript hydration")
			_, resident := rt.sessionDrivers.Lookup(child.ID)
			assert.False(t, resident, "historical display never publishes execution driver")
		})
	}
}
