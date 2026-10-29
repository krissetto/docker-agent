package message

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestUserAttachmentRowsRenderAndHitTestAcrossWidths(t *testing.T) {
	msg := types.User("Original @/tmp/report.pdf must remain copyable.")
	msg.DisplayContent = "Original  must remain copyable."
	msg.Attachments = []types.UserAttachment{
		{Name: "Quarterly report 世界.pdf", MediaType: "application/pdf", Available: true, Data: []byte{1, 2}},
		{Name: "second.txt", MediaType: "text/plain", Available: true, Text: "stored text"},
	}
	mv := New(animation.NewRuntime(), msg, nil)
	for _, width := range []int{64, 28, 12} {
		mv.SetSize(width, 0)
		rendered := mv.Render(width)
		plain := ansi.Strip(rendered)
		assert.Contains(t, plain, "Quar")
		assert.Contains(t, plain, "seco")
		assert.NotContains(t, plain, "@/tmp/report.pdf")
		for line := range strings.SplitSeq(rendered, "\n") {
			assert.LessOrEqual(t, ansi.StringWidth(line), width, "width %d line %q", width, ansi.Strip(line))
		}
		found := 0
		for lineIndex, line := range strings.Split(plain, "\n") {
			if strings.Contains(line, "Quar") {
				index, ok := mv.AttachmentAt(lineIndex, 3)
				assert.True(t, ok)
				assert.Equal(t, 0, index)
				found++
			}
		}
		assert.Equal(t, 1, found)
		assert.Equal(t, msg.Content, mv.Message().Content)
	}
	_, ok := mv.AttachmentAt(-1, 0)
	assert.False(t, ok)
	_, ok = mv.AttachmentAt(0, 0)
	assert.False(t, ok)
}

func TestUserAttachmentNameSanitizationAndRenderCacheInvalidation(t *testing.T) {
	msg := types.User("prompt")
	msg.Attachments = []types.UserAttachment{{Name: "../safe\x1b[31m.txt\n", MediaType: "text/plain", Available: false}}
	mv := New(animation.NewRuntime(), msg, nil)
	first := ansi.Strip(mv.Render(60))
	assert.Contains(t, first, "safe[31m.txt")
	assert.NotContains(t, first, "../")
	assert.NotContains(t, first, "\x1b")
	msg.Attachments[0].Name = "changed.bin"
	second := ansi.Strip(mv.Render(60))
	assert.Contains(t, second, "changed.bin")
	assert.NotEqual(t, first, second)
	assert.NotPanics(t, func() { require.NotEmpty(t, mv.Render(1)) })
}

func TestAttachmentHitTestUsesRenderedRowsNotMatchingUserProse(t *testing.T) {
	msg := types.User("same.txt prose\n" + strings.Repeat("long prose ", 8))
	msg.DisplayContent = msg.Content
	msg.Attachments = []types.UserAttachment{
		{Name: "same.txt", MediaType: "text/plain", Available: true},
		{Name: "same.txt", MediaType: "text/plain", Available: true},
	}
	mv := New(animation.NewRuntime(), msg, nil)
	mv.SetSize(64, 0)
	plain := ansi.Strip(mv.Render(64))
	rowIndexes := make([]int, 0, 3)
	for lineIndex, line := range strings.Split(plain, "\n") {
		if strings.Contains(line, "same.txt") {
			rowIndexes = append(rowIndexes, lineIndex)
		}
	}
	require.Len(t, rowIndexes, 3, "prose plus two rendered attachment rows")
	_, proseHit := mv.AttachmentAt(rowIndexes[0], 4)
	assert.False(t, proseHit, "matching user prose is not an attachment hit target")
	first, ok := mv.AttachmentAt(rowIndexes[1], 4)
	require.True(t, ok)
	assert.Equal(t, 0, first)
	second, ok := mv.AttachmentAt(rowIndexes[2], 4)
	require.True(t, ok)
	assert.Equal(t, 1, second)
}

func TestAttachmentRowsStayOutsideCollapseHitZone(t *testing.T) {
	msg := types.User(strings.Repeat("long user line\n", maxUserMessageLines+2))
	msg.DisplayContent = msg.Content
	msg.Attachments = []types.UserAttachment{{Name: "last.txt", MediaType: "text/plain", Available: true}}
	mv := New(animation.NewRuntime(), msg, nil)
	mv.SetSize(50, 0)
	plain := ansi.Strip(mv.Render(50))
	lines := strings.Split(plain, "\n")
	attachmentLine := -1
	for i, line := range lines {
		if strings.Contains(line, "last.txt") {
			attachmentLine = i
			break
		}
	}
	require.NotEqual(t, -1, attachmentLine)
	assert.False(t, mv.IsToggleLine(attachmentLine), "attachment row must not toggle collapse")
	for i, line := range lines {
		if strings.Contains(line, "[+] expand ") {
			assert.True(t, mv.IsToggleLine(i))
			if i > 0 {
				assert.True(t, mv.IsToggleLine(i-1))
			}
		}
	}
}
