package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestListAgentsToolExposure(t *testing.T) {
	t.Parallel()

	var list tools.Tool
	for _, definition := range subagent.Definitions() {
		if definition.Name == subagent.ToolListAgents {
			list = definition
		}
	}
	require.Equal(t, subagent.ToolListAgents, list.Name)
	assert.True(t, list.Annotations.ReadOnlyHint)
	assert.Equal(t, "subagent", list.Category)
	schema, err := tools.SchemaToMap(list.Parameters)
	require.NoError(t, err)
	assert.Equal(t, "object", schema["type"])

	standalone := session.New()
	assert.Empty(t, addListAgentsTool(standalone, agent.New("solo", ""), nil), "standalone agents do not receive team discovery tools")

	configured := agent.New("root", "", agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"}))
	assert.Contains(t, listedToolNames(addListAgentsTool(session.New(), configured, nil)), subagent.ToolListAgents)

	leaf := session.New(session.WithAsyncSubagent(true))
	childTools := addListAgentsTool(leaf, agent.New("worker", ""), addAsyncChildTools(leaf, nil))
	assert.Contains(t, listedToolNames(childTools), subagent.ToolSendMessage)
	assert.Contains(t, listedToolNames(childTools), subagent.ToolListAgents)

	withExistingSend := addListAgentsTool(leaf, agent.New("worker", ""), []tools.Tool{{Name: subagent.ToolSendMessage}})
	assert.Contains(t, listedToolNames(withExistingSend), subagent.ToolListAgents, "existing child communication tool does not suppress discovery")
}

func TestHandleListAgentsShowsOnlyBoundedExistingInstances(t *testing.T) {
	t.Parallel()

	rootAgent := agent.New("root", "private root prompt", agent.WithDescription("Coordinates work"),
		agent.WithModel(coordinationReply("unused")),
		agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker", Name: "research", Description: "Research role"}), agent.WithToolSets(subagent.NewToolSet()))
	worker := agent.New("worker", "private worker prompt", agent.WithDescription("Worker description"),
		agent.WithModel(coordinationReply("unused")))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(rootAgent, worker)),
		WithSessionStore(session.NewInMemorySessionStore()), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	require.Contains(t, rt.toolMap, subagent.ToolListAgents, "normal local runtime execution dispatches list_agents")

	root := session.New(session.WithID("list-root"), session.WithAgentName("root"))
	rootBeforeChildren := callListAgents(t, rt, root, `{}`)
	assert.Equal(t, "root", rootBeforeChildren.CurrentAgent)
	assert.Nil(t, rootBeforeChildren.Parent)
	assert.Empty(t, rootBeforeChildren.Children)
	initialJSON := callListAgentsOutput(t, rt, root, `{}`)
	assert.NotContains(t, initialJSON, "configured_agents")
	assert.NotContains(t, initialJSON, "spawn_choices")
	assert.NotContains(t, initialJSON, "worker")

	rt.subagents.ensureRoot(root, "root")
	rootID := subagent.SessionRootID(root.ID)
	nodes := make([]subagent.Node, 0, 123)
	for i := range 123 {
		id := subagent.NodeID(fmt.Sprintf("%05x", i+1))
		nodes = append(nodes, subagent.Node{ID: id, Parent: rootID, Agent: "worker", Name: "research", Description: "Research role", State: subagent.NodeRunning, SessionID: fmt.Sprintf("private-session-%03d", i)})
	}
	require.NoError(t, rt.subagents.tree.AddSubtree(nodes))
	rt.subagents.mu.Lock()
	for _, node := range nodes {
		rt.subagents.children[node.ID] = &childRecord{
			name: node.DisplayName(), parentSession: root.ID, sessionID: node.SessionID,
			durable: session.ChildRecord{Node: node},
		}
	}
	rt.subagents.mu.Unlock()

	firstPage := callListAgents(t, rt, root, `{}`)
	require.Len(t, firstPage.Children, defaultListAgentsLimit)
	assert.Equal(t, "00001", firstPage.Children[0].ID)
	assert.Equal(t, "0000a", firstPage.Children[9].ID)
	assert.Equal(t, firstPage.Children[9].ID, firstPage.NextCursor)
	assert.Equal(t, "worker", firstPage.Children[0].Agent)
	assert.Equal(t, "research", firstPage.Children[0].Name)
	assert.NotContains(t, callListAgentsOutput(t, rt, root, `{}`), "Research role")
	assert.Equal(t, subagent.NodeRunning, firstPage.Children[0].State)

	secondPageArgs := `{"cursor":"` + firstPage.NextCursor + `"}`
	secondPage := callListAgents(t, rt, root, secondPageArgs)
	require.Len(t, secondPage.Children, defaultListAgentsLimit)
	assert.Equal(t, "0000b", secondPage.Children[0].ID)
	assert.NotEqual(t, firstPage.Children[0].ID, secondPage.Children[0].ID)
	assert.Equal(t, secondPage.Children[9].ID, secondPage.NextCursor)

	maxPage := callListAgents(t, rt, root, `{"limit":500}`)
	require.Len(t, maxPage.Children, maxListAgentsLimit)
	assert.Equal(t, maxPage.Children[49].ID, maxPage.NextCursor)
	lastPage := callListAgents(t, rt, root, `{"limit":50,"cursor":"`+maxPage.NextCursor+`"}`)
	require.Len(t, lastPage.Children, maxListAgentsLimit)
	assert.Equal(t, lastPage.Children[len(lastPage.Children)-1].ID, lastPage.NextCursor)
	finalPage := callListAgents(t, rt, root, `{"limit":50,"cursor":"`+lastPage.NextCursor+`"}`)
	require.Len(t, finalPage.Children, len(nodes)-maxListAgentsLimit*2)
	assert.Empty(t, finalPage.NextCursor)

	invalidCursor, err := rt.handleListAgents(t.Context(), root, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"cursor":"not-a-child"}`}}, nil, tools.NopRuntime{})
	require.NoError(t, err)
	assert.True(t, invalidCursor.IsError)
	longCursor, err := rt.handleListAgents(t.Context(), root, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"cursor":"` + strings.Repeat("x", maxListAgentsCursorLen+1) + `"}`}}, nil, tools.NopRuntime{})
	require.NoError(t, err)
	assert.True(t, longCursor.IsError)
	negativeLimit, err := rt.handleListAgents(t.Context(), root, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"limit":-1}`}}, nil, tools.NopRuntime{})
	require.NoError(t, err)
	assert.True(t, negativeLimit.IsError)

	firstOutput := callListAgentsOutput(t, rt, root, `{}`)
	assert.Equal(t, firstOutput, callListAgentsOutput(t, rt, root, `{}`), "same topology produces deterministic bounded output")
	assert.NotContains(t, firstOutput, "private root prompt")
	assert.NotContains(t, firstOutput, "private worker prompt")
	assert.NotContains(t, firstOutput, "private-session")
	assert.NotContains(t, firstOutput, "configured_agents")
	assert.NotContains(t, firstOutput, "spawn_choices")

	leafNode := subagent.Node{ID: "fffff", Parent: nodes[0].ID, Agent: "worker", Name: "helper", State: subagent.NodeIdle, SessionID: "leaf-session"}
	require.NoError(t, rt.subagents.tree.Add(leafNode))
	rt.subagents.mu.Lock()
	rt.subagents.children[leafNode.ID] = &childRecord{
		name: leafNode.DisplayName(), parentSession: nodes[0].SessionID, sessionID: leafNode.SessionID,
		durable: session.ChildRecord{Node: leafNode},
	}
	rt.subagents.mu.Unlock()
	leaf := session.New(session.WithID("leaf-session"), session.WithAgentName("worker"), session.WithAsyncSubagent(true))
	childView := callListAgents(t, rt, leaf, `{}`)
	assert.Equal(t, "worker", childView.CurrentAgent)
	require.NotNil(t, childView.Parent)
	assert.Equal(t, string(nodes[0].ID), childView.Parent.ID)
	assert.Equal(t, "worker", childView.Parent.Agent)
	assert.Equal(t, "research", childView.Parent.Name)
	assert.Empty(t, childView.Children)
	assert.NotContains(t, callListAgentsOutput(t, rt, leaf, `{}`), "leaf-session")
}

func callListAgents(t *testing.T, rt *LocalRuntime, sess *session.Session, args string) listedAgents {
	t.Helper()
	result, err := rt.handleListAgents(t.Context(), sess, tools.ToolCall{Function: tools.FunctionCall{Arguments: args}}, nil, tools.NopRuntime{})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.IsError)
	var output listedAgents
	require.NoError(t, json.Unmarshal([]byte(result.Output), &output))
	return output
}

func callListAgentsOutput(t *testing.T, rt *LocalRuntime, sess *session.Session, args string) string {
	t.Helper()
	result, err := rt.handleListAgents(t.Context(), sess, tools.ToolCall{Function: tools.FunctionCall{Arguments: args}}, nil, tools.NopRuntime{})
	require.NoError(t, err)
	require.NotNil(t, result)
	return strings.TrimSpace(result.Output)
}

func listedToolNames(toolDefs []tools.Tool) []string {
	names := make([]string, 0, len(toolDefs))
	for _, definition := range toolDefs {
		names = append(names, definition.Name)
	}
	return names
}

func TestListAgentsTreeScopeIncludesWholeRuntimeTreeAndStableSelfParent(t *testing.T) {
	t.Parallel()
	rootAgent := agent.New("root", "", agent.WithModel(coordinationReply("unused")))
	worker := agent.New("worker", "", agent.WithModel(coordinationReply("unused")))
	teamConfigs := map[string]latest.AgentConfig{
		"root":   {Name: "root", SubagentAccess: &latest.SubagentAccessConfig{ListAgents: latest.SubagentAccessTree, SendMessage: latest.SubagentAccessTree}},
		"worker": {Name: "worker", SubagentAccess: &latest.SubagentAccessConfig{ListAgents: latest.SubagentAccessTree, SendMessage: latest.SubagentAccessTree}},
	}
	tm := team.New(team.WithAgents(rootAgent, worker), team.WithAgentConfigs(teamConfigs))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionStore(session.NewInMemorySessionStore()), WithSubagentStore(subagent.NewInMemoryStore()), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	root := session.New(session.WithID("tree-root"), session.WithAgentName("root"))
	rt.subagents.ensureRoot(root, "root")
	rootNode := subagent.SessionRootID(root.ID)
	child := subagent.Node{ID: "a0001", Parent: rootNode, Agent: "worker", Name: "worker-one", State: subagent.NodeIdle, SessionID: "tree-child"}
	grandchild := subagent.Node{ID: "a0002", Parent: child.ID, Agent: "worker", Name: "worker-two", State: subagent.NodeStopped, SessionID: "tree-grandchild"}
	require.NoError(t, rt.subagents.tree.AddSubtree([]subagent.Node{child, grandchild}))
	rt.subagents.mu.Lock()
	rt.subagents.children[child.ID] = &childRecord{name: child.DisplayName(), parentSession: root.ID, sessionID: child.SessionID, durable: session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: child, Revision: 1}}
	rt.subagents.children[grandchild.ID] = &childRecord{name: grandchild.DisplayName(), parentSession: child.SessionID, sessionID: grandchild.SessionID, durable: session.ChildRecord{RootSessionID: root.ID, ParentSessionID: child.SessionID, Node: grandchild, Revision: 1}}
	rt.subagents.mu.Unlock()
	rootPage := callListAgents(t, rt, root, `{}`)
	assert.Equal(t, "tree", rootPage.Scope)
	require.NotNil(t, rootPage.Self)
	assert.Equal(t, string(rootNode), rootPage.Self.ID)
	assert.Len(t, rootPage.Agents, 3)
	byID := map[string]listedLiveAgent{}
	for _, item := range rootPage.Agents {
		byID[item.ID] = item
	}
	require.NotNil(t, byID["a0001"].Messageable)
	assert.True(t, *byID["a0001"].Messageable)
	require.NotNil(t, byID["a0002"].Messageable)
	assert.False(t, *byID["a0002"].Messageable, "stopped nodes are never messageable")
	assert.Len(t, rootPage.Children, 1)
	assert.Equal(t, "a0001", rootPage.Children[0].ID)
	firstPage := callListAgents(t, rt, root, `{"limit":2}`)
	assert.Len(t, firstPage.Agents, 2)
	assert.Equal(t, "a0002", firstPage.NextCursor)
	secondPage := callListAgents(t, rt, root, `{"limit":2,"cursor":"`+firstPage.NextCursor+`"}`)
	assert.Len(t, secondPage.Agents, 1)
	assert.Equal(t, string(rootNode), secondPage.Agents[0].ID)
	assert.Empty(t, secondPage.NextCursor)
	childSession := session.New(session.WithID(child.SessionID), session.WithParentID(root.ID), session.WithAgentName("worker"), session.WithAsyncSubagent(true))
	childPage := callListAgents(t, rt, childSession, `{}`)
	require.NotNil(t, childPage.Self)
	assert.Equal(t, "a0001", childPage.Self.ID)
	require.NotNil(t, childPage.Parent)
	assert.Equal(t, string(rootNode), childPage.Parent.ID)
	badRootCursor, err := rt.handleListAgents(t.Context(), root, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"cursor":"root:other-root"}`}}, nil, tools.NopRuntime{})
	require.NoError(t, err)
	assert.True(t, badRootCursor.IsError, "a different root id is never a valid cursor")
}

