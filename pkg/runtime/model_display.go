package runtime

import (
	"strconv"
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// cachedModelDatabase never initializes a store or asks it to load metadata.
// Stores without a memory-only projection use the immutable embedded catalog.
func cachedModelDatabase(store ModelStore) *modelsdev.Database {
	if cached, ok := store.(interface{ CachedSnapshot() *modelsdev.Database }); ok {
		if db := cached.CachedSnapshot(); db != nil {
			return db
		}
	}
	return modelsdev.EmbeddedSnapshot()
}

// projectModelDisplay copies only immutable display scalars and owned tier
// slices. cfg must come from the same effective provider snapshot as the model
// identity; a catalog reasoning flag is never used as a cycling capability.
func projectModelDisplay(details *AgentDetails, cfg latest.ModelConfig, db *modelsdev.Database, canSwitch bool) {
	details.ModelID = cfg.Model
	details.ModelName = cfg.Model
	catalogModel, _ := db.LookupModel(modelsdev.NewID(cfg.Provider, cfg.Model))
	if catalogModel != nil && strings.TrimSpace(catalogModel.Name) != "" {
		details.ModelName = catalogModel.Name
	}

	budget := cfg.ThinkingBudget
	model := strings.ToLower(cfg.Model)
	supports := (budget != nil && !budget.IsDisabled()) ||
		modelinfo.UsesReasoningEffort(cfg.Model) || modelinfo.UsesThinkingLevel(cfg.Model) ||
		strings.HasPrefix(model, "gemini-2.5") || modelinfo.IsBedrockClaudeID(cfg.Model) ||
		strings.HasPrefix(model, "claude-") ||
		(catalogModel != nil && modelinfo.IsClaudeFamily(catalogModel.Family))

	details.ThinkingLevels = nil
	details.CanCycleThinking = false
	if supports {
		levels := modelinfo.SupportedThinkingLevels(cfg.Provider, cfg.Model)
		details.ThinkingLevels = make([]string, len(levels))
		for i, level := range levels {
			details.ThinkingLevels[i] = level.String()
		}
		details.CanCycleThinking = canSwitch && len(levels) > 1
	}

	switch {
	case cfg.Model == "":
		details.ThinkingMode, details.ThinkingLevel = "unknown", "unknown"
	case !supports:
		details.ThinkingMode, details.ThinkingLevel = "unsupported", "unsupported"
	case budget == nil:
		details.ThinkingMode, details.ThinkingLevel = "default", "default"
	case budget.IsDisabled():
		details.ThinkingMode, details.ThinkingLevel = "off", "off"
	case budget.IsAdaptive():
		details.ThinkingMode = "adaptive"
		details.ThinkingLevel, _ = budget.AdaptiveEffort()
	case budget.Effort != "":
		if level, ok := budget.EffortLevel(); ok {
			details.ThinkingMode, details.ThinkingLevel = "effort", level.String()
		} else {
			details.ThinkingMode, details.ThinkingLevel = "unknown", "unknown"
		}
	case budget.Tokens == -1:
		details.ThinkingMode, details.ThinkingLevel = "auto", "auto"
	case budget.Tokens > 0:
		details.ThinkingMode, details.ThinkingLevel = "tokens", strconv.Itoa(budget.Tokens)
	default:
		details.ThinkingMode, details.ThinkingLevel = "unknown", "unknown"
	}
}

// ThinkingControl returns the primary reasoning target, preserving compatibility
// with projections where the active model is already the primary binding.
func (details AgentDetails) ThinkingControl() ThinkingDetails {
	if details.PrimaryThinking != nil {
		control := *details.PrimaryThinking
		if control.Mode == "" && control.Level == "" {
			control.Mode, control.Level = "unknown", "unknown"
		}
		return control
	}
	model := details.ModelID
	if model == "" {
		model = details.Model
	}
	if details.Provider != "" {
		model = details.Provider + "/" + model
	}
	return ThinkingDetails{ModelRef: model, Mode: details.ThinkingMode, Level: details.ThinkingLevel, Levels: details.ThinkingLevels, CanCycle: details.CanCycleThinking}
}
