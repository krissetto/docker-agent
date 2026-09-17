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

func TestAgentInputKeepsUserSurfaceWithBorderIdentity(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	const body = "Natural spaced prose cafe\u0301 界 **literal body** \x1b[1mintentional bold\x1b[22m normal \x1b]8;;https://example.invalid\x1b\\linked words\x1b]8;;\x1b\\"
	input := session.UserMessage(body)
	input.InputOrigin = session.InputOriginAgent
	msg := types.Input(input)
	msg.InputReference = lifecycle.InputReference{Kind: lifecycle.InputReferenceNode, ID: "abcde-full-id", Name: "worker", Agent: "worker", DisplayID: "abcde"}
	for _, light := range []bool{false, true} {
		if light {
			theme := *original
			theme.Colors.TextPrimary, theme.Colors.TextMuted = "#112233", "#445566"
			styles.ApplyTheme(&theme)
		}
		for _, width := range []int{1, 2, 8, 30, 60, 160} {
			got, user := RenderInputLines(msg, width), RenderUserLines(msg.Content, width)
			require.Equal(t, body, msg.Content)
			if width == 160 {
				require.Contains(t, ansi.Strip(strings.Join(got[1:], "\n")), ansi.Strip(body))
				require.Contains(t, strings.Join(got[1:], "\n"), "\x1b]8;;https://example.invalid\x1b\\")
			}
			require.Equal(t, user[1:], got[1:], "agent content keeps exact USER body ANSI")
			require.Equal(t, width, ansi.StringWidth(got[0]))
			require.True(t, strings.HasPrefix(ansi.Strip(got[0]), "─"))
			require.True(t, strings.HasPrefix(got[0], seqPromptStart))
			require.True(t, strings.HasSuffix(got[0], seqOutputStart))
			require.Equal(t, 1, strings.Count(strings.Join(got, "\n"), seqPromptStart))
			require.Equal(t, 1, strings.Count(strings.Join(got, "\n"), seqOutputStart))
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
