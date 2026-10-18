package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamdef"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/teamedit"
)

type teamEditPersistenceProvider struct {
	*mockProvider
	mu       sync.Mutex
	calls    int
	action   string
	revision func() string
	path     string
	offered  [][]string
	seenText []string
}

func (p *teamEditPersistenceProvider) CreateChatCompletionStream(_ context.Context, messages []chat.Message, available []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	for _, message := range messages {
		p.seenText = append(p.seenText, message.Content)
	}
	var names []string
	for _, tool := range available {
		names = append(names, tool.Name)
	}
	p.offered = append(p.offered, names)
	switch p.calls {
	case 1:
		args := fmt.Sprintf(`{"action":"apply","revision":%q,"agent_key":"root","description":"persisted description"}`, p.revision())
		return newStreamBuilder().AddToolCallName("apply", teamedit.ToolName).AddToolCallArguments("apply", args).AddToolCallStopWithUsage(1, 1).Build(), nil
	case 2:
		if p.action == "persist" || strings.HasPrefix(p.action, "managed") || p.action == "persist-conflict" {
			args := fmt.Sprintf(`{"revision":%q}`, p.revision())
			return newStreamBuilder().AddToolCallName("persist", teamedit.PersistToolName).AddToolCallArguments("persist", args).AddToolCallStopWithUsage(1, 1).Build(), nil
		}
		args := fmt.Sprintf(`{"revision":%q,"path":%q}`, p.revision(), p.path)
		return newStreamBuilder().AddToolCallName("export", teamedit.ExportToolName).AddToolCallArguments("export", args).AddToolCallStopWithUsage(1, 1).Build(), nil
	default:
		return newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build(), nil
	}
}

