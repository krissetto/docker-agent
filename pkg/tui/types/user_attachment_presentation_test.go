package types

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

func imageDocument(name string) chat.MessagePart {
	return chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
		Name: name, MimeType: "image/jpeg", Source: chat.DocumentSource{InlineData: []byte{0xff, 0xd8}},
	}}
}

func TestUserAttachmentMarkerReplacesStructuredReferenceInPlace(t *testing.T) {
	content := "also, @/tmp/filename.png "
	display, attachments := ProjectUserAttachments(content, []chat.MessagePart{imageDocument("filename.png")})
	assert.Equal(t, "also, 📎1 ", display)
	require.Len(t, attachments, 1)
	assert.Equal(t, "filename.png", attachments[0].Name)
	assert.Equal(t, "image/jpeg", attachments[0].MediaType, "presentation keeps the actual MIME")
	assert.Equal(t, "/tmp/filename.png", attachments[0].SourcePath)
}

func TestUserAttachmentMarkersNumberByAttachmentOrderNotReferenceOrder(t *testing.T) {
	content := "Compare @/tmp/b.png with @/tmp/notes.txt"
	display, attachments := ProjectUserAttachments(content, []chat.MessagePart{
		{Type: chat.MessagePartTypeText, Text: content + "\n\n<attached_file path=\"/tmp/notes.txt\">\nnotes\n</attached_file>"},
		imageDocument("b.png"),
	})
	require.Len(t, attachments, 2)
	assert.Equal(t, "notes.txt", attachments[0].Name)
	assert.Equal(t, "b.png", attachments[1].Name)
	assert.Equal(t, "Compare 📎2 with 📎1", display)
}

func TestUserAttachmentMarkerKeepsUnicodeNamesAndPaths(t *testing.T) {
	content := "看 @/tmp/résumé 世界.txt 谢谢"
	display, attachments := ProjectUserAttachments(content, []chat.MessagePart{
		{Type: chat.MessagePartTypeText, Text: content + "\n\n<attached_file path=\"/tmp/résumé 世界.txt\">\n正文\n</attached_file>"},
	})
	require.Len(t, attachments, 1)
	assert.Equal(t, "résumé 世界.txt", attachments[0].Name)
	assert.Equal(t, "看 📎1 谢谢", display)
}

func TestUserAttachmentWithoutRecoverableReferenceHasNoInsertion(t *testing.T) {
	content := "please review the screenshot"
	display, attachments := ProjectUserAttachments(content, []chat.MessagePart{imageDocument("shot.png")})
	assert.Equal(t, content, display)
	require.Len(t, attachments, 1)
	assert.Equal(t, "shot.png", attachments[0].Name)
	assert.Equal(t, "image/jpeg", attachments[0].MediaType)
}

func TestUserAttachmentMarkerPreservesLiteralSimilarText(t *testing.T) {
	content := "see shot.png and @/tmp/shot.png.bak, then @/tmp/shot.png"
	display, attachments := ProjectUserAttachments(content, []chat.MessagePart{imageDocument("shot.png")})
	require.Len(t, attachments, 1)
	assert.Equal(t, "see shot.png and @/tmp/shot.png.bak, then 📎1", display)
}

func TestUserAttachmentRepeatedReferenceReusesSameNumber(t *testing.T) {
	display, attachments := ProjectUserAttachments("@/tmp/shot.png and @/tmp/shot.png!", []chat.MessagePart{imageDocument("shot.png")})
	require.Len(t, attachments, 1)
	assert.Equal(t, "📎1 and 📎1!", display)
}

func TestUserAttachmentSharedPathReferencesKeepEveryOccurrence(t *testing.T) {
	content := "@/tmp/a.txt then @/tmp/a.txt then @/tmp/a.txt"
	display, attachments := ProjectUserAttachments(content, []chat.MessagePart{
		{Type: chat.MessagePartTypeText, Text: content + "\n\n<attached_file path=\"/tmp/a.txt\">\nbody\n</attached_file>"},
		{Type: chat.MessagePartTypeFile, File: &chat.MessageFile{Path: "/tmp/a.txt", MimeType: "text/plain"}},
	})
	require.Len(t, attachments, 2)
	assert.Equal(t, "📎1 then 📎2 then 📎2", display)
}

func TestUserAttachmentAmbiguousBasenameLiteralIsPreserved(t *testing.T) {
	content := "@/one/shot.png and @/two/shot.png"
	display, attachments := ProjectUserAttachments(content, []chat.MessagePart{imageDocument("shot.png")})
	require.Len(t, attachments, 1)
	assert.Equal(t, content, display, "two absolute matches for one name cannot be attributed")
	assert.Empty(t, attachments[0].SourcePath)
}

func TestUserAttachmentMarkerStableAcrossInputProjections(t *testing.T) {
	content := "also, @/tmp/filename.png "
	parts := []chat.MessagePart{
		{Type: chat.MessagePartTypeText, Text: content},
		imageDocument("filename.png"),
	}
	live := session.UserMessageAt(testTime(), content, parts...)
	liveMsg := Input(live)
	replayed := session.UserMessageAt(testTime(), content, parts...)
	replayMsg := Input(replayed)
	assert.Equal(t, liveMsg.DisplayContent, replayMsg.DisplayContent)
	assert.Equal(t, "also, 📎1 ", liveMsg.DisplayContent)
	assert.Equal(t, content, liveMsg.Content, "canonical content stays unchanged for copy and edit")
	assert.Equal(t, content, replayMsg.Content)
}

func TestUserAttachmentSameBasenameDifferentPathStaysLiteral(t *testing.T) {
	display, attachments := ProjectUserAttachments("@/other/a.txt and @/actual/a.txt", []chat.MessagePart{
		{Type: chat.MessagePartTypeFile, File: &chat.MessageFile{Path: "/actual/a.txt", MimeType: "text/plain"}},
	})
	require.Len(t, attachments, 1)
	assert.Equal(t, "/actual/a.txt", attachments[0].SourcePath)
	assert.Equal(t, "@/other/a.txt and 📎1", display, "same-basename path without a marker keeps its @ and text")
}