func TestConfiguredLeafAgentExposesOnlyGrantedTools(t *testing.T) {
	access := &latest.SubagentAccessConfig{ListAgents: latest.SubagentAccessTree, SendMessage: latest.SubagentAccessTree}
	rootAgent := agent.New("root", "", agent.WithModel(coordinationReply("unused")), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "leaf"}))
	leafAgent := agent.New("leaf", "", agent.WithModel(coordinationReply("unused")))
	tm := team.New(team.WithAgents(rootAgent, leafAgent), team.WithAgentConfigs(map[string]latest.AgentConfig{
		"root": {Name: "root"}, "leaf": {Name: "leaf", SubagentAccess: access},
	}))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionStore(session.NewInMemorySessionStore()), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	parent := session.New(session.WithID("host-parent"), session.WithAgentName("root"))
	parentHandle, err := rt.CreateSession(t.Context(), parent, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	child := session.New(session.WithID("host-leaf"), session.WithAgentName("leaf"))
	rt.subagents.registerChild(parent, "root", "abcde", "leaf", child)
	handle, err := rt.SessionByID(child.ID)
	require.NoError(t, err)
	executable, err := handle.(SessionExecutableToolProvider).SessionExecutableTools(t.Context())
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{subagent.ToolListAgents, subagent.ToolSendMessage}, listedToolNames(executable), "hosted leaf catalog exposes grants only, not spawn/read/stop")
	toolByName := map[string]tools.Tool{}
	for _, tool := range executable {
		toolByName[tool.Name] = tool
	}
	listResult, err := toolByName[subagent.ToolListAgents].Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: subagent.ToolListAgents, Arguments: `{}`}}, tools.NopRuntime{})
	require.NoError(t, err)
	assert.False(t, listResult.IsError, listResult.Output)
	sendResult, err := toolByName[subagent.ToolSendMessage].Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: subagent.ToolSendMessage, Arguments: `{"to":"parent","message":"hosted-send"}`}}, tools.NopRuntime{})
	require.NoError(t, err)
	assert.False(t, sendResult.IsError, sendResult.Output)
	for _, mismatched := range []string{subagent.ToolSendMessage, subagent.ToolStopSubagent, subagent.ToolReadSubagent, "unregistered_tool"} {
		result, callErr := toolByName[subagent.ToolListAgents].Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: mismatched, Arguments: `{"subagent_id":"abcde"}`}}, tools.NopRuntime{})
		require.NoError(t, callErr)
		require.NotNil(t, result)
		assert.True(t, result.IsError, "descriptor for list_agents cannot dispatch %q", mismatched)
	}
	assert.Contains(t, rt.toolMap, subagent.ToolListAgents)
	assert.Contains(t, rt.toolMap, subagent.ToolSendMessage)
	assert.NotNil(t, parentHandle)
}

