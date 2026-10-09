package runtime

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
	"github.com/docker/docker-agent/pkg/tools/builtin/handoff"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
)

func TestActiveCapabilitiesAgreeAcrossHandleAges(t *testing.T) {
	store := coordinationSQLite(t)
	rootRestart, otherRestart := &restartableToolset{desc: "restart"}, &restartableToolset{desc: "restart"}
	rootPrompt, otherPrompt := &portablePromptToolset{text: "root prompt"}, &portablePromptToolset{text: "other prompt"}
	tm := team.New(team.WithAgents(
		agent.New("root", "", agent.WithModel(coordinationReply("root")), agent.WithCommands(types.Commands{"switch": {Agent: "other"}}), agent.WithToolSets(rootPrompt, rootRestart, &toolListToolset{names: []string{"root_tool"}})),
		agent.New("other", "", agent.WithModel(coordinationReply("other")), agent.WithCommands(types.Commands{"other": {Instruction: "other command"}}), agent.WithToolSets(otherPrompt, otherRestart, &toolListToolset{names: []string{"other_tool"}})),
	))
	cfg := &ModelSwitcherConfig{ProviderRegistry: testProviderRegistry(), EnvProvider: environment.NewMapEnvProvider(nil), Models: map[string]latest.ModelConfig{"override": {Provider: "openai", Model: "gpt-5"}}}
	r, err := NewLocalRuntime(t.Context(), tm, WithSessionStore(store), WithModelStore(emptyCatalogStore{}), WithSessionCompaction(false), WithModelSwitcherConfig(cfg))
	require.NoError(t, err)
	owner := NewSessionRuntimeSupervisor(r)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	h := coordinationCreate(t, owner.Runtime(), "root", "")
	// Prime the initial pure metadata cache before changing the active binding.
	require.Equal(t, "root", h.Metadata().AgentName)
	sub, err := h.(SessionLegacyCommandInput).RunLegacyCommandTurn(t.Context(), []LegacyCommandMessage{{Role: chat.MessageRoleUser, Input: TurnInput{Content: "/switch"}}}, "", "switch")
	require.NoError(t, err)
	coordinationAwait(t, h, sub.TurnID)
	newer, err := owner.Runtime().SessionByID(h.ID())
	require.NoError(t, err)
	for _, handle := range []SessionHandle{h, newer} {
		require.Equal(t, "other", handle.AgentName())
		info, err := handle.(SessionToolInspector).InspectTools(t.Context())
		require.NoError(t, err)
		names := make([]string, 0, len(info.Tools))
		for _, tool := range info.Tools {
			names = append(names, tool.Name)
			require.Nil(t, tool.Handler)
		}
		require.Contains(t, names, "other_tool")
		require.NotContains(t, names, "root_tool")
		prompts, err := handle.(SessionMCPPrompts).MCPPrompts(t.Context())
		require.NoError(t, err)
		require.Contains(t, prompts, "0/a/b")
		text, err := handle.(SessionMCPPrompts).ExecuteMCPPrompt(t.Context(), "0/a/b", nil)
		require.NoError(t, err)
		require.Equal(t, "other prompt", text)
		require.NoError(t, handle.(SessionToolsetController).RestartToolset(t.Context(), "restart"))
		presentation, err := handle.(SessionAgentInfoProvider).SessionAgentInfo(t.Context())
		require.NoError(t, err)
		require.Equal(t, "other", presentation.Agent.AgentName)
		require.Equal(t, "other command", presentation.Commands["other"].Instruction)
		var emitted []Event
		handle.(*sessionHandle).EmitPinnedAgentInfo(t.Context(), EventSinkFunc(func(e Event) { emitted = append(emitted, e) }))
		require.Equal(t, presentation.Agent.AgentName, emitted[0].(*AgentInfoEvent).AgentName)
		require.Equal(t, presentation.Agent.Model, emitted[0].(*AgentInfoEvent).Model)
	}
	require.Zero(t, rootRestart.restartCall)
	require.Equal(t, 2, otherRestart.restartCall)
	require.Equal(t, h.Metadata(), newer.Metadata())
	require.Equal(t, h.AvailableModels(t.Context()), newer.AvailableModels(t.Context()))
	require.Equal(t, h.ThinkingLevels(t.Context()), newer.ThinkingLevels(t.Context()))
	require.Equal(t, h.CurrentThinkingLevel(t.Context()), newer.CurrentThinkingLevel(t.Context()))
	require.NoError(t, h.SetModel(t.Context(), "override"))
	require.Equal(t, "override", newer.Metadata().Model)
	loaded, err := store.GetSession(t.Context(), h.ID())
	require.NoError(t, err)
	require.Equal(t, "override", loaded.AgentModelOverrides["other"])
	require.Empty(t, loaded.AgentModelOverrides["root"])
	require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context())))
	restarted, err := NewLocalRuntime(t.Context(), tm, WithSessionStore(store), WithModelStore(emptyCatalogStore{}), WithSessionCompaction(false), WithModelSwitcherConfig(cfg))
	require.NoError(t, err)
	restartedOwner := NewSessionRuntimeSupervisor(restarted)
	t.Cleanup(func() { require.NoError(t, restartedOwner.Shutdown(context.WithoutCancel(t.Context()))) })
	restored, err := restartedOwner.Runtime().CreateSession(t.Context(), loaded, SessionBinding{})
	require.NoError(t, err)
	require.Equal(t, "other", restored.AgentName())
	require.Equal(t, "override", restored.Metadata().Model)
	require.Equal(t, "gpt-5", restored.(*sessionHandle).driver.activeBinding().models[0].BaseConfig().ModelConfig.Model)
}

