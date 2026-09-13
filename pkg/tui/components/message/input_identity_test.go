package message

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
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
			assert.Equal(t, firstCellANSI(want[0]), firstCellANSI(got[0]), "ordinary USER left border glyph and ANSI style are unchanged")
			assert.NotContains(t, ansi.Strip(got[0]), "━", "identity row must not add a horizontal rule")
			assert.NotContains(t, ansi.Strip(got[0]), "┏")
			view.SetHovered(true)
			assert.Equal(t, len(got), view.Height(width), "hover cannot change geometry")
			view.SetHovered(false)
		}
	}
}

// Cutting a cell also retains subsequent zero-width escapes; stop at its glyph.
func firstCellANSI(line string) string {
	p := ansi.GetParser()
	defer ansi.PutParser(p)
	var state byte
	for offset := 0; offset < len(line); {
		_, width, n, next := ansi.DecodeSequence(line[offset:], state, p)
		if n == 0 {
			break
		}
		offset += n
		if width > 0 {
			return line[:offset]
		}
		state = next
	}
	return line
}