func TestSubagentAccessGrantsAreIndependent(t *testing.T) {
	tests := []struct {
		name   string
		access *latest.SubagentAccessConfig
		want   []string
	}{
		{name: "absent"},
		{name: "list only", access: &latest.SubagentAccessConfig{ListAgents: latest.SubagentAccessTree}, want: []string{subagent.ToolListAgents}},
		{name: "send only", access: &latest.SubagentAccessConfig{SendMessage: latest.SubagentAccessTree}, want: []string{subagent.ToolSendMessage}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			leaf := agent.New("leaf", "", agent.WithModel(coordinationReply("unused")))
			configs := map[string]latest.AgentConfig{}
			if tt.access != nil {
				configs["leaf"] = latest.AgentConfig{Name: "leaf", SubagentAccess: tt.access}
			}
			tm := team.New(team.WithAgents(leaf), team.WithAgentConfigs(configs))
			rt, err := NewLocalRuntime(t.Context(), tm, WithSessionStore(session.NewInMemorySessionStore()), WithModelStore(mockModelStore{}))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, rt.Close()) })
			handle, err := rt.CreateSession(t.Context(), session.New(session.WithID("independent-"+tt.name), session.WithAgentName("leaf")), SessionBinding{AgentName: "leaf"})
			require.NoError(t, err)
			executable, err := handle.(SessionExecutableToolProvider).SessionExecutableTools(t.Context())
			require.NoError(t, err)
			assert.ElementsMatch(t, tt.want, listedToolNames(executable))
		})
	}
}

