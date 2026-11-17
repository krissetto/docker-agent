package shell

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	builtinshell "github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// uncoloredRunes returns visible non-space text whose foreground is the terminal default.
func uncoloredRunes(line string) []string {
	parser := ansi.GetParser()
	defer ansi.PutParser(parser)
	var state byte
	fg := false
	var out []string
	for rest := line; rest != ""; {
		seq, width, n, next := ansi.DecodeSequence(rest, state, parser)
		if n == 0 {
			break
		}
		if ansi.HasCsiPrefix(seq) && parser.Command() == 'm' {
			params := parser.Params()
			if len(params) == 0 {
				fg = false
			}
			for i := 0; i < len(params); i++ {
				switch code := params[i].Param(0); {
				case code == 0 || code == 39:
					fg = false
				case code == 38:
					fg = true
					i += 2
				case code >= 30 && code <= 37, code >= 90 && code <= 97:
					fg = true
				}
			}
		} else if width > 0 && !fg && strings.TrimSpace(seq) != "" {
			out = append(out, seq)
		}
		state, rest = next, rest[n:]
	}
	return out
}

// A command that resets SGR mid-line must not strip the muted header color from the rest of its preview.
func TestShellHeaderPreviewKeepsColorAcrossCommandReset(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	command := "printf '\x1b[31mred\x1b[0m plain tail'\necho second"
	msg := types.ToolCallMessage("root", tools.ToolCall{ID: "reset", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: shellArgs(command)}}, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusCompleted)
	msg.Content = "first\n" + strings.Repeat("output line\n", 14) + "last"
	view := New(ar, msg, service.StaticSessionState{}).(*shellModel)
	t.Cleanup(func() { animation.StopView(view) })
	view.SetSize(80, 0)

	headerRow := func(rendered string) string {
		row, _, _ := strings.Cut(rendered, "\n")
		return row
	}
	assertHeaderColored := func(label, rendered string) {
		t.Helper()
		row := headerRow(rendered)
		require.Contains(t, ansi.Strip(row), "plain tail", label)
		require.Empty(t, uncoloredRunes(row), "header preview keeps muted color: %s", label)
	}

	assertHeaderColored("collapsed", view.View())
	view.SetExpanded(true)
	require.Empty(t, uncoloredRunes(headerRow(view.View())), "expanded header is colored")
	require.True(t, view.WheelAt(10, 1, 1000))
	assertHeaderColored("expanded and scrolled", view.View())
	view.SetExpanded(false)
	assertHeaderColored("collapsed after expand", view.View())
	view.SetExpanded(true)
	assertHeaderColored("expanded after collapse", view.View())
	view.SetExpanded(false)
	assertHeaderColored("collapsed again", view.View())
}
