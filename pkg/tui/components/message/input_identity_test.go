package message

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestInputIdentityUserBodyBackgroundAndNarrowBorder(t *testing.T) {
	for _, mode := range []string{"turn", "steer", ""} {
		input := session.UserMessage("literal **markdown** 界\n" + strings.Repeat("wrapped body ", 10))
		input.InputOrigin, input.InputMode, input.SenderName, input.SenderID = session.InputOriginAgent, mode, "worker", "abcde-full-id"
		msg := types.Input(input)
		msg.InputReference = lifecycle.InputReference{Kind: lifecycle.InputReferenceNode, ID: input.SenderID, Name: "worker", Agent: "worker", DisplayID: "abcde"}
		view := New(animation.NewRuntime(), msg, nil)
		user := New(animation.NewRuntime(), types.User(input.Message.Content), nil)
		for _, width := range []int{1, 2, 3, 4, 8, 12, 28, 80} {
			got := strings.Split(view.Render(width), "\n")
			want := strings.Split(user.Render(width), "\n")
			require.Equal(t, want[1:], got[1:], "agent body and ANSI background must exactly match USER: mode=%q width=%d", mode, width)
			assert.Equal(t, width, ansi.StringWidth(got[0]))
			border := lipgloss.NewStyle().
				Foreground(styles.UserMessageStyle.GetBorderLeftForeground()).
				Background(styles.UserMessageStyle.GetBorderLeftBackground()).Render("┏")
			assert.True(t, strings.HasPrefix(got[0], border), "top corner must use the USER left-border color")
			if width >= 4 {
				rule := lipgloss.NewStyle().
					Foreground(styles.UserMessageStyle.GetBorderLeftForeground()).
					Background(styles.UserMessageStyle.GetBackground()).Render("━ ")
				rulePrefix := strings.TrimSuffix(rule, "\x1b[m")
				rulePrefix = strings.TrimSuffix(rulePrefix, "\x1b[0m")
				assert.True(t, strings.HasPrefix(strings.TrimPrefix(got[0], border), rulePrefix), "top rule must use the USER border color and body background")
			}
			view.SetHovered(true)
			assert.Equal(t, len(got), view.Height(width), "hover cannot change geometry")
			view.SetHovered(false)
		}
	}
}
