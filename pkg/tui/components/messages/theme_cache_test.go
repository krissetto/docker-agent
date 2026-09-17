package messages

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/agentidentity"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/subagentindex"
)

func TestWarmAgentTranscriptRestylesAndKeepsFullIdentityHitCells(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	index := subagentindex.New()
	index.Reset(subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "abcde-full-node-id", SessionID: "child-session", Agent: "worker", Name: "Worker 界"}}}})
	m := newModel(animation.NewRuntime(), 60, 15, &service.SessionState{}, index)
	t.Cleanup(m.StopAnimations)
	input := session.UserMessage("ordinary user surface body")
	input.InputOrigin, input.SenderID, input.SenderName = session.InputOriginAgent, "child-session", "worker"
	m.AddInputMessage(input, 0)
	before := m.View()
	generation := m.contentGeneration
	theme := *original
	theme.Colors.Background, theme.Colors.TextPrimary, theme.Colors.TextMuted = "#ffffff", "#111111", "#445566"
	theme.Colors.BackgroundAlt = "#dddddd"
	styles.ApplyTheme(&theme)
	after := m.View()
	require.NotEqual(t, before, after)
	require.Greater(t, m.contentGeneration, generation)
	require.Equal(t, ansi.Strip(before), ansi.Strip(after))
	require.Contains(t, ansi.Strip(strings.Split(after, "\n")[0]), "━ Worker 界 (abcde) ━")
	hits := 0
	for y, line := range m.renderedLines {
		for _, span := range extractOSC8Links(line) {
			if span.url != agentidentity.Link {
				continue
			}
			for x := span.startCol; x < span.endCol; x++ {
				id, ok := m.SubagentNodeAt(x, y)
				require.True(t, ok)
				require.Equal(t, subagent.NodeID("abcde-full-node-id"), id)
				hits++
			}
		}
	}
	require.Positive(t, hits)
	rebuilds, misses, renders := m.ResizeCacheStats()
	require.Equal(t, after, m.View())
	r, miss, render := m.ResizeCacheStats()
	require.Equal(t, rebuilds, r)
	require.Equal(t, misses, miss)
	require.Equal(t, renders, render)
}

func TestInlineEditThemeRefreshPreservesTextAndSelection(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	m := NewScrollableView(animation.NewRuntime(), 60, 12, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	m.AddUserMessage("editable body")
	m.StartInlineEdit(0, 0, "editable body")
	before := m.View()
	position := m.inlineEditTextarea.Line()
	theme := *original
	theme.Colors.BackgroundAlt, theme.Colors.TextPrimary = "#dddddd", "#112233"
	styles.ApplyTheme(&theme)
	after := m.View()
	require.NotEqual(t, before, after)
	require.Equal(t, "editable body", m.inlineEditTextarea.Value())
	require.Equal(t, position, m.inlineEditTextarea.Line())
	require.True(t, m.IsInlineEditing())
	require.Equal(t, inlineEditStyles(), m.inlineEditTextarea.Styles())
}
