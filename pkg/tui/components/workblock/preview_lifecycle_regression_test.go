package workblock

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestLivePreviewRevealKeepsDeadlineGeometryAndShiftedRows(t *testing.T) {
	for _, width := range []int{3, 12, 60} {
		t.Run(strings.Repeat("w", width), func(t *testing.T) {
			clock := &fakeClock{now: time.Unix(1, 0)}
			ar := animation.NewRuntimeWithScheduler(clock)
			t.Cleanup(ar.Stop)
			m := New(ar)
			t.Cleanup(m.StopAnimation)
			m.SetSize(width, 20)
			child := reasoningblock.New(ar, "preview", "root", service.StaticSessionState{})
			child.SetReasoning("a")
			child.SetReasoningActive(true)
			m.SetItems([]Item{{SourceIndex: 1, View: child}})
			m.SetStatus(Status{Active: true})
			child.AppendReasoning("\n\nb")
			m.InvalidateSource(1)
			require.True(t, m.previewFade.Running())
			require.Equal(t, []bool{false, true}, m.previewFading)
			deadline := ar.Now() + m.previewFade.Duration() - m.previewFade.Elapsed()
			require.Equal(t, 4, m.Height(), "live preview reserves its bounded row budget")
			generation := m.VisualGeneration()
			advance(t, ar, m, clock, 80*time.Millisecond)
			require.Greater(t, m.VisualGeneration(), generation)
			child.AppendReasoning("c\n\nd\n\ne")
			m.InvalidateSource(1)
			require.Equal(t, deadline, ar.Now()+m.previewFade.Duration()-m.previewFade.Elapsed(), "chunks never restart reveal")
			require.Equal(t, 4, m.Height())
			for _, row := range strings.Split(m.View(), "\n")[1:] {
				require.Equal(t, width, ansi.StringWidth(row), "opacity never changes width, even at narrow widths")
			}
			advance(t, ar, m, clock, time.Second)
			require.False(t, m.previewFade.Running())
			child.AppendReasoning("\n\nf")
			m.InvalidateSource(1)
			require.Equal(t, []bool{false, false, true}, m.previewFading, "shifted tail rows do not re-fade")
			m.SetStatus(Status{})
			advance(t, ar, m, clock, time.Second)
			require.False(t, m.NeedsTick())
			require.Zero(t, ar.ActiveCount(), "completed preview releases its transition and spinner")
		})
	}
}

func TestPreviewLifecycleSnapsOnResizeHideExpandAndReplay(t *testing.T) {
	for _, stop := range []string{"resize", "hide", "expand", "settle"} {
		t.Run(stop, func(t *testing.T) {
			ar := animation.NewRuntime()
			t.Cleanup(ar.Stop)
			m := New(ar)
			t.Cleanup(m.StopAnimation)
			child := reasoningblock.New(ar, "preview", "root", service.StaticSessionState{})
			child.SetReasoning("a")
			child.SetReasoningActive(true)
			m.SetItems([]Item{{SourceIndex: 1, View: child}})
			require.Zero(t, ar.ActiveCount(), "replayed reasoning is immediately settled")
			m.SetStatus(Status{Active: true, Waiting: "Waiting for approval"})
			child.AppendReasoning("\n\nb")
			m.InvalidateSource(1)
			require.True(t, m.previewFade.Running())
			switch stop {
			case "resize":
				m.SetSize(20, 10)
			case "hide":
				m.StopAnimation()
			case "expand":
				m.SetExpandedImmediately(true)
			case "settle":
				m.SetStatus(Status{})
				m.SetSize(20, 10)
			}
			require.False(t, m.previewFade.Running())
			require.Zero(t, ar.ActiveCount())
			require.False(t, m.NeedsTick(), "waiting status alone must not request ticks")
			if stop == "hide" {
				child.AppendReasoning("\n\nthree")
				m.InvalidateSource(1)
				m.ResumeAnimation()
				require.Zero(t, ar.ActiveCount(), "hidden arrivals and waiting resume do not start stale animations")
			}
		})
	}
}

type previewSource struct {
	fakeView

	lines []string
}

func (v *previewSource) PreviewLines() ([]string, bool) {
	return slices.Clone(v.lines), false
}
func (*previewSource) ReasoningActive() bool { return true }
func (*previewSource) AdoptWorkOwner(any)    {}
func (*previewSource) ReleaseWorkOwner(any)  {}

