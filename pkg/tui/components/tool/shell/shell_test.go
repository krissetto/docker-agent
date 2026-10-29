package shell

import (
	"encoding/json"
	"fmt"
	"image/color"
	"strconv"
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
				if width >= 35 {
					require.Contains(t, ansi.Strip(compact), ">", "visible header includes persistent disclosure control")
				}
				require.NotContains(t, ansi.Strip(compact), "running")
				require.NotContains(t, ansi.Strip(compact), "pending")
				require.NotContains(t, ansi.Strip(compact), "done")
				require.NotContains(t, ansi.Strip(compact), "failed")
			}
			if status == types.ToolStatusRunning {
				msg.StartedAt = new(time.Now().Add(-2 * time.Second))
			}
			view.SetSize(100, 0)
			compact := view.View()
			require.True(t, strings.HasPrefix(ansi.Strip(compact), "  "), "shell preserves the usual two-column tool gutter")
			require.Contains(t, ansi.Strip(compact), "shell")
			if status == types.ToolStatusRunning {
				elapsed := time.Since(*msg.StartedAt).Truncate(time.Second).String()
				require.Contains(t, ansi.Strip(compact), elapsed)
			}
			if status == types.ToolStatusCompleted {
				require.Contains(t, ansi.Strip(compact), "✓")
				require.Contains(t, ansi.Strip(compact), "✓ shell", "status and tool name have one stable separator")
			}
			if status == types.ToolStatusError {
				require.Contains(t, ansi.Strip(compact), "✗")
				require.Contains(t, ansi.Strip(compact), "error")
			}
			require.Contains(t, ansi.Strip(compact), "printf 'wide 界 output'; … (+1 lines)")
			require.Contains(t, compact, styles.ToolMessageStyle.Render("printf 'wide 界 output'; … (+1 lines)"), "collapsed preview is muted")
			view.SetExpanded(true)
			rendered := view.View()
			plain := ansi.Strip(rendered)
			require.Contains(t, plain, "shell")
			require.NotContains(t, plain, "(+1 lines)", "expanded header avoids duplicating command preview")
			require.Contains(t, plain, "$")
			require.Contains(t, plain, "─")
			require.Contains(t, plain, "printf 'wide 界 output';")
			require.Contains(t, plain, "echo second")
			require.Equal(t, 1, strings.Count(plain, "printf 'wide 界 output'"), "script is shown once")
			require.Contains(t, plain, "stdout line one")
			require.Equal(t, 1, strings.Count(plain, "stdout line one"))
			require.True(t, view.WheelAt(10, 1, 1000), "expanded shell body scrolls independently")
			rendered = view.View()
			require.Contains(t, ansi.Strip(rendered), "(+1 lines)", "header retains command context when body scrolls")
			plain = ansi.Strip(rendered)
			require.Contains(t, plain, "stderr line")
			for row, line := range strings.Split(rendered, "\n") {
				require.Equal(t, 100, ansi.StringWidth(line), "expanded rows fill the themed card width; row=%d", row)
			}
			view.SetExpanded(false)
			require.NotContains(t, ansi.Strip(view.View()), "stdout line one")
		})
	}
}

