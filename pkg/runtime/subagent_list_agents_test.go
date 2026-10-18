package runtime

import (
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
		agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker", Name: "research", Description: "Research role"}))
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
	assert.Equal(t, "Research role", firstPage.Children[0].Description)
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
	assert.Equal(t, listedAgentRelationship{Agent: "worker", Name: "research"}, *childView.Parent)
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
