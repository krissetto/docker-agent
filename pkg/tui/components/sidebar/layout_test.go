package sidebar

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/scrollbar"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestDefaultLayoutConfig(t *testing.T) {
	t.Parallel()

	cfg := DefaultLayoutConfig()

	assert.Equal(t, 1, cfg.PaddingLeft, "default PaddingLeft should be 1")
	assert.Equal(t, 0, cfg.PaddingRight, "default PaddingRight should be 0")
	assert.Equal(t, 1, cfg.ScrollbarGap, "default ScrollbarGap should be 1")
}

func TestScrollbarWidthConstant(t *testing.T) {
	t.Parallel()

	// Verify scrollbar.Width is the expected value
	assert.Equal(t, 1, scrollbar.Width, "scrollbar.Width constant should be 1")
}

func TestLayoutConfigCompute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		config           LayoutConfig
		outerWidth       int
		scrollbarVisible bool
		wantContentWidth int
	}{
		{
			name:             "default config without scrollbar",
			config:           DefaultLayoutConfig(),
			outerWidth:       40,
			scrollbarVisible: false,
			wantContentWidth: 39, // 40 - 1 (paddingLeft) - 0 (paddingRight)
		},
		{
			name:             "default config with scrollbar",
			config:           DefaultLayoutConfig(),
			outerWidth:       40,
			scrollbarVisible: true,
			wantContentWidth: 37, // 40 - 1 (paddingLeft) - 0 (paddingRight) - 1 (scrollbarWidth) - 1 (scrollbarGap)
		},
		{
			name: "custom config without scrollbar",
			config: LayoutConfig{
				PaddingLeft:  2,
				PaddingRight: 1,
				ScrollbarGap: 2,
			},
			outerWidth:       50,
			scrollbarVisible: false,
			wantContentWidth: 47, // 50 - 2 - 1
		},
		{
			name: "custom config with scrollbar",
			config: LayoutConfig{
				PaddingLeft:  2,
				PaddingRight: 1,
				ScrollbarGap: 2,
			},
			outerWidth:       50,
			scrollbarVisible: true,
			wantContentWidth: 44, // 50 - 2 - 1 - 1 (scrollbarWidth constant) - 2
		},
		{
			name:             "minimum content width",
			config:           DefaultLayoutConfig(),
			outerWidth:       2,
			scrollbarVisible: true,
			wantContentWidth: 1, // Should never go below 1
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			metrics := tt.config.Compute(tt.outerWidth, tt.scrollbarVisible)

			assert.Equal(t, tt.outerWidth, metrics.OuterWidth)
			assert.Equal(t, tt.wantContentWidth, metrics.ContentWidth)
			assert.Equal(t, tt.scrollbarVisible, metrics.ScrollbarVisible)
		})
	}
}

func TestMetricsAccessors(t *testing.T) {
	t.Parallel()

	cfg := LayoutConfig{
		PaddingLeft:  3,
		PaddingRight: 2,
		ScrollbarGap: 2,
	}

	metrics := cfg.Compute(50, true)

	assert.Equal(t, 3, metrics.PaddingLeft())
	assert.Equal(t, 2, metrics.PaddingRight())
	assert.Equal(t, 2, metrics.ScrollbarGap())
}

// BenchmarkSidebarVerticalView_Scroll benchmarks the verticalView() method during scrolling.
// This verifies that scrolling uses the render cache instead of re-rendering sections.
func BenchmarkSidebarVerticalView_Scroll(b *testing.B) {
	sessionState := &service.SessionState{}
	m := New(animation.NewRuntime(), b.Context(), sessionState).(*model)
	m.SetSize(35, 50)
	m.SetMode(ModeVertical)

	// Add some agents to create content
	m.SetTeamInfo([]runtime.AgentDetails{
		{Name: "agent1", Model: "gpt-4", Description: "First agent with a long description that might wrap"},
		{Name: "agent2", Model: "claude-3", Description: "Second agent with another description"},
		{Name: "agent3", Model: "gemini", Description: "Third agent"},
	})

	// Add some token usage
	m.SetTokenUsage(&runtime.TokenUsageEvent{
		SessionID:    "session-1",
		AgentContext: runtime.AgentContext{AgentName: "agent1"},
		Usage: &runtime.Usage{
			InputTokens:  10000,
			OutputTokens: 5000,
			Cost:         0.50,
		},
	})

	// Add some tools
	m.SetToolsetInfo(25, false)

	// Initial render to populate cache
	_ = m.verticalView()

	b.ResetTimer()
	b.ReportAllocs()

	for i := range b.N {
		// Simulate scrolling by updating scroll offset
		m.scrollview.SetScrollOffset(i % 10)
		_ = m.verticalView()
	}
}

