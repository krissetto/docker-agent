package chat

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatPageImagePressReleaseUsesPreparedOriginAndOwner(t *testing.T) {
	p, _ := newGeneratedMediaTestPage(t, nil)
	p.SetRoutingID(p.app.Session().ID)
	// Recording wrapper forwards Model methods but optional capabilities are
	// deliberately unwrapped here to exercise the production messages owner.
	rec := p.messages.(*mediaRecordingMessages)
	p.messages = rec.Model
	p.SetRoutingID(p.app.Session().ID)
	p.SetSize(100, 30)
	p.messages.SetPosition(13, 17)
	p.messages.SetSize(45, 10)
	img, ok := tuiimage.FromBytes("page-image.png", "image/png", testPNGBytes(t))
	require.True(t, ok)
	p.messages.AppendAssistantMedia("root", []types.AssistantMedia{{ID: 1, Image: &img}})
	view := p.messages.View()
	x, y := -1, -1
	for row, line := range strings.Split(view, "\n") {
		if at := strings.Index(line, "\x1b_cagent-image;"); at >= 0 {
			x = 13 + ansi.StringWidth(line[:at])
			y = 17 + row
			break
		}
	}
	require.GreaterOrEqual(t, x, 13)
	p.messages.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.True(t, p.messages.IsSelecting(), "press retains pointer routing until release")
	_, cmd := p.handleMouseRelease(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.NotNil(t, cmd)
	var collect func(tea.Cmd)
	opened := false
	collect = func(cmd tea.Cmd) {
		if cmd == nil {
			return
		}
		v := cmd()
		if batch, ok := v.(tea.BatchMsg); ok {
			for _, child := range batch {
				collect(child)
			}
		}
		if msg, ok := v.(msgtypes.OpenImagePreviewMsg); ok {
			opened = true
			assert.Equal(t, p.app.Session().ID, msg.SessionID)
			assert.Equal(t, "page-image.png", msg.Preview.Name())
			msg.Preview.Close()
		}
	}
	collect(cmd)
	assert.True(t, opened)
	assert.False(t, p.messages.IsSelecting())
}
