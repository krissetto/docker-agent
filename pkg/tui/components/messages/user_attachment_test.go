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
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestUserAttachmentClickReleaseAndDragOnScrolledLaterMessage(t *testing.T) {
	var copied string
	t.Cleanup(SetClipboardWriterForTest(func(text string) error { copied = text; return nil }))
	m := newMediaTestModel(t)
	defer m.StopAnimations()
	m.SetImagePreviewSessionID("owner")
	m.SetSize(80, 40)
	m.AddUserMessage("earlier user message")
	stored := session.UserMessage("second message", chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
		Name: "résumé 世界.txt", MimeType: "text/plain", Source: chat.DocumentSource{InlineText: "saved body"},
	}})
	stored.InputOrigin = session.InputOriginUser
	m.AddInputMessage(stored, 3)
	second := m.messages[1]
	require.Equal(t, types.MessageTypeUser, second.Type)
	require.Len(t, second.Attachments, 1)
	require.Equal(t, "saved body", second.Attachments[0].Text)
	m.ensureAllItemsRendered()
	view, ok := m.views[1].(interface {
		AttachmentAt(line, col int) (int, bool)
	})
	require.True(t, ok)
	messageView := m.views[1].(interface{ View() string })
	localLine, col := -1, -1
	for line, text := range strings.Split(ansi.Strip(messageView.View()), "\n") {
		if strings.Contains(text, "résumé") {
			localLine = line
			col = strings.Index(text, "résumé") + 1
			break
		}
	}
	require.GreaterOrEqual(t, localLine, 0)
	index, hit := view.AttachmentAt(localLine, col)
	require.True(t, hit)
	require.Zero(t, index)
	globalLine := m.lineOffsets[1] + localLine
	m.setScrollOffset(2)
	x, y := m.xPos+col, m.yPos+globalLine-m.scrollOffset
	require.Greater(t, globalLine, localLine, "later-message selection must use transcript coordinates")
	m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.True(t, m.IsSelecting())
	assert.Equal(t, globalLine, m.selection.startLine, "selection anchor uses global line after scroll")
	_, cmd := m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.NotNil(t, cmd)
	opened, ok := cmd().(messages.OpenUserAttachmentPreviewMsg)
	require.True(t, ok)
	assert.Equal(t, "owner", opened.SessionID)
	assert.Equal(t, "résumé 世界.txt", opened.Attachment.Name)
	assert.Equal(t, "saved body", opened.Attachment.Text)
	assert.False(t, m.IsSelecting())

	m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	m.Update(tea.MouseMotionMsg{X: x + 1, Y: y, Button: tea.MouseLeft})
	assert.Nil(t, m.attachmentClick, "drag cancels the preview capture")
	assert.True(t, m.IsSelecting(), "attachment drag retains normal text selection")
	_, cmd = m.Update(tea.MouseReleaseMsg{X: x + 1, Y: y, Button: tea.MouseLeft})
	assertNoUserAttachmentPreview(t, cmd)
	assert.NotEmpty(t, copied, "attachment drag copies selected text instead of opening a preview")
}

func TestUserAttachmentCaptureCancelsOnScrollResizeAndOwnerChange(t *testing.T) {
	for _, change := range []string{"scroll", "resize", "owner"} {
		t.Run(change, func(t *testing.T) {
			m := newMediaTestModel(t)
			defer m.StopAnimations()
			m.SetImagePreviewSessionID("first")
			if change == "scroll" {
				for range 30 {
					m.AddUserMessage(strings.Repeat("filler row\n", 4))
				}
				m.ensureAllItemsRendered()
				m.scrollToBottom()
			}
			msg := types.User("prompt")
			msg.Attachments = []types.UserAttachment{{Name: "attached.txt", MediaType: "text/plain", Text: "inline", Available: true}}
			m.addMessage(msg)
			m.ensureAllItemsRendered()
			messageIndex := len(m.messages) - 1
			view := m.views[messageIndex].(interface{ View() string })
			localLine, col := -1, -1
			for line, rendered := range strings.Split(ansi.Strip(view.View()), "\n") {
				if strings.Contains(rendered, "attached.txt") {
					localLine, col = line, strings.Index(rendered, "attached.txt")+1
					break
				}
			}
			require.GreaterOrEqual(t, localLine, 0)
			_, hit := m.views[messageIndex].(interface {
				AttachmentAt(line, col int) (int, bool)
			}).AttachmentAt(localLine, col)
			require.True(t, hit)
			globalLine := m.lineOffsets[messageIndex] + localLine
			x, y := m.xPos+col, m.yPos+globalLine-m.scrollOffset
			m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			require.NotNil(t, m.attachmentClick)
			switch change {
			case "scroll":
				m.Update(messages.WheelCoalescedMsg{Delta: -1, X: x, Y: y})
			case "resize":
				m.SetSize(60, 24)
			case "owner":
				m.SetImagePreviewSessionID("second")
			}
			assert.Nil(t, m.attachmentClick)
			_, cmd := m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
			assertNoUserAttachmentPreview(t, cmd)
		})
	}
}