// BenchmarkSidebarVerticalView_NoCache benchmarks verticalView() when cache is always dirty.
// This shows the cost of full re-rendering for comparison.
func BenchmarkSidebarVerticalView_NoCache(b *testing.B) {
	sessionState := &service.SessionState{}
	m := New(animation.NewRuntime(), b.Context(), sessionState).(*model)
	m.SetSize(35, 50)
	m.SetMode(ModeVertical)

	// Add some agents to create content
	m.SetTeamInfo([]runtime.AgentDetails{
		{Name: "agent1", Model: "gpt-4", Description: "First agent with a long description that might wrap"},
		{Name: "agent2", Model: "claude-3", Description: "Second agent with another description"},
		{Name: "agent3", Model: "gemini", Description: "Third agent"},
	})

	// Add some token usage
	m.SetTokenUsage(&runtime.TokenUsageEvent{
		SessionID:    "session-1",
		AgentContext: runtime.AgentContext{AgentName: "agent1"},
		Usage: &runtime.Usage{
			InputTokens:  10000,
			OutputTokens: 5000,
			Cost:         0.50,
		},
	})

	// Add some tools
	m.SetToolsetInfo(25, false)

	b.ReportAllocs()

	for b.Loop() {
		// Invalidate cache before each render to force full re-render
		m.invalidateCache()
		_ = m.verticalView()
	}
}

func TestExpandedYoloFooterStaysPinnedWhileContentScrolls(t *testing.T) {
	t.Parallel()

	m := newVisibilityTestSidebar(t).model
	t.Cleanup(m.ar.Stop)
	m.SetSize(40, 6)
	m.SetPosition(10, 20)
	m.sessionState.SetYoloMode(true)
	m.sessionState.SetHideToolResults(true)

	lines := strings.Split(m.View(), "\n")
	require.Len(t, lines, 6)
	assert.True(t, strings.HasSuffix(ansi.Strip(lines[5]), "YOLO  ^y"))
	assert.Contains(t, ansi.Strip(strings.Join(m.cachedLines, "\n")), "Tool output hidden")
	assert.NotContains(t, strings.Join(m.cachedLines, "\n"), "YOLO")
	kind, _ := m.HandleClickType(m.layoutCfg.PaddingLeft, 5)
	assert.Equal(t, ClickNone, kind, "the footer is not a scroll-content row")
	barX := m.scrollview.ScrollbarX()
	assert.Nil(t, m.SetScrollbarPointer(barX, 25), "hovering the footer must not reveal the scrollbar")
	assert.NotNil(t, m.SetScrollbarPointer(barX, 24), "hovering the last viewport row reveals the scrollbar")
	m.ClearScrollbarPointer()

	m.scrollview.ScrollToBottom()
	scrolled := strings.Split(m.View(), "\n")
	require.Len(t, scrolled, len(lines))
	assert.NotEqual(t, lines[0], scrolled[0])
	assert.Equal(t, lines[5], scrolled[5], "the badge stays pinned")

	m.sessionState.SetYoloMode(false)
	m.Update(messages.SessionToggleChangedMsg{})
	assert.NotContains(t, m.View(), "YOLO")
	assert.Equal(t, 6, m.scrollview.VisibleHeight())
}

func TestExpandedRosterUpdatesModelWithoutShowingProvider(t *testing.T) {
	t.Parallel()

	m := newVisibilityTestSidebar(t).model
	t.Cleanup(m.ar.Stop)
	m.SetTeamInfo(nil)
	m.SetAgentInfo("root", "openai/gpt-4o", "", 0, "", 0)
	m.SetSize(MinWidth, 20)
	startup := ansi.Strip(m.View())
	assert.Contains(t, startup, "▶ root")
	assert.Contains(t, startup, "gpt-4o", "AgentInfo can arrive before TeamInfo")
	assert.NotContains(t, startup, "openai")

	m.SetTeamInfo([]runtime.AgentDetails{
		{Name: "root", Provider: "openai", Model: "gpt-4o", Thinking: "high"},
		{Name: "reviewer", Provider: "anthropic", Model: "claude-sonnet"},
	})
	initial := ansi.Strip(m.View())
	assert.Contains(t, initial, "▶ root")
	assert.Contains(t, initial, "gpt-4o")
	assert.Contains(t, initial, "claude-sonnet")
	assert.NotContains(t, initial, "openai")
	assert.NotContains(t, initial, "anthropic")
	assert.NotContains(t, initial, "▰")

	m.sessionState.SetCurrentAgentName("reviewer")
	m.SetAgentInfo("reviewer", "dmr/ai/llama3.2", "", 0, "", 0)
	updated := ansi.Strip(m.View())
	assert.Contains(t, updated, "▶ reviewer")
	assert.Contains(t, updated, "gpt-4o", "the other agent's model is unchanged")
	assert.Contains(t, updated, "ai/llama3.2", "slashes in the model name survive")
	assert.NotContains(t, updated, "dmr/")
	assert.NotContains(t, updated, "claude-sonnet")
	assert.NotContains(t, updated, "▶ root")
	for y, row := range strings.Split(updated, "\n") {
		if strings.Contains(row, "ai/llama3.2") {
			kind, agent := m.HandleClickType(m.layoutCfg.PaddingLeft+agentMarkerWidth, y)
			assert.Equal(t, ClickAgent, kind)
			assert.Equal(t, "reviewer", agent)
		}
	}
}