func TestListOnlyGrantDoesNotAuthorizeSiblingSend(t *testing.T) {
	m := newTestSubagentManager(t)
	root := session.New(session.WithID("list-only-root"))
	sender := session.New(session.WithID("list-only-sender"), session.WithAgentName("worker"))
	target := session.New(session.WithID("list-only-target"), session.WithAgentName("worker"))
	m.registerChild(root, "root", "e0001", "worker", sender)
	m.registerChild(root, "root", "e0002", "worker", target)
	m.mu.Lock()
	m.children["e0001"].durable = session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: subagent.Node{ID: "e0001", Agent: "worker", Parent: subagent.SessionRootID(root.ID), SessionID: sender.ID, State: subagent.NodeIdle}}
	m.children["e0002"].durable = session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: subagent.Node{ID: "e0002", Agent: "worker", Parent: subagent.SessionRootID(root.ID), SessionID: target.ID, State: subagent.NodeIdle}}
	m.mu.Unlock()
	m.r.team = team.New(team.WithAgents(agent.New("root", "root"), agent.New("worker", "worker")), team.WithAgentConfigs(map[string]latest.AgentConfig{"worker": {Name: "worker", SubagentAccess: &latest.SubagentAccessConfig{ListAgents: latest.SubagentAccessTree}}}))
	m.r.agents = newAgentRouter(m.r.team, "root")
	result, err := m.r.handleSendMessage(t.Context(), sender, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"to":"e0002","message":"denied"}`}}, nil, tools.NopRuntime{})
	require.NoError(t, err)
	require.True(t, result.IsError)
}

func TestListAgentsUsesSessionPinnedAccessRevision(t *testing.T) {
	oldAccess := &latest.SubagentAccessConfig{ListAgents: latest.SubagentAccessTree}
	oldTeam := team.New(team.WithAgents(agent.New("root", "old", agent.WithModel(coordinationReply("unused")))), team.WithAgentConfigs(map[string]latest.AgentConfig{"root": {Name: "root", SubagentAccess: oldAccess}}))
	newTeam := team.New(team.WithAgents(agent.New("root", "new", agent.WithModel(coordinationReply("unused")))), team.WithAgentConfigs(map[string]latest.AgentConfig{"root": {Name: "root", SubagentAccess: &latest.SubagentAccessConfig{ListAgents: latest.SubagentAccessChildren}}}))
	oldEnv, err := NewTeamEnvironment("list-access-r1", "source", "baseline", "team", oldTeam, ModelSwitcherConfig{}, func(context.Context) error { return nil })
	require.NoError(t, err)
	store, err := NewTeamEnvironmentStore(oldEnv)
	require.NoError(t, err)
	rt, err := NewLocalRuntime(t.Context(), oldTeam, WithTeamEnvironmentStore(store), WithSessionStore(session.NewInMemorySessionStore()), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	owner := NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	handle, err := owner.Runtime().CreateSession(t.Context(), session.New(session.WithID("pinned-access")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	sess, err := handle.Snapshot(t.Context())
	require.NoError(t, err)
	nextEnv, err := NewTeamEnvironment("list-access-r2", "source", "baseline", "team", newTeam, ModelSwitcherConfig{}, func(context.Context) error { return nil })
	require.NoError(t, err)
	require.NoError(t, store.Publish("source", "list-access-r1", nextEnv))
	ctx, release, err := rt.captureEnvironmentForOperation(t.Context(), sess)
	require.NoError(t, err)
	defer release()
	assert.Same(t, oldTeam, rt.teamForContext(ctx, sess))
	page := callListAgents(t, rt, sess, `{}`)
	assert.Equal(t, "tree", page.Scope, "a published team edit cannot mutate the existing session's captured grant")
	oldAccess.ListAgents = latest.SubagentAccessChildren
	page = callListAgents(t, rt, sess, `{}`)
	assert.Equal(t, "tree", page.Scope, "caller mutation of the original pointer cannot widen/narrow captured session access")
}

func TestSendMessageGrantUsesSessionPinnedEnvironmentRevision(t *testing.T) {
	rootAgent := agent.New("root", "", agent.WithModel(coordinationReply("unused")))
	workerAgent := agent.New("worker", "", agent.WithModel(coordinationReply("unused")))
	oldTeam := team.New(team.WithAgents(rootAgent, workerAgent), team.WithAgentConfigs(map[string]latest.AgentConfig{
		"root":   {Name: "root"},
		"worker": {Name: "worker", SubagentAccess: &latest.SubagentAccessConfig{SendMessage: latest.SubagentAccessChildren}},
	}))
	newRoot := agent.New("root", "", agent.WithModel(coordinationReply("unused")))
	newWorker := agent.New("worker", "", agent.WithModel(coordinationReply("unused")))
	newTeam := team.New(team.WithAgents(newRoot, newWorker), team.WithAgentConfigs(map[string]latest.AgentConfig{
		"root":   {Name: "root"},
		"worker": {Name: "worker", SubagentAccess: &latest.SubagentAccessConfig{SendMessage: latest.SubagentAccessTree}},
	}))
	oldEnv, err := NewTeamEnvironment("send-access-r1", "source", "baseline", "team", oldTeam, ModelSwitcherConfig{}, func(context.Context) error { return nil })
	require.NoError(t, err)
	environments, err := NewTeamEnvironmentStore(oldEnv)
	require.NoError(t, err)
	rt, err := NewLocalRuntime(t.Context(), oldTeam, WithTeamEnvironmentStore(environments), WithSessionStore(session.NewInMemorySessionStore()), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	owner := NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	root := session.New(session.WithID("send-revision-root"), session.WithAgentName("root"))
	_, err = owner.Runtime().CreateSession(t.Context(), root, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	sender := session.New(session.WithID("send-revision-sender"), session.WithAgentName("worker"))
	target := session.New(session.WithID("send-revision-target"), session.WithAgentName("worker"))
	rt.subagents.registerChild(root, "root", "f0001", "worker", sender)
	rt.subagents.registerChild(root, "root", "f0002", "worker", target)
	rt.subagents.mu.Lock()
	rt.subagents.children["f0001"].durable = session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: subagent.Node{ID: "f0001", Agent: "worker", Parent: subagent.SessionRootID(root.ID), SessionID: sender.ID, State: subagent.NodeIdle}}
	rt.subagents.children["f0002"].durable = session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: subagent.Node{ID: "f0002", Agent: "worker", Parent: subagent.SessionRootID(root.ID), SessionID: target.ID, State: subagent.NodeIdle}}
	rt.subagents.mu.Unlock()
	senderHandle, err := rt.SessionByID(sender.ID)
	require.NoError(t, err)
	senderDriver := senderHandle.(*sessionHandle).driver
	oldContext, releaseOld, err := senderDriver.acquireOperationEnvironment(t.Context())
	require.NoError(t, err)
	defer releaseOld()
	assert.Same(t, oldTeam, rt.teamForContext(oldContext, sender))
	newEnv, err := NewTeamEnvironment("send-access-r2", "source", "baseline", "team", newTeam, ModelSwitcherConfig{}, func(context.Context) error { return nil })
	require.NoError(t, err)
	require.NoError(t, environments.Publish("source", "send-access-r1", newEnv))
	call := tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"to":"f0002","message":"peer"}`}}
	oldResult, err := rt.handleSendMessage(oldContext, sender, call, nil, tools.NopRuntime{})
	require.NoError(t, err)
	require.True(t, oldResult.IsError, "captured children-only revision cannot message a peer after a newer tree grant publishes")
	require.NoError(t, senderDriver.pinCurrentTeamEnvironment(t.Context()), "next execution boundary adopts the newly published policy")
	newContext, releaseNew, err := senderDriver.acquireOperationEnvironment(t.Context())
	require.NoError(t, err)
	defer releaseNew()
	assert.Same(t, newTeam, rt.teamForContext(newContext, sender))
	newResult, err := rt.handleSendMessage(newContext, sender, call, nil, tools.NopRuntime{})
	require.NoError(t, err)
	require.False(t, newResult.IsError, newResult.Output, "adopted tree-grant revision can message same-root peer")
}