func TestLivePreviewDoesNotShrinkBetweenChunksOrRestartOnUnchangedInvalidation(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1, 0)}
	ar := animation.NewRuntimeWithScheduler(clock)
	t.Cleanup(ar.Stop)
	m := New(ar)
	t.Cleanup(m.StopAnimation)
	child := &previewSource{lines: []string{"first"}}
	m.SetItems([]Item{{SourceIndex: 1, View: child}})
	m.SetStatus(Status{Active: true})
	require.Equal(t, 4, m.Height())
	child.lines = []string{"first", "second"}
	m.InvalidateSource(1)
	advance(t, ar, m, clock, 80*time.Millisecond)
	elapsed := m.previewFade.Elapsed()
	m.InvalidateSource(1)
	m.SetItems([]Item{{SourceIndex: 1, View: child}})
	require.Equal(t, elapsed, m.previewFade.Elapsed(), "projection rebuilds and unchanged invalidations preserve the clock")
	child.lines = nil
	m.InvalidateSource(1)
	require.Equal(t, 4, m.Height(), "transient empty markdown tail cannot shrink live geometry")
	require.Contains(t, ansi.Strip(m.View()), "first", "a transient empty tail retains paint until retirement")
	child.lines = []string{"last"}
	m.InvalidateSource(1)
	require.Equal(t, 4, m.Height())
	m.SetStatus(Status{})
	advance(t, ar, m, clock, time.Second)
	require.Equal(t, 2, m.Height(), "settlement retains the latest actual row without live padding")
	require.Contains(t, ansi.Strip(m.View()), "last")
	require.Zero(t, ar.ActiveCount())
}

// A source reset/deletion is authoritative, unlike a pause for tools. Preserve
// its paint only for retirement, then release both text and reserved rows.
func TestEmptyAndDeletedPreviewSourcesRetireWithoutLosingPaint(t *testing.T) {
	for _, mutation := range []string{"empty", "delete", "replace"} {
		t.Run(mutation, func(t *testing.T) {
			clock := &fakeClock{now: time.Unix(1, 0)}
			ar := animation.NewRuntimeWithScheduler(clock)
			t.Cleanup(ar.Stop)
			m := New(ar)
			t.Cleanup(m.StopAnimation)
			child := reasoningblock.New(ar, "r", "root", service.StaticSessionState{})
			child.SetReasoning("one\n\ntwo\n\nthree")
			child.SetReasoningActive(true)
			m.SetItems([]Item{{SourceIndex: 1, View: child}})
			m.SetStatus(Status{Active: true, Waiting: "Waiting"})
			advance(t, ar, m, clock, time.Second)
			before := ansi.Strip(m.View())
			switch mutation {
			case "empty":
				child.ClearReasoning()
				m.InvalidateSource(1)
			case "delete":
				m.SetItems(nil)
			case "replace":
				replacement := reasoningblock.New(ar, "empty", "root", service.StaticSessionState{})
				replacement.SetReasoningActive(true)
				m.SetItems([]Item{{SourceIndex: 1, View: replacement}})
			}
			require.Equal(t, before, ansi.Strip(m.View()), "retirement keeps the last painted rows")
			require.True(t, m.previewRetire.Running())
			deadline := ar.Now() + m.previewRetire.Duration() - m.previewRetire.Elapsed()
			advance(t, ar, m, clock, 80*time.Millisecond)
			m.Invalidate()
			require.Equal(t, deadline, ar.Now()+m.previewRetire.Duration()-m.previewRetire.Elapsed(), "unchanged empty sources cannot restart retirement")
			advance(t, ar, m, clock, time.Second)
			require.Equal(t, 1, m.Height(), "empty sources cannot retain blank rows forever")
			require.False(t, m.NeedsTick())
			m.StopAnimation()
			m.ResumeAnimation()
			m.SetSize(12, 4)
			require.Equal(t, 1, m.Height(), "resume/resize cannot resurrect cleared content")
		})
	}
}

func TestClearingOnlyActiveChildFallsBackToSettledHistoryPreview(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1, 0)}
	ar := animation.NewRuntimeWithScheduler(clock)
	t.Cleanup(ar.Stop)
	m := New(ar)
	t.Cleanup(m.StopAnimation)
	history := reasoningblock.New(ar, "history", "root", service.StaticSessionState{})
	history.SetReasoning("settled history")
	live := reasoningblock.New(ar, "live", "root", service.StaticSessionState{})
	live.SetReasoning("live tail")
	live.SetReasoningActive(true)
	m.SetItems([]Item{{SourceIndex: 1, View: history}, {SourceIndex: 2, View: live}})
	m.SetStatus(Status{Active: true, Waiting: "Waiting"})
	advance(t, ar, m, clock, time.Second)
	live.ClearReasoning()
	m.InvalidateSource(2)
	require.Contains(t, ansi.Strip(m.View()), "settled history", "source clearing exposes the remaining displayable tail")
	advance(t, ar, m, clock, time.Second)
	require.Equal(t, 2, m.Height(), "settled history uses only its actual row")
	require.Equal(t, []string{"settled history"}, m.previewLines)
	m.Invalidate()
	require.Equal(t, 2, m.Height())
}
