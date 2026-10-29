package editor

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestCommentInputRestoresComposerAndCannotSend(t *testing.T) {
	e := New(nil).(*editor)
	e.SetValue("keep draft\nsecond line")
	e.textarea.PlaceCursor(3, 1)
	line, column := e.textarea.Line(), e.textarea.Column()

	require.True(t, e.BeginCommentInput())
	e.SetCommentInputValue("review note")
	assert.Nil(t, e.SendContent())
	assert.Nil(t, e.SetRecording(true))
	require.Error(t, e.AttachFile("somewhere.go"))
	_, cmd := e.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	assert.Nil(t, cmd, "Enter stays in the comment and never yields SendMsg")
	_, cmd = e.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt})
	assert.Nil(t, cmd, "Alt+Enter cannot send or steer while writing a review comment")

	body, active := e.FinishCommentInput()
	require.True(t, active)
	assert.Equal(t, "review note", body)
	assert.Equal(t, "keep draft\nsecond line", e.Value())
	assert.Equal(t, line, e.textarea.Line())
	assert.Equal(t, column, e.textarea.Column())
	assert.False(t, e.CommentInputActive())
}

func TestSetValueDuringCommentInputUpdatesRestoredComposer(t *testing.T) {
	e := New(nil).(*editor)
	e.SetValue("original")
	require.True(t, e.BeginCommentInput())
	e.SetCommentInputValue("comment body")
	e.SetValue("restored pending message")
	assert.Equal(t, "comment body", e.CommentInputValue())
	body, active := e.FinishCommentInput()
	require.True(t, active)
	assert.Equal(t, "comment body", body)
	assert.Equal(t, "restored pending message", e.Value())
}

func TestCommentInputAllowsSafeEditorLifecycleMessages(t *testing.T) {
	e := New(nil).(*editor)
	require.True(t, e.BeginCommentInput())
	e.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	assert.True(t, e.CommentInputActive())
	assert.Empty(t, e.CommentInputValue())

	_, active := e.FinishCommentInput()
	assert.True(t, active)
	_, cmd := e.Update(messages.ThemeChangedMsg{})
	assert.Nil(t, cmd)
}
