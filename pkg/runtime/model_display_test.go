package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/team"
)

type displayPoisonStore struct{}

func (displayPoisonStore) GetModel(context.Context, modelsdev.ID) (*modelsdev.Model, error) {
	panic("display fetched model")
}

func (displayPoisonStore) GetDatabase(context.Context) (*modelsdev.Database, error) {
	panic("display loaded database")
}

func TestCachedModelDatabaseNeverLoads(t *testing.T) {
	t.Parallel()
	assert.Same(t, modelsdev.EmbeddedSnapshot(), cachedModelDatabase(displayPoisonStore{}))
	assert.Same(t, modelsdev.EmbeddedSnapshot(), cachedModelDatabase(nil))
	lazy := &lazyModelStore{}
	assert.Same(t, modelsdev.EmbeddedSnapshot(), cachedModelDatabase(lazy))
	assert.Nil(t, lazy.st.Load(), "display must not initialize the lazy store")
	db := &modelsdev.Database{}
	lazy.st.Store(modelsdev.NewDatabaseStore(db))
	assert.Same(t, db, cachedModelDatabase(lazy))
}

func TestProjectModelDisplayBudgetModes(t *testing.T) {
	t.Parallel()
	db := &modelsdev.Database{}
	for _, tc := range []struct {
		name   string
		model  string
		budget *latest.ThinkingBudget
		mode   string
		level  string
		cycle  bool
	}{
		{"default", "claude-sonnet-4-6", nil, "default", "default", true},
		{"off tokens", "claude-sonnet-4-6", &latest.ThinkingBudget{}, "off", "off", true},
		{"off effort", "claude-sonnet-4-6", &latest.ThinkingBudget{Effort: "none"}, "off", "off", true},
		{"adaptive", "claude-sonnet-4-6", &latest.ThinkingBudget{Effort: "adaptive"}, "adaptive", "high", true},
		{"adaptive max", "claude-sonnet-4-6", &latest.ThinkingBudget{Effort: "adaptive/max"}, "adaptive", "max", true},
		{"effort", "claude-sonnet-4-6", &latest.ThinkingBudget{Effort: "high"}, "effort", "high", true},
		{"tokens", "claude-sonnet-4-6", &latest.ThinkingBudget{Tokens: 2048}, "tokens", "2048", true},
		{"auto", "gemini-2.5-pro", &latest.ThinkingBudget{Tokens: -1}, "auto", "auto", true},
		{"unknown budget", "claude-sonnet-4-6", &latest.ThinkingBudget{Effort: "invalid"}, "unknown", "unknown", true},
		{"unsupported", "plain-chat", nil, "unsupported", "unsupported", false},
		{"unknown model", "", nil, "unknown", "unknown", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			details := AgentDetails{Model: "display alias", Thinking: "legacy"}
			cfg := latest.ModelConfig{Provider: "anthropic", Model: tc.model, ThinkingBudget: tc.budget}
			projectModelDisplay(&details, cfg, db, true)
			assert.Equal(t, tc.mode, details.ThinkingMode)
			assert.Equal(t, tc.level, details.ThinkingLevel)
			assert.Equal(t, tc.cycle, details.CanCycleThinking)
			assert.Equal(t, "display alias", details.Model)
			assert.Equal(t, "legacy", details.Thinking)
			if tc.cycle {
				levels := modelinfo.SupportedThinkingLevels(cfg.Provider, cfg.Model)
				require.Len(t, details.ThinkingLevels, len(levels))
				for i, level := range levels {
					assert.Equal(t, level.String(), details.ThinkingLevels[i])
				}
			}
			projectModelDisplay(&details, cfg, db, false)
			assert.False(t, details.CanCycleThinking, "no switcher means no cycle action")
		})
	}
}

