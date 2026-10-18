package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/docker/docker-agent/pkg/chat"
)

const (
	recentChatMetadataFieldLimit = 4096
	recentChatAttributesLimit    = 32 * 1024
	recentChatAgentFieldLimit    = 256
	recentChatMaxItems           = 256
	recentChatMaxScan            = 512
	recentChatMaxItemText        = 16 * 1024
)

var ErrRecentChatMetadataTooLarge = errors.New("recent-chat metadata exceeds its bounded field limit")

// RecentChatMetadata contains only identity and workspace fields needed to
// authorize a bounded transcript read. It never includes session state/history.
type RecentChatMetadata struct {
	ID              string
	ParentID        string
	WorkingDir      string
	Source          string
	AgentID         string
	AgentName       string
	ParentAgentID   string
	ParentAgentName string
}

type RecentChatMessage struct {
	ID        int64
	Position  int
	Role      string
	AgentName string
	Text      string
	Truncated bool
}

type RecentChatChild struct {
	RootSessionID   string
	ParentSessionID string
	NodeID          string
	NodeParent      string
	NodeSessionID   string
	NodeAgent       string
}

type RecentChatItems struct {
	Messages           []RecentChatMessage
	HasMore            bool
	NextBeforePosition int
	NextBeforeItemID   int64
}

// RecentChatStore offers bounded, non-hydrating identity and transcript reads.
type RecentChatStore interface {
	ReadRecentChatMetadata(context.Context, string) (*RecentChatMetadata, error)
	ReadRecentChatItems(context.Context, string, int, int64, int, int, int) (RecentChatItems, error)
	ReadRecentChatChild(context.Context, string) (RecentChatChild, error)
	ValidateRecentChatAnchor(context.Context, string, int, int64) error
}

func (s *InMemorySessionStore) ReadRecentChatMetadata(ctx context.Context, id string) (*RecentChatMetadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if id == "" {
		return nil, ErrEmptyID
	}
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	stored, ok := s.sessions.Load(id)
	if !ok {
		return nil, ErrNotFound
	}
	stored.mu.RLock()
	defer stored.mu.RUnlock()
	attrs := stored.Attributes
	metadata := &RecentChatMetadata{
		ID: stored.ID, ParentID: stored.ParentID, WorkingDir: stored.WorkingDir,
		Source: attrs["docker-agent.actor.source"], AgentID: attrs["docker-agent.actor.agent_id"],
		AgentName: attrs["docker-agent.actor.agent"], ParentAgentID: attrs["docker-agent.actor.parent_agent_id"],
		ParentAgentName: attrs["docker-agent.actor.parent_agent"],
	}
	if err := validateRecentChatMetadata(metadata); err != nil {
		return nil, err
	}
	return metadata, nil
}

func (s *SQLiteSessionStore) ReadRecentChatMetadata(ctx context.Context, id string) (*RecentChatMetadata, error) {
	if id == "" {
		return nil, ErrEmptyID
	}
	const fieldLimit = recentChatMetadataFieldLimit + 1
	query := `SELECT CAST(substr(CAST(id AS BLOB), 1, ? ) AS BLOB), CAST(substr(CAST(COALESCE(parent_id, '') AS BLOB), 1, ?) AS BLOB), CAST(substr(CAST(COALESCE(working_dir, '') AS BLOB), 1, ?) AS BLOB), CAST(substr(CAST(COALESCE(attributes, '{}') AS BLOB), 1, ?) AS BLOB) FROM sessions WHERE id = ?`
	var rowID, parent, working, attributes []byte
	err := s.db.QueryRowContext(ctx, query, fieldLimit, fieldLimit, fieldLimit, recentChatAttributesLimit+1, id).Scan(&rowID, &parent, &working, &attributes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, classifySQLiteContextError(ctx, err)
	}
	if len(attributes) > recentChatAttributesLimit || len(rowID) > recentChatMetadataFieldLimit || len(parent) > recentChatMetadataFieldLimit || len(working) > recentChatMetadataFieldLimit {
		return nil, ErrRecentChatMetadataTooLarge
	}
	var attrs map[string]string
	if err := json.Unmarshal(attributes, &attrs); err != nil {
		return nil, fmt.Errorf("reading bounded recent-chat metadata attributes: %w", err)
	}
	metadata := &RecentChatMetadata{
		ID: string(rowID), ParentID: string(parent), WorkingDir: string(working),
		Source: attrs["docker-agent.actor.source"], AgentID: attrs["docker-agent.actor.agent_id"],
		AgentName: attrs["docker-agent.actor.agent"], ParentAgentID: attrs["docker-agent.actor.parent_agent_id"],
		ParentAgentName: attrs["docker-agent.actor.parent_agent"],
	}
	if err := validateRecentChatMetadata(metadata); err != nil {
		return nil, err
	}
	return metadata, nil
}

