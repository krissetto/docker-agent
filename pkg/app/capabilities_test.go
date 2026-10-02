package app

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/stretchr/testify/require"
)

type parityHandle struct {
	projectionSession
	caps    runtime.SessionCapabilities
	calls   int
	failure error
	key     string
}

func (h *parityHandle) Metadata() runtime.SessionMetadata {
	return runtime.SessionMetadata{SessionID: h.ID(), AgentName: "worker", Capabilities: h.caps}
}
func (h *parityHandle) InspectTools(context.Context) (runtime.SessionToolsInfo, error) {
	h.calls++
	return runtime.SessionToolsInfo{Tools: []tools.Tool{{Name: "worker-tool"}}}, h.failure
}
func (h *parityHandle) EffectivePermissions(context.Context) (runtime.SessionPermissionsInfo, error) {
	h.calls++
	return runtime.SessionPermissionsInfo{Policy: session.SafetyPolicy("ask"), Source: &runtime.PermissionsInfo{Deny: []string{"worker-rule"}}}, h.failure
}
func (h *parityHandle) RestartToolset(context.Context, string) error { h.calls++; return h.failure }
func (h *parityHandle) MCPPrompts(context.Context) (map[string]tools.PromptInfo, error) {
	h.calls++
	return map[string]tools.PromptInfo{"0/a/b": {Name: "a/b"}}, h.failure
}
func (h *parityHandle) ExecuteMCPPrompt(_ context.Context, key string, _ map[string]string) (string, error) {
	h.calls++
	h.key = key
	return "prompt text", h.failure
}
func (h *parityHandle) SessionAgentInfo(context.Context) (runtime.SessionAgentInfo, error) {
	return runtime.SessionAgentInfo{Commands: types.Commands{"bound": {Instruction: "worker command"}}}, h.failure
}

func TestParityCapabilitiesBoundAndOldPeer(t *testing.T) {
	h := &parityHandle{projectionSession: projectionSession{id: "bound"}}
	a := &App{currentState: sessionState{session: session.New(session.WithID("bound")), handle: h}}
	// The concrete handle implements everything, but old peers advertise none.
	_, err := a.InspectTools(t.Context())
	require.ErrorIs(t, err, runtime.ErrUnsupported)
	_, err = a.EffectivePermissions(t.Context())
	require.ErrorIs(t, err, runtime.ErrUnsupported)
	_, err = a.MCPPrompts(t.Context())
	require.ErrorIs(t, err, runtime.ErrUnsupported)
	_, err = a.ExecuteMCPPrompt(t.Context(), "0/a/b", nil)
	require.ErrorIs(t, err, runtime.ErrUnsupported)
	require.ErrorIs(t, a.RestartToolset(t.Context(), "worker"), runtime.ErrUnsupported)
	require.Zero(t, h.calls)
	h.caps = runtime.SessionCapabilities{ToolInspection: true, PermissionsInspection: true, MCPPrompts: true, ToolsetRestart: true}
	info, err := a.InspectTools(t.Context())
	require.NoError(t, err)
	require.Equal(t, "worker-tool", info.Tools[0].Name)
	perms, err := a.EffectivePermissions(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{"worker-rule"}, CombinedPermissions(perms).Deny)
	text, err := a.ExecuteMCPPrompt(t.Context(), "0/a/b", nil)
	require.NoError(t, err)
	require.Equal(t, "prompt text", text)
	require.Equal(t, "0/a/b", h.key)
	require.Empty(t, h.inputs, "prompt retrieval must not submit a turn")
	h.failure = errors.New("remote failed")
	_, err = a.InspectTools(t.Context())
	require.ErrorIs(t, err, h.failure)
	require.ErrorIs(t, a.RestartToolset(t.Context(), "worker"), h.failure)
}

func TestParityMetadataGettersNeverPerformIO(t *testing.T) {
	h := &parityHandle{projectionSession: projectionSession{id: "bound"}, caps: runtime.SessionCapabilities{MCPPrompts: true, ToolInspection: true}}
	a := &App{currentState: sessionState{session: session.New(session.WithID("bound")), handle: h}}
	require.Empty(t, a.CurrentMCPPrompts(t.Context()))
	require.Empty(t, a.CurrentAgentCommands(t.Context()))
	require.Zero(t, h.calls)
	require.NoError(t, a.RefreshCommandMetadata(t.Context()))
	calls := h.calls
	for range 3 {
		require.Contains(t, a.CurrentMCPPrompts(t.Context()), "0/a/b")
		require.Contains(t, a.CurrentAgentCommands(t.Context()), "bound")
		a.CurrentAgentToolsetStatuses()
	}
	require.Equal(t, calls, h.calls)
	a.currentState.handle = &projectionSession{id: "replacement"}
	require.Empty(t, a.CurrentMCPPrompts(t.Context()))
	require.Empty(t, a.CurrentAgentCommands(t.Context()))
}

func TestParityCommandMetadataFailureCanRetry(t *testing.T) {
	h := &parityHandle{projectionSession: projectionSession{id: "bound"}, failure: errors.New("transient metadata")}
	a := &App{currentState: sessionState{session: session.New(session.WithID("bound")), handle: h}}
	require.ErrorIs(t, a.RefreshCommandMetadata(t.Context()), h.failure)
	require.Empty(t, a.CurrentAgentCommands(t.Context()))
	h.failure = nil
	require.NoError(t, a.RefreshCommandMetadata(t.Context()))
	require.Contains(t, a.CurrentAgentCommands(t.Context()), "bound")
}

func TestParityCapabilityViewPinsHandleBeforeAsyncWork(t *testing.T) {
	original := &parityHandle{projectionSession: projectionSession{id: "original"}, caps: runtime.SessionCapabilities{MCPPrompts: true}}
	replacement := &parityHandle{projectionSession: projectionSession{id: "replacement"}, caps: runtime.SessionCapabilities{MCPPrompts: true}}
	a := &App{currentState: sessionState{session: session.New(session.WithID("original")), handle: original}}
	bound := a.CapabilityView()
	a.currentState = sessionState{session: session.New(session.WithID("replacement")), handle: replacement}
	_, err := bound.ExecuteMCPPrompt(t.Context(), "0/a/b", nil)
	require.NoError(t, err)
	require.Equal(t, 1, original.calls)
	require.Zero(t, replacement.calls, "queued metadata request cannot retarget on session switch")
}
