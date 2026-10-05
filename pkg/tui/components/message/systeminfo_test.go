package message

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestUserSystemInfoLiteralIsNotRuntimeAttribution(t *testing.T) {
	const literal = `<system_info>Subagent "worker" (a1b2c) finished.</system_info>`
	view := New(animation.NewRuntime(), types.User(literal), nil)
	out := stripANSI(view.Render(100))
	assert.Contains(t, out, literal)
	assert.NotContains(t, out, "replied")
	assert.NotContains(t, out, "system info received")
}

func TestTypedAgentBodyIsAttributedLiteralText(t *testing.T) {
	const body = "<system_info>agent literal</system_info>\n**readable Markdown**\n```go\nfmt.Println(42)\n```"
	input := session.UserMessage(body)
	input.InputOrigin, input.InputMode = session.InputOriginAgent, "steer"
	input.SenderName, input.SenderID = "worker", "child"
	msg := types.Input(input)
	view := New(animation.NewRuntime(), msg, nil)
	assert.NotContains(t, view.Render(100), "agent literal")
	view.SetExpanded(true)
	out := stripANSI(view.Render(100))
	assert.Contains(t, out, "worker (child)")
	assert.Contains(t, out, "<system_info>agent literal</system_info>")
	assert.Contains(t, out, "**readable Markdown**")
	assert.Contains(t, out, "fmt.Println(42)")
	assert.NotContains(t, out, "system info received")
	_, segmented := view.RenderedSegments(100)
	assert.False(t, segmented)
}

func TestTypedDelegationUsesUserStyle(t *testing.T) {
	input := session.UserMessage("original delegation **literal** <system_info>kept</system_info>")
	input.InputOrigin, input.InputMode = session.InputOriginAgent, "turn"
	input.SenderName, input.SenderID = "director", "12345678-long-id"
	actual := New(animation.NewRuntime(), types.Input(input), nil)
	actual.SetExpanded(true)
	user := New(animation.NewRuntime(), types.User(input.Message.Content), nil)
	assert.Equal(t, strings.Split(stripANSI(user.Render(100)), "\n")[1:], strings.Split(stripANSI(actual.Render(100)), "\n")[2:])
	assert.Contains(t, stripANSI(actual.Render(100)), "director (12345)")
	assert.False(t, boldAtText(t, actual.Render(100), "original delegation"))
	assert.True(t, boldAtText(t, user.Render(100), "original delegation"))
}
