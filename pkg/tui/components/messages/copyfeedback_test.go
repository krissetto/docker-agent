package messages

import (
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// The transient "copied" confirmation must keep the background band the copy
// label sat on: the user message band and the code block band.
func TestCopiedFlashKeepsUserMessageBackground(t *testing.T) {
	t.Parallel()

	sessionPos := 0
	userMsg := &types.Message{
		Type:            types.MessageTypeUser,
		Content:         "copy me",
		SessionPosition: &sessionPos,
	}
	m := hoverActionModel(t, userMsg)
	m.handleMouseMotion(tea.MouseMotionMsg{X: 5, Y: 1})
	m.View()

	line, col := findLabel(t, m, types.MessageCopyLabel)
	wantBg := backgroundAt(strings.Split(m.View(), "\n")[line], col)
	require.NotNil(t, wantBg, "the user message copy label must sit on a background band")

	m.handleMouseClick(tea.MouseClickMsg{X: col, Y: line, Button: tea.MouseLeft})

	after := strings.Split(m.View(), "\n")[line]
	require.Contains(t, ansi.Strip(after), types.CopiedFeedbackLabel)
	assert.Equal(t, wantBg, backgroundAt(after, col),
		"the copied label must keep the copy label's background")
}

func TestCopiedFlashKeepsCodeBlockBackground(t *testing.T) {
	t.Parallel()

	m := NewScrollableView(animation.NewRuntime(), 80, 40, &service.SessionState{}).(*model)
	m.SetSize(80, 40)
	m.AddShellOutputMessage("total 0")
	m.renderDirty = true
	m.View()
	m.ensureAllItemsRendered()

	line, col := findLabel(t, m, markdown.CodeBlockCopyIcon)
	wantBg := backgroundAt(strings.Split(m.View(), "\n")[line], col)
	require.NotNil(t, wantBg, "the code block copy icon must sit on the code block band")

	m.handleMouseClick(tea.MouseClickMsg{X: col, Y: line, Button: tea.MouseLeft})

	after := strings.Split(m.View(), "\n")[line]
	require.Contains(t, ansi.Strip(after), types.CopiedFeedbackLabel)
	assert.Equal(t, wantBg, backgroundAt(after, col),
		"the copied label must keep the code block background")
}

func TestBackgroundAt(t *testing.T) {
	t.Parallel()

	rgb := func(r, g, b uint8) color.Color { return color.RGBA{R: r, G: g, B: b, A: 0xff} }

	cases := map[string]struct {
		line string
		col  int
		want color.Color
	}{
		"plain text":                           {line: "hello", col: 2, want: nil},
		"truecolor background":                 {line: "\x1b[48;2;1;2;3mhello\x1b[m", col: 2, want: rgb(1, 2, 3)},
		"basic background":                     {line: "\x1b[41mhello\x1b[m", col: 0, want: ansi.Red},
		"bright background":                    {line: "\x1b[102mhello\x1b[m", col: 0, want: ansi.BrightGreen},
		"indexed background":                   {line: "\x1b[48;5;10mhello\x1b[m", col: 0, want: ansi.IndexedColor(10)},
		"reset before column":                  {line: "\x1b[41mab\x1b[mcd", col: 3, want: nil},
		"bg 49 clears background":              {line: "\x1b[41mab\x1b[49mcd", col: 3, want: nil},
		"fg only keeps outer bg":               {line: "\x1b[48;2;1;2;3m  \x1b[38;5;8mhello", col: 4, want: rgb(1, 2, 3)},
		"foreground payload is not background": {line: "\x1b[48;2;36;40;59m\x1b[38;2;104;49;0mhello", col: 2, want: rgb(36, 40, 59)},
		"underline payload is not background":  {line: "\x1b[48;2;36;40;59m\x1b[58;2;104;49;0mhello", col: 2, want: rgb(36, 40, 59)},
		"indexed foreground is not background": {line: "\x1b[48;2;36;40;59m\x1b[38;5;104mhello", col: 2, want: rgb(36, 40, 59)},
		"invalid utf-8 terminates":             {line: "\x1b[41m\x80\x80hello", col: 4, want: ansi.Red},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, backgroundAt(tc.line, tc.col))
		})
	}
}