func TestLiveAndReplayUserInputCarryPreviewProjection(t *testing.T) {
	for _, replay := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "replay"}[replay], func(t *testing.T) {
			m := newMediaTestModel(t)
			defer m.StopAnimations()
			input := session.UserMessage("hello", chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
				Name: "notes.md", MimeType: "text/markdown", Source: chat.DocumentSource{InlineText: "note text"},
			}})
			input.InputOrigin = session.InputOriginUser
			if replay {
				sess := session.New(session.WithID("replay-attachments"))
				sess.AddMessage(input)
				m.LoadFromSession(sess, nil)
			} else {
				m.AddInputMessage(input, 0)
			}
			require.Len(t, m.messages, 1)
			assert.Equal(t, "hello", m.messages[0].Content, "canonical content remains available for copy/edit")
			require.Len(t, m.messages[0].Attachments, 1)
			assert.Equal(t, "note text", m.messages[0].Attachments[0].Text)
			assert.Contains(t, ansi.Strip(m.View()), "notes.md")
		})
	}
}

func assertNoUserAttachmentPreview(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	var visit func(tea.Cmd)
	visit = func(command tea.Cmd) {
		if command == nil {
			return
		}
		value := command()
		switch commands := value.(type) {
		case tea.BatchMsg:
			for _, child := range commands {
				visit(child)
			}
		case core.SequenceMsg:
			for _, child := range commands {
				visit(child)
			}
		}
		_, preview := value.(messages.OpenUserAttachmentPreviewMsg)
		assert.False(t, preview, "drag command must not contain a user attachment preview")
	}
	visit(cmd)
}

func TestSelectedUserAttachmentPreviewKeyboardKeysAreScoped(t *testing.T) {
	m := newMediaTestModel(t)
	defer m.StopAnimations()
	m.SetImagePreviewSessionID("keyboard-owner")
	msg := types.User("prompt")
	msg.Attachments = []types.UserAttachment{
		{Name: "first.txt", MediaType: "text/plain", Text: "first", Available: true},
		{Name: "second.txt", MediaType: "text/plain", Text: "second", Available: true},
	}
	msg.SessionPosition = new(int)
	m.addMessage(msg)
	m.Focus()
	m.selectedMessageIndex = 0
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'p'})
	require.NotNil(t, cmd)
	opened, ok := cmd().(messages.OpenUserAttachmentPreviewMsg)
	require.True(t, ok)
	assert.Equal(t, "first.txt", opened.Attachment.Name)
	_, cmd = m.Update(tea.KeyPressMsg{Code: '2'})
	require.NotNil(t, cmd)
	opened, ok = cmd().(messages.OpenUserAttachmentPreviewMsg)
	require.True(t, ok)
	assert.Equal(t, "second.txt", opened.Attachment.Name)
	_, cmd = m.Update(tea.KeyPressMsg{Code: '3'})
	assert.Nil(t, cmd)
	m.Blur()
	_, cmd = m.Update(tea.KeyPressMsg{Code: 'p'})
	assert.Nil(t, cmd, "preview keys do nothing outside message selection mode")
}

func TestAttachmentClickWorksBesideCompactWorkHeader(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(map[bool]string{false: "expanded-transcript", true: "compact-work"}[compact], func(t *testing.T) {
			m := newMediaTestModel(t)
			defer m.StopAnimations()
			m.SetCompactWorkLog(compact)
			m.SetImagePreviewSessionID("work-owner")
			msg := types.User("request")
			msg.Attachments = []types.UserAttachment{{Name: "user.txt", MediaType: "text/plain", Text: "stored", Available: true}}
			m.addMessage(msg)
			m.addMessage(types.Agent(types.MessageTypeAssistantReasoningBlock, "root", "reasoning child"))
			m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "final answer"))
			m.ensureAllItemsRendered()
			if compact {
				block := m.workHeaderBySource[1]
				require.NotNil(t, block)
				block.SetSize(m.contentWidth()+1, 0)
				m.invalidateAllItems()
				m.ensureAllItemsRendered()
				assert.Contains(t, ansi.Strip(m.View()), "Work")
			}
			view := m.views[0].(interface {
				View() string
				AttachmentAt(line, col int) (int, bool)
			})
			localLine, col := -1, -1
			for line, rendered := range strings.Split(ansi.Strip(view.View()), "\n") {
				if strings.Contains(rendered, "user.txt") {
					localLine, col = line, strings.Index(rendered, "user.txt")+1
					break
				}
			}
			require.GreaterOrEqual(t, localLine, 0)
			x, y := m.xPos+col, m.yPos+m.lineOffsets[0]+localLine-m.scrollOffset
			m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			_, cmd := m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
			require.NotNil(t, cmd)
			opened, ok := cmd().(messages.OpenUserAttachmentPreviewMsg)
			require.True(t, ok)
			assert.Equal(t, "user.txt", opened.Attachment.Name)
			assert.Contains(t, ansi.Strip(m.View()), "final answer")
		})
	}
}