func TestTeamEditPersistenceBoundSessionNormalDispatch(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"persist", "export", "managed", "managed-conflict", "persist-conflict"} {
		t.Run(action, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "team.yaml")
			yaml := `version: "16"
id: team-edit-persist
# Authored secrets must never be returned by persistence or export tools.
# PRIVATE_TEAM_YAML_SENTINEL
agents:
  root:
    id: root-stable
    model: openai/test
    instruction: Before
    toolsets:
      - type: team_edit
`
			require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))
			source := config.NewFileSource(path)
			runtimeProvider := &teamEditPersistenceProvider{mockProvider: &mockProvider{id: "test/team-edit-persistence"}, action: action, path: filepath.Join(dir, "exported.yaml")}
			options := teamdef.EditableSourceOptions{Source: source, Authority: teamdef.EditAuthorityRestricted, ExportRoot: dir}
			if action == "persist" || action == "persist-conflict" {
				options.WritablePath = path
			}
			editor, err := teamdef.NewEditor(t.Context(), teamdef.EditableSourceOptions{
				Source: source, Authority: options.Authority, WritablePath: options.WritablePath, ExportRoot: options.ExportRoot,
				BuildValidator: func(ctx context.Context, candidate config.Source) error {
					_, err := config.Load(ctx, candidate)
					return err
				},
			})
			require.NoError(t, err)
			runtimeProvider.revision = func() string { return editor.CurrentRevision().ID }

			build := func(ctx context.Context, candidate config.Source, revision, sourceID, baselineID string, _ *config.RuntimeConfig, _ []teamloader.Opt) (*TeamEnvironment, error) {
				cfg, err := config.Load(ctx, candidate)
				if err != nil {
					return nil, err
				}
				var root *agent.Agent
				for _, agentConfig := range cfg.Agents {
					if agentConfig.Name == "root" {
						root = agent.New(agentConfig.Name, agentConfig.Instruction, agent.WithModel(runtimeProvider))
						break
					}
				}
				if root == nil {
					return nil, fmt.Errorf("root agent missing")
				}
				tm := team.New(team.WithAgents(root), team.WithAgentConfigs(map[string]latest.AgentConfig{"root": cfg.Agents[0]}), team.WithPermissions(permissions.NewCheckerFromRules(nil, nil, nil)))
				return NewTeamEnvironment(revision, sourceID, baselineID, cfg.ID, tm, ModelSwitcherConfig{}, func(ctx context.Context) error { return tm.StopToolSets(ctx) })
			}
			initial, err := build(t.Context(), source, editor.CurrentRevision().ID, "source-persistence", editor.BaselineRevisionID(), nil, nil)
			require.NoError(t, err)
			environments, err := NewTeamEnvironmentStore(initial)
			require.NoError(t, err)
			builder := func(ctx context.Context, candidate config.Source, revision, sourceID, baselineID string, runConfig *config.RuntimeConfig, opts []teamloader.Opt) (*TeamEnvironment, error) {
				return build(ctx, candidate, revision, sourceID, baselineID, runConfig, opts)
			}
			persistence := &TeamEditPersistence{}
			managedCalls := 0
			if strings.HasPrefix(action, "managed") {
				persistence.ExpectedSourceRevision = "managed-r1"
				persistence.Persist = func(ctx context.Context, expected string, candidate config.Source) (string, error) {
					managedCalls++
					require.Equal(t, "managed-r1", expected)
					data, err := candidate.Read(ctx)
					require.NoError(t, err)
					require.Contains(t, string(data), "persisted description")
					require.Contains(t, string(data), "PRIVATE_TEAM_YAML_SENTINEL")
					if action == "managed-conflict" {
						return "", fmt.Errorf("managed source revision conflict")
					}
					return "managed-r2", nil
				}
			}
			rt, err := NewLocalRuntime(t.Context(), initial.Team(), WithSessionStore(session.NewInMemorySessionStore()), WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithTeamEditor(editor, environments, builder, config.TeamEditGrant{Mode: "restricted"}), WithTeamEditPersistence(persistence, dir))
			require.NoError(t, err)
			owner := NewSessionRuntimeSupervisor(rt)
			t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
			handle, err := owner.Runtime().CreateSession(t.Context(), session.New(session.WithID("team-edit-persist-"+action), session.WithSafetyPolicy(session.SafetyPolicyAutonomous)), SessionBinding{AgentName: "root"})
			require.NoError(t, err)

			first, err := handle.Submit(t.Context(), TurnInput{Content: "edit the team"})
			require.NoError(t, err)
			require.NoError(t, handle.AwaitTurn(t.Context(), first.TurnID))
			if action == "persist-conflict" {
				require.NoError(t, os.WriteFile(path, []byte("external writer wins\n"), 0o600))
			}
			second, err := handle.Submit(t.Context(), TurnInput{Content: "now " + action + " this revision"})
			require.NoError(t, err)
			require.NoError(t, handle.AwaitTurn(t.Context(), second.TurnID))
			runtimeProvider.mu.Lock()
			offered := append([][]string(nil), runtimeProvider.offered...)
			runtimeProvider.mu.Unlock()
			require.GreaterOrEqual(t, len(offered), 3)
			require.Contains(t, offered[0], teamedit.PersistToolName)
			require.Contains(t, offered[0], teamedit.ExportToolName)
			runtimeProvider.mu.Lock()
			seenText := strings.Join(runtimeProvider.seenText, "\n")
			runtimeProvider.mu.Unlock()
			require.NotContains(t, seenText, "PRIVATE_TEAM_YAML_SENTINEL", "tool receipts must not expose authored YAML")
			if strings.HasPrefix(action, "managed") {
				if action == "managed" {
					require.Equal(t, "managed-r2", persistence.ExpectedSourceRevision)
				} else {
					require.Equal(t, "managed-r1", persistence.ExpectedSourceRevision)
				}
				require.Equal(t, 1, managedCalls)
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Contains(t, string(data), "Before", "managed persistence must not write the frozen/local input file")
			} else if action == "persist-conflict" {
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, "external writer wins\n", string(data))
			} else if action == "persist" {
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Contains(t, string(data), "persisted description")
				require.NotContains(t, string(data), "${")
			} else {
				data, err := os.ReadFile(runtimeProvider.path)
				require.NoError(t, err)
				require.Contains(t, string(data), "persisted description")
				info, err := os.Stat(runtimeProvider.path)
				require.NoError(t, err)
				require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
				_, err = exportTeamEditSource(editor, editor.CurrentRevision(), runtimeProvider.path)
				require.Error(t, err, "export must not overwrite an existing destination")
			}
		})
	}
}
