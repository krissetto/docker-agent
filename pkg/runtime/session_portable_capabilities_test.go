package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

type portablePromptToolset struct {
	text   string
	called string
}

func (*portablePromptToolset) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{{Name: "bound_tool"}}, nil
}
func (*portablePromptToolset) ListPrompts(context.Context) ([]tools.PromptInfo, error) {
	return []tools.PromptInfo{{Name: "a/b"}}, nil
}
func (p *portablePromptToolset) GetPrompt(_ context.Context, name string, _ map[string]string) (*mcp.GetPromptResult, error) {
	p.called = name
	return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Content: &mcp.TextContent{Text: p.text}}}}, nil
}

func TestPortableCapabilitiesUseBoundAgent(t *testing.T) {
	first, second := &portablePromptToolset{text: "first"}, &portablePromptToolset{text: "second"}
	restart := &restartableToolset{desc: "restart", state: lifecycle.StateInfo{State: lifecycle.StateFailed, LastError: errors.New("secret-token")}}
	other := agent.New("other", "", agent.WithModel(&mockProvider{id: "test/other"}), agent.WithToolSets(&toolListToolset{names: []string{"wrong_agent"}}))
	bound := agent.New("bound", "", agent.WithDescription("bound description"), agent.WithCommands(types.Commands{"bound-command": {Instruction: "hello"}}), agent.WithModel(&mockProvider{id: "test/bound"}), agent.WithToolSets(first, second, restart))
	r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(other, bound), team.WithPermissions(permissions.NewCheckerFromRules([]string{"source_allow"}, []string{"source_ask"}, []string{"source_deny"}))), WithCurrentAgent("other"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	sess := session.New(session.WithSafetyPolicy(session.SafetyPolicyBalanced), session.WithPermissions(&session.PermissionsConfig{Allow: []string{"session_allow"}, Deny: []string{"session_deny"}}))
	h, err := r.CreateSession(t.Context(), sess, SessionBinding{AgentName: "bound"})
	require.NoError(t, err)
	presentation, err := h.(SessionAgentInfoProvider).SessionAgentInfo(t.Context())
	require.NoError(t, err)
	require.Equal(t, "bound", presentation.Agent.AgentName)
	require.Equal(t, "bound description", presentation.Agent.Description)
	require.Equal(t, "test/bound", presentation.Agent.Model)
	require.Equal(t, "hello", presentation.Commands["bound-command"].Instruction)
	info, err := h.(SessionToolInspector).InspectTools(t.Context())
	require.NoError(t, err)
	for _, tool := range info.Tools {
		require.NotEqual(t, "wrong_agent", tool.Name)
		require.Nil(t, tool.Handler)
	}
	require.Len(t, info.Statuses, 3)
	require.Equal(t, "toolset unavailable", info.Statuses[2].LastError)
	require.NoError(t, h.(SessionToolsetController).RestartToolset(t.Context(), "restart"))
	require.Equal(t, 1, restart.restartCall)
	policy, err := h.(SessionPermissionsInspector).EffectivePermissions(t.Context())
	require.NoError(t, err)
	require.Equal(t, session.SafetyPolicyBalanced, policy.Policy)
	require.Equal(t, []string{"source_allow"}, policy.Source.Allow)
	require.Equal(t, []string{"session_deny"}, policy.Session.Deny)
	policy.Session.Deny[0] = "changed"
	again, err := h.(SessionPermissionsInspector).EffectivePermissions(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{"session_deny"}, again.Session.Deny)
	prompts, err := h.(SessionMCPPrompts).MCPPrompts(t.Context())
	require.NoError(t, err)
	require.Len(t, prompts, 2)
	for key, prompt := range prompts {
		require.Equal(t, "a/b", prompt.Name)
		text, err := h.(SessionMCPPrompts).ExecuteMCPPrompt(t.Context(), key, nil)
		require.NoError(t, err)
		require.Contains(t, []string{"first", "second"}, text)
	}
	require.Equal(t, "a/b", first.called)
	require.Equal(t, "a/b", second.called)
}

func TestPortableBranchExactCutsAndPrecondition(t *testing.T) {
	r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("bound", "", agent.WithModel(&mockProvider{id: "test/bound"})))), WithSessionStore(session.NewInMemorySessionStore()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	sess := session.New(session.WithWorkingDir(t.TempDir()), session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
	sess.PriorSafetyPolicy = session.SafetyPolicyBalanced
	sess.HideToolResults = true
	sess.SetAttribute("docker-agent.actor.source", "source.yaml")
	sess.Messages = []session.Item{{Message: &session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "one"}}}, {Summary: "summary"}, {SubSession: session.New()}, {Message: &session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "two"}}}}
	h, err := r.CreateSession(t.Context(), sess, SessionBinding{AgentName: "bound", Model: "test/override"})
	require.NoError(t, err)
	snapshot, err := h.Snapshot(t.Context())
	require.NoError(t, err)
	pos := 3
	_, _, err = r.BranchSession(t.Context(), h.ID(), BranchOptions{Position: &pos})
	var typed *SessionError
	require.ErrorAs(t, err, &typed)
	require.Equal(t, SessionErrorStale, typed.Kind)
	branched, child, err := r.BranchSession(t.Context(), h.ID(), BranchOptions{Position: &pos, ExpectedSnapshot: SnapshotProof(snapshot)})
	require.NoError(t, err)
	require.Len(t, child.Messages, 3)
	require.Equal(t, "summary", child.Messages[1].Summary)
	require.NotEqual(t, snapshot.Messages[2].SubSession.ID, child.Messages[2].SubSession.ID)
	require.Equal(t, child.ID, child.Messages[2].SubSession.ParentID)
	require.Equal(t, "bound", branched.AgentName())
	require.Equal(t, "test/override", branched.Metadata().Model)
	require.Equal(t, sess.WorkingDir, child.WorkingDir)
	require.True(t, child.HideToolResults)
	require.Equal(t, session.SafetyPolicyBalanced, child.PriorSafetyPolicy)
	require.Equal(t, "source.yaml", child.AttributesSnapshot()["docker-agent.actor.source"])
	_, full, err := r.BranchSession(t.Context(), h.ID(), BranchOptions{})
	require.NoError(t, err)
	require.Len(t, full.Messages, 4)
	driver := h.(*sessionHandle).driver
	require.NoError(t, driver.ownerCall(t.Context(), func() error {
		driver.sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "changed"}})
		return nil
	}))
	_, _, err = r.BranchSession(t.Context(), h.ID(), BranchOptions{Position: &pos, ExpectedSnapshot: SnapshotProof(snapshot)})
	require.ErrorAs(t, err, &typed)
	require.Equal(t, SessionErrorStale, typed.Kind)
	require.NoError(t, driver.ownerCall(t.Context(), func() error {
		driver.phase = sessionRunning
		return nil
	}))
	_, _, err = r.BranchSession(t.Context(), h.ID(), BranchOptions{})
	require.ErrorAs(t, err, &typed)
	require.Equal(t, SessionErrorCapacity, typed.Kind)
	require.NoError(t, driver.ownerCall(t.Context(), func() error {
		driver.phase = sessionIdle
		return nil
	}))
}
