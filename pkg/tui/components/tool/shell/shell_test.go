package shell

import (
	"encoding/json"
	"fmt"
	"image/color"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	builtinshell "github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/agentmessage"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestFormatShellOutputRetainsFullOutput(t *testing.T) {
	output := "first\r\n" + strings.Repeat("long line 界 e\u0301\x1b[31m red\x1b[0m\n", 25) + "last\r"
	got := formatShellOutput(output, 24)
	plain := ansi.Strip(got)
	require.Contains(t, plain, "first")
	require.Contains(t, plain, "last")
	require.Equal(t, 27, strings.Count(got, "\n")+1)
	for _, line := range strings.Split(got, "\n") {
		require.LessOrEqual(t, ansi.StringWidth(line), 22)
	}
}

func TestShellDisclosureSummaryDetailsAndGeometry(t *testing.T) {
	for _, status := range []types.ToolStatus{types.ToolStatusPending, types.ToolStatusRunning, types.ToolStatusCompleted, types.ToolStatusError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			ar := animation.NewRuntime()
			t.Cleanup(ar.Stop)
			msg := types.ToolCallMessage("root", tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: `{"cmd":"printf 'wide 界 output'; \n echo second"}`}}, tools.Tool{Name: builtinshell.ToolNameShell}, status)
			msg.Content = "stdout line one\n" + strings.Repeat("long output line\n", 24) + "stderr line"
			view := New(ar, msg, service.StaticSessionState{}).(*shellModel)
			for _, width := range []int{100, 36, 20, 6, 3, 2, 1, 0, 100} {
				view.SetSize(width, 0)
				compact := view.View()
				require.NotContains(t, ansi.Strip(compact), "stdout line one")
				require.NotContains(t, compact, "\n", "summary always occupies a single terminal line")
				require.LessOrEqual(t, ansi.StringWidth(compact), width)
				if width >= 2 {
					for col := range width {
						require.True(t, view.IsToggleAt(0, col), "whole header row toggles at %d,%d", width, col)
					}
				}
				require.NotContains(t, ansi.Strip(compact), "running")
				require.NotContains(t, ansi.Strip(compact), "pending")
				require.NotContains(t, ansi.Strip(compact), "done")
				require.NotContains(t, ansi.Strip(compact), "failed")
				require.NotContains(t, ansi.Strip(compact), ">", "no visible disclosure arrow")
			}
			if status == types.ToolStatusRunning {
				msg.StartedAt = timePtr(time.Now().Add(-2 * time.Second))
			}
			view.SetSize(100, 0)
			compact := view.View()
			require.True(t, strings.HasPrefix(ansi.Strip(compact), "  "), "shell preserves the usual two-column tool gutter")
			require.Contains(t, ansi.Strip(compact), "shell")
			if status == types.ToolStatusRunning {
				elapsed := time.Since(*msg.StartedAt).Truncate(time.Second).String()
				require.Contains(t, ansi.Strip(compact), "("+elapsed+")")
			}
			if status == types.ToolStatusCompleted {
				require.Contains(t, ansi.Strip(compact), "✓")
			}
			if status == types.ToolStatusError {
				require.Contains(t, ansi.Strip(compact), "✗")
			}
			require.Contains(t, ansi.Strip(compact), "printf 'wide 界 output'; … (+1 lines)")
			require.Contains(t, compact, styles.ToolMessageStyle.Render("printf 'wide 界 output'; … (+1 lines)"), "collapsed preview is muted")
			view.SetExpanded(true)
			rendered := view.View()
			plain := ansi.Strip(rendered)
			require.Contains(t, plain, "shell")
			require.NotContains(t, plain, "(+1 lines)", "expanded header omits summary")
			require.Contains(t, plain, "Command")
			require.Contains(t, plain, "Output")
			require.Contains(t, plain, "printf 'wide 界 output';")
			require.Contains(t, plain, "echo second")
			require.Equal(t, 1, strings.Count(plain, "printf 'wide 界 output'"), "script is shown once")
			require.Contains(t, plain, "stdout line one")
			require.Contains(t, plain, "stderr line")
			require.Equal(t, 1, strings.Count(plain, "stdout line one"))
			for row, line := range strings.Split(rendered, "\n") {
				require.Equal(t, 100, ansi.StringWidth(line), "expanded rows fill the themed card width; row=%d", row)
			}
			view.SetExpanded(false)
			require.NotContains(t, ansi.Strip(view.View()), "stdout line one")
		})
	}
}

