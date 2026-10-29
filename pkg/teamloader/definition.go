package teamloader

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/teamdef"
)

// authoredDefinitionSnapshot builds a display-safe snapshot from config.Load's
// parsed author source before model resolution, environment expansion or tool
// construction. It keeps selected-agent instructions but excludes provider
// definitions and all connection/auth/tool config that may contain secrets.
func authoredDefinitionSnapshot(cfg *latest.Config, sourceIdentity string, external []teamdef.AgentDefinition) (teamdef.DefinitionSnapshot, error) {
	teamID := cfg.ID
	if teamID == "" {
		teamID = teamdef.SourceIdentity(sourceIdentity)
	}
	members := make([]teamdef.AgentDefinition, 0, len(cfg.Agents)+len(external))
	unavailable := make([]string, 0)
	for _, a := range cfg.Agents {
		id := a.ID
		if id == "" {
			id = a.Name
		}
		definition := teamdef.AgentDefinition{
			TeamID: teamID, AgentID: id, AgentKey: a.Name, Name: a.Name,
			Description: a.Description, WelcomeMessage: a.WelcomeMessage, Instruction: a.Instruction,
			Model: safeModelRef(a.Model), Safety: string(a.Safety), ReadOnly: a.ReadOnly, MaxIterations: a.MaxIterations,
			SubAgents: safeReferences(a.SubAgents), Subagents: safeSubagentRefs(a.Subagents), SubagentAccess: cloneAccess(a.SubagentAccess),
			Handoffs: safeReferences(a.Handoffs), ForceHandoff: safeReference(a.ForceHandoff),
		}
		if a.Harness != nil {
			definition.Harness = a.Harness.Type
		}
		for _, toolset := range a.Toolsets {
			definition.Toolsets = append(definition.Toolsets, teamdef.ToolsetDefinition{Type: toolset.Type, Name: toolset.Name, Tools: append([]string(nil), toolset.Tools...), Instruction: toolset.Instruction, ReadOnly: toolset.ReadOnly})
		}
		for path, present := range map[string]bool{
			"fallback": a.Fallback != nil, "skills": a.Skills.Enabled(), "commands": len(a.Commands) != 0,
			"hooks": a.Hooks != nil, "additional_options": a.AddEnvironmentInfo || a.AddDate || a.AddDescriptionParameter || a.CodeModeTools || a.AddPromptFilesDepth != 0 || a.NumHistoryItems != 0,
			"toolset_connection_auth_config": len(a.Toolsets) != 0,
		} {
			if present {
				path = "agents." + a.Name + "." + path
				definition.Unavailable = append(definition.Unavailable, path)
				unavailable = append(unavailable, path)
			}
		}
		members = append(members, definition)
	}
	for i := range external {
		external[i].TeamID = teamID
		if external[i].AgentID == "" {
			external[i].AgentID = external[i].Name
		}
		external[i].Unavailable = append(external[i].Unavailable, "external_definition_graph")
		path := "agents." + external[i].AgentKey + ".external_definition_graph"
		unavailable = append(unavailable, path)
	}
	members = append(members, external...)
	for _, item := range []struct {
		path    string
		present bool
	}{
		{"providers", len(cfg.Providers) != 0}, {"models", len(cfg.Models) != 0}, {"toolsets", len(cfg.Toolsets) != 0},
		{"mcps", len(cfg.MCPs) != 0}, {"rag", len(cfg.RAG) != 0}, {"commands", len(cfg.Commands) != 0},
		{"skills", len(cfg.Skills) != 0}, {"permissions", cfg.Permissions != nil}, {"runtime", cfg.Runtime != nil},
		{"budget", cfg.Budget != nil || len(cfg.Budgets) != 0},
	} {
		if item.present {
			unavailable = append(unavailable, item.path)
		}
	}
	snapshot, err := teamdef.NewDefinitionSnapshot(teamID, members, unavailable...)
	if err != nil {
		return teamdef.DefinitionSnapshot{}, fmt.Errorf("snapshotting team definition: %w", err)
	}
	return snapshot, nil
}

func safeReferences(refs []string) []string {
	out := make([]string, len(refs))
	for i, ref := range refs {
		out[i] = safeReference(ref)
	}
	return out
}

func safeSubagentRefs(refs latest.SubagentRefs) latest.SubagentRefs {
	out := append(latest.SubagentRefs(nil), refs...)
	for i := range out {
		out[i].Agent = safeReference(out[i].Agent)
	}
	return out
}

func safeReference(ref string) string {
	if strings.Contains(ref, "://") {
		return safeURL(ref)
	}
	return ref
}

func safeModelRef(model string) string {
	if model == "" {
		return ""
	}
	if strings.Contains(model, "://") {
		return safeURL(model)
	}
	return model
}

func safeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[redacted-url]"
	}
	if u.User != nil {
		u.User = url.User("redacted")
	}
	if u.RawQuery != "" {
		u.RawQuery = "redacted"
	}
	if u.Fragment != "" {
		u.Fragment = "redacted"
	}
	return u.String()
}

func cloneAccess(access *latest.SubagentAccessConfig) *latest.SubagentAccessConfig {
	if access == nil {
		return nil
	}
	clone := *access
	return &clone
}
