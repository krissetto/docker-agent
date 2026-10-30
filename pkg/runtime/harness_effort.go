package runtime

import (
	"context"
	"fmt"
	"slices"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/effort"
)

// harnessEffortOverride is one session's effort choice for a harness agent. It
// is session-scoped and keyed by agent name, so it never reaches the shared
// agent configuration and never applies after the session switches agents.
type harnessEffortOverride struct {
	agentName string
	level     effort.Level
	set       bool
}

// harnessEffortLevels are the effort values harness config validation accepts.
var harnessEffortLevels = []effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max}

type harnessEffortContextKey struct{}

// withHarnessEffortOverride pins a session's harness effort on ctx so the run,
// projections and provider construction see the session's choice.
func withHarnessEffortOverride(ctx context.Context, o harnessEffortOverride) context.Context {
	if !o.set {
		return ctx
	}
	return context.WithValue(ctx, harnessEffortContextKey{}, o)
}

// harnessEffortFromContext returns the override pinned on ctx for agentName.
func harnessEffortFromContext(ctx context.Context, agentName string) harnessEffortOverride {
	o, ok := ctx.Value(harnessEffortContextKey{}).(harnessEffortOverride)
	if !ok || o.agentName != agentName {
		return harnessEffortOverride{}
	}
	return o
}

// harnessEffortState returns the levels a harness agent's effort can take and
// its current effort: the session override when one applies, otherwise the
// configured effort, "" while the CLI picks it.
func harnessEffortState(a *agent.Agent, o harnessEffortOverride) ([]effort.Level, effort.Level, error) {
	cfg := a.Harness()
	if cfg == nil {
		return nil, "", fmt.Errorf("agent %q is not a harness: %w", a.Name(), ErrUnsupported)
	}
	if cfg.Type != "claude-code" && cfg.Type != "command-code" {
		return nil, "", fmt.Errorf("harness %q does not support thinking levels: %w", cfg.Type, ErrUnsupported)
	}
	current := effort.Level(cfg.Effort)
	if o.set && o.agentName == a.Name() {
		current = o.level
	}
	return slices.Clone(harnessEffortLevels), current, nil
}

// harnessConfigFor returns the harness configuration a session run uses: the
// agent's configuration with the session's effort applied to a copy.
func harnessConfigFor(ctx context.Context, a *agent.Agent) *latest.HarnessConfig {
	cfg := a.Harness()
	o := harnessEffortFromContext(ctx, a.Name())
	if cfg == nil || !o.set {
		return cfg
	}
	scoped := *cfg
	scoped.Effort = string(o.level)
	return &scoped
}

// harnessThinkingDisplay fills the thinking display of a harness agent under
// ctx's session scope and returns its thinking label ("" when unsupported,
// "adaptive" while the CLI picks the effort).
func (r *LocalRuntime) harnessThinkingDisplay(ctx context.Context, a *agent.Agent, display *AgentDetails) string {
	levels, current, err := harnessEffortState(a, harnessEffortFromContext(ctx, a.Name()))
	if err != nil {
		return ""
	}
	display.ThinkingLevels = make([]string, len(levels))
	for i, level := range levels {
		display.ThinkingLevels[i] = level.String()
	}
	display.CanCycleThinking = r.SupportsModelSwitching() && len(levels) > 1
	if current == "" {
		display.ThinkingMode, display.ThinkingLevel = "adaptive", ""
		return "adaptive"
	}
	display.ThinkingMode, display.ThinkingLevel = "effort", current.String()
	return current.String()
}
