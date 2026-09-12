package server

import (
	"context"
	"sort"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/subagent"
)

var (
	_ runtime.SessionCatalog = (*SessionManager)(nil)
	_ runtime.TreeInspector  = (*SessionManager)(nil)
)

// ListSessions exposes the manager-owned catalog without leaking stores or
// registries. It validates persisted routing and child tree membership without
// attaching, restoring, or starting any session.
func (sm *SessionManager) ListSessions(ctx context.Context) ([]runtime.SessionCatalogEntry, error) {
	sessions, err := sm.sessionStore.GetSessions(ctx)
	if err != nil {
		return nil, err
	}
	index := newSessionCatalogIndex(sessions)
	out := make([]runtime.SessionCatalogEntry, 0, len(sessions))
	for _, sess := range sessions {
		_, _, cost := sess.TokensAndCost()
		entry := runtime.SessionCatalogEntry{SessionID: sess.ID, Title: sess.TitleSnapshot(), AgentName: sess.AgentName, CreatedAt: sess.CreatedAt, Starred: sess.Starred, NumMessages: len(sess.GetAllMessages()), Cost: cost, WorkingDir: sess.WorkingDir}
		if entry.AgentName == "" {
			entry.AgentName = sess.AttributesSnapshot()[sessionAgentAttribute]
		}
		if active, loaded := sm.runtimeSessions.Load(sess.ID); loaded && active.handle != nil {
			entry.Loadable = true
			entry.AgentName = active.handle.Metadata().AgentName
		} else if entry.AgentName != "" {
			registry, _, routeErr := sm.sessionRegistryForCreate(sess.AttributesSnapshot()[sessionSourceAttribute])
			if routeErr == nil {
				if sess.ParentID == "" {
					entry.Loadable = true
				} else if root, rootErr := index.root(sess); rootErr == nil {
					if treeErr := index.validateChild(ctx, registry, sess, root); treeErr == nil {
						_, entry.Loadable = registry.(runtime.TreeRestorer)
					}
				}
			}
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// InspectSessionTree resolves the owning runtime through Handle, then delegates
// only the narrow topology capability.
func (sm *SessionManager) InspectSessionTree(ctx context.Context, sessionID string) (*subagent.Snapshot, error) {
	handle, err := sm.Handle(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	registry := sm.sessionRegistry
	if active, ok := sm.runtimeSessions.Load(handle.ID()); ok && active.registry != nil {
		registry = active.registry
	}
	inspector, ok := registry.(runtime.TreeInspector)
	if !ok {
		return nil, &runtime.SessionError{Kind: runtime.SessionErrorUnsupported, SessionID: sessionID, Operation: "inspect_tree"}
	}
	return inspector.InspectSessionTree(ctx, sessionID)
}
