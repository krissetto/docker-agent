package types_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// Remote sessions arrive as api.SessionSnapshot JSON and reach the TUI through types.Input.
func TestRemoteSnapshotRoundTripKeepsNumberedDisplayAndStoredPayload(t *testing.T) {
	const canonical = "also, @/tmp/filename.png \n\nRead @/tmp/notes.txt"
	imageBytes := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10}
	sess := session.New(session.WithID("remote-attachments"))
	sess.AddMessage(session.UserMessageAt(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), canonical,
		chat.MessagePart{Type: chat.MessagePartTypeText, Text: canonical + "\n\n<attached_file path=\"/tmp/notes.txt\">\nnotes body\n</attached_file>"},
		chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
			Name: "filename.png", MimeType: "image/jpeg", Source: chat.DocumentSource{InlineData: imageBytes},
		}},
	))

	wire, err := json.Marshal(api.SessionSnapshot[string, string, any]{Session: sess})
	require.NoError(t, err)
	var decoded api.SessionSnapshot[string, string, any]
	require.NoError(t, json.Unmarshal(wire, &decoded))
	require.NotNil(t, decoded.Session)

	var projected []*types.Message
	for _, item := range decoded.Session.Messages {
		if item.IsMessage() && item.Message != nil {
			projected = append(projected, types.Input(item.Message))
		}
	}
	require.Len(t, projected, 1)
	msg := projected[0]

	assert.Equal(t, canonical, msg.Content, "canonical content survives the wire unchanged")
	assert.Equal(t, "also, 📎2 \n\nRead 📎1", msg.DisplayContent, "numbered display is derived after decode")
	require.Len(t, msg.Attachments, 2)
	assert.Equal(t, "notes.txt", msg.Attachments[0].Name)
	assert.Equal(t, "text/plain", msg.Attachments[0].MediaType)
	assert.Equal(t, "notes body", msg.Attachments[0].Text)
	assert.Equal(t, "filename.png", msg.Attachments[1].Name)
	assert.Equal(t, "image/jpeg", msg.Attachments[1].MediaType, "actual MIME survives the wire")
	assert.Equal(t, imageBytes, msg.Attachments[1].Data, "source bytes survive the wire")
	assert.True(t, msg.Attachments[1].Available)
}
