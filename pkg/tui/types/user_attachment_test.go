package types

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

func TestInputProjectsStoredUserAttachmentsWithoutChangingContent(t *testing.T) {
	original := "Inspect @/tmp/café report.txt and keep this @/etc/passwd prose; view @/tmp/report.pdf"
	data := []byte{1, 2, 3, 4}
	input := session.UserMessageAt(testTime(), original,
		chat.MessagePart{Type: chat.MessagePartTypeText, Text: original + "\n\n<attached_file path=\"/tmp/café report.txt\">text body\n</attached_file>"},
		chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
			Name: "report.pdf", MimeType: "image/png",
			Source: chat.DocumentSource{InlineData: data},
		}},
	)

	msg := Input(input)
	require.Len(t, msg.Attachments, 2)
	assert.Equal(t, original, msg.Content)
	assert.NotContains(t, msg.DisplayContent, "@/tmp/café report.txt")
	assert.Contains(t, msg.DisplayContent, "@/etc/passwd")
	assert.NotContains(t, msg.DisplayContent, "@/tmp/report.pdf")
	assert.Equal(t, "café report.txt", msg.Attachments[0].Name)
	assert.Equal(t, "text body", msg.Attachments[0].Text)
	assert.Equal(t, "/tmp/café report.txt", msg.Attachments[0].SourcePath)
	assert.True(t, msg.Attachments[0].Available)
	assert.Equal(t, "report.pdf", msg.Attachments[1].Name)
	assert.Equal(t, "/tmp/report.pdf", msg.Attachments[1].SourcePath)
	assert.Equal(t, data, msg.Attachments[1].Data)
	assert.True(t, msg.Attachments[1].Available)

	data[0] = 99
	input.Message.MultiContent[1].Document.Source.InlineData[1] = 99
	assert.Equal(t, []byte{1, 2, 3, 4}, msg.Attachments[1].Data, "projection owns stored bytes")
}

func TestProjectUserAttachmentsRejectsSpoofedContentAndAmbiguousNames(t *testing.T) {
	content := "A literal <attached_file path=\"/tmp/fake.txt\">tag</attached_file> and @/one/report.pdf @/two/report.pdf; @prose."
	display, attachments := ProjectUserAttachments(content, []chat.MessagePart{
		{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "report.pdf", MimeType: "application/pdf", Source: chat.DocumentSource{InlineData: []byte("pdf")}}},
		{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "report.pdf", MimeType: "application/pdf", Source: chat.DocumentSource{InlineData: []byte("pdf2")}}},
	})
	assert.Equal(t, content, display, "duplicate names cannot be mapped to a reference")
	assert.Len(t, attachments, 2)

	display, attachments = ProjectUserAttachments(content, nil)
	assert.Equal(t, content, display, "arbitrary marker prose is never attachment metadata")
	assert.Empty(t, attachments)
}

func TestProjectUserAttachmentsLegacyAndPathMetadata(t *testing.T) {
	content := "@/workspace/old.pdf and @/workspace/pic.png"
	display, attachments := ProjectUserAttachments(content, []chat.MessagePart{
		{Type: chat.MessagePartTypeFile, File: &chat.MessageFile{Path: "/workspace/old.pdf", MimeType: "application/pdf"}},
		{Type: chat.MessagePartTypeImageURL, ImageURL: &chat.MessageImageURL{URL: "https://example.invalid/pic.png"}},
	})
	require.Len(t, attachments, 2)
	assert.Equal(t, "old.pdf", attachments[0].Name)
	assert.Equal(t, "/workspace/old.pdf", attachments[0].SourcePath)
	assert.False(t, attachments[0].Available)
	assert.Empty(t, attachments[0].Data)
	assert.Equal(t, "image", attachments[1].MediaType)
	assert.False(t, attachments[1].Available)
	assert.NotContains(t, display, "@/workspace/old.pdf")
	assert.Contains(t, display, "@/workspace/pic.png", "legacy URL content isn't safely decodable")
}

