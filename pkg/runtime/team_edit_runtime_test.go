package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamdef"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/teamedit"
)

const liveTeamEditYAML = `version: "16"
id: team-edit-live
agents:
  root:
    id: root-stable
    model: openai/test
    instruction: Before instructions
    toolsets:
      - type: team_edit
`

type teamEditTurnProvider struct {
	*mockProvider
	mu              sync.Mutex
	calls           int
	revision        string
	currentRevision func() string
	toolNames       [][]string
	systemPrompts   [][]string
}

func (p *teamEditTurnProvider) CreateChatCompletionStream(_ context.Context, messages []chat.Message, offered []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	var names []string
	for _, tool := range offered {
		names = append(names, tool.Name)
	}
	var prompts []string
	for _, message := range messages {
		if message.Role == chat.MessageRoleSystem {
			prompts = append(prompts, message.Content)
		}
	}
	p.toolNames, p.systemPrompts = append(p.toolNames, names), append(p.systemPrompts, prompts)
	if p.calls == 1 {
		args := fmt.Sprintf(`{"action":"apply","revision":%q,"agent_key":"root","instruction":"After edit instructions"}`, p.revision)
		return newStreamBuilder().AddToolCallName("edit-call", teamedit.ToolName).AddToolCallArguments("edit-call", args).AddToolCallStopWithUsage(2, 2).Build(), nil
	}
	if p.calls == 2 {
		args := fmt.Sprintf(`{"action":"apply","revision":%q,"agent_key":"root","description":"second edit"}`, p.currentRevision())
		return newStreamBuilder().AddToolCallName("edit-call-2", teamedit.ToolName).AddToolCallArguments("edit-call-2", args).AddToolCallStopWithUsage(2, 2).Build(), nil
	}
	return newStreamBuilder().AddContent("third turn completed").AddStopWithUsage(2, 2).Build(), nil
}

