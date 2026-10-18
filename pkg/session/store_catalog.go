package session

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
)

// SessionCatalogSummaryStore reads root listing metadata with direct transcript
// metrics aggregated across its persisted descendants.
type SessionCatalogSummaryStore interface {
	GetSessionCatalogSummaries(ctx context.Context) ([]Summary, error)
}

func (s *InMemorySessionStore) GetSessionCatalogSummaries(ctx context.Context) ([]Summary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	byID := make(map[string]*Session, s.sessions.Length())
	s.sessions.Range(func(id string, value *Session) bool {
		byID[id] = value
		return true
	})
	rootFor := make(map[string]string, len(byID))
	for id := range byID {
		if _, known := rootFor[id]; known {
			continue
		}
		path := make([]string, 0, 8)
		seen := make(map[string]bool)
		rootID := ""
		for current := id; current != ""; {
			if cached, known := rootFor[current]; known {
				rootID = cached
				break
			}
			if seen[current] {
				break
			}
			seen[current] = true
			path = append(path, current)
			value := byID[current]
			if value == nil {
				break
			}
			if value.ParentID == "" {
				rootID = current
				break
			}
			current = value.ParentID
		}
		for _, member := range path {
			rootFor[member] = rootID
		}
	}
	totals := make(map[string]Summary, len(byID))
	for id, value := range byID {
		rootID := rootFor[id]
		if rootID == "" {
			continue
		}
		messages, cost := directConversationMetrics(value)
		total := totals[rootID]
		total.NumMessages += messages
		total.Cost += cost
		totals[rootID] = total
	}
	out := make([]Summary, 0, len(totals))
	for id, metrics := range totals {
		root := byID[id]
		if root == nil || root.ParentID != "" {
			continue
		}
		overrides, _ := root.ModelStateSnapshot()
		out = append(out, Summary{ID: root.ID, Title: root.TitleSnapshot(), CreatedAt: root.CreatedAt, Starred: root.Starred,
			NumMessages: metrics.NumMessages, Cost: metrics.Cost, WorkingDir: root.WorkingDir,
			Attributes: root.AttributesSnapshot(), AgentModelOverrides: overrides})
	}
	slices.SortFunc(out, func(a, b Summary) int {
		if n := b.CreatedAt.Compare(a.CreatedAt); n != 0 {
			return n
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return out, nil
}

func directConversationMetrics(sess *Session) (int, float64) {
	sess.mu.RLock()
	defer sess.mu.RUnlock()
	messages := 0
	var cost float64
	for _, item := range sess.Messages {
		if item.Message != nil {
			if item.Message.Message.Role != "system" {
				messages++
			}
			cost += item.Message.Message.Cost
		} else if item.Summary != "" {
			cost += item.Cost
		}
	}
	return messages, cost
}

// parent_id is the persisted catalog ancestry, including imported legacy
// children. Embedded subsession references do not establish catalog membership.
const sessionCatalogSummaryQuery = `WITH RECURSIVE roots(session_id, root_id, path) AS (
		SELECT id, id, printf('/%s/', hex(id)) FROM sessions WHERE parent_id IS NULL OR parent_id = ''
		UNION ALL
		SELECT child.id, roots.root_id, roots.path || printf('%s/', hex(child.id))
		FROM sessions child JOIN roots ON child.parent_id = roots.session_id
		WHERE instr(roots.path, printf('/%s/', hex(child.id))) = 0
	)
	SELECT root.id, root.title, root.created_at, root.starred,
		SUM(member.catalog_num_messages), SUM(member.catalog_cost),
		root.working_dir, root.attributes, root.agent_model_overrides
	FROM roots JOIN sessions root ON root.id = roots.root_id
	JOIN sessions member ON member.id = roots.session_id
	GROUP BY root.id
	ORDER BY root.created_at DESC, root.id ASC`

func (s *SQLiteSessionStore) GetSessionCatalogSummaries(ctx context.Context) ([]Summary, error) {
	rows, err := s.db.QueryContext(ctx, sessionCatalogSummaryQuery)
	if err != nil {
		return nil, classifySQLiteContextError(ctx, err)
	}
	defer rows.Close()
	var out []Summary
	for rows.Next() {
		var row Summary
		var createdAt string
		var workingDir, attributesJSON, overridesJSON sql.NullString
		if err := rows.Scan(&row.ID, &row.Title, &createdAt, &row.Starred, &row.NumMessages, &row.Cost, &workingDir, &attributesJSON, &overridesJSON); err != nil {
			return nil, classifySQLiteContextError(ctx, err)
		}
		row.CreatedAt, row.WorkingDir = parseCreatedAt(createdAt), workingDir.String
		row.Attributes, err = decodeAttributes(attributesJSON)
		if err != nil {
			return nil, fmt.Errorf("unmarshaling session attributes for %s: %w", row.ID, classifySQLiteContextError(ctx, err))
		}
		row.AgentModelOverrides, err = decodeAttributes(overridesJSON)
		if err != nil {
			return nil, fmt.Errorf("unmarshaling session model overrides for %s: %w", row.ID, classifySQLiteContextError(ctx, err))
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, classifySQLiteContextError(ctx, err)
	}
	return out, nil
}

var _ SessionCatalogSummaryStore = (*InMemorySessionStore)(nil)
var _ SessionCatalogSummaryStore = (*SQLiteSessionStore)(nil)
