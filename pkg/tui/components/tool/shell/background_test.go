package shell

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestBackgroundShellStates(t *testing.T) {
	now := time.Date(2026, 6, 12, 12, 0, 10, 0, time.UTC)
	for _, tc := range []struct {
		name          string
		state         tools.BackgroundProcessState
		authoritative bool
		exit          int
		timeout       bool
		status        string
		failed        bool
		running       bool
	}{
		{name: "running", state: tools.BackgroundProcessRunning, authoritative: true, status: "10s", running: true},
		{name: "unverified running", state: tools.BackgroundProcessRunning, status: "ended"},
		{name: "completed", state: tools.BackgroundProcessCompleted, status: "4s"},
		{name: "exit", state: tools.BackgroundProcessFailed, exit: 23, status: "exit 23", failed: true},
		{name: "timeout", state: tools.BackgroundProcessFailed, exit: -1, timeout: true, status: "timeout", failed: true},
		{name: "failed", state: tools.BackgroundProcessFailed, exit: -1, status: "failed", failed: true},
		{name: "stopped", state: tools.BackgroundProcessStopped, status: "stopped"},
		{name: "unknown", state: "unknown", status: "ended"},
		{name: "legacy", status: "in background"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := newScrollableShell(t, 100)
			view.message.Background = &types.ShellBackground{ProcessID: "job_12_3", State: tc.state, Authoritative: tc.authoritative, ExitCode: tc.exit, TimedOut: tc.timeout, StartedAt: now.Add(-10 * time.Second), FinishedAt: now.Add(-6 * time.Second)}
			icon, status := view.backgroundHeader(now)
			require.Equal(t, tc.status, ansi.Strip(status))
			require.Equal(t, tc.running, view.backgroundRunning())
			if tc.failed {
				require.Contains(t, ansi.Strip(icon), "✗")
			} else if !tc.running {
				require.Contains(t, ansi.Strip(icon), "✓")
			}
			view.SetExpanded(false)
			collapsed := ansi.Strip(view.View())
			require.NotContains(t, collapsed, "\n")
			if !tc.running {
				require.Contains(t, collapsed, tc.status)
			}
			require.LessOrEqual(t, ansi.StringWidth(collapsed), 100)
			view.SetExpanded(true)
			require.True(t, view.WheelAt(2, 1, 1000))
			expanded := ansi.Strip(view.View())
			footer := "end of output"
			if tc.running {
				footer = "live · running in background"
			} else if tc.state == "" {
				footer = "live output unavailable"
			}
			require.Contains(t, expanded, footer+" · job_12_3")
			require.Contains(t, expanded, "started 2026-06-12T12:00:00Z")
			require.LessOrEqual(t, strings.Count(view.details(), "\n")+1, maxBodyRows)
		})
	}
}

func TestBackgroundShellFooterWithoutOutput(t *testing.T) {
	view := newScrollableShell(t, 100)
	view.message.Background = &types.ShellBackground{ProcessID: "job_12_3", State: tools.BackgroundProcessRunning, Authoritative: true}
	view.message.Content = ""
	view.message.ToolCall.Function.Arguments = `{}`
	plain := ansi.Strip(view.View())
	require.Contains(t, plain, "live · running in background · job_12_3")
	require.NotContains(t, plain, "started")
	require.LessOrEqual(t, strings.Count(view.details(), "\n")+1, maxBodyRows)
}

func TestBackgroundShellHandoffIsNotOutput(t *testing.T) {
	view := newScrollableShell(t, 100)
	view.message.Background = &types.ShellBackground{ProcessID: "job_12_3"}
	view.message.Content = "Command continues in the background as process ID job_12_3. Completion output will be delivered automatically. guidance"
	plain := ansi.Strip(view.View())
	require.Contains(t, plain, "$ printf output")
	require.Contains(t, plain, "live output unavailable · job_12_3")
	require.NotContains(t, plain, "Command continues")
	require.NotContains(t, plain, "guidance")
}

