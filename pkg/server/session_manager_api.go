package server

import (
	"context"
	"errors"
	"sort"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

var (
	_ runtime.SessionCatalog        = (*SessionManager)(nil)
	_ runtime.SessionSummaryCatalog = (*SessionManager)(nil)
	_ runtime.TreeInspector         = (*SessionManager)(nil)
	_ runtime.SessionViewInfoReader = (*SessionManager)(nil)
	_ runtime.SessionViewPreparer   = (*SessionManager)(nil)
)

// ListSessions exposes the manager-owned catalog without leaking stores or
// registries. Metadata is a routing hint, never a child membership grant;
// browsing does not attach, restore, or start sessions.
func (sm *SessionManager) ListSessions(ctx context.Context) ([]runtime.SessionCatalogEntry, error) {
	rows, err := sm.ListSessionSummaries(ctx, runtime.SessionSummaryOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]runtime.SessionCatalogEntry, 0, len(rows))
	for _, row := range rows {
		out = append(out, runtime.SessionCatalogEntry{SessionID: row.SessionID, Title: row.Title, AgentName: row.AgentName, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, Starred: row.Starred, NumMessages: row.NumMessages, Cost: row.Cost, WorkingDir: row.WorkingDir, Loadable: row.Loadable})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].SessionID < out[j].SessionID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// ListSessionSummaries reads only metadata. Cold child rows are candidates,
// not access grants: durable membership is checked by the confirmed loader.
func (sm *SessionManager) ListSessionSummaries(ctx context.Context, options runtime.SessionSummaryOptions) ([]runtime.SessionSummaryEntry, error) {
	store, ok := sm.sessionStore.(session.PagedSummaryStore)
	if !ok {
		return nil, runtime.UnsupportedSessionOperation("", "session_summaries")
	}
	var rows []session.Summary
	optionsPage := session.SummaryPageOptions{IncludeChildren: options.IncludeChildren, Limit: api.SessionCatalogMaxLimit}
	for {
		page, err := store.GetSessionSummaryPage(ctx, optionsPage)
		if err != nil {
			return nil, err
		}
		rows = append(rows, page.Summaries...)
		if !page.HasMore {
			break
		}
		if len(page.Summaries) == 0 {
			return nil, errors.New("invalid empty continuation page")
		}
		last := page.Summaries[len(page.Summaries)-1]
		if last.CreatedAt.Equal(optionsPage.AfterCreatedAt) && last.ID == optionsPage.AfterID {
			return nil, errors.New("session summary cursor did not advance")
		}
		optionsPage.AfterCreatedAt, optionsPage.AfterID = last.CreatedAt, last.ID
	}
	out := make([]runtime.SessionSummaryEntry, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if row.ID == "" || seen[row.ID] {
			return nil, errors.New("invalid or duplicate session summary identity")
		}
		seen[row.ID] = true
		if !options.IncludeChildren && row.ParentID != "" {
			continue
		}
		out = append(out, sm.summaryEntry(row))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].SessionID < out[j].SessionID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
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
	reader, ok := registry.(runtime.SessionViewInfoReader)
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
