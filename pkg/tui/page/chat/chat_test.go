package chat

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestExtractAttachmentsFromSession(t *testing.T) {
	t.Parallel()

	sess := session.New()
	p := newTestChatPage(t)
	p.app = app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(queueTestRuntime{}))

	msg := session.Message{
		Message: chat.Message{
			Role: chat.MessageRoleUser,
			MultiContent: []chat.MessagePart{
				{Type: chat.MessagePartTypeText, Text: "main user prompt"},
				// 1. Document with InlineText
				{
					Type: chat.MessagePartTypeDocument,
					Document: &chat.Document{
						Name: "text_doc.txt",
						Source: chat.DocumentSource{
							InlineText: "text content",
						},
					},
				},
				// 2. Document with InlineData
				{
					Type: chat.MessagePartTypeDocument,
					Document: &chat.Document{
						Name:     "binary_doc.png",
						MimeType: "image/png",
						Source: chat.DocumentSource{
							InlineData: []byte("binary data"),
						},
					},
				},
				// 3. Legacy Text Contents
				{
					Type: chat.MessagePartTypeText,
					Text: "Contents of legacy_doc.txt: legacy content",
				},
				// 4. Empty Document
				{
					Type: chat.MessagePartTypeDocument,
					Document: &chat.Document{
						Name: "empty_doc.txt",
					},
				},
			},
		},
	}
	sess.Messages = append(sess.Messages, session.Item{Message: &msg})

	attachments := p.extractAttachmentsFromSession(0)
	require.Len(t, attachments, 3, "expected exactly 3 extracted attachments")

	// 1. Document with InlineText
	assert.Equal(t, "text_doc.txt", attachments[0].Name)
	assert.Equal(t, "text content", attachments[0].Content)
	assert.Empty(t, attachments[0].Data)

	// 2. Document with InlineData
	assert.Equal(t, "binary_doc.png", attachments[1].Name)
	assert.Equal(t, "image/png", attachments[1].MimeType)
	assert.Equal(t, []byte("binary data"), attachments[1].Data)
	assert.Empty(t, attachments[1].Content)

	// 3. Legacy Text Contents
	assert.Equal(t, "legacy_doc.txt", attachments[2].Name)
	assert.Equal(t, "legacy content", attachments[2].Content)
	assert.Empty(t, attachments[2].Data)
	assert.Empty(t, attachments[2].MimeType)
}