func TestShellStatusIconKeepsToolNameGapStable(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	msg := types.ToolCallMessage("root", tools.ToolCall{ID: "spacing", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: shellArgs("printf spacing")}}, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusRunning)
	view := New(ar, msg, service.StaticSessionState{}).(*shellModel)
	view.SetSize(80, 0)
	running := ansi.Strip(view.View())
	msg.ToolStatus = types.ToolStatusCompleted
	completed := ansi.Strip(view.View())
	require.Equal(t, strings.Index(running, "shell"), strings.Index(completed, "shell"), "spinner and completed glyph align the tool label")
	msg.ToolStatus = types.ToolStatusError
	errored := ansi.Strip(view.View())
	require.Equal(t, strings.Index(running, "shell"), strings.Index(errored, "shell"), "error icon keeps the same label gutter")
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
			msg.Content = "stdout \x1b[38;2;20;180;220mtruecolor\x1b[0m rest\tvalue\nsecond \x1b[48;2;2;3;4moutput\x1b[49m end\n" + strings.Repeat("additional output\n", 30)
			view := New(ar, msg, service.StaticSessionState{}).(*shellModel)
			view.SetSize(64, 0)
			view.SetExpanded(true)
			wantBG := color.NRGBAModel.Convert(styles.UserMessageStyle.GetBackground())
			rendered := view.View()
			require.Contains(t, ansi.Strip(rendered), "$")
			require.Contains(t, ansi.Strip(rendered), "─")
			view.WheelAt(10, 1, 1_000_000)
			rendered = view.View()
			footer := ""
			for _, row := range strings.Split(rendered, "\n") {
				if strings.Contains(ansi.Strip(row), "end of output") {
					footer = row
				}
			}
			require.NotEmpty(t, footer, "footer row is present: %q", ansi.Strip(rendered))
			require.Contains(t, footer, "48;2;", "footer carries inherited card background")
			for row, line := range strings.Split(rendered, "\n") {
				require.Equal(t, 64, ansi.StringWidth(line), "row %d is card-width", row)
				for col := 1; col < 64; col++ {
					if bg := backgroundAt(line, col); bg != nil {
						require.Equal(t, wantBG, color.NRGBAModel.Convert(bg), "disclosure surface is continuous: row=%d col=%d line=%q", row, col, ansi.Strip(line))
					}
				}
			}
			view.SetSize(40, 0)
			resized := view.View()
			for row, line := range strings.Split(resized, "\n") {
				require.Equal(t, 40, ansi.StringWidth(line), "resized row %d remains card-width", row)
				for col := 1; col < 40; col++ {
					if bg := backgroundAt(line, col); bg != nil {
						require.Equal(t, wantBG, color.NRGBAModel.Convert(bg), "resized surface is continuous: row=%d col=%d", row, col)
					}
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

func TestShellPartialArgumentsFailureVisibilityAndTerminalSanitizing(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	msg := types.ToolCallMessage("root", tools.ToolCall{ID: "partial", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: `{"cmd":"printf 'partial`}}, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusPending)
	view := New(ar, msg, service.StaticSessionState{}).(*shellModel)
	view.SetSize(80, 0)
	require.Contains(t, ansi.Strip(view.View()), "printf 'partial", "incomplete streamed JSON still exposes its command preview")
	msg.ToolStatus = types.ToolStatusCompleted
	msg.Content = "Error executing command: exit status 17\nOutput: no\n"
	header := ansi.Strip(view.View())
	require.Contains(t, header, "✗")
	require.Contains(t, header, "exit 17", "non-zero shell exit stays visible in the collapsed header")
	msg.Content = "Command timed out after 30s\nOutput: still running"
	require.Contains(t, ansi.Strip(view.View()), "timeout")
	msg.Content = "Build output: Error executing command: exit status 1 was included in stdout"
	require.NotContains(t, ansi.Strip(view.View()), "exit 1", "ordinary output containing error-looking text does not imply failure")
	msg.Content = "Error: 0 errors, completed successfully"
	require.NotContains(t, ansi.Strip(view.View()), "failed", "successful stdout that begins with Error is not a failure")
	msg.Content = "[shell-hint] Check shell syntax.\n\nError executing command: exit status 12\nOutput: failed"
	require.Contains(t, ansi.Strip(view.View()), "exit 12", "known shell hint prelude preserves non-zero failure status")
	output := formatShellOutput("safe\x1b[31m red\x1b[0m \x1b]0;title\a \x1b[2J\x1b[?25l \x1bP1;2|private\x1b\\\nnext\x00\r", 40)
	plain := ansi.Strip(output)
	require.Contains(t, plain, "red")
	require.Contains(t, plain, "next")
	require.NotContains(t, output, "title")
	require.NotContains(t, output, "private")
	require.NotContains(t, output, "2J")
	require.NotContains(t, output, "?25l")
	require.NotContains(t, output, "\x00")
	rawC1 := formatShellOutput("safe\x9b2J\x9d52;title\x9c invalid\xffbytes", 40)
	require.NotContains(t, rawC1, "\x9b")
	require.NotContains(t, rawC1, "\x9d")
	require.NotContains(t, rawC1, "\x9c")
	require.NotContains(t, rawC1, "\xff")
	require.Contains(t, ansi.Strip(rawC1), "invalid\xEF\xBF\xBDbytes")
	animation.StopView(view)
}

func TestShellOutputWrapPreservesTerminalColumnIndentation(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	msg := types.ToolCallMessage("root", tools.ToolCall{ID: "columns", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: shellArgs("printf x")}}, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusCompleted)
	msg.Content = "first row\nsecond row"
	view := New(ar, msg, service.StaticSessionState{}).(*shellModel)
	view.SetSize(24, 0)
	view.SetExpanded(true)
	body := strings.Join(view.detailsLines(), "\n")
	lines := strings.Split(ansi.Strip(body), "\n")
	first, second := -1, -1
	for _, line := range lines {
		if strings.Contains(line, "first row") {
			first = strings.Index(line, "first row")
		}
		if strings.Contains(line, "second row") {
			second = strings.Index(line, "second row")
		}
	}
	require.GreaterOrEqual(t, first, 0)
	require.Equal(t, first, second, "output lines preserve the terminal's leading column")
}

