package runtime

import (
	"context"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/team"
)

func (h *sessionHandle) observationPresentation(ctx context.Context, observed driverObservation) []Event {
	return h.observationPresentationForTeam(ctx, observed, h.runtime.teamForSession(observed.session))
}

func (h *sessionHandle) observationPresentationForTeam(ctx context.Context, observed driverObservation, tm *team.Team) []Event {
	if tm == nil {
		return nil
	}
	a, err := tm.Agent(observed.status.AgentName)
	if err != nil || a == nil {
		return nil
	}
	ctx = withHarnessEffortOverride(ctx, observed.harness)
	ctx = agent.WithContextModels(ctx, observed.status.AgentName, observed.models)
	modelID := h.runtime.getEffectiveModelID(ctx, a)
	var limit int64
	if len(observed.models) > 0 {
		model := observed.models[0]
		if cooldown := h.runtime.fallback.cooldowns.Get(a.Name()); cooldown != nil {
			fallbacks := a.FallbackModels()
			if cooldown.fallbackIndex >= 0 && cooldown.fallbackIndex < len(fallbacks) {
				model = fallbacks[cooldown.fallbackIndex]
			}
		}
		limit = declaredContextLimit(model)
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
	info := AgentInfo(a.Name(), label, a.Description(), a.WelcomeMessage(), limit).(*AgentInfoEvent)
	info.ExecutionCapabilities = executionCapabilities(a)
	details := h.runtime.agentDetailsFromTeamForTeam(ctx, tm)
	return []Event{info, TeamInfo(details, a.Name())}
}