func TestBackgroundShellSanitizesOutput(t *testing.T) {
	view := newScrollableShell(t, 100)
	view.message.Background = &types.ShellBackground{ProcessID: "job_12_3", State: tools.BackgroundProcessCompleted}
	view.message.Content = "first\r\nsecond\rthird\x1b[2J\x1b]0;unsafe title\x07\x00\b\x7f\u009b\x1b[31mred\x1b[0m\ttail"
	rendered := view.View()
	plain := ansi.Strip(rendered)
	require.Contains(t, plain, "first")
	require.Contains(t, plain, "second")
	require.Contains(t, plain, "thirdred    tail")
	for _, control := range []string{"\r", "\x1b[2J", "unsafe title", "\x00", "\b", "\x7f", "\u009b"} {
		require.NotContains(t, rendered, control)
	}
}

func TestBackgroundShellFollowsOnlyAtBottom(t *testing.T) {
	for _, atBottom := range []bool{true, false} {
		t.Run(map[bool]string{true: "bottom", false: "scrolled up"}[atBottom], func(t *testing.T) {
			view := newScrollableShell(t, 100)
			view.message.Background = &types.ShellBackground{ProcessID: "job_12_3", State: tools.BackgroundProcessRunning, Authoritative: true}
			view.View()
			require.True(t, view.WheelAt(2, 1, 1000))
			if !atBottom {
				require.True(t, view.WheelAt(2, 1, -3))
			}
			old := view.bodyOffset
			view.message.AppendToolOutput("new one\nnew two\n")
			view.View()
			if atBottom {
				require.Greater(t, view.bodyOffset, old)
				require.Equal(t, view.bodyMaxOffset(len(view.detailsLines())), view.bodyOffset)
			} else {
				require.Equal(t, old, view.bodyOffset)
				require.Contains(t, ansi.Strip(view.View()), "more lines · scroll")
			}
			offset, bottom, ok := view.BodyScrollState()
			require.True(t, ok)
			require.Equal(t, atBottom, bottom)
			replacement := newScrollableShell(t, 100)
			replacement.message = view.message
			replacement.PreserveBodyScrollFrom(view)
			replacement.View()
			require.Equal(t, offset, replacement.bodyOffset)
		})
	}
}

func TestBackgroundShellSpinnerLifecycle(t *testing.T) {
	view := newScrollableShell(t, 100)
	bg := &types.ShellBackground{ProcessID: "job_12_3", State: tools.BackgroundProcessRunning, Authoritative: true}
	view.message.Background = bg
	view.Init()
	require.True(t, view.backgroundSpinnerActive)
	bg.State = tools.BackgroundProcessCompleted
	view.Update(nil)
	require.False(t, view.backgroundSpinnerActive)
	bg.State = tools.BackgroundProcessRunning
	view.Update(nil)
	require.True(t, view.backgroundSpinnerActive)
	view.StopAnimation()
	require.False(t, view.backgroundSpinnerActive)
	view.ResumeAnimation()
	require.True(t, view.backgroundSpinnerActive)
	bg.Authoritative = false
	view.Update(nil)
	require.False(t, view.backgroundSpinnerActive)
	animation.StopView(view)
}

func TestBackgroundShellDoesNotChangeForeground(t *testing.T) {
	view := newScrollableShell(t, 100)
	foreground := view.View()
	collapsed := view.CollapsedView()
	view.message.Background = &types.ShellBackground{ProcessID: "job_12_3", State: tools.BackgroundProcessCompleted}
	require.Contains(t, ansi.Strip(view.View()), "job_12_3")
	view.message.Background = nil
	require.Equal(t, foreground, view.View())
	require.Equal(t, collapsed, view.CollapsedView())
	require.Equal(t, styles.ToolCompletedIcon.Render("✓"), view.base.StatusIcon())
	require.NotContains(t, ansi.Strip(view.View()), "job_12_3")
}

func TestBackgroundShellUnavailableOutputKeepsRunningStatus(t *testing.T) {
	view := newScrollableShell(t, 100)
	view.message.Background = &types.ShellBackground{ProcessID: "job_12_3", State: tools.BackgroundProcessRunning, Authoritative: true, OutputUnavailable: true}
	require.True(t, view.backgroundRunning())
	require.True(t, view.NeedsTick())
	require.Contains(t, ansi.Strip(view.View()), "live output unavailable · job_12_3")
	require.NotContains(t, ansi.Strip(view.View()), "live · running")
}