func TestProjectModelDisplayCanonicalIdentity(t *testing.T) {
	t.Parallel()
	db := &modelsdev.Database{Providers: map[string]modelsdev.Provider{
		"amazon-bedrock": {Models: map[string]modelsdev.Model{
			"anthropic.claude-sonnet-4-6": {Name: "Claude Sonnet 4.6", Family: "claude"},
			"reasoning-flag-only":         {Name: "Reasoning Flag", Reasoning: true},
		}},
	}}
	cfg := latest.ModelConfig{Provider: "amazon-bedrock", Model: "eu.anthropic.claude-sonnet-4-6"}
	details := AgentDetails{Model: "friendly config alias"}
	projectModelDisplay(&details, cfg, db, true)
	assert.Equal(t, cfg.Model, details.ModelID)
	assert.Equal(t, "Claude Sonnet 4.6", details.ModelName)
	assert.Equal(t, "friendly config alias", details.Model)
	assert.True(t, details.CanCycleThinking)
	firstLevels := details.ThinkingLevels
	projectModelDisplay(&details, cfg, db, true)
	firstLevels[0] = "mutated"
	assert.NotEqual(t, "mutated", details.ThinkingLevels[0])

	cfg.Model = "reasoning-flag-only"
	projectModelDisplay(&details, cfg, db, true)
	assert.Equal(t, "unsupported", details.ThinkingMode)
	assert.False(t, details.CanCycleThinking, "catalog reasoning is not effort-cycle capability")
	assert.Empty(t, details.ThinkingLevels)
}

func TestAgentDisplayPreservesProviderIdentityWithoutBaseConfig(t *testing.T) {
	t.Parallel()
	db := &modelsdev.Database{Providers: map[string]modelsdev.Provider{
		"test": {Models: map[string]modelsdev.Model{
			"model-id": {Name: "Friendly model"},
		}},
	}}
	root := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/model-id"}))
	r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithModelStore(modelsdev.NewDatabaseStore(db)))
	require.NoError(t, err)
	details := r.agentDetailsFromTeam(t.Context())
	require.Len(t, details, 1)
	assert.Equal(t, "test", details[0].Provider)
	assert.Equal(t, "model-id", details[0].Model)
	assert.Equal(t, "model-id", details[0].ModelID)
	assert.Equal(t, "Friendly model", details[0].ModelName)
	assert.Equal(t, "unknown", details[0].ThinkingMode)
	assert.Equal(t, "unknown", details[0].ThinkingLevel)
	assert.False(t, details[0].CanCycleThinking)
	assert.Empty(t, details[0].ThinkingLevels)
}

func TestPopulateCatalogMetadataUsesCacheAndPreservesAlias(t *testing.T) {
	t.Parallel()
	r := &LocalRuntime{modelsStore: displayPoisonStore{}}
	choice := ModelChoice{Name: "configured", Ref: "configured", Model: "display alias"}
	r.populateCatalogMetadata(t.Context(), &choice, "openai", "gpt-4o")
	assert.Equal(t, "gpt-4o", choice.ModelID)
	assert.NotEmpty(t, choice.ModelName)
	assert.Equal(t, "display alias", choice.Model)
	assert.Equal(t, "configured", choice.Ref)
	assert.Equal(t, "configured", choice.Name)
}

func TestFallbackPrimaryThinkingProjectionJSONRoundTrip(t *testing.T) {
	original := AgentDetails{
		Name: "root", Provider: "openai", ModelID: "gpt-4o", ModelName: "GPT-4o",
		ThinkingMode: "unsupported", ThinkingLevel: "unsupported",
		PrimaryThinking: &ThinkingDetails{ModelRef: "openai/gpt-5", Mode: "effort", Level: "high", Levels: []string{"low", "high"}, CanCycle: true},
	}
	data, err := json.Marshal(original)
	require.NoError(t, err)
	var remote AgentDetails
	require.NoError(t, json.Unmarshal(data, &remote))
	assert.Equal(t, original, remote)
	assert.Equal(t, "openai/gpt-5", remote.ThinkingControl().ModelRef)
	assert.True(t, remote.ThinkingControl().CanCycle)
	assert.False(t, remote.CanCycleThinking)
	assert.Equal(t, "unsupported", remote.ThinkingLevel)
	remote.PrimaryThinking = nil
	data, err = json.Marshal(remote)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "primary_thinking", "ordinary wire shape remains unchanged")
}
