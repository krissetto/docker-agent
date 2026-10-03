package tui

import (
	"context"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

// preparedViewHandle is a settled canonical handle bound to its own agent.
type preparedViewHandle struct {
	*lifecycleHandle

	agent    string
	snapshot *session.Session
}

func (h *preparedViewHandle) AgentName() string { return h.agent }
func (h *preparedViewHandle) Metadata() runtime.SessionMetadata {
	return runtime.SessionMetadata{SessionID: h.id, AgentName: h.agent}
}

func (h *preparedViewHandle) Snapshot(context.Context) (*session.Session, error) {
	return h.snapshot.Clone(), nil
}

// preparedViewSessions opens child views only through PrepareSessionView, the
// route shared by in-process and remote owners. Child snapshots carry their
// parent so the host resolves them to the root's canonical owner.
type preparedViewSessions struct {
	openSubagentSessions

	root     *session.Session
	views    map[string]runtime.SubagentAttachInfo
	prepares func()
}

func (r *preparedViewSessions) handle(id string) (*preparedViewHandle, bool) {
	if id == r.root.ID {
		return &preparedViewHandle{lifecycleHandle: &lifecycleHandle{id: id}, agent: r.root.AgentName, snapshot: r.root}, true
	}
	info, ok := r.views[id]
	if !ok || info.Session == nil {
		return nil, false
	}
	snapshot := info.Session.Clone()
	snapshot.ParentID = info.ParentSessionID
	return &preparedViewHandle{lifecycleHandle: &lifecycleHandle{id: id}, agent: info.Agent, snapshot: snapshot}, true
}

func (r *preparedViewSessions) SessionByID(id string) (runtime.SessionHandle, error) {
	if h, ok := r.handle(id); ok {
		return h, nil
	}
	return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: id, Operation: "lookup"}
}

func (r *preparedViewSessions) PrepareSessionView(_ context.Context, id string) (runtime.PreparedSessionView, error) {
	if r.prepares != nil {
		r.prepares()
	}
	info, ok := r.views[id]
	h, found := r.handle(id)
	if !ok || !found {
		return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: id, Operation: "prepare_view"}
	}
	attach := info
	return &preparedViewFixture{
		handle: h,
		info:   runtime.PreparedSessionViewInfo{SessionID: id, RootSessionID: r.root.ID, Session: info.Session, Binding: runtime.SessionBinding{AgentName: info.Agent}, WorkingDir: info.Session.WorkingDir, Attach: &attach},
	}, nil
}

type preparedViewFixture struct {
	info   runtime.PreparedSessionViewInfo
	handle runtime.SessionHandle
}

func (p *preparedViewFixture) Info() runtime.PreparedSessionViewInfo {
	info := p.info
	info.Session = info.Session.Clone()
	return info
}

func (p *preparedViewFixture) Commit(context.Context) (runtime.CommittedSessionView, error) {
	return runtime.CommittedSessionView{SessionHandle: p.handle, Info: p.Info()}, nil
}
func (*preparedViewFixture) Abort() {}

func (r *sidebarAttachRuntime) preparedViews(root *session.Session) *preparedViewSessions {
	views := make(map[string]runtime.SubagentAttachInfo, len(r.infos))
	for _, info := range r.infos {
		views[info.Session.ID] = info
	}
	return &preparedViewSessions{root: root, views: views, prepares: func() { r.lookups.Add(1) }}
}
