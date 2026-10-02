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

type blockingSubagentControl struct {
	app.Services
	started, release chan struct{}
}

func (c *blockingSubagentControl) StopSubtree(subagent.NodeID) error {
	close(c.started)
	<-c.release
	return nil
}

func TestSubagentStopDoesNotBlockLeanEventLoop(t *testing.T) {
	m, _ := sessionModel(t)
	control := &blockingSubagentControl{Services: m.app.Runtime(), started: make(chan struct{}), release: make(chan struct{})}
	defer close(control.release)
	m.app = app.New(t.Context(), nil, session.New(session.WithID("root")), runtime.SessionBinding{}, app.WithRuntimeServices(control))
	m.viewers = &viewerHost{ctx: func() context.Context { return t.Context() }, events: make(chan any, 8)}
	m.screen.Subagents = ui.NewSubagentPicker(subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "root", SessionID: "root"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child", Parent: "root"}}}}}}, "root", "")
	m.handleSubagentPickerKey(t.Context(), ui.Key{Typ: ui.KeyDown})
	m.handleSubagentPickerKey(t.Context(), ui.Key{Typ: ui.KeyRune, Runes: []rune("s")})
	m.handleSubagentPickerKey(t.Context(), ui.Key{Typ: ui.KeyRune, Runes: []rune("y")})
	select {
	case <-control.started:
	case <-time.After(time.Second):
		t.Fatal("stop did not start")
	}
	m.handleSubagentPickerKey(t.Context(), ui.Key{Typ: ui.KeyEsc})
	require.Nil(t, m.screen.Subagents, "close view remains responsive while stop waits")
}
