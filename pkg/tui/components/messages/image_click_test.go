package messages

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/tool"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func mediaClickPoint(t *testing.T, m *model) (int, int) {
	t.Helper()
	for y, line := range strings.Split(m.View(), "\n") {
		if at := strings.Index(line, "\x1b_cagent-image;"); at >= 0 {
			return m.xPos + ansi.StringWidth(line[:at]), m.yPos + y
		}
	}
	t.Fatal("no rendered image")
	return 0, 0
}
func TestRenderedChatImageClickReleaseOpensCachedPreviewOnlyOnce(t *testing.T) {
	m := newMediaTestModel(t)
	defer m.StopAnimations()
	m.SetImagePreviewSessionID("owner")
	m.SetPosition(11, 15)
	inline := generatedMediaTestImage(t, "cat.png")
	m.AppendToLastMessage("root", "Caption before image")
	m.AppendAssistantMedia("root", []types.AssistantMedia{{ID: 1, Image: &inline}})
	x, y := mediaClickPoint(t, m)
	_, cmd := m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	assert.Nil(t, cmd)
	require.NotNil(t, m.imageClick)
	_, cmd = m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.NotNil(t, cmd)
	opened, ok := cmd().(msgtypes.OpenImagePreviewMsg)
	require.True(t, ok)
	assert.Equal(t, "owner", opened.SessionID)
	assert.Equal(t, "cat.png", opened.Preview.Name())
	fit, err := opened.Preview.Fit(100, 100, tuiimage.CellSize{})
	require.NoError(t, err)
	assert.Equal(t, inline.PNGData, fit.PNGData)
	opened.Preview.Close()
	assert.False(t, m.selection.active)
	_, cmd = m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	assert.Nil(t, cmd)
}
func TestChatImageDragWheelResizeAndBlurCancelPreview(t *testing.T) {
	for _, kind := range []string{"move", "wheel", "raw-wheel", "resize", "blur", "right", "session"} {
		t.Run(kind, func(t *testing.T) {
			m := newMediaTestModel(t)
			defer m.StopAnimations()
			m.SetImagePreviewSessionID("owner")
			inline := generatedMediaTestImage(t, "image.png")
			m.AppendToLastMessage("root", "text")
			m.AppendAssistantMedia("root", []types.AssistantMedia{{ID: 1, Image: &inline}})
			x, y := mediaClickPoint(t, m)
			m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			require.NotNil(t, m.imageClick)
			switch kind {
			case "move":
				m.Update(tea.MouseMotionMsg{X: x + 1, Y: y, Button: tea.MouseLeft})
			case "wheel":
				m.Update(msgtypes.WheelCoalescedMsg{Delta: 1})
			case "raw-wheel":
				m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
			case "resize":
				m.SetSize(40, 10)
			case "blur":
				m.Blur()
			case "right":
				m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseRight})
			case "session":
				m.SetImagePreviewSessionID("other")
			}
			assert.Nil(t, m.imageClick)
			_, cmd := m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
			if cmd != nil {
				_, opened := cmd().(msgtypes.OpenImagePreviewMsg)
				assert.False(t, opened)
			}
		})
	}
}
func TestImageCaptionsPaddingAndRightClickDoNotOpen(t *testing.T) {
	m := newMediaTestModel(t)
	defer m.StopAnimations()
	inline := generatedMediaTestImage(t, "caption.png")
	m.AppendToLastMessage("root", "text")
	m.AppendAssistantMedia("root", []types.AssistantMedia{{ID: 1, Image: &inline}})
	x, y := mediaClickPoint(t, m)
	for _, click := range []tea.MouseClickMsg{{X: x, Y: y - 1, Button: tea.MouseLeft}, {X: x - 1, Y: y, Button: tea.MouseLeft}, {X: x, Y: y, Button: tea.MouseRight}} {
		m.Update(click)
		assert.Nil(t, m.imageClick)
	}
}

func TestAssistantAndToolImageRowsShareClickPath(t *testing.T) {
	for _, kind := range []types.MessageType{types.MessageTypeAssistant, types.MessageTypeToolCall} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			m := newMediaTestModel(t)
			defer m.StopAnimations()
			m.SetImagePreviewSessionID("session")
			image := generatedMediaTestImage(t, "rendered.png")
			msg := types.Agent(kind, "root", "caption")
			if kind == types.MessageTypeToolCall {
				msg.Images = []tuiimage.Inline{image}
				msg.ToolStatus = types.ToolStatusCompleted
				msg.ToolCall.Function.Name = "image_tool"
			} else {
				msg.Content = "caption\n\n![rendered](data:image/png;base64," + base64.StdEncoding.EncodeToString(image.PNGData) + ")"
			}
			var dispatch func(tea.Cmd)
			dispatch = func(cmd tea.Cmd) {
				if cmd == nil {
					return
				}
				value := cmd()
				if batch, ok := value.(tea.BatchMsg); ok {
					for _, child := range batch {
						dispatch(child)
					}
					return
				}
				_, next := m.Update(value)
				dispatch(next)
			}
			if kind == types.MessageTypeToolCall {
				view := tool.New(m.ar, msg, m.sessionState)
				view.SetSize(m.contentWidth(), 0)
				m.messages = append(m.messages, msg)
				m.views = append(m.views, view)
				m.renderDirty = true
			} else {
				dispatch(m.addMessage(msg))
			}
			require.Contains(t, m.View(), "cagent-image;", "real user/assistant markdown or tool image must render")
			x, y := mediaClickPoint(t, m)
			m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			_, cmd := m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
			require.NotNil(t, cmd)
			opened, ok := cmd().(msgtypes.OpenImagePreviewMsg)
			require.True(t, ok)
			require.NotEmpty(t, opened.Preview.Name())
			opened.Preview.Close()
		})
	}
}