func TestModelResolutionRejectsConcurrentActiveSwitch(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	registry := provider.NewRegistry(map[string]provider.Factory{"openai": func(_ context.Context, cfg *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (provider.Provider, error) {
		close(started)
		<-release
		return newConfigProvider(*cfg), nil
	}})
	r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "", agent.WithModel(coordinationReply("root")), agent.WithCommands(types.Commands{"switch": {Agent: "other"}})), agent.New("other", "", agent.WithModel(coordinationReply("other"))))), WithSessionStore(coordinationSQLite(t)), WithSessionCompaction(false), WithModelStore(emptyCatalogStore{}), WithModelSwitcherConfig(&ModelSwitcherConfig{ProviderRegistry: registry, EnvProvider: environment.NewMapEnvProvider(nil), Models: map[string]latest.ModelConfig{"override": {Provider: "openai", Model: "gpt-5"}}}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	h, err := r.CreateSession(t.Context(), session.New(), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- h.SetModel(t.Context(), "override") }()
	<-started
	d := h.(*sessionHandle).driver
	sub, err := h.(SessionLegacyCommandInput).RunLegacyCommandTurn(t.Context(), []LegacyCommandMessage{{Role: chat.MessageRoleUser, Input: TurnInput{Content: "/switch"}}}, "", "switch")
	require.NoError(t, err)
	coordinationAwait(t, h, sub.TurnID)
	close(release)
	var stale *SessionError
	require.ErrorAs(t, <-done, &stale)
	require.Equal(t, SessionErrorStale, stale.Kind)
	snapshot, err := h.Snapshot(t.Context())
	require.NoError(t, err)
	require.Empty(t, snapshot.AgentModelOverrides)
	require.Equal(t, "other", h.AgentName())
	require.Empty(t, d.ModelRef())
}

func TestExecutionBudgetRebindPreservesWalletsAndDropsOldDeadline(t *testing.T) {
	r := &LocalRuntime{}
	WithBudget(&latest.BudgetConfig{MaxTokens: 100})(r)
	WithNamedBudgets(map[string]latest.BudgetConfig{"a": {MaxTime: latest.Duration{Duration: 30 * time.Millisecond}}, "b": {MaxTokens: 2}}, map[string][]string{"a": {"a"}, "b": {"b"}})(r)
	sess := session.New()
	ctx, cancel := r.executionBudgetContext(t.Context(), sess, agent.New("a", ""))
	defer cancel()
	wallet := r.rootBudget(sess)
	aStarted := wallet.trackers["a"].started
	runStarted := wallet.trackers[runBudgetName].started
	r.recordBudget(sess, agent.New("a", ""), &chat.Usage{InputTokens: 1}, nil, 0, &collectSink{})
	rebindExecutionBudget(ctx, "b")
	select {
	case <-ctx.Done():
		t.Fatal("A-only deadline canceled B")
	case <-time.After(60 * time.Millisecond):
	}
	require.Equal(t, aStarted, wallet.trackers["a"].started)
	require.Equal(t, runStarted, wallet.trackers[runBudgetName].started)
	require.Equal(t, int64(1), wallet.trackers[runBudgetName].snapshot().Tokens)
	r.recordBudget(sess, agent.New("b", ""), &chat.Usage{InputTokens: 2}, nil, 0, &collectSink{})
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("B exhaustion did not cancel rebound execution")
	}
}

