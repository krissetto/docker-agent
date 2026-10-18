package session

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

var (
	ErrTeamDefinitionNotFound = errors.New("team definition not found")
	ErrTeamDefinitionConflict = errors.New("immutable team definition conflict")
)

// TeamDefinitionPayload is one content-addressed, allow-listed immutable payload.
type TeamDefinitionPayload struct {
	Digest  string
	Kind    string
	Payload []byte
}

// TeamRevision binds a session to one team's immutable roster and selected agent.
type TeamRevision struct {
	SessionID   string
	Sequence    int64
	TeamID      string
	TeamDigest  string
	AgentDigest string
	AgentKey    string
	AgentID     string
	AgentName   string
	Origin      string
	CreatedAt   time.Time
}

// TeamRevisionStore persists immutable definitions and per-session bindings.
type TeamRevisionStore interface {
	StoreTeamRevision(context.Context, TeamRevision, []TeamDefinitionPayload) (TeamRevision, error)
	LatestTeamRevision(context.Context, string) (TeamRevision, error)
	TeamAgentDefinition(context.Context, string) (TeamDefinitionPayload, error)
}

func verifyTeamPayloads(revision TeamRevision, payloads []TeamDefinitionPayload) error {
	if revision.SessionID == "" || revision.TeamID == "" || revision.TeamDigest == "" || revision.AgentDigest == "" || revision.AgentKey == "" || revision.AgentID == "" || revision.AgentName == "" {
		return errors.New("team revision identity is incomplete")
	}
	byDigest := make(map[string]TeamDefinitionPayload, len(payloads))
	for _, payload := range payloads {
		if payload.Kind != "team" && payload.Kind != "agent" {
			return fmt.Errorf("unsupported team definition kind %q", payload.Kind)
		}
		if payload.Digest == "" || definitionDigest(payload.Payload) != payload.Digest {
			return errors.New("team definition digest does not match payload")
		}
		if prior, exists := byDigest[payload.Digest]; exists && (prior.Kind != payload.Kind || string(prior.Payload) != string(payload.Payload)) {
			return ErrTeamDefinitionConflict
		}
		byDigest[payload.Digest] = payload
	}
	roster, ok := byDigest[revision.TeamDigest]
	if !ok || roster.Kind != "team" {
		return errors.New("team revision payloads do not contain its roster")
	}
	selected, ok := byDigest[revision.AgentDigest]
	if !ok || selected.Kind != "agent" {
		return errors.New("team revision payloads do not contain selected agent")
	}
	var manifest struct {
		FormatVersion int    `json:"format_version"`
		TeamID        string `json:"team_id"`
		Agents        []struct {
			Key    string `json:"key"`
			ID     string `json:"id"`
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(roster.Payload, &manifest); err != nil {
		return fmt.Errorf("decoding team roster payload: %w", err)
	}
	if manifest.FormatVersion != 1 || manifest.TeamID != revision.TeamID {
		return errors.New("team roster format or identity does not match revision")
	}
	seen := make(map[string]bool, len(manifest.Agents))
	selectedMatch := false
	for _, member := range manifest.Agents {
		if member.Key == "" || member.ID == "" || member.Name == "" || member.Digest == "" || seen[member.Key] {
			return errors.New("team roster has incomplete or duplicate member identity")
		}
		seen[member.Key] = true
		entry, exists := byDigest[member.Digest]
		if !exists || entry.Kind != "agent" {
			return errors.New("team roster references a missing agent definition")
		}
		var definition struct {
			FormatVersion int    `json:"format_version"`
			TeamID        string `json:"team_id"`
			AgentID       string `json:"agent_id"`
			AgentKey      string `json:"agent_key"`
			Name          string `json:"name"`
		}
		if err := json.Unmarshal(entry.Payload, &definition); err != nil {
			return fmt.Errorf("decoding agent definition payload: %w", err)
		}
		if definition.FormatVersion != 1 || definition.TeamID != revision.TeamID || definition.AgentID != member.ID || definition.AgentKey != member.Key || definition.Name != member.Name {
			return errors.New("team roster member differs from immutable agent definition")
		}
		if member.Key == revision.AgentKey && member.ID == revision.AgentID && member.Name == revision.AgentName && member.Digest == revision.AgentDigest {
			selectedMatch = true
		}
	}
	if !selectedMatch {
		return errors.New("selected agent identity does not match immutable team roster")
	}
	return nil
}

func definitionDigest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func sameTeamRevision(a, b TeamRevision) bool {
	return a.SessionID == b.SessionID && a.TeamID == b.TeamID && a.TeamDigest == b.TeamDigest && a.AgentDigest == b.AgentDigest && a.AgentKey == b.AgentKey && a.AgentID == b.AgentID && a.AgentName == b.AgentName && a.Origin == b.Origin
}

func cloneTeamPayload(p TeamDefinitionPayload) TeamDefinitionPayload {
	p.Payload = slices.Clone(p.Payload)
	return p
}

func (s *InMemorySessionStore) StoreTeamRevision(ctx context.Context, revision TeamRevision, payloads []TeamDefinitionPayload) (TeamRevision, error) {
	if err := ctx.Err(); err != nil {
		return TeamRevision{}, err
	}
	if err := verifyTeamPayloads(revision, payloads); err != nil {
		return TeamRevision{}, err
	}
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	if s.teamDefinitions == nil {
		s.teamDefinitions = make(map[string]TeamDefinitionPayload)
	}
	if s.teamRevisions == nil {
		s.teamRevisions = make(map[string][]TeamRevision)
	}
	if _, exists := s.sessions.Load(revision.SessionID); !exists {
		return TeamRevision{}, ErrNotFound
	}
	for _, payload := range payloads {
		if old, ok := s.teamDefinitions[payload.Digest]; ok && (old.Kind != payload.Kind || string(old.Payload) != string(payload.Payload)) {
			return TeamRevision{}, ErrTeamDefinitionConflict
		}
	}
	for _, payload := range payloads {
		if _, ok := s.teamDefinitions[payload.Digest]; !ok {
			s.teamDefinitions[payload.Digest] = cloneTeamPayload(payload)
		}
	}
	history := s.teamRevisions[revision.SessionID]
	if n := len(history); n != 0 {
		last := history[n-1]
		if sameTeamRevision(last, revision) {
			return last, nil
		}
		revision.Sequence = last.Sequence + 1
	} else {
		revision.Sequence = 1
	}
	if revision.CreatedAt.IsZero() {
		revision.CreatedAt = time.Now().UTC()
	}
	s.teamRevisions[revision.SessionID] = append(history, revision)
	return revision, nil
}

func (s *InMemorySessionStore) LatestTeamRevision(ctx context.Context, sessionID string) (TeamRevision, error) {
	if err := ctx.Err(); err != nil {
		return TeamRevision{}, err
	}
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	history := s.teamRevisions[sessionID]
	if len(history) == 0 {
		return TeamRevision{}, ErrTeamDefinitionNotFound
	}
	return history[len(history)-1], nil
}

func (s *InMemorySessionStore) TeamAgentDefinition(ctx context.Context, digest string) (TeamDefinitionPayload, error) {
	if err := ctx.Err(); err != nil {
		return TeamDefinitionPayload{}, err
	}
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	payload, ok := s.teamDefinitions[digest]
	if !ok || payload.Kind != "agent" {
		return TeamDefinitionPayload{}, ErrTeamDefinitionNotFound
	}
	return cloneTeamPayload(payload), nil
}

func (s *SQLiteSessionStore) StoreTeamRevision(ctx context.Context, revision TeamRevision, payloads []TeamDefinitionPayload) (TeamRevision, error) {
	if err := verifyTeamPayloads(revision, payloads); err != nil {
		return TeamRevision{}, err
	}
	tx, err := beginSQLiteWrite(ctx, s.db)
	if err != nil {
		return TeamRevision{}, classifySQLiteContextError(ctx, err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, payload := range payloads {
		if err := insertImmutablePayload(ctx, tx, "team_definition_entries", payload); err != nil {
			return TeamRevision{}, classifySQLiteContextError(ctx, err)
		}
	}
	var last TeamRevision
	last, err = scanTeamRevision(tx.QueryRowContext(ctx, `SELECT session_id, sequence, team_id, team_digest, agent_digest, agent_key, agent_id, agent_name, origin, created_at FROM session_team_revisions WHERE session_id=? ORDER BY sequence DESC LIMIT 1`, revision.SessionID))
	if err == nil {
		if sameTeamRevision(last, revision) {
			if err := tx.Commit(); err != nil {
				return TeamRevision{}, classifySQLiteContextError(ctx, err)
			}
			return last, nil
		}
		revision.Sequence = last.Sequence + 1
	} else if errors.Is(err, sql.ErrNoRows) {
		revision.Sequence = 1
	} else {
		return TeamRevision{}, classifySQLiteContextError(ctx, err)
	}
	if revision.CreatedAt.IsZero() {
		revision.CreatedAt = time.Now().UTC()
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO session_team_revisions(session_id, sequence, team_id, team_digest, agent_digest, agent_key, agent_id, agent_name, origin, created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, revision.SessionID, revision.Sequence, revision.TeamID, revision.TeamDigest, revision.AgentDigest, revision.AgentKey, revision.AgentID, revision.AgentName, revision.Origin, revision.CreatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return TeamRevision{}, classifySQLiteContextError(ctx, err)
	}
	if err = tx.Commit(); err != nil {
		return TeamRevision{}, classifySQLiteContextError(ctx, err)
	}
	return revision, nil
}

func insertImmutablePayload(ctx context.Context, tx *sql.Tx, table string, payload TeamDefinitionPayload) error {
	var existingKind string
	var existing string
	err := tx.QueryRowContext(ctx, `SELECT kind, payload FROM `+table+` WHERE digest=?`, payload.Digest).Scan(&existingKind, &existing)
	if err == nil {
		if existingKind != payload.Kind || existing != string(payload.Payload) {
			return ErrTeamDefinitionConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+table+`(digest, kind, payload) VALUES(?,?,?)`, payload.Digest, payload.Kind, string(payload.Payload))
	return err
}

func (s *SQLiteSessionStore) LatestTeamRevision(ctx context.Context, sessionID string) (TeamRevision, error) {
	revision, err := scanTeamRevision(s.db.QueryRowContext(ctx, `SELECT session_id, sequence, team_id, team_digest, agent_digest, agent_key, agent_id, agent_name, origin, created_at FROM session_team_revisions WHERE session_id=? ORDER BY sequence DESC LIMIT 1`, sessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return TeamRevision{}, ErrTeamDefinitionNotFound
	}
	return revision, classifySQLiteContextError(ctx, err)
}

func (s *SQLiteSessionStore) TeamAgentDefinition(ctx context.Context, digest string) (TeamDefinitionPayload, error) {
	var kind, payload string
	err := s.db.QueryRowContext(ctx, `SELECT kind, payload FROM team_definition_entries WHERE digest=?`, digest).Scan(&kind, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return TeamDefinitionPayload{}, ErrTeamDefinitionNotFound
	}
	if err != nil {
		return TeamDefinitionPayload{}, classifySQLiteContextError(ctx, err)
	}
	if definitionDigest([]byte(payload)) != digest {
		return TeamDefinitionPayload{}, ErrTeamDefinitionConflict
	}
	if kind != "agent" {
		return TeamDefinitionPayload{}, ErrTeamDefinitionNotFound
	}
	return TeamDefinitionPayload{Digest: digest, Kind: kind, Payload: []byte(payload)}, nil
}

func scanTeamRevision(row interface{ Scan(...any) error }) (TeamRevision, error) {
	var revision TeamRevision
	var created string
	err := row.Scan(&revision.SessionID, &revision.Sequence, &revision.TeamID, &revision.TeamDigest, &revision.AgentDigest, &revision.AgentKey, &revision.AgentID, &revision.AgentName, &revision.Origin, &created)
	if err != nil {
		return TeamRevision{}, err
	}
	revision.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	return revision, err
}

var _ TeamRevisionStore = (*InMemorySessionStore)(nil)
var _ TeamRevisionStore = (*SQLiteSessionStore)(nil)
