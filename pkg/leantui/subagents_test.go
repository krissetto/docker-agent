package leantui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

func TestSubagentsPickerBusyEscapeAndStaleEnterDoNotSend(t *testing.T) {
	m, handle := sessionModel(t)
	handle.submitted = nil
	m.setTestBusy(true)
	m.screen.Editor.SetText("unsent draft")
	m.handleEvent(t.Context(), &runtime.SubagentTreeEvent{Snapshot: subagent.Snapshot{
		Root: "stable-root", Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "stable-root", SessionID: m.app.Session().ID, Name: "Root"},
			Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child", Name: "Worker"}}},
		}},
	}})
	require.True(t, m.handleSlash(t.Context(), "/subagents", busySubmitSteer))
	require.NotNil(t, m.screen.Subagents)
	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyDown})
	m.handleEvent(t.Context(), &runtime.SubagentTreeEvent{Snapshot: subagent.Snapshot{}})
	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyEnter})
	require.NotNil(t, m.screen.Subagents, "a stale row cannot attach or send")
	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyEsc})
	assert.Nil(t, m.screen.Subagents)
	assert.Equal(t, "unsent draft", m.screen.Editor.Text())
	assert.True(t, m.busy())
	assert.Zero(t, handle.stops)
	assert.Empty(t, handle.submitted)
	assert.Empty(t, handle.sent)
	assert.False(t, m.interruptPending)
}

func TestSubagentsPickerPublicCommandAttachesNestedDuplicateName(t *testing.T) {
	f := newViewerLifecycleFixture(t)
	nested, err := f.borrowed.CreateSession(t.Context(), session.New(session.WithID("33333333-3333-4333-8333-333333333333")), runtime.SessionBinding{AgentName: "worker", ParentSessionID: f.child.ID()})
	require.NoError(t, err)
	nestedID, ok := f.local.SubagentNodeForSession(nested.ID())
	require.True(t, ok)
	snapshot := f.local.SubagentTree().Snapshot()
	f.m.handleEvent(t.Context(), &runtime.SubagentTreeEvent{Snapshot: snapshot})
	originalScreen := f.m.screen
	f.m.screen.Editor.SetText("original draft")
	require.True(t, f.m.handleSlash(t.Context(), "/subagents", busySubmitSteer))
	require.NotNil(t, f.m.screen.Subagents)
	f.m.handleKey(t.Context(), ui.Key{Typ: ui.KeyRight})
	selected, ok := f.m.screen.Subagents.Current()
	require.True(t, ok)
	require.Equal(t, f.node, selected.ID)
	f.m.handleKey(t.Context(), ui.Key{Typ: ui.KeyRight})
	selected, ok = f.m.screen.Subagents.Current()
	require.True(t, ok)
	require.Equal(t, nestedID, selected.ID)
	f.m.handleKey(t.Context(), ui.Key{Typ: ui.KeyEnter})
	require.Equal(t, nested.ID(), f.m.app.SessionHandle().ID())
	require.Equal(t, nestedID, f.m.app.AttachedSubagent().NodeID)
	assert.Nil(t, originalScreen.Subagents)
	assert.Equal(t, "original draft", originalScreen.Editor.Text())
	assert.Empty(t, f.provider.calls, "opening a viewer never submits a message")
	assert.Zero(t, f.provider.canceled.Load())

	f.m.handleEvent(t.Context(), &runtime.SubagentTreeEvent{Snapshot: snapshot})
	f.m.screen.Editor.SetText("nested draft")
	require.True(t, f.m.handleSlash(t.Context(), "/subagents", busySubmitSteer))
	selected, ok = f.m.screen.Subagents.Current()
	require.True(t, ok)
	assert.Equal(t, nestedID, selected.ID, "current attached row is selected")
	f.m.handleKey(t.Context(), ui.Key{Typ: ui.KeyEsc})
	require.True(t, f.m.handleSlash(t.Context(), "/back", busySubmitSteer))
	assert.Same(t, f.root, f.m.app)
	assert.Same(t, originalScreen, f.m.screen)
	assert.Equal(t, "original draft", f.m.screen.Editor.Text())
	assert.Empty(t, f.provider.calls)
	assert.Zero(t, f.provider.canceled.Load())
}

func TestSubagentsPickerComposerCommandAndAliasRemoval(t *testing.T) {
	m, handle := sessionModel(t)
	handle.submitted = nil
	m.refreshCommands(t.Context())
	for _, alias := range []string{"subagent-view", "subagent-attach"} {
		for _, command := range builtinCommands() {
			assert.NotEqual(t, alias, command.Name)
		}
		assert.False(t, m.handleCapabilityCommand(t.Context(), alias, "child", busySubmitSteer))
	}
	m.screen.Editor.SetText("/subagents")
	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyEnter})
	require.NotNil(t, m.screen.Subagents)
	assert.Empty(t, m.screen.Editor.Text())
	assert.Empty(t, handle.submitted)
	assert.Empty(t, handle.sent)
	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyEsc})
	assert.Nil(t, m.screen.Subagents)
}
