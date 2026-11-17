package reasoningblock

import (
	"encoding/json"
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

// uncoloredText returns visible non-space text whose foreground is the terminal default.
func uncoloredText(line string) string {
	parser := ansi.GetParser()
	defer ansi.PutParser(parser)
	var state byte
	fg := false
	var out strings.Builder
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
			out.WriteString(seq)
		}
		state, rest = next, rest[n:]
	}
	return out.String()
}

// A collapsed reasoning block shows nested shell summaries; a command with an
// SGR reset must keep the summary's muted color after the reset.
func TestCollapsedReasoningKeepsNestedShellSummaryColored(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	block := New(ar, "shell-summary", "root", service.StaticSessionState{})
	t.Cleanup(block.StopAnimation)
	args, err := json.Marshal(builtinshell.RunShellArgs{Cmd: "printf '\x1b[31mred\x1b[0m plain tail'"})
	require.NoError(t, err)
	block.AddToolCall(types.ToolCallMessage("root", tools.ToolCall{
		ID:       "nested-shell",
		Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: string(args)},
	}, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusRunning))

	var row string
	for line := range strings.SplitSeq(block.View(), "\n") {
		if strings.Contains(ansi.Strip(line), "plain tail") {
			row = line
		}
	}
	require.NotEmpty(t, row, "collapsed reasoning shows the nested shell summary")
	require.Empty(t, uncoloredText(row), "nested shell summary keeps muted color after the command's reset")
}
