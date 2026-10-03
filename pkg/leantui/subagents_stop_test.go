package leantui

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

// blockingTreeHandle advertises the portable stop-tree capability; its stop
// blocks so the test proves the event loop never waits on the owner.
type blockingTreeHandle struct {
	*leanSession

	started, release chan struct{}
}

func (h *blockingTreeHandle) Metadata() runtime.SessionMetadata {
	metadata := h.leanSession.Metadata()
	metadata.Capabilities.StopSubtree = true
	return metadata
}

func (h *blockingTreeHandle) StopSubtree(context.Context) error {
	close(h.started)
	<-h.release
	return nil
}

type blockingTreeSessions struct {
	handles map[string]runtime.SessionHandle
}

func (r *blockingTreeSessions) CreateSession(_ context.Context, sess *session.Session, _ runtime.SessionBinding) (runtime.SessionHandle, error) {
	return r.handles[sess.ID], nil
}

func (r *blockingTreeSessions) SessionByID(id string) (runtime.SessionHandle, error) {
	if h := r.handles[id]; h != nil {
		return h, nil
	}
	return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: id}
}
func (*blockingTreeSessions) DeleteSession(context.Context, string) error { return nil }

func TestSubagentStopDoesNotBlockLeanEventLoop(t *testing.T) {
	m, _ := sessionModel(t)
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	handles := map[string]runtime.SessionHandle{
		"root":  &blockingTreeHandle{leanSession: &leanSession{id: "root"}, started: make(chan struct{}), release: release},
		"child": &blockingTreeHandle{leanSession: &leanSession{id: "child"}, started: started, release: release},
	}
	m.app = app.New(t.Context(), &blockingTreeSessions{handles: handles}, session.New(session.WithID("root")), runtime.SessionBinding{AgentName: "agent"}, app.WithRuntimeServices(m.app.Runtime()))
	require.True(t, m.app.CanStopSubtree())
	m.viewers = &viewerHost{ctx: func() context.Context { return t.Context() }, events: make(chan any, 8)}
	tree := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "root", SessionID: "root"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child", Parent: "root", SessionID: "child"}}}}}}
	m.subagentSnapshot = &tree
	m.screen.Subagents = ui.NewSubagentPicker(tree, "root", "")
	m.handleSubagentPickerKey(t.Context(), ui.Key{Typ: ui.KeyDown})
	m.handleSubagentPickerKey(t.Context(), ui.Key{Typ: ui.KeyRune, Runes: []rune("s")})
	m.handleSubagentPickerKey(t.Context(), ui.Key{Typ: ui.KeyRune, Runes: []rune("y")})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stop did not reach the child handle")
	}
	m.handleSubagentPickerKey(t.Context(), ui.Key{Typ: ui.KeyEsc})
	require.Nil(t, m.screen.Subagents, "close view remains responsive while stop waits")
}
