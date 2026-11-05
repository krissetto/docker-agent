package messages

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	builtinshell "github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/tool/shell"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type shellDragBody interface {
	IsExpanded() bool
	IsBodyDragging() bool
	BodyScrollState() (offset int, atBottom, ok bool)
}

// newShellDragTranscript puts an expanded, scrollable shell card first in a
// transcript scrolled to the top, so the body's gutter is on screen.
func newShellDragTranscript(t *testing.T) (*model, shellDragBody) {
	t.Helper()
	ar := animation.NewRuntimeWithScheduler(&shellDragClock{now: time.Unix(1, 0)})
	t.Cleanup(ar.Stop)
	m := NewScrollableView(ar, 80, 20, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	msg := types.ToolCallMessage("root", tools.ToolCall{ID: "shell", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: `{"cmd":"printf rows"}`}}, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusCompleted)
	msg.Content = strings.Repeat("output row\n", 40)
	view := shell.New(ar, msg, m.sessionState)
	view.SetSize(m.contentWidth(), 0)
	view.(interface{ SetExpanded(bool) }).SetExpanded(true)
	m.messages = append(m.messages, msg)
	m.views = append(m.views, view)
	m.renderDirty = true
	_ = m.View()
	m.scrollOffset, m.userHasScrolled = 0, true
	m.scrollview.SetScrollOffset(0)
	_ = m.View()
	return m, view.(shellDragBody)
}

type shellDragClock struct{ now time.Time }

func (c *shellDragClock) Now() time.Time { return c.now }
func (c *shellDragClock) Tick(delay time.Duration, create func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg { c.now = c.now.Add(delay); return create(c.now) }
}

// shellDragGutter is the screen column of the expanded shell body's scrollbar.
func shellDragGutter(m *model) int {
	frame := styles.UserMessageStyle
	inner := m.contentWidth() - frame.GetHorizontalFrameSize()
	return m.xPos + frame.GetBorderLeftSize() + frame.GetPaddingLeft() + inner - 1
}

// TestTranscriptShellBodyThumbDragCapturesUntilRelease: a press on an inner
// body thumb captures the drag in the item (not the transcript) until release,
// and the release is consumed rather than starting a text selection.
func TestTranscriptShellBodyThumbDragCapturesUntilRelease(t *testing.T) {
	m, view := newShellDragTranscript(t)
	m.updateScrollState()
	gutter := shellDragGutter(m)
	firstY := m.yPos + m.lineOffsets[0] + 2 - m.scrollOffset

	_, _ = m.Update(tea.MouseClickMsg{X: gutter, Y: firstY, Button: tea.MouseLeft})
	require.True(t, view.IsBodyDragging(), "pressing the body thumb starts a body drag")
	require.Equal(t, 0, m.bodyDragIdx)
	require.False(t, m.selection.mouseButtonDown, "a body thumb press never starts a text selection")
	require.False(t, m.IsScrollbarDragging(), "the transcript scrollbar is not involved")

	_, _ = m.Update(tea.MouseMotionMsg{X: gutter, Y: firstY + 3, Button: tea.MouseLeft})
	offset, _, _ := view.BodyScrollState()
	require.Positive(t, offset, "dragging the thumb scrolls the body")
	require.Zero(t, m.scrollOffset, "the transcript keeps its offset during a body drag")

	_, _ = m.Update(tea.MouseMotionMsg{X: -100, Y: 500, Button: tea.MouseLeft})
	_, atBottom, _ := view.BodyScrollState()
	require.True(t, atBottom, "capture follows the pointer outside the item")
	require.Zero(t, m.scrollOffset)

	_, cmd := m.Update(tea.MouseReleaseMsg{X: -100, Y: -100, Button: tea.MouseLeft})
	require.False(t, view.IsBodyDragging(), "release outside the body ends the drag")
	require.Equal(t, -1, m.bodyDragIdx)
	require.False(t, m.selection.active, "the release is consumed, never a copy")
	if cmd != nil {
		_, isURL := cmd().(msgtypes.OpenURLMsg)
		require.False(t, isURL)
	}

	offset, _, _ = view.BodyScrollState()
	_, _ = m.Update(tea.MouseMotionMsg{X: gutter, Y: firstY + 1, Button: tea.MouseLeft})
	after, _, _ := view.BodyScrollState()
	require.Equal(t, offset, after, "motion after release is ordinary hover")
}
