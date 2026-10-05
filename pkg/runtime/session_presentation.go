package runtime

import (
	"context"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

func (h *sessionHandle) observationPresentation(ctx context.Context, observed driverObservation) []Event {
	if h.runtime.team == nil {
		return nil
	}
	a, err := h.runtime.team.Agent(observed.status.AgentName)
	if err != nil || a == nil {
		return nil
	}
	ctx = agent.WithContextModels(ctx, observed.status.AgentName, observed.models)
	modelID := modelsdev.ID{}
	var limit int64
	if len(observed.models) > 0 {
		model := observed.models[0]
		modelID = model.ID()
		limit = providerContextLimit(model)
		if limit == 0 {
			if metadata, ok := cachedModelDatabase(h.runtime.modelsStore).LookupModel(modelID); ok {
				limit = int64(metadata.Limit.Context)
			}
		}
	}
	label := modelID.String()
	if a.HasHarness() {
		label = agentModelLabel(ctx, a)
		limit = 0
	}
	info := AgentInfo(a.Name(), label, a.Description(), a.WelcomeMessage(), limit)
	var details []AgentDetails
	for _, name := range h.runtime.team.AgentNames() {
		member, err := h.runtime.team.Agent(name)
		if err != nil {
			continue
		}
		models := member.ConfiguredModels()
		if name == observed.status.AgentName {
			models = observed.models
		}
		detail := AgentDetails{Name: name, Description: member.Description(), Commands: member.Commands(), ThinkingMode: "unknown", ThinkingLevel: "unknown"}
		if len(models) > 0 {
			model := models[0]
			id := model.ID()
			cfg := model.BaseConfig().ModelConfig
			if cfg.Provider == "" {
				cfg.Provider = id.Provider
			}
			if cfg.Model == "" {
				cfg.Model = id.Model
			}
			detail.Provider, detail.Model = id.Provider, id.Model
			projectModelDisplay(&detail, cfg, cachedModelDatabase(h.runtime.modelsStore), h.runtime.SupportsModelSwitching())
		} else if member.HasHarness() {
			detail.Model, detail.ModelName = member.HarnessType(), member.HarnessType()
		}
		details = append(details, detail)
	}
	return []Event{info, TeamInfo(details, a.Name())}
}