func TestTeamEditNormalDispatchAdoptsPublishedEnvironmentOnNextTurn(t *testing.T) {
	store := session.NewInMemorySessionStore()
	source := config.NewBytesSource("team.yaml", []byte(liveTeamEditYAML))
	editor, err := teamdef.NewEditor(t.Context(), teamdef.EditableSourceOptions{
		Source:    source,
		Authority: teamdef.EditAuthorityRestricted,
		BuildValidator: func(ctx context.Context, candidate config.Source) error {
			_, err := config.Load(ctx, candidate)
			return err
		},
	})
	require.NoError(t, err)
	initialRevision := editor.CurrentRevision()
	var providerUnderTest *teamEditTurnProvider
	buildTeam := func(ctx context.Context, candidate config.Source, revision, sourceID, baselineID string, _ *config.RuntimeConfig, _ []teamloader.Opt) (*TeamEnvironment, error) {
		data, err := candidate.Read(ctx)
		if err != nil {
			return nil, err
		}
		loaded, err := config.Load(ctx, config.NewBytesSource("team.yaml", data))
		if err != nil {
			return nil, err
		}
		agentConfig := loaded.Agents[0]
		var agentOptions []agent.Opt
		agentOptions = append(agentOptions, agent.WithModel(providerUnderTest))
		if agentConfig.Instruction == "After edit instructions" {
			agentOptions = append(agentOptions, agent.WithToolSets(&toolListToolset{names: []string{"revised_only_tool"}}))
		}
		root := agent.New(agentConfig.Name, agentConfig.Instruction, agentOptions...)
		tm := team.New(team.WithAgents(root), team.WithAgentConfigs(map[string]latest.AgentConfig{agentConfig.Name: agentConfig}), team.WithPermissions(permissions.NewCheckerFromRules(nil, nil, nil)))
		return NewTeamEnvironment(revision, sourceID, baselineID, loaded.ID, tm, ModelSwitcherConfig{}, func(ctx context.Context) error { return tm.StopToolSets(ctx) })
	}
	initialTeam, err := buildTeam(t.Context(), source, initialRevision.ID, "source-local", editor.BaselineRevisionID(), nil, nil)
	require.NoError(t, err)
	environments, err := NewTeamEnvironmentStore(initialTeam)
	require.NoError(t, err)
	providerUnderTest = &teamEditTurnProvider{mockProvider: &mockProvider{id: "test/team-edit"}, revision: initialRevision.ID}
	providerUnderTest.currentRevision = func() string { return editor.CurrentRevision().ID }
	// Rebuild the initial environment with the real provider used by the test.
	require.NoError(t, environments.Close(t.Context()))
	initialTeam, err = buildTeam(t.Context(), source, initialRevision.ID, "source-local", editor.BaselineRevisionID(), nil, nil)
	require.NoError(t, err)
	environments, err = NewTeamEnvironmentStore(initialTeam)
	require.NoError(t, err)
	builder := func(ctx context.Context, candidate config.Source, revision, sourceID, baselineID string, runConfig *config.RuntimeConfig, loaderOptions []teamloader.Opt) (*TeamEnvironment, error) {
		return buildTeam(ctx, candidate, revision, sourceID, baselineID, runConfig, loaderOptions)
	}
	rt, err := NewLocalRuntime(t.Context(), initialTeam.Team(), WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithTeamEditor(editor, environments, builder, config.TeamEditGrant{Mode: "restricted"}))
	require.NoError(t, err)
	owner := NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	view := owner.Runtime()
	handle, err := view.CreateSession(t.Context(), session.New(session.WithID("team-edit-normal"), session.WithSafetyPolicy(session.SafetyPolicyAutonomous)), SessionBinding{AgentName: "root"})
	require.NoError(t, err)

	first, err := handle.Submit(t.Context(), TurnInput{Content: "edit this team"})
	require.NoError(t, err)
	require.NoError(t, handle.AwaitTurn(t.Context(), first.TurnID))
	current, err := environments.AcquireCurrent()
	require.NoError(t, err)
	publishedRevision := current.Environment().RevisionID()
	require.NotEqual(t, initialRevision.ID, publishedRevision)
	require.Equal(t, publishedRevision, editor.CurrentRevision().ID)
	require.NoError(t, current.Release(t.Context()))
	activeHandle, err := view.SessionByID(handle.ID())
	require.NoError(t, err)
	activeSnapshot, err := activeHandle.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, "root-stable", activeSnapshot.AgentID)
	require.Equal(t, "root", activeSnapshot.AttributesSnapshot()[SessionAgentAttribute])

	second, err := handle.Submit(t.Context(), TurnInput{Content: "continue with the revised agent"})
	require.NoError(t, err)
	require.NoError(t, handle.AwaitTurn(t.Context(), second.TurnID))
	secondPublished := editor.CurrentRevision().ID
	require.NotEqual(t, publishedRevision, secondPublished, "a baseline actor can edit the current source head again")
	third, err := handle.Submit(t.Context(), TurnInput{Content: "continue after the second edit"})
	require.NoError(t, err)
	require.NoError(t, handle.AwaitTurn(t.Context(), third.TurnID))
	providerUnderTest.mu.Lock()
	defer providerUnderTest.mu.Unlock()
	require.Len(t, providerUnderTest.toolNames, 3)
	require.Contains(t, providerUnderTest.toolNames[0], teamedit.ToolName)
	require.Contains(t, providerUnderTest.toolNames[1], teamedit.ToolName)
	require.Contains(t, providerUnderTest.toolNames[2], teamedit.ToolName)
	require.NotContains(t, providerUnderTest.toolNames[0], "revised_only_tool")
	require.Contains(t, providerUnderTest.toolNames[1], "revised_only_tool")
	require.Contains(t, providerUnderTest.toolNames[2], "revised_only_tool")
	require.Contains(t, strings.Join(providerUnderTest.systemPrompts[0], "\n"), "Before instructions")
	require.Contains(t, strings.Join(providerUnderTest.systemPrompts[1], "\n"), "After edit instructions")
	require.Contains(t, strings.Join(providerUnderTest.systemPrompts[2], "\n"), "After edit instructions")
}

