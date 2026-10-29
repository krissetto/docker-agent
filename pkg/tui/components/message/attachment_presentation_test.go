package message

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func projectedUserMessage(t *testing.T, content string, parts ...chat.MessagePart) *types.Message {
	t.Helper()
	input := session.UserMessage(content, parts...)
	input.InputOrigin = session.InputOriginUser
	msg := types.Input(input)
	require.Equal(t, types.MessageTypeUser, msg.Type)
	return msg
}

func plainLineContaining(t *testing.T, plain, needle string) int {
	t.Helper()
	for i, line := range strings.Split(plain, "\n") {
		if strings.Contains(line, needle) {
			return i
		}
	}
	require.FailNow(t, "line not rendered", "missing %q in\n%s", needle, plain)
	return -1
}

func TestUserAttachmentInlineMarkerMatchesFooterWithBlankSeparator(t *testing.T) {
	msg := projectedUserMessage(t, "also, @/tmp/filename.png ", chat.MessagePart{
		Type: chat.MessagePartTypeDocument, Document: &chat.Document{
			Name: "filename.png", MimeType: "image/jpeg", Source: chat.DocumentSource{InlineData: []byte{0xff, 0xd8}},
		},
	})
	mv := New(animation.NewRuntime(), msg, nil)
	mv.SetSize(60, 0)
	plain := ansi.Strip(mv.Render(60))

	paragraph := plainLineContaining(t, plain, "also, 📎1")
	lines := strings.Split(plain, "\n")
	require.Less(t, paragraph+2, len(lines))
	assert.Empty(t, strings.Trim(lines[paragraph+1], " ┃"), "one blank row separates the footer")
	assert.Contains(t, lines[paragraph+2], "📎1 filename.png")
	assert.NotContains(t, plain, "image/jpeg", "footer omits the MIME label")
	assert.NotContains(t, plain, "▣")
	assert.Equal(t, "also, @/tmp/filename.png ", msg.Content, "canonical content stays unchanged")
}

func TestUserAttachmentFooterNumbersStayStableAcrossRenderWidths(t *testing.T) {
	msg := types.User("Compare @/tmp/b.png with @/tmp/a.txt")
	msg.DisplayContent = "Compare 📎2 with 📎1"
	msg.Attachments = []types.UserAttachment{
		{Name: "a.txt", MediaType: "text/plain", Text: "a", Available: true},
		{Name: "résumé 世界.png", MediaType: "image/png", Data: []byte{1}, Available: true},
	}
	mv := New(animation.NewRuntime(), msg, nil)
	for _, width := range []int{64, 24, 12} {
		mv.SetSize(width, 0)
		rendered := mv.Render(width)
		plain := ansi.Strip(rendered)
		for line := range strings.SplitSeq(rendered, "\n") {
			assert.LessOrEqual(t, ansi.StringWidth(line), width, "width %d line %q", width, ansi.Strip(line))
		}
		if width == 64 {
			assert.Contains(t, plain, "Compare 📎2 with 📎1")
		}
		assert.Contains(t, plain, "📎1 a.txt")
		assert.Contains(t, plain, "📎2 résu", "footer label truncates at narrow widths")
		assert.NotContains(t, plain, "text/plain")
		assert.NotContains(t, plain, "image/png")
	}
	mv.SetSize(64, 0)
	plain := ansi.Strip(mv.Render(64))
	first := plainLineContaining(t, plain, "📎1 a.txt")
	second := plainLineContaining(t, plain, "📎2 résumé")
	assert.Equal(t, first+1, second, "footer rows stay contiguous")
	index, ok := mv.AttachmentAt(first, 3)
	require.True(t, ok)
	assert.Equal(t, 0, index)
	index, ok = mv.AttachmentAt(second, 3)
	require.True(t, ok)
	assert.Equal(t, 1, index)
}

func TestUserAttachmentWithoutRecoverablePositionShowsFooterOnly(t *testing.T) {
	msg := types.User("please review the screenshot")
	msg.DisplayContent = msg.Content
	msg.Attachments = []types.UserAttachment{{Name: "shot.png", MediaType: "image/png", Data: []byte{1}, Available: true}}
	mv := New(animation.NewRuntime(), msg, nil)
	mv.SetSize(60, 0)
	plain := ansi.Strip(mv.Render(60))
	assert.Equal(t, 1, strings.Count(plain, "📎"), "no guessed inline marker")
	assert.Contains(t, plain, "please review the screenshot")
	paragraph := plainLineContaining(t, plain, "please review the screenshot")
	assert.Equal(t, paragraph+2, plainLineContaining(t, plain, "📎1 shot.png"))
}

func TestUserAttachmentUnavailableFooterKeepsNumberWithoutMIME(t *testing.T) {
	msg := types.User("prompt")
	msg.DisplayContent = "prompt 📎1"
	msg.Attachments = []types.UserAttachment{{Name: "big.bin", MediaType: "application/octet-stream", UnavailableReason: "attachment too large to preview"}}
	mv := New(animation.NewRuntime(), msg, nil)
	mv.SetSize(60, 0)
	plain := ansi.Strip(mv.Render(60))
	assert.Contains(t, plain, "📎1 big.bin · unavailable")
	assert.NotContains(t, plain, "application/octet-stream")
}