func TestListAgentsTitlesCurrentLiveAndPersistedWithoutTranscriptLoads(t *testing.T) {
	ctx := t.Context()
	storeBase := session.NewInMemorySessionStore().(*session.InMemorySessionStore)
	reads := &historicalReadCounts{InMemorySessionStore: storeBase}
	rootAgent := agent.New("root", "", agent.WithModel(coordinationReply("unused")))
	worker := agent.New("worker", "", agent.WithModel(coordinationReply("unused")))
	tm := team.New(team.WithAgents(rootAgent, worker), team.WithAgentConfigs(map[string]latest.AgentConfig{
		"root":   {Name: "root", SubagentAccess: &latest.SubagentAccessConfig{ListAgents: latest.SubagentAccessTree}},
		"worker": {Name: "worker", SubagentAccess: &latest.SubagentAccessConfig{ListAgents: latest.SubagentAccessTree}},
	}))
	rt, err := NewLocalRuntime(ctx, tm, WithSessionStore(reads), WithSubagentStore(subagent.NewInMemoryStore()), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })

	root := session.New(session.WithID("titles-root"), session.WithAgentName("root"), session.WithTitle("Persisted root"))
	current := session.New(session.WithID("titles-child"), session.WithParentID(root.ID), session.WithAgentName("worker"), session.WithAsyncSubagent(true), session.WithTitle("Live child"))
	persisted := session.New(session.WithID("titles-grandchild"), session.WithParentID(current.ID), session.WithAgentName("worker"), session.WithTitle("Persisted grandchild"))
	rootNode := subagent.Node{ID: subagent.SessionRootID(root.ID), Agent: "root", SessionID: root.ID, State: subagent.NodeIdle}
	childNode := subagent.Node{ID: "b0001", Parent: rootNode.ID, Agent: "worker", Name: "worker-child", SessionID: current.ID, State: subagent.NodeIdle, Description: "do not leak role"}
	grandNode := subagent.Node{ID: "b0002", Parent: childNode.ID, Agent: "worker", Name: "worker-grandchild", SessionID: persisted.ID, State: subagent.NodeIdle, Description: "do not leak role"}
	require.NoError(t, reads.AddSession(ctx, root))
	require.NoError(t, reads.AdmitChild(ctx, session.ChildAdmission{Child: current, Record: session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: childNode}}))
	require.NoError(t, reads.AdmitChild(ctx, session.ChildAdmission{Child: persisted, Record: session.ChildRecord{RootSessionID: root.ID, ParentSessionID: current.ID, Node: grandNode}}))
	require.NoError(t, rt.subagents.tree.AddSubtree([]subagent.Node{rootNode, childNode, grandNode}))
	liveRoot := session.New(session.WithID(root.ID), session.WithAgentName("root"), session.WithTitle("Live root"))
	rt.sessionDrivers.Get(liveRoot)
	rt.subagents.mu.Lock()
	rt.subagents.children[childNode.ID] = &childRecord{name: childNode.DisplayName(), parentSession: root.ID, sessionID: current.ID, durable: session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: childNode}}
	rt.subagents.children[grandNode.ID] = &childRecord{name: grandNode.DisplayName(), parentSession: current.ID, sessionID: persisted.ID, durable: session.ChildRecord{RootSessionID: root.ID, ParentSessionID: current.ID, Node: grandNode}}
	rt.subagents.mu.Unlock()
	// Keep only the caller resident. The parent and grandchild titles must be read from metadata.
	rt.sessionDrivers.Get(current)
	page := callListAgents(t, rt, current, `{}`)
	require.NotNil(t, page.Self)
	assert.Equal(t, "Live child", page.Self.SessionTitle)
	require.NotNil(t, page.Parent)
	assert.Equal(t, "Live root", page.Parent.SessionTitle)
	assert.Len(t, page.Agents, 3)
	byID := map[string]listedLiveAgent{}
	for _, item := range page.Agents {
		byID[item.ID] = item
	}
	assert.Equal(t, "Live child", byID["b0001"].SessionTitle)
	assert.Equal(t, "Live root", byID[string(rootNode.ID)].SessionTitle, "resident root title wins over stale persisted metadata")
	assert.Equal(t, "Persisted grandchild", byID["b0002"].SessionTitle)
	output := callListAgentsOutput(t, rt, current, `{}`)
	assert.NotContains(t, output, "do not leak role")
	assert.NotContains(t, output, "titles-child\"")
	reads.mu.Lock()
	defer reads.mu.Unlock()
	assert.Empty(t, reads.full, "list agents must not hydrate any transcript")
	assert.Empty(t, reads.own, "list agents must not read own history")
	assert.Contains(t, reads.metadata, persisted.ID, "persisted titles use metadata-only reads")
	assert.Contains(t, reads.metadata, root.ID, "parent title uses metadata-only reads")
}