func TestShellWheelBoundariesAndCacheInvalidation(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	command := strings.Repeat("printf line\n", 30)
	msg := types.ToolCallMessage("root", tools.ToolCall{ID: "scroll", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: shellArgs(command)}}, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusRunning)
	msg.Content = strings.Repeat("output\n", 50)
	view := New(ar, msg, service.StaticSessionState{}).(*shellModel)
	view.SetSize(60, 0)
	require.False(t, view.WheelAt(0, 2, 1), "header doesn't scroll body")
	require.False(t, view.WheelAt(1, 2, -1), "collapsed body passes wheel through")
	view.SetExpanded(true)
	first, last := shellBodyOutputRows(len(view.detailsLines()))
	require.False(t, view.WheelAt(first-1, 2, -1), "body padding above the output passes wheel through")
	require.True(t, view.WheelAt(first, 2, -1), "top output boundary is consumed")
	require.True(t, view.WheelAt(first, 2, 4))
	require.True(t, view.WheelAt(first, 2, 1_000_000))
	require.True(t, view.WheelAt(last, 2, 1), "bottom output boundary is consumed")
	require.False(t, view.WheelAt(last+1, 2, 1), "footer hint passes wheel through")
	view.SetSize(40, 0)
	require.Zero(t, view.bodyOffset, "resize resets body scroll to top")
	msg.Content += "fresh output"
	lines := view.detailsLines()
	require.Contains(t, ansi.Strip(strings.Join(lines, "\n")), "fresh output", "output mutation invalidates wrapped body cache")
	animation.StopView(view)
}

func TestShellPreviewBudgetAndHoverDisclosureStayStableAcrossStreamingStates(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	msg := types.ToolCallMessage("root", tools.ToolCall{ID: "budget", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: shellArgs(strings.Repeat("printf x; ", 20))}}, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusPending)
	view := New(ar, msg, service.StaticSessionState{}).(*shellModel)
	view.SetSize(64, 0)
	pending := ansi.Strip(view.View())
	require.Contains(t, pending, ">", "disclosure stays visible while pending")
	commandStart := strings.Index(pending, "printf")
	msg.ToolStatus = types.ToolStatusRunning
	msg.StartedAt = new(time.Now().Add(-4 * time.Second))
	running := ansi.Strip(view.View())
	require.Contains(t, running, "4s")
	require.Contains(t, running, ">", "disclosure stays visible while running")
	require.Equal(t, commandStart, strings.Index(running, "printf"), "elapsed status must not steal command preview budget")
	view.SetHoveredAt(0, true)
	hovered := ansi.Strip(view.View())
	require.Contains(t, hovered, ">", "hover exposes disclosure control while arguments are streaming")
	require.Equal(t, commandStart, strings.Index(hovered, "printf"), "hover does not steal command preview budget")
	require.NotEqual(t, running, view.View(), "hover changes semantic emphasis without hiding control")
	view.SetHoveredAt(0, false)
	msg.ToolStatus = types.ToolStatusCompleted
	msg.Content = "Error executing command: exit status 9\nOutput: failed"
	completed := ansi.Strip(view.View())
	require.Contains(t, completed, "exit 9")
	require.Contains(t, completed, ">", "disclosure stays visible on completion")
	require.Equal(t, commandStart, strings.Index(completed, "printf"), "completed status does not steal command preview budget")
	animation.StopView(view)
}

func TestExpandedShellStaysBoundedAtNarrowWidths(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	msg := types.ToolCallMessage("root", tools.ToolCall{ID: "narrow", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: shellArgs("printf '界'; printf second")}}, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusCompleted)
	msg.Content = strings.Repeat("界\toutput\n", 20)
	view := New(ar, msg, service.StaticSessionState{}).(*shellModel)
	view.SetExpanded(true)
	for _, width := range []int{24, 12, 11, 10, 6, 3, 1, 0} {
		view.SetSize(width, 0)
		for row, line := range strings.Split(view.View(), "\n") {
			require.LessOrEqual(t, ansi.StringWidth(line), width, "row %d at width %d", row, width)
		}
		if width < 12 {
			require.False(t, view.WheelAt(1, 0, 1), "hidden narrow-width body must not capture wheel")
		}
	}
	animation.StopView(view)
}

