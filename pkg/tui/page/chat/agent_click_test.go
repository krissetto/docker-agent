package chat

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
)

func TestAgentClickCmd_LeftClickSwitches(t *testing.T) {
	t.Parallel()

	cmd := (&chatPage{}).agentClickCmd("helper", tea.MouseLeft, 0)
	require.NotNil(t, cmd)

	msg, ok := cmd().(msgtypes.SwitchAgentMsg)
	require.True(t, ok, "a plain left-click switches to the agent")
	assert.Equal(t, "helper", msg.AgentName)
}

// TestAgentClickCmd_LeftClickOnCurrentAgentStillSwitches documents the routing
// change: left-click now always switches, even on the already-current agent (a
// harmless no-op switch). Details are reached via right-click / Ctrl+click.
func TestAgentClickCmd_LeftClickOnCurrentAgentStillSwitches(t *testing.T) {
	t.Parallel()

	cmd := (&chatPage{}).agentClickCmd("root", tea.MouseLeft, 0)
	require.NotNil(t, cmd)

	_, ok := cmd().(msgtypes.SwitchAgentMsg)
	assert.True(t, ok, "left-click always switches, never opens details")
}

func TestAgentClickCmd_RightClickOpensDetails(t *testing.T) {
	t.Parallel()

	cmd := (&chatPage{}).agentClickCmd("helper", tea.MouseRight, 0)
	require.NotNil(t, cmd)

	msg, ok := cmd().(msgtypes.ShowAgentDetailsMsg)
	require.True(t, ok, "right-click opens the details dialog")
	assert.Equal(t, "helper", msg.AgentName)
}

func TestAgentClickCmd_CtrlLeftClickOpensDetails(t *testing.T) {
	t.Parallel()

	cmd := (&chatPage{}).agentClickCmd("helper", tea.MouseLeft, tea.ModCtrl)
	require.NotNil(t, cmd)

	msg, ok := cmd().(msgtypes.ShowAgentDetailsMsg)
	require.True(t, ok, "Ctrl+left-click opens the details dialog (fallback)")
	assert.Equal(t, "helper", msg.AgentName)
}

func TestAgentClickCmd_EmptyAgentNoCmd(t *testing.T) {
	t.Parallel()

	assert.Nil(t, (&chatPage{}).agentClickCmd("", tea.MouseLeft, 0))
}

func TestAgentClickCmd_OtherButtonNoCmd(t *testing.T) {
	t.Parallel()

	assert.Nil(t, (&chatPage{}).agentClickCmd("helper", tea.MouseMiddle, 0),
		"middle-click is not a handled gesture")
}

func TestThinkingClickEmitsCanonicalOriginWithoutChangingProjection(t *testing.T) {
	p := newLayoutTestPage(t, msgtypes.SidebarRight)
	p.sessionState.SetCurrentAgentName("root")
	p.sidebar.SetTeamInfo([]runtime.AgentDetails{{Name: "root", Provider: "provider", ModelID: "canonical", ModelName: "Friendly", ThinkingMode: "effort", ThinkingLevel: "high", CanCycleThinking: true}})
	cmd := p.cycleThinkingLevelCmd("origin-session")
	require.NotNil(t, cmd)
	assert.Equal(t, msgtypes.CycleThinkingLevelMsg{SessionID: "origin-session", AgentName: "root", ModelRef: "provider/canonical"}, cmd())
	assert.Nil(t, p.cycleThinkingLevelCmd(""))
	p.sidebar.SetTeamInfo([]runtime.AgentDetails{{Name: "root", Provider: "provider", ModelID: "canonical", ThinkingMode: "unsupported"}})
	assert.Nil(t, p.cycleThinkingLevelCmd("origin-session"))
}

func TestFallbackThinkingClickCarriesPrimaryAndDisplayedOrigins(t *testing.T) {
	p := newLayoutTestPage(t, msgtypes.SidebarRight)
	p.sessionState.SetCurrentAgentName("root")
	p.sidebar.SetTeamInfo([]runtime.AgentDetails{{
		Name: "root", Provider: "provider", ModelID: "fallback", ThinkingMode: "unsupported",
		PrimaryThinking: &runtime.ThinkingDetails{ModelRef: "provider/primary", Mode: "effort", Level: "high", CanCycle: true},
	}})
	cmd := p.cycleThinkingLevelCmd("origin-session")
	require.NotNil(t, cmd)
	assert.Equal(t, msgtypes.CycleThinkingLevelMsg{SessionID: "origin-session", AgentName: "root", ModelRef: "provider/primary", DisplayedModelRef: "provider/fallback"}, cmd())
	p.sidebar.SetTeamInfo([]runtime.AgentDetails{{
		Name: "root", Provider: "provider", ModelID: "fallback", ThinkingMode: "effort", CanCycleThinking: true,
		PrimaryThinking: &runtime.ThinkingDetails{ModelRef: "provider/primary", Mode: "unsupported"},
	}})
	assert.Nil(t, p.cycleThinkingLevelCmd("origin-session"), "fallback capability must not enable primary mutation")
}