func TestListAgentsSpawnableDescriptionsRemainInToolDescriptionOnly(t *testing.T) {
	rootAgent := agent.New("root", "", agent.WithModel(coordinationReply("unused")), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker", Name: "research", Description: "Research role"}), agent.WithToolSets(subagent.NewToolSet()))
	worker := agent.New("worker", "", agent.WithModel(coordinationReply("unused")))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(rootAgent, worker)), WithSessionStore(session.NewInMemorySessionStore()), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	handle, err := rt.CreateSession(t.Context(), session.New(session.WithID("catalog-root"), session.WithAgentName("root")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	toolDefs, err := handle.(SessionExecutableToolProvider).SessionExecutableTools(t.Context())
	require.NoError(t, err)
	inspected, err := handle.(SessionToolInspector).InspectTools(t.Context())
	require.NoError(t, err)
	var inspectedSpawn tools.Tool
	for _, definition := range inspected.Tools {
		if definition.Name == subagent.ToolSpawnSubagent {
			inspectedSpawn = definition
			break
		}
	}
	require.Equal(t, subagent.ToolSpawnSubagent, inspectedSpawn.Name)
	assert.Contains(t, inspectedSpawn.Description, "research: Research role")
	var spawn tools.Tool
	for _, definition := range toolDefs {
		if definition.Name == subagent.ToolSpawnSubagent {
			spawn = definition
			break
		}
	}
	require.Equal(t, subagent.ToolSpawnSubagent, spawn.Name)
	assert.Contains(t, spawn.Description, "research: Research role")
	assert.NotContains(t, spawn.Description, "worker: Research role")
	listOutput := callListAgentsOutput(t, rt, session.New(session.WithID("catalog-root"), session.WithAgentName("root")), `{}`)
	assert.NotContains(t, listOutput, "Research role")
	assert.NotContains(t, listOutput, "configured_agents")
}

func TestEnrichSpawnDescriptionClonesAndScopesCatalog(t *testing.T) {
	base := []tools.Tool{{Name: subagent.ToolSpawnSubagent, Description: "shared spawn description"}}
	callerA := agent.New("a", "", agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker", Name: "research"}))
	callerB := agent.New("b", "", agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker", Name: "review"}))
	a := enrichSpawnToolDescription(callerA, base, []subagent.AllowedSubagent{{Agent: "worker", Name: "research", Description: "Research role"}})
	b := enrichSpawnToolDescription(callerB, base, []subagent.AllowedSubagent{{Agent: "worker", Name: "review", Description: "Review role"}})
	require.NotSame(t, &base[0], &a[0])
	assert.Equal(t, "shared spawn description", base[0].Description, "shared definitions remain unchanged")
	assert.Contains(t, a[0].Description, "research: Research role")
	assert.NotContains(t, a[0].Description, "Review role")
	assert.Contains(t, b[0].Description, "review: Review role")
	assert.NotContains(t, b[0].Description, "Research role")
	again := enrichSpawnToolDescription(callerA, a, []subagent.AllowedSubagent{{Agent: "worker", Name: "research", Description: "Research role"}})
	assert.Equal(t, a[0].Description, again[0].Description, "refresh does not duplicate the catalog")
	codeMode := []tools.Tool{{Name: "run_tools_with_javascript", Description: "code-mode help"}}
	wrapped := enrichSpawnToolDescription(callerA, codeMode, []subagent.AllowedSubagent{{Agent: "worker", Name: "research", Description: "Research role"}})
	assert.Contains(t, wrapped[0].Description, "research: Research role")
	wrappedAgain := enrichSpawnToolDescription(callerA, wrapped, []subagent.AllowedSubagent{{Agent: "worker", Name: "research", Description: "Research role"}})
	assert.Equal(t, wrapped[0].Description, wrappedAgain[0].Description, "code mode refresh does not duplicate catalog")
	assert.Equal(t, "code-mode help", codeMode[0].Description)
}

// The model sees the allow-list only through the spawn tool description, and
// exactly the pinned entries appear there with no extra names.
func TestSpawnToolDescriptionCarriesExactAllowlistOnly(t *testing.T) {
	caller := agent.New("root", "", agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker", Name: "research"}, latest.SubagentRef{Agent: "worker", Name: "review"}))
	allowed := []subagent.AllowedSubagent{
		{Agent: "worker", Name: "research", Description: "Research role"},
		{Agent: "worker", Name: "review", Description: "Review role"},
	}
	out := enrichSpawnToolDescription(caller, []tools.Tool{{Name: subagent.ToolSpawnSubagent, Description: "shared spawn description"}}, allowed)
	require.Len(t, out, 1)
	description := out[0].Description
	require.Contains(t, description, spawnCatalogMarker)
	catalog := strings.TrimSpace(description[strings.Index(description, spawnCatalogMarker)+len(spawnCatalogMarker):])
	assert.Equal(t, []string{"- research: Research role", "- review: Review role"}, strings.Split(catalog, "\n"))
	assert.NotContains(t, subagent.HarnessPrompt(allowed), "Research role", "role descriptions stay out of the harness prompt")
}