func validateRecentChatMetadata(metadata *RecentChatMetadata) error {
	if metadata == nil || metadata.ID == "" || len(metadata.ID) > recentChatMetadataFieldLimit ||
		len(metadata.ParentID) > recentChatMetadataFieldLimit || len(metadata.WorkingDir) > recentChatMetadataFieldLimit ||
		len(metadata.Source) > recentChatMetadataFieldLimit || len(metadata.AgentID) > recentChatMetadataFieldLimit ||
		len(metadata.AgentName) > recentChatAgentFieldLimit || len(metadata.ParentAgentID) > recentChatMetadataFieldLimit ||
		len(metadata.ParentAgentName) > recentChatAgentFieldLimit {
		return ErrRecentChatMetadataTooLarge
	}
	return nil
}

func (s *InMemorySessionStore) ReadRecentChatItems(ctx context.Context, id string, beforePosition int, beforeItemID int64, maxMessages, maxScanned, maxTextBytes int) (RecentChatItems, error) {
	if err := validateRecentChatRead(ctx, id, beforePosition, beforeItemID, maxMessages, maxScanned, maxTextBytes); err != nil {
		return RecentChatItems{}, err
	}
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	stored, ok := s.sessions.Load(id)
	if !ok {
		return RecentChatItems{}, ErrNotFound
	}
	stored.mu.RLock()
	defer stored.mu.RUnlock()
	if beforeItemID != 0 {
		beforePosition = -1
		for position := range stored.Messages {
			if item := stored.Messages[position]; item.Message != nil && item.Message.ID == beforeItemID {
				beforePosition = position
				break
			}
		}
		if beforePosition < 0 {
			return RecentChatItems{}, ErrRecentChatCursorStale
		}
	}

	var result RecentChatItems
	scanned := 0
	for position := len(stored.Messages) - 1; position >= 0 && scanned < maxScanned; position-- {
		if err := ctx.Err(); err != nil {
			return RecentChatItems{}, err
		}
		if (beforeItemID != 0 || beforePosition > 0) && position >= beforePosition {
			continue
		}
		scanned++
		item := &stored.Messages[position]
		if item.Message == nil || item.Message.Implicit || item.Message.Pending {
			continue
		}
		message := item.Message
		role := message.Message.Role
		if role != chat.MessageRoleUser && role != chat.MessageRoleAssistant && role != chat.MessageRoleTool {
			continue
		}
		text, wasTruncated := recentChatDisplayText(message.Message, maxTextBytes)
		text, clipped := truncateRecentChatText(text, maxTextBytes)
		wasTruncated = wasTruncated || clipped
		result.Messages = append(result.Messages, RecentChatMessage{ID: message.ID, Position: position, Role: string(role), AgentName: truncateRecentChatAgent(message.AgentName), Text: text, Truncated: wasTruncated})
		if len(result.Messages) == maxMessages {
			result.HasMore = position > 0
			if result.HasMore {
				result.NextBeforePosition, result.NextBeforeItemID = position, message.ID
			}
			break
		}
	}
	if !result.HasMore && scanned == maxScanned {
		boundary := len(stored.Messages) - scanned
		if beforeItemID != 0 || beforePosition > 0 {
			boundary = beforePosition - scanned
		}
		if boundary > 0 {
			result.HasMore = true
			result.NextBeforePosition = boundary
			if item := stored.Messages[boundary].Message; item != nil {
				result.NextBeforeItemID = item.ID
			}
		}
	}
	return result, nil
}

