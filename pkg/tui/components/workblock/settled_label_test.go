package workblock

import (
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
)

func TestSettledLabelFromAuthoritativeDuration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  Status
		want    string
		omitted bool
	}{
		{name: "completed", status: Status{ToolCount: 1, Elapsed: 12400 * time.Millisecond}, want: "Work [+] (1 tool) · 12s"},
		{name: "stopped", status: Status{Elapsed: 12400 * time.Millisecond, Outcome: OutcomeStopped}, want: "Work [+] · Stopped after 12s"},
		{name: "failed", status: Status{Elapsed: 3 * time.Second, Outcome: OutcomeFailed}, want: "Work [+] · Failed after 3s"},
		{name: "stopped sub-second keeps outcome without seconds", status: Status{Elapsed: 999 * time.Millisecond, Outcome: OutcomeStopped}, want: "Work [+] · Stopped"},
		{name: "failed sub-second keeps outcome without seconds", status: Status{Elapsed: 500 * time.Millisecond, Outcome: OutcomeFailed}, want: "Work [+] · Failed"},
		{name: "stopped without accepted duration", status: Status{ToolCount: 1, Outcome: OutcomeStopped}, want: "Work [+] (1 tool) · Stopped"},
		{name: "failed without accepted duration", status: Status{Outcome: OutcomeFailed}, want: "Work [+] · Failed"},
		{name: "completed sub-second omitted", status: Status{Elapsed: 999 * time.Millisecond}, omitted: true},
		{name: "unknown omitted", status: Status{ToolCount: 2}, omitted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := New(animation.NewRuntime())
			t.Cleanup(model.StopAnimation)
			model.SetStatus(tc.status)
			header := ansi.Strip(model.header())
			if tc.omitted {
				require.NotContains(t, header, "·")
				require.NotContains(t, header, "after")
				return
			}
			require.Equal(t, "  "+tc.want, header)
		})
	}
}

func TestWaitingLabelReplacesWorkingAndPausesSpinner(t *testing.T) {
	ar := animation.NewRuntimeWithScheduler(&fakeClock{now: time.Unix(1, 0)})
	model := New(ar)
	t.Cleanup(model.StopAnimation)

	model.SetStatus(Status{Active: true, ToolCount: 1})
	require.EqualValues(t, 1, ar.ActiveCount(), "a running turn animates its header spinner")
	require.Contains(t, ansi.Strip(model.header()), "Working")

	model.SetStatus(Status{Active: true, ToolCount: 1, Waiting: "Waiting for approval"})
	header := ansi.Strip(model.header())
	require.Contains(t, header, "Waiting for approval")
	require.NotContains(t, header, "Working")
	require.Contains(t, header, "(1 tool)")
	require.Zero(t, ar.ActiveCount(), "the spinner pauses while blocked on a human")
	require.NotContains(t, header, model.spinner.RawFrame(), "no spinner frame while blocked")

	model.SetStatus(Status{Active: true, ToolCount: 1})
	require.EqualValues(t, 1, ar.ActiveCount(), "resolving the wait re-arms the spinner")
	require.Contains(t, ansi.Strip(model.header()), "Working")

	model.SetStatus(Status{ToolCount: 1, Elapsed: 2 * time.Second})
	require.Zero(t, ar.ActiveCount())
	require.Contains(t, ansi.Strip(model.header()), "· 2s")
}
