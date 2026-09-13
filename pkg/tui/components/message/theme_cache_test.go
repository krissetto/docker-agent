package message

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestWarmMessageCachesFollowThemeGeneration(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	input := session.UserMessage("agent body")
	input.InputOrigin, input.SenderID, input.SenderName = session.InputOriginAgent, "full-node", "worker"
	agent := types.Input(input)
	agent.InputReference = lifecycle.InputReference{Kind: lifecycle.InputReferenceNode, ID: "full-node", Name: "worker", Agent: "worker", DisplayID: "full-"}
	ar := animation.NewRuntime()
	messages := []*types.Message{types.User("user body"), agent, types.Agent(types.MessageTypeAssistant, "root", "# Stable heading\n\n```go\nvar x = 1\n```\n\nmutable tail")}
	views := make([]*messageModel, len(messages))
	before := make([]string, len(messages))
	for i, msg := range messages {
		views[i] = New(ar, msg, nil)
		before[i] = views[i].Render(60)
		_, _ = views[i].RenderedSegments(60)
	}
	theme := *original
	theme.Colors.Background, theme.Colors.TextPrimary = "#ffffff", "#111111"
	theme.Colors.TextMuted = "#445566"
	styles.ApplyTheme(&theme)
	for i, view := range views {
		got := view.Render(60)
		fresh := New(ar, messages[i], nil)
		require.Equal(t, fresh.Render(60), got)
		require.NotEqual(t, before[i], got)
		segments, ok := view.RenderedSegments(60)
		if ok {
			freshSegments, freshOK := fresh.RenderedSegments(60)
			require.True(t, freshOK)
			require.Equal(t, freshSegments, segments)
			require.Equal(t, strings.Split(got, "\n"), append(append(append([]string{}, segments.Header...), segments.Stable...), segments.Tail...))
		}
	}
}

func TestAgentIdentityReferenceParticipatesInWarmCacheKey(t *testing.T) {
	input := session.UserMessage("body")
	input.InputOrigin = session.InputOriginAgent
	msg := types.Input(input)
	msg.InputReference = lifecycle.InputReference{Name: "before"}
	view := New(animation.NewRuntime(), msg, nil)
	before := view.Render(60)
	msg.InputReference.Name = "after"
	require.NotEqual(t, before, view.Render(60))
	require.Contains(t, view.Render(60), "after")
}