func TestShellDisclosureUpdatesAndStateSurvivesViewReplacement(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	msg := types.ToolCallMessage("root", tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: `{"cmd":"printf one"}`}}, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusPending)
	view := New(ar, msg, service.StaticSessionState{}).(*shellModel)
	msg.ToolCall.Function.Arguments += ` tail` // an incomplete fragment cannot be parsed yet
	view.View()
	msg.ToolCall.Function.Arguments = `{"cmd":"printf one two"}`
	msg.AppendToolOutput("live stdout\n")
	require.NotContains(t, ansi.Strip(view.View()), "live stdout")
	view.SetExpanded(true)
	require.Contains(t, ansi.Strip(view.View()), "live stdout")

	nextMessage := types.ToolCallMessage("root", msg.ToolCall, msg.ToolDefinition, types.ToolStatusCompleted)
	nextMessage.Content = msg.Content
	next := New(ar, nextMessage, service.StaticSessionState{}).(*shellModel)
	agentmessage.PreserveExpansion(view, next)
	require.True(t, next.IsExpanded())
	require.Contains(t, ansi.Strip(next.View()), "live stdout")
	animation.StopView(view)
	animation.StopView(next)
}

func TestExpandedShellCellsShareTheThemedDisclosureBackground(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })

	for _, background := range []string{"#243647", "#354657"} {
		t.Run(background, func(t *testing.T) {
			theme := styles.DefaultTheme()
			theme.Colors.BackgroundAlt = background
			styles.ApplyTheme(theme)

			ar := animation.NewRuntime()
			t.Cleanup(ar.Stop)
			command := "printf first\nprintf second"
			started := time.Now().Add(-3 * time.Second)
			msg := types.ToolCallMessage("root", tools.ToolCall{ID: "surface", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: shellArgs(command)}}, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusRunning)
			msg.StartedAt = &started
			msg.Content = "stdout \x1b[38;2;20;180;220mtruecolor\x1b[0m rest\tvalue\nsecond \x1b[48;2;2;3;4moutput\x1b[49m end"
			view := New(ar, msg, service.StaticSessionState{}).(*shellModel)
			view.SetSize(64, 0)
			view.SetExpanded(true)
			rendered := view.View()
			require.Contains(t, ansi.Strip(rendered), "Command")
			require.Contains(t, ansi.Strip(rendered), "Output")
			require.Contains(t, rendered, "38;2;20;180;220", "output foreground survives nested SGR reset")
			require.Contains(t, rendered, "48;2;2;3;4", "output background is preserved inside the inherited card surface")
			wantBG := color.NRGBAModel.Convert(styles.UserMessageStyle.GetBackground())
			for row, line := range strings.Split(rendered, "\n") {
				require.Equal(t, 64, ansi.StringWidth(line), "row %d is card-width", row)
				for col := 1; col < 64; col++ {
					require.Equal(t, wantBG, color.NRGBAModel.Convert(backgroundAt(line, col)), "disclosure surface is continuous: row=%d col=%d line=%q", row, col, ansi.Strip(line))
				}
			}
			view.SetSize(40, 0)
			resized := view.View()
			for row, line := range strings.Split(resized, "\n") {
				require.Equal(t, 40, ansi.StringWidth(line), "resized row %d remains card-width", row)
				for col := 1; col < 40; col++ {
					require.Equal(t, wantBG, color.NRGBAModel.Convert(backgroundAt(line, col)), "resized surface is continuous: row=%d col=%d", row, col)
				}
			}
			animation.StopView(view)
		})
	}
}

func shellArgs(command string) string {
	encoded, _ := json.Marshal(builtinshell.RunShellArgs{Cmd: command})
	return string(encoded)
}

func backgroundAt(line string, col int) color.Color {
	parser := ansi.GetParser()
	defer ansi.PutParser(parser)
	var state byte
	var bg color.Color
	for x := 0; line != ""; {
		sequence, width, consumed, next := ansi.DecodeSequence(line, state, parser)
		if consumed == 0 {
			break
		}
		if ansi.HasCsiPrefix(sequence) && parser.Command() == 'm' {
			params := parser.Params()
			if len(params) == 0 {
				bg = nil
			}
			for i := 0; i < len(params); i++ {
				switch params[i].Param(0) {
				case 0, 49:
					bg = nil
				case 48:
					var parsed color.Color
					read := ansi.ReadStyleColor(params[i:], &parsed)
					if parsed != nil {
						bg = parsed
					}
					i += max(0, read-1)
				}
			}
		}
		if width > 0 && x+width > col {
			if bg == nil {
				return nil
			}
			return color.NRGBAModel.Convert(bg)
		}
		x += width
		state, line = next, line[consumed:]
	}
	return nil
}

func TestShellPreviewUsesFirstUsefulLineAndCountsRemainingLines(t *testing.T) {
	preview, remaining := shellPreview("#!/bin/sh\n\n  echo one\n\necho two\n")
	require.Equal(t, "echo one … (+1 lines)", preview)
	require.Equal(t, 1, remaining)
}

func timePtr(t time.Time) *time.Time { return &t }