func TestShellBodyViewportRowsAndWheelThresholds(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	for _, targetRows := range []int{10, 11, 12, 100} {
		t.Run(strconv.Itoa(targetRows), func(t *testing.T) {
			msg := types.ToolCallMessage("root", tools.ToolCall{ID: "rows", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: shellArgs("printf output")}}, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusRunning)
			view := New(ar, msg, service.StaticSessionState{}).(*shellModel)
			view.SetSize(80, 0)
			view.SetExpanded(true)
			lines := view.detailsLines()
			for count := 0; len(lines) < targetRows && count < 500; count++ {
				msg.Content = strings.Repeat("line\n", count)
				view.bodyLines = nil
				lines = view.detailsLines()
			}
			require.Equal(t, targetRows, len(lines), "fixture should exercise exact body-row threshold")
			details := view.details()
			require.LessOrEqual(t, strings.Count(details, "\n")+1, maxBodyRows, "expanded detail including footer stays row-bounded")
			if shellBodyFits(targetRows) {
				require.False(t, view.WheelAt(1, 1, 1), "threshold-sized bodies have no internal scroll")
			} else {
				first, last := shellBodyOutputRows(targetRows)
				require.True(t, view.WheelAt(first, 1, 1), "first output row scrolls down")
				require.Equal(t, 1, view.bodyOffset)
				require.False(t, view.WheelAt(last+1, 1, 1), "footer hint is not a body hit target")
				for range 1000 {
					view.WheelAt(first, 1, 1)
				}
				require.Equal(t, shellBodyMaxOffset(targetRows), view.bodyOffset)
				require.True(t, view.WheelAt(last, 1, 1), "bottom output boundary is consumed")
			}
		})
	}
}

func TestShellBodyScrollStatePreservedAcrossResultReplacement(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	for _, atBottom := range []bool{false, true} {
		t.Run(fmt.Sprintf("bottom=%t", atBottom), func(t *testing.T) {
			msg := types.ToolCallMessage("root", tools.ToolCall{ID: "preserve", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: shellArgs("printf preserve")}}, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusRunning)
			msg.Content = strings.Repeat("line\n", 40)
			old := New(ar, msg, service.StaticSessionState{}).(*shellModel)
			old.SetSize(80, 0)
			old.SetExpanded(true)
			old.detailsLines()
			if atBottom {
				old.bodyOffset = shellBodyMaxOffset(len(old.bodyLines))
			} else {
				old.bodyOffset = 5
			}
			wantOffset := old.bodyOffset
			nextMessage := *msg
			nextMessage.ToolStatus = types.ToolStatusCompleted
			nextMessage.Content += "final"
			next := New(ar, &nextMessage, service.StaticSessionState{}).(*shellModel)
			next.SetSize(80, 0)
			next.PreserveBodyScrollFrom(old)
			next.SetExpanded(old.IsExpanded())
			next.detailsLines()
			if atBottom {
				require.Equal(t, shellBodyMaxOffset(len(next.bodyLines)), next.bodyOffset)
			} else {
				require.Equal(t, min(wantOffset, shellBodyMaxOffset(len(next.bodyLines))), next.bodyOffset)
			}
		})
	}
}

func TestExpandedShellHeaderIsStableOnOutputGrowth(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	msg := types.ToolCallMessage("root", tools.ToolCall{ID: "growth", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: shellArgs("printf growth")}}, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusRunning)
	view := New(ar, msg, service.StaticSessionState{}).(*shellModel)
	view.SetSize(80, 0)
	view.SetExpanded(true)
	view.View()
	msg.Content = strings.Repeat("output line\n", 40)
	first := view.View()
	second := view.View()
	require.Equal(t, first, second, "body auto-follow and header command context settle in the same frame")
	require.Contains(t, ansi.Strip(first), "printf growth")
}

func TestLongWrappedCommandKeepsPromptANSISequencesIntact(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	command := strings.Repeat("printf 'long command'; ", 12)
	msg := types.ToolCallMessage("root", tools.ToolCall{ID: "wrapped", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: shellArgs(command)}}, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusCompleted)
	view := New(ar, msg, service.StaticSessionState{}).(*shellModel)
	view.SetSize(20, 0)
	view.SetExpanded(true)
	body := view.detailsLines()
	for _, line := range body {
		require.LessOrEqual(t, ansi.StringWidth(line), 20, "line should fit available content width")
	}
	plain := ansi.Strip(strings.Join(body, "\n"))
	require.Contains(t, plain, "$ printf")
	require.Contains(t, strings.Join(strings.Fields(plain), ""), "longcommand", "wrapped command keeps its words")
	require.NotContains(t, strings.Join(body, "\n"), "\x1b[...m", "prompt styles stay valid after wrapping")
}
