package runtime

import (
	"context"
	"encoding/json"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// recordTeamRevision pins the safe immutable source revision to a canonical
// session after its row exists. Programmatic teams without authored snapshots
// remain compatible and simply have no historical definition to record.
func recordTeamRevision(ctx context.Context, store session.Store, tm *team.Team, sess *session.Session, boundAgent string) error {
	if store == nil || tm == nil || sess == nil {
		return nil
	}
	revisions, ok := store.(session.TeamRevisionStore)
	if !ok {
		return nil
	}
	snapshot := tm.DefinitionSnapshot()
	if snapshot.TeamDigest == "" {
		return nil
	}
	agentEntry, ok := snapshot.AgentDefinition(boundAgent)
	if !ok {
		return nil
	}
	var definition struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.Unmarshal(agentEntry.Payload, &definition); err != nil {
		return err
	}
	id := definition.AgentID
	if cfg, ok := tm.AgentConfig(boundAgent); ok && cfg.ID != "" {
		id = cfg.ID
	}
	payloads := make([]session.TeamDefinitionPayload, 0, len(snapshot.Agents))
	for _, entry := range snapshot.Entries() {
		payloads = append(payloads, session.TeamDefinitionPayload{Digest: entry.Digest, Kind: entry.Kind, Payload: entry.Payload})
	}
	attrs := sess.AttributesSnapshot()
	_, err := revisions.StoreTeamRevision(ctx, session.TeamRevision{
		SessionID: sess.ID, TeamID: snapshot.TeamID, TeamDigest: snapshot.TeamDigest,
		AgentDigest: agentEntry.Digest, AgentKey: boundAgent, AgentID: id,
		AgentName: boundAgent, Origin: attrs["docker-agent.actor.source"],
	}, payloads)
	return err
}