// A real canonical handoff must change budget membership before B's first wait.
func TestHandoffRebindCancelsCooperativeProvider(t *testing.T) {
	for _, mode := range []string{"time", "shared_tokens"} {
		t.Run(mode, func(t *testing.T) {
			entered := make(chan struct{})
			var calls atomic.Int32
			bProvider := &coordinationProvider{mockProvider: &mockProvider{id: "test/b"}, call: func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
				if calls.Add(1) > 1 {
					return newStreamBuilder().AddContent("spent").AddStopWithUsage(2, 0).Build(), nil
				}
				close(entered)
				<-ctx.Done()
				return nil, ctx.Err()
			}}
			b := agent.New("b", "", agent.WithModel(bProvider))
			a := agent.New("a", "", agent.WithModel(&queueProvider{id: "test/a", streams: []chat.MessageStream{newStreamBuilder().AddToolCallName("handoff", "handoff").AddToolCallArguments("handoff", `{"agent":"b"}`).AddToolCallStopWithUsage(1, 1).Build()}}), agent.WithHandoffs(b), agent.WithSubAgents(b), agent.WithToolSets(handoff.New()))
			limit := latest.BudgetConfig{MaxTokens: 2}
			if mode == "time" {
				limit = latest.BudgetConfig{MaxTime: latest.Duration{Duration: 30 * time.Millisecond}}
			}
			r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(a, b)), WithCurrentAgent("a"), WithSessionCompaction(false), WithModelStore(emptyCatalogStore{}), WithNamedBudgets(map[string]latest.BudgetConfig{"b": limit}, map[string][]string{"b": {"b"}}))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, r.Close()) })
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			sess := session.New(session.WithAgentName("a"), session.WithNonInteractive(true), session.WithToolsApproved(true), session.WithUserMessage("start"))
			done := make(chan struct{})
			go func() {
				for range r.runExecution(ctx, sess) {
				}
				close(done)
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("handoff did not reach B provider")
			}
			if mode == "shared_tokens" {
				sibling := session.New(session.WithAgentName("a"), session.WithParentID(sess.ID), session.WithAsyncSubagent(true))
				r.subagents.registerChild(sess, "a", "aaaaa", "a", sibling)
				result := r.RunAgent(ctx, agenttool.RunParams{ParentSession: sibling, AgentName: "b", Task: "spend shared wallet"})
				require.Empty(t, result.ErrMsg)
			}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("handed-off execution did not obey B budget")
			}
			require.NoError(t, ctx.Err())
			require.NotNil(t, r.rootBudget(sess).exceededFor("b"))
		})
	}
}

