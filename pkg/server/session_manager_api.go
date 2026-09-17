package server

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

var (
	_ runtime.SessionCatalog        = (*SessionManager)(nil)
	_ runtime.SessionSummaryCatalog = (*SessionManager)(nil)
	_ runtime.TreeInspector         = (*SessionManager)(nil)
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

// ListSessionSummaries reads only metadata. Cold child rows are candidates,
// not access grants: durable membership is checked by the confirmed loader.
func (sm *SessionManager) ListSessionSummaries(ctx context.Context, options runtime.SessionSummaryOptions) ([]runtime.SessionSummaryEntry, error) {
	store, ok := sm.sessionStore.(session.ScopedSummaryStore)
	if !ok {
		return nil, runtime.UnsupportedSessionOperation("", "session_summaries")
	}
	rows, err := store.GetSessionSummariesWithScope(ctx, session.SummaryScope{IncludeChildren: options.IncludeChildren})
	if err != nil {
		return nil, err
	}
	// These detached shells carry routing metadata only. The shared ancestry
	// index never loads message payloads or performs per-ID store lookups.
	sessions := make([]*session.Session, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		if row.ID == "" || seen[row.ID] {
			return nil, errors.New("invalid or duplicate session summary identity")
		}
		seen[row.ID] = true
		sessions = append(sessions, &session.Session{ID: row.ID, ParentID: row.ParentID, AgentName: row.Attributes[sessionAgentAttribute], Attributes: row.Attributes})
	}
	index := newSessionCatalogIndex(sessions)
	out := make([]runtime.SessionSummaryEntry, 0, len(rows))
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !options.IncludeChildren && row.ParentID != "" {
			continue
		}
		sess := index.byID[row.ID]
		entry := runtime.SessionSummaryEntry{SessionID: row.ID, ParentID: row.ParentID, Title: row.Title, AgentName: sess.AgentName, Model: row.AgentModelOverrides[sess.AgentName], Source: row.Attributes[sessionSourceAttribute], CreatedAt: row.CreatedAt, UpdatedAt: row.CreatedAt.Format(time.RFC3339), Starred: row.Starred, NumMessages: row.NumMessages, Cost: row.Cost, WorkingDir: row.WorkingDir}
		if active, loaded := sm.runtimeSessions.Load(row.ID); loaded && active.handle != nil {
			metadata := active.handle.Metadata()
			entry.Loaded, entry.Loadable = true, true
			entry.AgentName, entry.Model = metadata.AgentName, metadata.Model
		} else if entry.AgentName == "" {
			entry.RouteError = "session is not attachable"
		} else if root, rootErr := index.root(sess); rootErr != nil {
			entry.RouteError = rootErr.Error()
		} else if root.AgentName == "" {
			entry.RouteError = "persisted root session agent is unavailable"
		} else if registry, _, routeErr := sm.sessionRegistryForCreate(root.AttributesSnapshot()[sessionSourceAttribute]); routeErr != nil {
			entry.RouteError = "persisted session source is unavailable or ambiguous"
		} else if row.ParentID == "" {
			entry.Loadable = true
		} else if _, ok := registry.(runtime.TreeRestorer); !ok {
			entry.RouteError = "runtime cannot load durable child session tree"
		} else {
			entry.RequiresConfirmation = true
		}
		out = append(out, entry)
	}
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

// viewRegistry resolves persisted source/ancestry without calling the mutating
// Handle path. The runtime performs the full confirmed membership preflight.
func (sm *SessionManager) viewRegistry(ctx context.Context, id string) (runtime.SessionRuntime, error) {
	selected, err := sm.sessionStore.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	root, err := sm.sessionRestoreRoot(ctx, selected)
	if err != nil {
		return nil, err
	}
	registry, _, err := sm.sessionRegistryForCreate(root.AttributesSnapshot()[sessionSourceAttribute])
	return registry, err
}

func (sm *SessionManager) ConfirmedSessionViewInfo(ctx context.Context, id string) (runtime.PreparedSessionViewInfo, error) {
	registry, err := sm.viewRegistry(ctx, id)
	if err != nil {
		return runtime.PreparedSessionViewInfo{}, err
	}
	reader, ok := registry.(interface {
		ConfirmedSessionViewInfo(ctx context.Context, sessionID string) (runtime.PreparedSessionViewInfo, error)
	})
	if !ok {
		return runtime.PreparedSessionViewInfo{}, runtime.UnsupportedSessionOperation(id, "prepare_view")
	}
	return reader.ConfirmedSessionViewInfo(ctx, id)
}

func (sm *SessionManager) PrepareSessionView(ctx context.Context, id string) (runtime.PreparedSessionView, error) {
	registry, err := sm.viewRegistry(ctx, id)
	if err != nil {
		return nil, err
	}
	preparer, ok := registry.(runtime.SessionViewPreparer)
	if !ok {
		return nil, runtime.UnsupportedSessionOperation(id, "prepare_view")
	}
	prepared, err := preparer.PrepareSessionView(ctx, id)
	if err != nil {
		return nil, err
	}
	return &managerPreparedView{PreparedSessionView: prepared, manager: sm, registry: registry}, nil
}

type managerPreparedView struct {
	runtime.PreparedSessionView

	manager  *SessionManager
	registry runtime.SessionRuntime
}

func (p *managerPreparedView) Commit(ctx context.Context) (runtime.CommittedSessionView, error) {
	result, err := p.PreparedSessionView.Commit(ctx)
	if err == nil {
		p.manager.runtimeSessions.Store(result.Info.SessionID, &activeRuntimes{handle: result.SessionHandle, registry: p.registry})
	}
	return result, err
}