func TestTeamEditBuilderFailureLeavesSourceAndEnvironmentUnchanged(t *testing.T) {
	store := session.NewInMemorySessionStore()
	source := config.NewBytesSource("team.yaml", []byte(liveTeamEditYAML))
	editor, err := teamdef.NewEditor(t.Context(), teamdef.EditableSourceOptions{Source: source, Authority: teamdef.EditAuthorityRestricted, BuildValidator: func(ctx context.Context, candidate config.Source) error {
		_, err := config.Load(ctx, candidate)
		return err
	}})
	require.NoError(t, err)
	initialRevision := editor.CurrentRevision()
	provider := &teamEditTurnProvider{mockProvider: &mockProvider{id: "test/team-edit"}, revision: initialRevision.ID}
	provider.currentRevision = func() string { return editor.CurrentRevision().ID }
	build := func(ctx context.Context, candidate config.Source, revision, sourceID, baselineID string, _ *config.RuntimeConfig, _ []teamloader.Opt) (*TeamEnvironment, error) {
		loaded, err := config.Load(ctx, candidate)
		if err != nil {
			return nil, err
		}
		root := agent.New("root", loaded.Agents[0].Instruction, agent.WithModel(provider))
		tm := team.New(team.WithAgents(root), team.WithAgentConfigs(map[string]latest.AgentConfig{"root": loaded.Agents[0]}))
		return NewTeamEnvironment(revision, sourceID, baselineID, loaded.ID, tm, ModelSwitcherConfig{}, func(ctx context.Context) error { return tm.StopToolSets(ctx) })
	}
	initial, err := build(t.Context(), source, initialRevision.ID, "source-local", editor.BaselineRevisionID(), nil, nil)
	require.NoError(t, err)
	envs, err := NewTeamEnvironmentStore(initial)
	require.NoError(t, err)
	failingBuilder := func(context.Context, config.Source, string, string, string, *config.RuntimeConfig, []teamloader.Opt) (*TeamEnvironment, error) {
		return nil, fmt.Errorf("candidate provider initialization failed")
	}
	rt, err := NewLocalRuntime(t.Context(), initial.Team(), WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithTeamEditor(editor, envs, failingBuilder, config.TeamEditGrant{Mode: "restricted"}))
	require.NoError(t, err)
	owner := NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	h, err := owner.Runtime().CreateSession(t.Context(), session.New(session.WithID("builder-failure"), session.WithSafetyPolicy(session.SafetyPolicyAutonomous)), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	first, err := h.Submit(t.Context(), TurnInput{Content: "try edit"})
	require.NoError(t, err)
	require.NoError(t, h.AwaitTurn(t.Context(), first.TurnID))
	require.Equal(t, initialRevision.ID, editor.CurrentRevision().ID)
	current, err := envs.AcquireCurrent()
	require.NoError(t, err)
	require.Equal(t, initialRevision.ID, current.Environment().RevisionID())
	require.NoError(t, current.Release(t.Context()))
}

func TestTeamEditRestrictedAllowlistDoesNotExposeTool(t *testing.T) {
	store := session.NewInMemorySessionStore()
	source := config.NewBytesSource("team.yaml", []byte(liveTeamEditYAML))
	editor, err := teamdef.NewEditor(t.Context(), teamdef.EditableSourceOptions{Source: source, Authority: teamdef.EditAuthorityRestricted, BuildValidator: func(ctx context.Context, candidate config.Source) error {
		_, err := config.Load(ctx, candidate)
		return err
	}})
	require.NoError(t, err)
	initialRevision := editor.CurrentRevision()
	provider := coordinationReply("no edit")
	loaded, err := config.Load(t.Context(), source)
	require.NoError(t, err)
	root := agent.New("root", "Before instructions", agent.WithModel(provider))
	tm := team.New(team.WithAgents(root), team.WithAgentConfigs(map[string]latest.AgentConfig{"root": loaded.Agents[0]}))
	initial, err := NewTeamEnvironment(initialRevision.ID, "source-local", editor.BaselineRevisionID(), loaded.ID, tm, ModelSwitcherConfig{}, func(ctx context.Context) error { return tm.StopToolSets(ctx) })
	require.NoError(t, err)
	envs, err := NewTeamEnvironmentStore(initial)
	require.NoError(t, err)
	builder := func(context.Context, config.Source, string, string, string, *config.RuntimeConfig, []teamloader.Opt) (*TeamEnvironment, error) {
		return nil, fmt.Errorf("should not build")
	}
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionStore(store), WithModelStore(mockModelStore{}), WithTeamEditor(editor, envs, builder, config.TeamEditGrant{Mode: "restricted", AllowedAgentIDsSet: true, AllowedAgentIDs: []string{"someone-else"}}))
	require.NoError(t, err)
	owner := NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	h, err := owner.Runtime().CreateSession(t.Context(), session.New(session.WithID("allowlist-denied"), session.WithSafetyPolicy(session.SafetyPolicyAutonomous)), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	info, err := h.(SessionToolInspector).InspectTools(t.Context())
	require.NoError(t, err)
	var offeredNames []string
	for _, tool := range info.Tools {
		offeredNames = append(offeredNames, tool.Name)
	}
	require.NotContains(t, offeredNames, teamedit.ToolName)
}

type fullReplaceBudgetProvider struct {
	*mockProvider
	calls     int
	revision  func() string
	candidate string
}

func (p *fullReplaceBudgetProvider) CreateChatCompletionStream(_ context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	p.calls++
	if p.calls != 1 {
		return newStreamBuilder().AddContent("must not run under tightened policy").AddStopWithUsage(1, 1).Build(), nil
	}
	args, err := json.Marshal(teamedit.Call{Action: "replace", Revision: p.revision(), YAML: p.candidate})
	if err != nil {
		return nil, err
	}
	return newStreamBuilder().AddToolCallName("replace", teamedit.ToolName).AddToolCallArguments("replace", string(args)).AddToolCallStopWithUsage(4, 4).Build(), nil
}

func TestFullTeamReplacePublishesCandidateBudgetForNextGeneration(t *testing.T) {
	const initialYAML = `version: "16"
id: team-edit-budget
agents:
  root:
    id: root-stable
    model: openai/test
    toolsets:
      - type: team_edit
`
	const candidateYAML = `version: "16"
id: team-edit-budget
budget:
  max_tokens: 1
agents:
  root:
    id: root-stable
    model: openai/test
    toolsets:
      - type: team_edit
`
	source := config.NewBytesSource("team.yaml", []byte(initialYAML))
	editor, err := teamdef.NewEditor(t.Context(), teamdef.EditableSourceOptions{
		Source: source, Authority: teamdef.EditAuthorityFull,
		BuildValidator: func(ctx context.Context, candidate config.Source) error {
			_, err := config.Load(ctx, candidate)
			return err
		},
	})
	require.NoError(t, err)
	providerUnderTest := &fullReplaceBudgetProvider{mockProvider: &mockProvider{id: "test/full-budget-edit"}, candidate: candidateYAML}
	providerUnderTest.revision = func() string { return editor.CurrentRevision().ID }
	providerRegistry := provider.NewRegistry(map[string]provider.Factory{"openai": func(_ context.Context, _ *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (provider.Provider, error) {
		return providerUnderTest, nil
	}})
	build := func(ctx context.Context, candidate config.Source, revision, sourceID, baselineID string, _ *config.RuntimeConfig, _ []teamloader.Opt) (*TeamEnvironment, error) {
		loaded, err := teamloader.LoadWithConfig(ctx, candidate, &config.RuntimeConfig{EnvProviderForTests: environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test"})}, teamloader.WithToolsetRegistry(teamloader.NewToolsetRegistry(map[string]teamloader.ToolsetCreator{teamedit.ToolsetType: teamedit.Creator})), teamloader.WithProviderRegistry(providerRegistry))
		if err != nil {
			return nil, err
		}
		var runBudget *latest.BudgetConfig
		if loaded.Budget != nil {
			cloned := *loaded.Budget
			runBudget = &cloned
		}
		policy := BudgetPolicyConfig{Run: runBudget, Named: loaded.Budgets, AgentBudgets: loaded.AgentBudgets}
		return NewTeamEnvironment(revision, sourceID, baselineID, loaded.Definitions.TeamID, loaded.Team, ModelSwitcherConfig{}, func(ctx context.Context) error { return loaded.Team.StopToolSets(ctx) }, WithTeamEnvironmentBudgetConfig(policy))
	}
	initial, err := build(t.Context(), source, editor.CurrentRevision().ID, "budget-source", editor.BaselineRevisionID(), nil, nil)
	require.NoError(t, err)
	environments, err := NewTeamEnvironmentStore(initial)
	require.NoError(t, err)
	rt, err := NewLocalRuntime(t.Context(), initial.Team(), WithSessionStore(session.NewInMemorySessionStore()), WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithTeamEditor(editor, environments, build, config.TeamEditGrant{Mode: "full"}))
	require.NoError(t, err)
	owner := NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	handle, err := owner.Runtime().CreateSession(t.Context(), session.New(session.WithID("full-budget-replace"), session.WithSafetyPolicy(session.SafetyPolicyAutonomous)), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	first, err := handle.Submit(t.Context(), TurnInput{Content: "replace team and tighten the budget"})
	require.NoError(t, err)
	require.NoError(t, handle.AwaitTurn(t.Context(), first.TurnID))
	current, err := environments.AcquireCurrent()
	require.NoError(t, err)
	budget := current.Environment().BudgetPolicy().Run
	require.NotNil(t, budget)
	require.EqualValues(t, 1, budget.MaxTokens)
	require.NoError(t, current.Release(t.Context()))
	second, err := handle.Submit(t.Context(), TurnInput{Content: "continue"})
	require.NoError(t, err)
	require.NoError(t, handle.AwaitTurn(t.Context(), second.TurnID))
	require.Equal(t, 1, providerUnderTest.calls, "the candidate revision's low budget stops its next generation before another model call")
	budgetContext, release, err := rt.captureEnvironmentForOperation(t.Context(), handle.(*sessionHandle).driver.session())
	require.NoError(t, err)
	defer release()
	usage := rt.budgetUsageForContext(budgetContext, handle.(*sessionHandle).driver.session(), "root")
	require.NotEmpty(t, usage)
	require.GreaterOrEqual(t, usage[0].Snapshot.Tokens, int64(8))
}