func TestSkillOperationUsesReservedActiveAgentAcrossHandleAges(t *testing.T) {
	skillsSet := skillstool.New([]skills.Skill{{Name: "greet", Context: "fork", InlineContent: "Say hello."}}, "")
	r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(
		agent.New("root", "", agent.WithModel(coordinationReply("root")), agent.WithCommands(types.Commands{"switch": {Agent: "other"}})),
		agent.New("other", "", agent.WithModel(coordinationReply("skill reply")), agent.WithToolSets(skillsSet)),
	)), WithSessionStore(session.NewInMemorySessionStore()), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	h, err := r.CreateSession(t.Context(), session.New(), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	sub, err := h.(SessionLegacyCommandInput).RunLegacyCommandTurn(t.Context(), []LegacyCommandMessage{{Role: chat.MessageRoleUser, Input: TurnInput{Content: "/switch"}}}, "", "switch")
	require.NoError(t, err)
	coordinationAwait(t, h, sub.TurnID)
	newer, err := r.SessionByID(h.ID())
	require.NoError(t, err)
	for i, handle := range []SessionHandle{h, newer} {
		require.Equal(t, "root", handle.(*sessionHandle).agentName, "creation identity must not depend on handle age")
		obs, err := handle.Observe(t.Context(), ObserveOptions{Buffer: 128})
		require.NoError(t, err)
		operationID := []string{"old-handle", "new-handle"}[i]
		require.NoError(t, handle.StartSkillFork(t.Context(), operationID, skillstool.RunSkillArgs{Name: "greet"}))
		statuses := []string{}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		for len(statuses) < 2 {
			select {
			case envelope := <-obs.Events:
				if event, ok := envelope.Event.(*SkillOperationEvent); ok && event.OperationID == operationID {
					require.Equal(t, "other", event.GetAgentName())
					require.Equal(t, handle.ID(), event.SessionID)
					statuses = append(statuses, event.Status)
				}
			case <-ctx.Done():
				t.Fatal("skill operation did not settle")
			}
		}
		require.Equal(t, []string{"accepted", "completed"}, statuses)
		cancel()
		obs.Cancel()
	}
}

func TestSettledReplacementCannotChangeActiveModelBinding(t *testing.T) {
	r := newDriverTestRuntime(t)
	d := r.sessionDrivers.Get(session.New(session.WithID(t.Name()), session.WithAgentName("root")))
	h := &sessionHandle{runtime: r, driver: d, sessionID: d.identityID}
	snapshot, err := d.ownerSnapshot(t.Context())
	require.NoError(t, err)
	next := snapshot.Clone()
	next.SetAgentModelOverride(h.AgentName(), "test/other")
	require.False(t, ReplaceSettledSession(h, next))
	next = snapshot.Clone()
	next.SetAttribute(SessionAgentAttribute, "other")
	require.False(t, ReplaceSettledSession(h, next))
	binding := d.activeBinding()
	require.True(t, ReplaceSettledSession(h, snapshot))
	require.ErrorIs(t, d.validateBinding(t.Context(), binding), &SessionError{Kind: SessionErrorStale})
}

type capabilityBarrierToolset struct {
	entered, release chan struct{}
}

func (s *capabilityBarrierToolset) Tools(context.Context) ([]tools.Tool, error) {
	close(s.entered)
	<-s.release
	return []tools.Tool{{Name: "old_binding"}}, nil
}

type capabilityBarrierProvider struct {
	*mockProvider
	armed            atomic.Bool
	entered, release chan struct{}
}

func (p *capabilityBarrierProvider) ID() modelsdev.ID {
	if p.armed.CompareAndSwap(true, false) {
		close(p.entered)
		<-p.release
	}
	return p.mockProvider.ID()
}

func TestStaleCapabilitiesDiscardResolvedData(t *testing.T) {
	for _, kind := range []string{"tools", "agent-info"} {
		t.Run(kind, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			p := &capabilityBarrierProvider{mockProvider: &mockProvider{id: "test/old-binding"}, entered: entered, release: release}
			a := agent.New("root", "", agent.WithModel(p))
			if kind == "tools" {
				agent.WithToolSets(&capabilityBarrierToolset{entered: entered, release: release})(a)
			}
			r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(a)), WithModelStore(mockModelStore{}))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, r.Close()) })
			handle, err := r.CreateSession(t.Context(), session.New(), SessionBinding{AgentName: "root"})
			require.NoError(t, err)
			h := handle.(*sessionHandle)
			if kind == "agent-info" {
				p.armed.Store(true)
			}
			result := make(chan error, 1)
			go func() {
				if kind == "tools" {
					info, err := h.InspectTools(t.Context())
					assert.Equal(t, SessionToolsInfo{}, info)
					result <- err
				} else {
					info, err := h.SessionAgentInfo(t.Context())
					assert.Equal(t, SessionAgentInfo{}, info)
					result <- err
				}
			}()
			inputBarrier(t, entered)
			h.driver.SetModelBinding("test/new-binding", []provider.Provider{&mockProvider{id: "test/new-binding"}})
			close(release)
			require.ErrorIs(t, inputResult(t, result), &SessionError{Kind: SessionErrorStale})
		})
	}
}
