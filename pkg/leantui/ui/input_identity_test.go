package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/agentidentity"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestAgentInputKeepsUserSurfaceWithoutTopRule(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	input := session.UserMessage("literal **body** 界")
	input.InputOrigin = session.InputOriginAgent
	msg := types.Input(input)
	msg.InputReference = lifecycle.InputReference{Kind: lifecycle.InputReferenceNode, ID: "abcde-full-id", Name: "worker", Agent: "worker", DisplayID: "abcde"}
	for _, light := range []bool{false, true} {
		if light {
			theme := *original
			theme.Colors.TextPrimary, theme.Colors.TextMuted = "#112233", "#445566"
			styles.ApplyTheme(&theme)
		}
		for _, width := range []int{1, 2, 8, 30, 60} {
			got, user := RenderInputLines(msg, width), RenderUserLines(msg.Content, width)
			require.Equal(t, user[1:], got[1:], "agent content keeps exact USER body ANSI")
			require.Equal(t, width, ansi.StringWidth(got[0]))
			require.NotContains(t, ansi.Strip(got[0]), "━")
			require.NotContains(t, ansi.Strip(got[0]), "┏")
			if width >= 30 {
				require.Contains(t, ansi.Strip(got[0]), msg.InputReference.Label())
				require.Contains(t, got[0], ansi.SetHyperlink(agentidentity.Link))
				require.Contains(t, got[0], strings.TrimSuffix(styles.AgentIdentityStyle("worker", false).Render("worker"), "\x1b[m"))
				require.Contains(t, got[0], strings.TrimSuffix(styles.MutedStyle.Render(" (abcde)"), "\x1b[m"))
				require.Equal(t, 1, strings.Count(got[0], ansi.SetHyperlink(agentidentity.Link)))
			}
		}
	}
}
