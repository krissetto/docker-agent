package message

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/reviewdraft"
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

func TestTypedReviewReferencesRenderAsReadableHumanUserMessage(t *testing.T) {
	body := reviewdraft.SerializeWithGeneral([]reviewdraft.Comment{
		{ID: "internal", Anchor: reviewdraft.Anchor{
			RepositoryID: "repo-id", RepositoryName: "demo", WorktreeID: "wt-id", WorktreePath: "/work/demo",
			Path: "src/main.go", Mode: "changes", Group: "unstaged", NewStart: 7, NewEnd: 8,
			Context: []string{"-old", "+new"}, Version: "internal-version",
		}, Body: "Please explain this"},
		{Anchor: reviewdraft.Anchor{
			RepositoryID: "repo-id", RepositoryName: "demo", WorktreeID: "wt-id", WorktreePath: "/work/demo",
			Path: "README.md", Mode: "files", NewStart: 2, NewEnd: 2,
			Context: []string{"# Readme"}, Version: "internal-version",
		}, Body: "Clarify this"},
	}, "General context")
	input := session.UserMessage(body)
	msg := types.Input(input)
	view := New(animation.NewRuntime(), msg, nil)
	readable := stripANSI(view.Render(100))
	assert.Contains(t, readable, "Git · src/main.go:7-8")
	assert.Contains(t, readable, "README.md:2")
	assert.Contains(t, readable, "+new")
	assert.Contains(t, readable, "Please explain this")
	assert.Contains(t, readable, "General context")
	assert.NotContains(t, readable, "<user-references")
	assert.NotContains(t, readable, "internal-version")
	assert.NotContains(t, readable, "internal")
	assert.Equal(t, types.MessageTypeUser, msg.Type, "reference presentation remains a human USER input")
	assert.Equal(t, body, msg.Content, "canonical payload remains intact for copy/edit/history")
	assert.NotContains(t, readable, "system info received")
	if dir := os.Getenv("TUI_REFERENCE_FIXTURE_DIR"); dir != "" {
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "reference-user.ansi.txt"), []byte(view.Render(100)), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "reference-user.plain.txt"), []byte(ansi.Strip(view.Render(100))), 0o644))
	}
}

func TestMalformedTypedReviewEnvelopeRemainsLiteralUserBody(t *testing.T) {
	const malformed = `<user-references version="1">\n{"comments":[]}\n</user-references> extra`
	view := New(animation.NewRuntime(), types.User(malformed), nil)
	assert.Contains(t, stripANSI(view.Render(100)), malformed)
}

func TestTypedReviewReferenceCollapseUsesReadableLinesAndPreservesCanonicalBody(t *testing.T) {
	anchor := reviewdraft.Anchor{
		RepositoryID: "repo", RepositoryName: "demo", WorktreeID: "wt", WorktreePath: "/work/demo",
		Path: "large.txt", Mode: "files", NewStart: 1, NewEnd: 1,
		Context: []string{"first"}, Version: "internal",
	}
	for range 35 {
		anchor.Context = append(anchor.Context, "snippet line")
	}
	body := reviewdraft.Serialize([]reviewdraft.Comment{{Anchor: anchor, Body: "last comment"}})
	view := New(animation.NewRuntime(), types.User(body), nil)
	collapsed := stripANSI(view.Render(100))
	assert.Contains(t, collapsed, "[+] expand")
	assert.NotContains(t, collapsed, "last comment")
	height := view.Height(100)
	assert.True(t, view.IsToggleAt(height-1, 1), "bottom padding is the standard collapse/expand hit target")
	assert.True(t, view.IsToggleAt(height-2, 1), "indicator line is the standard collapse/expand hit target")
	assert.True(t, view.IsToggleAt(height-3, 1), "preceding blank line remains a hit target")
	assert.False(t, view.IsToggleAt(height-4, 1), "content remains outside the hit target")
	view.Toggle()
	expanded := stripANSI(view.Render(100))
	assert.Contains(t, expanded, "last comment")
	assert.Equal(t, body, view.message.Content, "display projection never mutates source/copy/edit content")
}
