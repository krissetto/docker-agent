package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
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
	ID           string `json:"id,omitempty"`
	Agent        string `json:"agent"`
	Name         string `json:"name,omitempty"`
	SessionTitle string `json:"session_title,omitempty"`
	sessionID    string
}

type listedLiveAgent struct {
	ID           string             `json:"id"`
	Agent        string             `json:"agent"`
	Name         string             `json:"name,omitempty"`
	SessionTitle string             `json:"session_title,omitempty"`
	State        subagent.NodeState `json:"state"`
	ParentID     string             `json:"parent_id,omitempty"`
	Messageable  *bool              `json:"messageable,omitempty"`
	sessionID    string
}

type listedAgents struct {
	CurrentAgent string                   `json:"current_agent"`
	Self         *listedLiveAgent         `json:"self,omitempty"`
	Parent       *listedAgentRelationship `json:"parent,omitempty"`
	Scope        string                   `json:"scope,omitempty"`
	Children     []listedLiveAgent        `json:"children"`
	Agents       []listedLiveAgent        `json:"agents,omitempty"`
	NextCursor   string                   `json:"next_cursor,omitempty"`
}

func (r *LocalRuntime) handleListAgents(ctx context.Context, sess *session.Session, tc tools.ToolCall, _ EventSink, _ tools.Runtime) (*tools.ToolCallResult, error) {
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

	team := r.teamForContext(ctx, sess)
	current := r.resolveSessionAgentContext(ctx, sess)
	if current == nil {
		return tools.ResultError("current agent is unavailable in the captured configuration"), nil
	}
	result := listedAgents{Children: []listedLiveAgent{}, CurrentAgent: current.Name()}
	var access *latest.SubagentAccessConfig
	if team != nil {
		if cfg, ok := team.AgentConfig(current.Name()); ok {
			access = cfg.SubagentAccess
		}
	}
	scope := "children"
	if access != nil && access.ListAgents != "" {
		scope = string(access.ListAgents)
	}
	titleCache := map[string]string{}
	titleFor := func(id string) string {
		if title, ok := titleCache[id]; ok {
			return title
		}
		title := r.sessionTitle(ctx, id, sess)
		titleCache[id] = title
		return title
	}
	if scope == "tree" {
		result.Scope = "tree"
		all, selfID, rootID, err := r.subagents.listRootTree(ctx, sess)
		if err != nil {
			return tools.ResultError(err.Error()), nil
		}
		selfParent := ""
		if selfIndex := slices.IndexFunc(all, func(item listedLiveAgent) bool { return item.ID == string(selfID) }); selfIndex >= 0 {
			selfParent = all[selfIndex].ParentID
		}
		canMessageTree := access != nil && access.SendMessage == latest.SubagentAccessTree
		for i := range all {
			isParentOrChild := all[i].ID == selfParent || all[i].ParentID == string(selfID)
			messageable := all[i].ID != string(selfID) && all[i].State != subagent.NodeStopped && (canMessageTree || isParentOrChild)
			all[i].Messageable = &messageable
		}
		if selfID != "" {
			if i := slices.IndexFunc(all, func(a listedLiveAgent) bool { return a.ID == string(selfID) }); i >= 0 {
				self := all[i]
				result.Self = &self
				if self.ParentID != "" {
					if parent := slices.IndexFunc(all, func(a listedLiveAgent) bool { return a.ID == self.ParentID }); parent >= 0 {
						result.Parent = &listedAgentRelationship{ID: all[parent].ID, Agent: all[parent].Agent, Name: all[parent].Name, sessionID: all[parent].sessionID}
					}
				}
			}
		}
		rootCursor := string(subagent.SessionRootID(rootID))
		if cursor != "" && cursor != rootCursor && !canonicalNodeCursor(cursor) {
			return tools.ResultError("cursor must be a canonical subagent node id"), nil
		}
		if strings.HasPrefix(cursor, "root:") && cursor != rootCursor {
			return tools.ResultError("cursor must be an id returned by this tree"), nil
		}
		if cursor != "" {
			index := slices.IndexFunc(all, func(a listedLiveAgent) bool { return a.ID > cursor })
			if index < 0 {
				all = []listedLiveAgent{}
			} else {
				all = all[index:]
			}
		}
		if len(all) > limit {
			result.Agents = slices.Clone(all[:limit])
			result.NextCursor = result.Agents[len(result.Agents)-1].ID
		} else {
			result.Agents = all
		}
		for _, a := range result.Agents {
			if a.ParentID == string(selfID) {
				result.Children = append(result.Children, a)
			}
		}
	} else {
		children, nextCursor, err := r.subagents.listDirectChildren(sess.ID, cursor, limit)
		if err != nil {
			return tools.ResultError(err.Error()), nil
		}
		result.Children, result.NextCursor = children, nextCursor
		if selfID, parent := r.subagents.sessionIdentity(sess.ID); selfID != "" {
			if node, exists := r.subagents.tree.Node(selfID); exists {
				self := listedLiveAgent{ID: string(selfID), Agent: node.Agent, Name: node.DisplayName(), State: node.State, ParentID: string(node.Parent), sessionID: node.SessionID}
				result.Self = &self
			}
			if result.Self == nil {
				result.Self = &listedLiveAgent{ID: string(selfID), Agent: current.Name(), State: subagent.NodeIdle, sessionID: sess.ID}
			}
			if parent != "" {
				if p, exists := r.subagents.tree.Node(parent); exists {
					result.Parent = &listedAgentRelationship{ID: string(parent), Agent: p.Agent, Name: p.DisplayName(), sessionID: p.SessionID}
				}
			}
		}
	}
	if result.Self != nil {
		result.Self.SessionTitle = titleFor(sess.ID)
	}
	for i := range result.Children {
		if result.Children[i].SessionTitle == "" {
			result.Children[i].SessionTitle = titleFor(result.Children[i].sessionID)
		}
	}
	for i := range result.Agents {
		if result.Agents[i].SessionTitle == "" {
			result.Agents[i].SessionTitle = titleFor(result.Agents[i].sessionID)
		}
	}
	if result.Parent != nil && result.Parent.SessionTitle == "" {
		result.Parent.SessionTitle = titleFor(result.Parent.sessionID)
	}
	return tools.ResultJSON(result), nil
}

