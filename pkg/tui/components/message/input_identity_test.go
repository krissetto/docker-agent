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
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestInputIdentityUserBodyBackgroundAndNarrowBorder(t *testing.T) {
	for _, mode := range []string{"turn", ""} {
		input := session.UserMessage("literal **markdown** 界\n" + strings.Repeat("wrapped body ", 10))
		input.InputOrigin, input.InputMode, input.SenderName, input.SenderID = session.InputOriginAgent, mode, "worker", "abcde-full-id"
		msg := types.Input(input)
		msg.InputReference = lifecycle.InputReference{Kind: lifecycle.InputReferenceNode, ID: input.SenderID, Name: "worker", Agent: "worker", DisplayID: "abcde"}
		view := New(animation.NewRuntime(), msg, nil)
		view.SetExpanded(true)
		user := New(animation.NewRuntime(), types.User(input.Message.Content), nil)
		for _, width := range []int{1, 2, 3, 4, 8, 12, 28, 80} {
			view.SetSize(width, 0)
			got := strings.Split(view.Render(width), "\n")[strings.Count(view.replyHeader(width), "\n")+1:]
			want := strings.Split(user.Render(width), "\n")
			require.Equal(t, ansi.Strip(strings.Join(want[1:], "\n")), ansi.Strip(strings.Join(got[1:], "\n")), "agent body keeps USER text and layout: mode=%q width=%d", mode, width)
			bodyStyle := styles.UserMessageStyle.Bold(false)
			innerWidth := width - bodyStyle.GetHorizontalFrameSize()
			// USER presentation trims terminal whitespace before layout; provenance
			// retains the original bytes, including this fixture's trailing space.
			content := strings.TrimRight(msg.Content, "\n\r\t ")
			normal := bodyStyle.PaddingTop(0).Width(width).Render(actionRow(innerWidth, false, types.MessageCopyLabel) + "\n" + content)
			require.Equal(t, input.Message.Content, msg.Content)
			require.Equal(t, strings.Split(normal, "\n")[1:], got[1:], "only inherited bold changes; USER body colors, padding and ANSI remain exact")
			header := view.replyHeader(width)
			for line := range strings.SplitSeq(header, "\n") {
				assert.LessOrEqual(t, ansi.StringWidth(line), width)
			}
			if width == 80 {
				assert.Contains(t, ansi.Strip(header), "worker (abcde) sent a message v")
			}

			view.SetHovered(true)
			assert.Equal(t, len(got)+strings.Count(view.replyHeader(width), "\n")+1, view.Height(width), "hover cannot change geometry")
			view.SetHovered(false)
		}
	}
}

func TestAgentBodyNormalWeightPreservesLiteralTextAndExplicitANSI(t *testing.T) {
	const body = "Natural spaced prose cafe\u0301 界 **literal bold** " + "\x1b[1mintentional bold\x1b[22m normal again " + "\x1b]8;;https://example.invalid\x1b\\linked words\x1b]8;;\x1b\\"
	input := session.UserMessage(body)
	input.InputOrigin, input.SenderName = session.InputOriginAgent, "worker"
	msg := types.Input(input)
	view := New(animation.NewRuntime(), msg, nil)
	view.SetExpanded(true)
	for _, selected := range []bool{false, true} {
		position := 0
		msg.SessionPosition = &position
		view.SetSelected(selected)
		out := view.Render(160)
		assert.Equal(t, body, msg.Content, "rendering does not rewrite source bytes")
		assert.Contains(t, ansi.Strip(out), ansi.Strip(body), "natural spaces, graphemes and literal Markdown remain intact")
		for _, word := range []string{"Natural", "**literal", "normal again", "linked words"} {
			assert.False(t, boldAtText(t, out, word), "ordinary agent prose: %s", word)
		}
		assert.True(t, boldAtText(t, out, "intentional bold"))
		assert.Contains(t, out, "\x1b]8;;https://example.invalid\x1b\\")
	}
	user := New(animation.NewRuntime(), types.User("Natural user prose"), nil)
	assert.True(t, boldAtText(t, user.Render(80), "Natural"), "ordinary USER emphasis is unchanged")
}

// Decode paint attributes at a visible text cell rather than matching SGR bytes.
func boldAtText(t *testing.T, rendered, text string) bool {
	t.Helper()
	for line := range strings.SplitSeq(rendered, "\n") {
		plain := ansi.Strip(line)
		offset := strings.Index(plain, text)
		if offset < 0 {
			continue
		}
		col := ansi.StringWidth(plain[:offset])
		p := ansi.GetParser()
		var state byte
		bold := false
		for x := 0; line != ""; {
			seq, width, n, next := ansi.DecodeSequence(line, state, p)
			if n == 0 {
				break
			}
			if ansi.HasCsiPrefix(seq) && p.Command() == 'm' {
				params := p.Params()
				if len(params) == 0 {
					bold = false
				}
				for i := 0; i < len(params); i++ {
					switch params[i].Param(0) {
					case 0, 22:
						bold = false
					case 1:
						bold = true
					case 38, 48, 58:
						if i+1 < len(params) {
							switch params[i+1].Param(0) {
							case 2:
								i += 4
							case 5:
								i += 2
							}
						}
					}
				}
			}
			if width > 0 && x+width > col {
				ansi.PutParser(p)
				return bold
			}
			x += width
			state, line = next, line[n:]
		}
		ansi.PutParser(p)
	}
	t.Fatalf("missing painted text %q", text)
	return false
}
