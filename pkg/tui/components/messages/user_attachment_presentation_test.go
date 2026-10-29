package messages

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestUserAttachmentMarkerLiveReplayCopyAndEdit(t *testing.T) {
	const canonical = "also, @/tmp/filename.png "
	for _, replay := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "replay"}[replay], func(t *testing.T) {
			m := newMediaTestModel(t)
			defer m.StopAnimations()
			m.SetSize(80, 40)
			input := session.UserMessage(canonical, chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
				Name: "filename.png", MimeType: "image/jpeg", Source: chat.DocumentSource{InlineData: []byte{0xff, 0xd8}},
			}})
			input.InputOrigin = session.InputOriginUser
			if replay {
				sess := session.New(session.WithID("replay-markers"))
				sess.AddMessage(input)
				m.LoadFromSession(sess, nil)
			} else {
				m.AddInputMessage(input, 0)
			}
			require.Len(t, m.messages, 1)
			msg := m.messages[0]
			assert.Equal(t, canonical, msg.Content, "copy and edit read canonical content")
			assert.Equal(t, "also, 📎1 ", msg.DisplayContent)

			plain := ansi.Strip(m.View())
			assert.Contains(t, plain, "also, 📎1")
			assert.Contains(t, plain, "📎1 filename.png")
			assert.NotContains(t, plain, "image/jpeg")
			assert.NotContains(t, plain, "@/tmp/filename.png", "display never shows the structured reference")

			m.Focus()
			m.selectedMessageIndex = 0
			_, cmd := m.Update(tea.KeyPressMsg{Code: 'e'})
			require.NotNil(t, cmd)
			edit, ok := cmd().(messages.EditUserMessageMsg)
			require.True(t, ok)
			assert.Equal(t, canonical, edit.OriginalContent)
			assert.Equal(t, canonical, copyableMessageContent(m.messages[0]), "copy action copies canonical content")
		})
	}
}

func TestUserAttachmentMissingReferenceShowsFooterOnlyLive(t *testing.T) {
	m := newMediaTestModel(t)
	defer m.StopAnimations()
	m.SetSize(80, 40)
	input := session.UserMessage("please review the screenshot", chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
		Name: "shot.png", MimeType: "image/png", Source: chat.DocumentSource{InlineData: []byte{1}},
	}})
	input.InputOrigin = session.InputOriginUser
	m.AddInputMessage(input, 0)
	require.Len(t, m.messages, 1)
	assert.Equal(t, "please review the screenshot", m.messages[0].DisplayContent)
	plain := ansi.Strip(m.View())
	assert.Equal(t, 1, strings.Count(plain, "📎"), "footer is the only numbered row")
	assert.Contains(t, plain, "📎1 shot.png")
	assert.NotContains(t, plain, "image/png")
}