func canonicalNodeCursor(cursor string) bool {
	if len(cursor) != 5 {
		return false
	}
	for _, char := range cursor {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

// sessionTitle reads only resident session snapshots or metadata-only store
// records. It deliberately does not fall back to transcript-hydrating Store reads.
func (r *LocalRuntime) sessionTitle(ctx context.Context, sessionID string, current *session.Session) string {
	if sessionID == "" {
		return ""
	}
	if r.sessionDrivers != nil {
		if driver, ok := r.sessionDrivers.Lookup(sessionID); ok {
			if live := driver.session(); live != nil {
				return live.TitleSnapshot()
			}
		}
	}
	if current != nil && current.ID == sessionID {
		return current.TitleSnapshot()
	}
	if metadata, ok := r.sessionStore.(session.MetadataStore); ok {
		loaded, err := metadata.GetSessionMetadata(ctx, sessionID)
		if err == nil && loaded != nil {
			return loaded.TitleSnapshot()
		}
	}
	return ""
}

// listRootTree combines the current root's persisted membership with the live
// tree projection without loading child transcripts or activating sessions.
func (m *subagentManager) listRootTree(ctx context.Context, sess *session.Session) ([]listedLiveAgent, subagent.NodeID, string, error) {
	if sess == nil {
		return nil, "", "", errors.New("session is required")
	}
	rootID, err := m.rootSessionFor(ctx, sess)
	if err != nil {
		return nil, "", "", err
	}
	rootNodeID := subagent.SessionRootID(rootID)
	nodes := map[subagent.NodeID]subagent.Node{}
	if m.r.subagentStore != nil {
		if snap, err := m.r.subagentStore.LoadTree(ctx, rootID); err == nil && snap != nil {
			var visit func(subagent.NodeSnapshot)
			visit = func(entry subagent.NodeSnapshot) {
				nodes[entry.Node.ID] = entry.Node
				for _, child := range entry.Children {
					visit(child)
				}
			}
			for _, entry := range snap.Nodes {
				visit(entry)
			}
		} else if err != nil {
			return nil, "", "", err
		}
	}

	if coordinator, ok := m.r.sessionStore.(session.CoordinationStore); ok {
		records, loadErr := coordinator.LoadChildren(ctx, rootID)
		if loadErr != nil {
			return nil, "", "", loadErr
		}
		for _, record := range records {
			nodes[record.Node.ID] = record.Node
		}
	}
	if m.tree != nil {
		if snapshot, ok := snapshotForRoot(m.tree.Snapshot(), rootNodeID); ok {
			var visit func(subagent.NodeSnapshot)
			visit = func(entry subagent.NodeSnapshot) {
				nodes[entry.Node.ID] = entry.Node
				for _, child := range entry.Children {
					visit(child)
				}
			}
			for _, entry := range snapshot.Nodes {
				visit(entry)
			}
		}
	}
	var rootMeta *session.Session
	if sess.ID == rootID {
		rootMeta = sess
	} else if metadata, ok := m.r.sessionStore.(session.MetadataStore); ok {
		rootMeta, err = metadata.GetSessionMetadata(ctx, rootID)
	} else {
		err = session.ErrNotFound
	}
	if err == nil {
		if _, exists := nodes[rootNodeID]; !exists {
			agentName := rootMeta.AttributesSnapshot()[SessionAgentAttribute]
			if agentName == "" {
				agentName = rootMeta.AgentName
			}
			nodes[rootNodeID] = subagent.Node{ID: rootNodeID, Agent: agentName, SessionID: rootID, State: subagent.NodeIdle}
		}
	} else if !errors.Is(err, session.ErrNotFound) {
		return nil, "", "", err
	}
	if root, exists := nodes[rootNodeID]; exists {
		root.SessionID = rootID
		if rootMeta != nil {
			if root.Agent == "" {
				root.Agent = rootMeta.AgentName
			}
		}
		nodes[rootNodeID] = root
	}
	// The live tree and durable rows can lag a driver transition. Prefer a
	// resident driver's state without causing restore or transcript hydration.
	for id, node := range nodes {
		if node.SessionID == "" {
			continue
		}
		if driver, ok := m.r.sessionDrivers.Lookup(node.SessionID); ok {
			status := driver.Status()
			switch status.State {
			case SessionStateRunning, SessionStateCancelling:
				node.State = subagent.NodeRunning
			case SessionStateQueued:
				node.State = subagent.NodeStarting
			default:
				if node.State == subagent.NodeRunning || node.State == subagent.NodeStarting {
					node.State = subagent.NodeIdle
				}
			}
			nodes[id] = node
		}
	}
	all := make([]listedLiveAgent, 0, len(nodes))
	for _, node := range nodes {
		all = append(all, listedLiveAgent{ID: string(node.ID), Agent: node.Agent, Name: node.DisplayName(), State: node.State, ParentID: string(node.Parent), sessionID: node.SessionID})
	}
	slices.SortFunc(all, func(a, b listedLiveAgent) int { return strings.Compare(a.ID, b.ID) })
	selfID := rootNodeID
	if id, ok := m.nodeForSession(sess.ID); ok {
		selfID = id
	} else if sess.ID != rootID {
		if coordinator, ok := m.r.sessionStore.(session.ChildRecordReader); ok {
			if record, loadErr := coordinator.LoadChild(ctx, sess.ID); loadErr == nil && record.RootSessionID == rootID && record.Node.SessionID == sess.ID {
				selfID = record.Node.ID
			}
		}
	}
	return all, selfID, rootID, nil
}

func (m *subagentManager) rootSessionFor(ctx context.Context, sess *session.Session) (string, error) {
	if sess == nil {
		return "", errors.New("session is required")
	}
	if sess.ParentID == "" {
		return sess.ID, nil
	}
	m.mu.Lock()
	var root string
	for _, rec := range m.children {
		if rec.sessionID != sess.ID {
			continue
		}
		if rec.parentSession != sess.ParentID {
			m.mu.Unlock()
			return "", fmt.Errorf("invalid subagent membership")
		}
		root = rec.durable.RootSessionID
		if root == "" {
			root = m.rootSessionLocked(sess.ID)
		}
		break
	}
	m.mu.Unlock()
	if root != "" {
		if root == sess.ID {
			return "", fmt.Errorf("invalid subagent root membership")
		}
		return root, nil
	}
	reader, ok := m.r.sessionStore.(session.ChildRecordReader)
	if !ok {
		return "", fmt.Errorf("subagent tree membership is unavailable")
	}
	record, err := reader.LoadChild(ctx, sess.ID)
	if err != nil {
		return "", err
	}
	if record.Node.SessionID != sess.ID || record.ParentSessionID != sess.ParentID || record.RootSessionID == "" {
		return "", fmt.Errorf("invalid subagent membership")
	}
	return record.RootSessionID, nil
}

// sessionIdentity returns the current node and its tree parent without loading a session.
func (m *subagentManager) sessionIdentity(sessionID string) (subagent.NodeID, subagent.NodeID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, rec := range m.children {
		if rec.sessionID == sessionID {
			return id, rec.durable.Node.Parent
		}
	}
	if tracked := m.sessions[sessionID]; tracked != nil {
		return tracked.node, ""
	}
	return subagent.SessionRootID(sessionID), ""
}

func (m *subagentManager) canMessageTarget(ctx context.Context, sess *session.Session, target string, access *latest.SubagentAccessConfig) bool {
	if sess == nil || target == "" {
		return false
	}
	_, parent := m.sessionIdentity(sess.ID)
	if target == string(parent) && parent != "" {
		if node, ok := m.tree.Node(parent); ok {
			return node.State != subagent.NodeStopped
		}
		return true
	}
	if access == nil || access.SendMessage == "" || access.SendMessage == latest.SubagentAccessChildren {
		m.mu.Lock()
		rec := m.children[subagent.NodeID(target)]
		ok := rec != nil && rec.parentSession == sess.ID && rec.durable.Node.State != subagent.NodeStopped && !m.sessionStoppingLocked(rec.sessionID)
		if node, live := m.tree.Node(subagent.NodeID(target)); live && node.State == subagent.NodeStopped {
			ok = false
		}
		m.mu.Unlock()
		return ok
	}
	all, self, _, err := m.listRootTree(ctx, sess)
	if err != nil || target == string(self) {
		return false
	}
	return slices.ContainsFunc(all, func(item listedLiveAgent) bool { return item.ID == target && item.State != subagent.NodeStopped })
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
		messageable := node.State != subagent.NodeStopped
		children = append(children, listedLiveAgent{
			ID: string(id), Agent: node.Agent, Name: node.DisplayName(), ParentID: string(node.Parent),
			State: node.State, Messageable: &messageable, sessionID: node.SessionID,
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
