package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func newTeamDefinitionSQLiteStore(t *testing.T) Store {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewSQLiteSessionStoreFromDB(t.Context(), db)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func buildTestTeamPayloads() ([]TeamDefinitionPayload, TeamRevision) {
	writer := []byte(`{"format_version":1,"team_id":"team","agent_id":"writer","agent_key":"old-key","name":"writer","instruction":"authored"}`)
	sibling := []byte(`{"format_version":1,"team_id":"team","agent_id":"sibling-id","agent_key":"sibling","name":"sibling"}`)
	writerDigest, siblingDigest := definitionDigest(writer), definitionDigest(sibling)
	roster, _ := json.Marshal(struct {
		FormatVersion int    `json:"format_version"`
		TeamID        string `json:"team_id"`
		Agents        []struct {
			Key    string `json:"key"`
			ID     string `json:"id"`
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"agents"`
	}{1, "team", []struct {
		Key    string `json:"key"`
		ID     string `json:"id"`
		Name   string `json:"name"`
		Digest string `json:"digest"`
	}{
		{Key: "old-key", ID: "writer", Name: "writer", Digest: writerDigest}, {Key: "sibling", ID: "sibling-id", Name: "sibling", Digest: siblingDigest},
	}})
	teamDigest := definitionDigest(roster)
	payloads := []TeamDefinitionPayload{{Digest: teamDigest, Kind: "team", Payload: roster}, {Digest: writerDigest, Kind: "agent", Payload: writer}, {Digest: siblingDigest, Kind: "agent", Payload: sibling}}
	revision := TeamRevision{SessionID: "session", TeamID: "team", TeamDigest: teamDigest, AgentDigest: writerDigest, AgentKey: "old-key", AgentID: "writer", AgentName: "writer", Origin: "team.yaml"}
	return payloads, revision
}

func TestTeamRevisionStoreDeduplicatesImmutableEntriesAndLooksUpSelectedAgent(t *testing.T) {
	for name, create := range map[string]func() Store{
		"memory": NewInMemorySessionStore,
		"sqlite": func() Store { return newTeamDefinitionSQLiteStore(t) },
	} {
		t.Run(name, func(t *testing.T) {
			store := create()
			require.NoError(t, store.AddSession(t.Context(), New(WithID("session"))))
			defs, revision := buildTestTeamPayloads()
			revisions := store.(TeamRevisionStore)
			first, err := revisions.StoreTeamRevision(t.Context(), revision, defs)
			require.NoError(t, err)
			assert.EqualValues(t, 1, first.Sequence)
			second, err := revisions.StoreTeamRevision(t.Context(), revision, defs)
			require.NoError(t, err)
			assert.EqualValues(t, 1, second.Sequence, "identical binding write is idempotent")
			latest, err := revisions.LatestTeamRevision(t.Context(), revision.SessionID)
			require.NoError(t, err)
			assert.Equal(t, revision.AgentDigest, latest.AgentDigest)
			stored, err := revisions.TeamAgentDefinition(t.Context(), revision.AgentDigest)
			require.NoError(t, err)
			assert.Equal(t, defs[1].Payload, stored.Payload)
			stored.Payload[0] = 'X'
			again, err := revisions.TeamAgentDefinition(t.Context(), revision.AgentDigest)
			require.NoError(t, err)
			assert.Equal(t, defs[1].Payload, again.Payload)
			_, err = revisions.TeamAgentDefinition(t.Context(), revision.TeamDigest)
			require.ErrorIs(t, err, ErrTeamDefinitionNotFound, "selected-agent lookup does not return team roster payload")
			invalid := append([]TeamDefinitionPayload(nil), defs...)
			invalid[1] = cloneTeamPayload(invalid[1])
			invalid[1].Payload = []byte(`{"different":true}`)
			_, err = revisions.StoreTeamRevision(t.Context(), revision, invalid)
			require.ErrorContains(t, err, "digest does not match")
		})
	}
}

func TestTeamRevisionStoreRequiresVerifiedPayloads(t *testing.T) {
	store := NewInMemorySessionStore().(TeamRevisionStore)
	_, err := store.StoreTeamRevision(context.Background(), TeamRevision{SessionID: "s", TeamID: "t"}, nil)
	require.Error(t, err)
}

func TestTeamRevisionDefinitionLookupAvoidsLoadingRosterPayload(t *testing.T) {
	store := NewInMemorySessionStore()
	defs, revision := buildTestTeamPayloads()
	revision.SessionID = "selected-session"
	require.NoError(t, store.AddSession(t.Context(), New(WithID(revision.SessionID))))
	_, err := store.(TeamRevisionStore).StoreTeamRevision(t.Context(), revision, defs)
	require.NoError(t, err)
	payload, err := store.(TeamRevisionStore).TeamAgentDefinition(t.Context(), revision.AgentDigest)
	require.NoError(t, err)
	assert.Contains(t, string(payload.Payload), "authored")
	assert.NotContains(t, string(payload.Payload), "sibling")
}