func (s *SQLiteSessionStore) ReadRecentChatItems(ctx context.Context, id string, beforePosition int, beforeItemID int64, maxMessages, maxScanned, maxTextBytes int) (RecentChatItems, error) {
	if err := validateRecentChatRead(ctx, id, beforePosition, beforeItemID, maxMessages, maxScanned, maxTextBytes); err != nil {
		return RecentChatItems{}, err
	}
	const displayText = `CASE
		WHEN json_extract(message_json, '$.role') = 'assistant' AND json_type(message_json, '$.presentation') = 'array' THEN COALESCE((
			SELECT group_concat(text, '') FROM (
				SELECT CAST(substr(CAST(COALESCE(json_extract(value, '$.text'), '') AS BLOB), 1, ?) AS BLOB) AS text
				FROM json_each(message_json, '$.presentation')
				WHERE json_extract(value, '$.type') = 'content' ORDER BY CAST(key AS INTEGER) LIMIT 16
			)
		), '')
		ELSE COALESCE(NULLIF(json_extract(message_json, '$.content'), ''), json_extract(message_json, '$.multi_content[0].text'), '') END`
	const query = `SELECT id, position, item_type, COALESCE(implicit, 0), COALESCE(actor_pending, 0), ` +
		`CAST(substr(CAST(COALESCE(agent_name, '') AS BLOB), 1, ?) AS BLOB), ` +
		`CASE WHEN item_type = 'message' AND COALESCE(implicit, 0) = 0 AND COALESCE(actor_pending, 0) = 0 THEN CAST(substr(CAST(COALESCE(json_extract(message_json, '$.role'), '') AS BLOB), 1, 33) AS BLOB) END, ` +
		`CASE WHEN item_type = 'message' AND COALESCE(implicit, 0) = 0 AND COALESCE(actor_pending, 0) = 0 AND json_extract(message_json, '$.role') IN ('user', 'assistant', 'tool') THEN CAST(substr(CAST(` + displayText + ` AS BLOB), 1, ?) AS BLOB) END, ` +
		`CASE WHEN json_extract(message_json, '$.role') = 'assistant' AND COALESCE(json_array_length(json_extract(message_json, '$.presentation')), 0) > 16 THEN 1 ELSE 0 END ` +
		`FROM session_items WHERE session_id = ? AND (? = 0 OR position < COALESCE((SELECT position FROM session_items WHERE session_id = ? AND id = ?), ?)) ORDER BY position DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, recentChatAgentFieldLimit+1, maxTextBytes+1, maxTextBytes+1, id, beforeItemID, id, beforeItemID, beforePosition, maxScanned+1)
	if err != nil {
		return RecentChatItems{}, classifySQLiteContextError(ctx, err)
	}
	defer rows.Close()
	type rawItem struct {
		id             int64
		position       int
		itemType       string
		implicit       bool
		pending        bool
		agent          []byte
		role           []byte
		text           []byte
		partsTruncated bool
	}
	raw := make([]rawItem, 0, maxScanned+1)
	for rows.Next() {
		var item rawItem
		if err := rows.Scan(&item.id, &item.position, &item.itemType, &item.implicit, &item.pending, &item.agent, &item.role, &item.text, &item.partsTruncated); err != nil {
			return RecentChatItems{}, classifySQLiteContextError(ctx, err)
		}
		raw = append(raw, item)
	}
	if err := rows.Err(); err != nil {
		return RecentChatItems{}, classifySQLiteContextError(ctx, err)
	}

	result := RecentChatItems{}
	scanCount := min(len(raw), maxScanned)
	for i := 0; i < scanCount; i++ {
		if err := ctx.Err(); err != nil {
			return RecentChatItems{}, err
		}
		item := raw[i]
		if item.itemType != "message" || item.implicit || item.pending || !visibleRecentChatRole(string(item.role)) {
			continue
		}
		text, wasTruncated := truncateRecentChatText(string(item.text), maxTextBytes)
		if len(item.text) > maxTextBytes || item.partsTruncated {
			wasTruncated = true
		}
		result.Messages = append(result.Messages, RecentChatMessage{ID: item.id, Position: item.position, Role: string(item.role), AgentName: truncateRecentChatAgent(string(item.agent)), Text: text, Truncated: wasTruncated})
		if len(result.Messages) == maxMessages {
			if i+1 < scanCount || len(raw) > maxScanned {
				result.HasMore = true
				result.NextBeforePosition, result.NextBeforeItemID = item.position, item.id
			}
			break
		}
	}
	if !result.HasMore && len(raw) > maxScanned {
		result.HasMore = true
		result.NextBeforePosition, result.NextBeforeItemID = raw[maxScanned-1].position, raw[maxScanned-1].id
	}
	return result, nil
}

func validateRecentChatRead(ctx context.Context, id string, beforePosition int, beforeItemID int64, maxMessages, maxScanned, maxTextBytes int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if id == "" || beforePosition < 0 || beforeItemID < 0 || maxMessages < 1 || maxMessages > recentChatMaxItems || maxScanned < maxMessages || maxScanned > recentChatMaxScan || maxTextBytes < 1 || maxTextBytes > recentChatMaxItemText {
		return errors.New("invalid bounded recent-chat read limits")
	}
	return nil
}

func visibleRecentChatRole(role string) bool {
	return role == string(chat.MessageRoleUser) || role == string(chat.MessageRoleAssistant) || role == string(chat.MessageRoleTool)
}

func recentChatDisplayText(message chat.Message, maxBytes int) (string, bool) {
	// ReasoningContent and presentation reasoning are intentionally private.
	if message.Role == chat.MessageRoleAssistant && message.Presentation != nil {
		var builder strings.Builder
		truncated, contentParts := false, 0
		for _, part := range message.Presentation {
			if part.Type != chat.AssistantPartContent {
				continue
			}
			contentParts++
			if contentParts > 16 {
				truncated = true
				break
			}
			remaining := maxBytes + 1 - builder.Len()
			if remaining <= 0 {
				truncated = true
				break
			}
			text, clipped := truncateRecentChatText(part.Text, remaining)
			builder.WriteString(text)
			if clipped {
				truncated = true
				break
			}
		}
		text, clipped := truncateRecentChatText(builder.String(), maxBytes)
		return text, truncated || clipped
	}
	text := message.Content
	if text == "" && len(message.MultiContent) > 0 && message.MultiContent[0].Type == chat.MessagePartTypeText {
		text = message.MultiContent[0].Text
	}
	text, truncated := truncateRecentChatText(text, maxBytes)
	return text, truncated
}

func truncateRecentChatText(text string, maxBytes int) (string, bool) {
	if len(text) <= maxBytes && utf8.ValidString(text) {
		return text, false
	}
	end := min(len(text), maxBytes)
	for end > 0 && !utf8.ValidString(text[:end]) {
		end--
	}
	return text[:end], true
}

func truncateRecentChatAgent(name string) string {
	if len(name) <= recentChatAgentFieldLimit {
		return name
	}
	clipped, _ := truncateRecentChatText(name, recentChatAgentFieldLimit)
	return clipped
}

func (s *InMemorySessionStore) ReadRecentChatChild(ctx context.Context, id string) (RecentChatChild, error) {
	if err := ctx.Err(); err != nil {
		return RecentChatChild{}, err
	}
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	record, ok := s.children[id]
	if !ok {
		return RecentChatChild{}, ErrNotFound
	}
	child := RecentChatChild{RootSessionID: record.RootSessionID, ParentSessionID: record.ParentSessionID, NodeID: string(record.Node.ID), NodeParent: string(record.Node.Parent), NodeSessionID: record.Node.SessionID, NodeAgent: record.Node.Agent}
	if len(child.RootSessionID) > recentChatMetadataFieldLimit || len(child.ParentSessionID) > recentChatMetadataFieldLimit || len(child.NodeID) > recentChatMetadataFieldLimit || len(child.NodeParent) > recentChatMetadataFieldLimit || len(child.NodeSessionID) > recentChatMetadataFieldLimit || len(child.NodeAgent) > recentChatAgentFieldLimit {
		return RecentChatChild{}, ErrRecentChatMetadataTooLarge
	}
	return child, nil
}

func (s *SQLiteSessionStore) ReadRecentChatChild(ctx context.Context, id string) (RecentChatChild, error) {
	if err := ctx.Err(); err != nil {
		return RecentChatChild{}, err
	}
	const query = `SELECT ` +
		`CAST(substr(CAST(json_extract(record, '$.RootSessionID') AS BLOB), 1, ?) AS BLOB), ` +
		`CAST(substr(CAST(json_extract(record, '$.ParentSessionID') AS BLOB), 1, ?) AS BLOB), ` +
		`CAST(substr(CAST(json_extract(record, '$.Node.id') AS BLOB), 1, ?) AS BLOB), ` +
		`CAST(substr(CAST(json_extract(record, '$.Node.parent') AS BLOB), 1, ?) AS BLOB), ` +
		`CAST(substr(CAST(json_extract(record, '$.Node.session_id') AS BLOB), 1, ?) AS BLOB), ` +
		`CAST(substr(CAST(json_extract(record, '$.Node.agent') AS BLOB), 1, ?) AS BLOB) FROM child_records WHERE session_id = ?`
	var root, parent, node, nodeParent, nodeSession, agent []byte
	err := s.db.QueryRowContext(ctx, query, recentChatMetadataFieldLimit+1, recentChatMetadataFieldLimit+1, recentChatMetadataFieldLimit+1, recentChatMetadataFieldLimit+1, recentChatMetadataFieldLimit+1, recentChatAgentFieldLimit+1, id).Scan(&root, &parent, &node, &nodeParent, &nodeSession, &agent)
	if errors.Is(err, sql.ErrNoRows) {
		return RecentChatChild{}, ErrNotFound
	}
	if err != nil {
		return RecentChatChild{}, classifySQLiteContextError(ctx, err)
	}
	if len(root) > recentChatMetadataFieldLimit || len(parent) > recentChatMetadataFieldLimit || len(node) > recentChatMetadataFieldLimit || len(nodeParent) > recentChatMetadataFieldLimit || len(nodeSession) > recentChatMetadataFieldLimit || len(agent) > recentChatAgentFieldLimit {
		return RecentChatChild{}, ErrRecentChatMetadataTooLarge
	}
	return RecentChatChild{RootSessionID: string(root), ParentSessionID: string(parent), NodeID: string(node), NodeParent: string(nodeParent), NodeSessionID: string(nodeSession), NodeAgent: string(agent)}, nil
}

func (s *InMemorySessionStore) ValidateRecentChatAnchor(ctx context.Context, id string, position int, itemID int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	stored, ok := s.sessions.Load(id)
	if !ok {
		return ErrNotFound
	}
	stored.mu.RLock()
	defer stored.mu.RUnlock()
	if position < 0 || position >= len(stored.Messages) {
		return ErrRecentChatCursorStale
	}
	item := stored.Messages[position]
	if itemID != 0 && (item.Message == nil || item.Message.ID != itemID) {
		return ErrRecentChatCursorStale
	}
	return nil
}

func (s *SQLiteSessionStore) ValidateRecentChatAnchor(ctx context.Context, id string, position int, itemID int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM session_items WHERE session_id = ? AND position = ? AND (? = 0 OR id = ?)`, id, position, itemID, itemID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRecentChatCursorStale
	}
	return classifySQLiteContextError(ctx, err)
}

var ErrRecentChatCursorStale = errors.New("recent-chat cursor no longer identifies its transcript item")
