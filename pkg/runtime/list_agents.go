package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
)

const (
	defaultListAgentsLimit = 10
	maxListAgentsLimit     = 50
	maxListAgentsCursorLen = 64
)

type listedAgentRelationship struct {
	Agent string `json:"agent"`
	Name  string `json:"name,omitempty"`
}

type listedLiveAgent struct {
	ID          string             `json:"id"`
	Agent       string             `json:"agent"`
	Name        string             `json:"name,omitempty"`
	Description string             `json:"description,omitempty"`
	State       subagent.NodeState `json:"state"`
}

type listedAgents struct {
	CurrentAgent string                   `json:"current_agent"`
	Parent       *listedAgentRelationship `json:"parent,omitempty"`
	Children     []listedLiveAgent        `json:"children"`
	NextCursor   string                   `json:"next_cursor,omitempty"`
}

func (r *LocalRuntime) handleListAgents(_ context.Context, sess *session.Session, tc tools.ToolCall, _ EventSink, _ tools.Runtime) (*tools.ToolCallResult, error) {
	var args subagent.ListAgentsArgs
	if tc.Function.Arguments != "" {
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}
	if args.Limit < 0 {
		return tools.ResultError("limit must not be negative"), nil
	}
	limit := args.Limit
	if limit == 0 {
		limit = defaultListAgentsLimit
	}
	limit = min(limit, maxListAgentsLimit)
	cursor := strings.TrimSpace(args.Cursor)
	if len(cursor) > maxListAgentsCursorLen {
		return tools.ResultError("cursor is too long"), nil
	}

	children, nextCursor, err := r.subagents.listDirectChildren(sess.ID, cursor, limit)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	result := listedAgents{Children: children, NextCursor: nextCursor}
	if a := r.resolveSessionAgent(sess); a != nil {
		result.CurrentAgent = a.Name()
	}
	if id, ok := r.subagents.nodeForSession(sess.ID); ok {
		if node, exists := r.subagents.tree.Node(id); exists && node.Parent != "" {
			if parent, exists := r.subagents.tree.Node(node.Parent); exists {
				result.Parent = &listedAgentRelationship{Agent: parent.Agent, Name: parent.DisplayName()}
			}
		}
	}
	return tools.ResultJSON(result), nil
}

// listDirectChildren returns a bounded page of live direct child instances.
func (m *subagentManager) listDirectChildren(parentSessionID, cursor string, limit int) ([]listedLiveAgent, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	children := make([]listedLiveAgent, 0)
	for id, rec := range m.children {
		if rec.parentSession != parentSessionID || rec.durable.Node.State == subagent.NodeStopped {
			continue
		}
		node := rec.durable.Node
		if live, ok := m.tree.Node(id); ok {
			node = live
		}
		if node.State == subagent.NodeStopped {
			continue
		}
		children = append(children, listedLiveAgent{
			ID: string(id), Agent: node.Agent, Name: node.DisplayName(),
			Description: node.Description, State: node.State,
		})
	}
	slices.SortFunc(children, func(a, b listedLiveAgent) int { return strings.Compare(a.ID, b.ID) })
	if cursor != "" {
		if len(cursor) != 5 {
			return nil, "", fmt.Errorf("cursor must be a canonical subagent node id")
		}
		for _, char := range cursor {
			if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
				return nil, "", fmt.Errorf("cursor must be a canonical subagent node id")
			}
		}
		index := slices.IndexFunc(children, func(child listedLiveAgent) bool { return child.ID > cursor })
		if index < 0 {
			return []listedLiveAgent{}, "", nil
		}
		children = children[index:]
	}
	if len(children) <= limit {
		return children, "", nil
	}
	page := children[:limit]
	return page, page[len(page)-1].ID, nil
}
