package teamdef

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// AgentDefinition is the authored, display-safe portion of an agent
// configuration retained for immutable history. Credential-bearing connection
// fields and resolved environment values are deliberately not represented.
// Instruction text is authored content and may itself contain a literal secret;
// such text is not heuristically scanned or redacted.
const DefinitionFormatVersion = 1

type AgentDefinition struct {
	FormatVersion  int                  `json:"format_version"`
	TeamID         string               `json:"team_id"`
	AgentID        string               `json:"agent_id"`
	AgentKey       string               `json:"agent_key"`
	Name           string               `json:"name"`
	Description    string               `json:"description,omitempty"`
	WelcomeMessage string               `json:"welcome_message,omitempty"`
	Instruction    string               `json:"instruction,omitempty"`
	Model          string               `json:"model,omitempty"`
	Safety         string               `json:"safety,omitempty"`
	ReadOnly       bool                 `json:"readonly,omitempty"`
	MaxIterations  int                  `json:"max_iterations,omitempty"`
	Harness        string               `json:"harness,omitempty"`
	Toolsets       []ToolsetDefinition  `json:"toolsets,omitempty"`
	SubAgents      []string             `json:"sub_agents,omitempty"`
	Subagents      []latest.SubagentRef `json:"subagents,omitempty"`
	Handoffs       []string             `json:"handoffs,omitempty"`
	ForceHandoff   string               `json:"force_handoff,omitempty"`
	Unavailable    []string             `json:"unavailable,omitempty"`
}

// ToolsetDefinition contains only useful non-credential authored fields.
type ToolsetDefinition struct {
	Type        string   `json:"type,omitempty"`
	Name        string   `json:"name,omitempty"`
	Tools       []string `json:"tools,omitempty"`
	Instruction string   `json:"instruction,omitempty"`
	ReadOnly    bool     `json:"readonly,omitempty"`
}

// DefinitionEntry is the immutable canonical payload stored by digest.
type DefinitionEntry struct {
	Digest  string
	Kind    string
	Payload []byte
}

// DefinitionSnapshot identifies a team's source revision and its independently
// addressable agent definitions. The team payload contains only the roster and
// entry digests, so selected-agent history never needs to hydrate siblings.
type DefinitionSnapshot struct {
	TeamID      string
	TeamDigest  string
	TeamPayload []byte
	TeamEntry   DefinitionEntry
	Agents      map[string]DefinitionEntry // keyed by authored agent key
	Unavailable []string
}

// NewDefinitionSnapshot canonicalizes the roster and computes immutable digests.
func NewDefinitionSnapshot(teamID string, agents []AgentDefinition, unavailable ...string) (DefinitionSnapshot, error) {
	if teamID == "" {
		return DefinitionSnapshot{}, fmt.Errorf("team definition identity is empty")
	}
	entries := make(map[string]DefinitionEntry, len(agents))
	type member struct {
		Key    string `json:"key"`
		ID     string `json:"id"`
		Name   string `json:"name"`
		Digest string `json:"digest"`
	}
	members := make([]member, 0, len(agents))
	for _, authored := range agents {
		if authored.AgentKey == "" || authored.AgentID == "" || authored.Name == "" || authored.TeamID != teamID {
			return DefinitionSnapshot{}, fmt.Errorf("agent definition has incomplete identity")
		}
		if _, duplicate := entries[authored.AgentKey]; duplicate {
			return DefinitionSnapshot{}, fmt.Errorf("duplicate agent definition key %q", authored.AgentKey)
		}
		authored.FormatVersion = DefinitionFormatVersion
		authored.Unavailable = slices.Clone(authored.Unavailable)
		slices.Sort(authored.Unavailable)
		authored.Unavailable = slices.Compact(authored.Unavailable)
		payload, err := json.Marshal(authored)
		if err != nil {
			return DefinitionSnapshot{}, err
		}
		digest := digestDefinition(payload)
		entries[authored.AgentKey] = DefinitionEntry{Digest: digest, Kind: "agent", Payload: payload}
		members = append(members, member{Key: authored.AgentKey, ID: authored.AgentID, Name: authored.Name, Digest: digest})
	}
	slices.SortFunc(members, func(a, b member) int {
		if a.Key < b.Key {
			return -1
		}
		if a.Key > b.Key {
			return 1
		}
		return 0
	})
	slices.Sort(unavailable)
	unavailable = slices.Compact(unavailable)
	teamPayload, err := json.Marshal(struct {
		FormatVersion int      `json:"format_version"`
		TeamID        string   `json:"team_id"`
		Agents        []member `json:"agents"`
		Unavailable   []string `json:"unavailable,omitempty"`
	}{DefinitionFormatVersion, teamID, members, unavailable})
	if err != nil {
		return DefinitionSnapshot{}, err
	}
	teamDigest := digestDefinition(teamPayload)
	roster := DefinitionEntry{Digest: teamDigest, Kind: "team", Payload: slices.Clone(teamPayload)}
	return DefinitionSnapshot{TeamID: teamID, TeamDigest: teamDigest, TeamPayload: teamPayload, TeamEntry: roster, Agents: entries, Unavailable: slices.Clone(unavailable)}, nil
}

func digestDefinition(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// CloneDefinitionEntry returns independent payload storage.
func CloneDefinitionEntry(entry DefinitionEntry) DefinitionEntry {
	entry.Payload = slices.Clone(entry.Payload)
	return entry
}

// AgentDefinition returns the frozen entry named by an authored key.
func (s DefinitionSnapshot) AgentDefinition(key string) (DefinitionEntry, bool) {
	entry, ok := s.Agents[key]
	if !ok || entry.Kind != "agent" {
		return DefinitionEntry{}, false
	}
	return CloneDefinitionEntry(entry), true
}

// TeamDefinitionEntry returns the immutable roster payload.
func (s DefinitionSnapshot) TeamDefinitionEntry() DefinitionEntry {
	return CloneDefinitionEntry(s.TeamEntry)
}

// Entries returns every immutable content-addressed payload in digest order.
func (s DefinitionSnapshot) Entries() []DefinitionEntry {
	entries := make([]DefinitionEntry, 0, len(s.Agents)+1)
	if s.TeamEntry.Digest != "" {
		entries = append(entries, CloneDefinitionEntry(s.TeamEntry))
	}
	for _, entry := range s.Agents {
		entries = append(entries, CloneDefinitionEntry(entry))
	}
	slices.SortFunc(entries, func(a, b DefinitionEntry) int {
		if a.Digest < b.Digest {
			return -1
		}
		if a.Digest > b.Digest {
			return 1
		}
		return 0
	})
	return entries
}

// CloneDefinitionSnapshot returns a deep copy of a snapshot.
func CloneDefinitionSnapshot(s DefinitionSnapshot) DefinitionSnapshot {
	s.TeamPayload = slices.Clone(s.TeamPayload)
	s.TeamEntry = CloneDefinitionEntry(s.TeamEntry)
	s.Unavailable = slices.Clone(s.Unavailable)
	agents := make(map[string]DefinitionEntry, len(s.Agents))
	for key, entry := range s.Agents {
		agents[key] = CloneDefinitionEntry(entry)
	}
	s.Agents = agents
	return s
}