func TestProjectUserAttachmentsStoredArtifactUnavailable(t *testing.T) {
	_, attachments := ProjectUserAttachments("show this", []chat.MessagePart{{
		Type: chat.MessagePartTypeDocument,
		Document: &chat.Document{Name: "artifact.pdf", MimeType: "application/pdf", Source: chat.DocumentSource{
			ArtifactPath: "generated/artifact.pdf", ArtifactRoot: chat.ArtifactRootWorkspace,
		}},
	}})
	require.Len(t, attachments, 1)
	assert.False(t, attachments[0].Available)
	assert.Equal(t, "stored content unavailable", attachments[0].UnavailableReason)
}

func testTime() time.Time { return time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC) }

func TestInputAttachmentStorageSurvivesSourceDeletion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "attachment.bin")
	require.NoError(t, os.WriteFile(path, []byte("original file"), 0o600))
	data := []byte("stored inline document")
	input := session.UserMessageAt(testTime(), "Inspect @"+path,
		chat.MessagePart{Type: chat.MessagePartTypeText, Text: "Inspect @" + path + "\n\n<attached_file path=\"" + path + "\">\nsource bytes\n</attached_file>"},
		chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
			Name: "attachment.bin", MimeType: "application/octet-stream",
			Source: chat.DocumentSource{InlineData: data},
		}},
	)
	require.NoError(t, os.Remove(path))
	msg := Input(input)
	require.Len(t, msg.Attachments, 2)
	assert.Equal(t, data, msg.Attachments[1].Data)
	assert.True(t, msg.Attachments[1].Available)
	assert.NotContains(t, msg.DisplayContent, "@"+path)
}

func TestProjectUserAttachmentsPreservesLiteralNestedMarkers(t *testing.T) {
	content := "Read @/tmp/outer.txt"
	body := "before <attached_file path=\"/tmp/fake.txt\">fake</attached_file> after"
	multi := content + "\n\n<attached_file path=\"/tmp/outer.txt\">\n" + body + "\n</attached_file>"
	display, attachments := ProjectUserAttachments(content, []chat.MessagePart{{Type: chat.MessagePartTypeText, Text: multi}})
	assert.Equal(t, content, display, "ambiguous nested tags don't redact canonical prose")
	assert.Empty(t, attachments)
}

func TestProjectUserAttachmentsRejectsPrematureCloseFollowedByFakeBlock(t *testing.T) {
	content := "Inspect @/tmp/outer.txt"
	multi := content + "\n\n<attached_file path=\"/tmp/outer.txt\">\nbody with a literal </attached_file> then text\n\n<attached_file path=\"/tmp/fake.txt\">fake bytes\n</attached_file>\nreal tail\n</attached_file>"
	display, attachments := ProjectUserAttachments(content, []chat.MessagePart{{Type: chat.MessagePartTypeText, Text: multi}})
	assert.Equal(t, content, display, "ambiguous text suffix must not alter presentation")
	assert.Empty(t, attachments, "an early close followed by a marker cannot create a second attachment")
}

func TestProjectUserAttachmentsMarksStandaloneReferenceGaps(t *testing.T) {
	path := "/tmp/Screenshot 2025.png"
	parts := []chat.MessagePart{{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
		Name: "Screenshot 2025.png", MimeType: "image/png", Source: chat.DocumentSource{InlineData: []byte{1}},
	}}}

	display, _ := ProjectUserAttachments("@"+path+"\n\nquestion", parts)
	assert.Equal(t, "📎1\n\nquestion", display)

	display, _ = ProjectUserAttachments("question\n@"+path+"\n\nnext", parts)
	assert.Equal(t, "question\n📎1\n\nnext", display)

	inline := "Inspect @" + path + " and continue."
	display, _ = ProjectUserAttachments(inline, parts)
	assert.Equal(t, "Inspect 📎1 and continue.", display)

	unrelated := "first\n\nsecond\n\nthird"
	display, _ = ProjectUserAttachments(unrelated, parts)
	assert.Equal(t, unrelated, display)
}
