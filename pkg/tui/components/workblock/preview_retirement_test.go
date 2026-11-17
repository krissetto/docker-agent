package workblock

import (
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestReasoningRetirementFadesBeforeOneGeometryDropAndResumes(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1, 0)}
	ar := animation.NewRuntimeWithScheduler(clock)
	t.Cleanup(ar.Stop)
	m := New(ar)
	t.Cleanup(m.StopAnimation)
	child := reasoningblock.New(ar, "r", "root", service.StaticSessionState{})
	child.SetReasoning("one\n\ntwo")
	m.SetItems([]Item{{SourceIndex: 1, View: child}})
	require.Equal(t, 1, m.Height(), "settled replay has no preview")
	child.SetReasoningActive(true)
	m.SetStatus(Status{Active: true, Waiting: "Waiting"})
	m.InvalidateSource(1)
	advance(t, ar, m, clock, time.Second)
	require.Equal(t, 4, m.Height())
	child.SetReasoningActive(false)
	m.InvalidateSource(1)
	require.True(t, m.previewRetire.Running())
	full := m.View()
	deadline := ar.Now() + m.previewRetire.Duration() - m.previewRetire.Elapsed()
	advance(t, ar, m, clock, 100*time.Millisecond)
	require.Equal(t, 4, m.Height(), "fade holds full reserved budget")
	require.NotEqual(t, full, m.View(), "fade changes paint before geometry")
	require.Equal(t, ansi.Strip(full), ansi.Strip(m.View()), "opacity preserves text and width until completion")
	require.Empty(t, m.Rows(), "preview remains inert")
	m.SetItems([]Item{{SourceIndex: 1, View: child}})
	require.Equal(t, deadline, ar.Now()+m.previewRetire.Duration()-m.previewRetire.Elapsed())
	child.SetReasoningActive(true)
	child.AppendReasoning(" again")
	m.InvalidateSource(1)
	require.False(t, m.previewRetire.Running())
	require.Equal(t, 4, m.Height())
	child.SetReasoningActive(false)
	m.InvalidateSource(1)
	advance(t, ar, m, clock, time.Second)
	require.Equal(t, 1, m.Height())
	require.True(t, m.Status().Active, "reasoning retirement does not settle ongoing work")
	require.False(t, m.NeedsTick())
	require.Zero(t, ar.ActiveCount())
	m.SetExpandedImmediately(true)
	require.Contains(t, ansi.Strip(m.View()), "one")
	require.Contains(t, ansi.Strip(m.View()), "two again")
	m.SetExpandedImmediately(false)
	require.Equal(t, 1, m.Height(), "history reveal cannot resurrect retired preview")
}

func TestReasoningRetirementCancellationReleasesLease(t *testing.T) {
	for _, cancel := range []string{"resize", "expand", "hide"} {
		t.Run(cancel, func(t *testing.T) {
			ar := animation.NewRuntime()
			t.Cleanup(ar.Stop)
			m := New(ar)
			t.Cleanup(m.StopAnimation)
			child := reasoningblock.New(ar, "r", "root", service.StaticSessionState{})
			child.SetReasoning("private history")
			child.SetReasoningActive(true)
			m.SetItems([]Item{{SourceIndex: 1, View: child}})
			m.SetStatus(Status{Active: true, Waiting: "Waiting"})
			child.SetReasoningActive(false)
			m.InvalidateSource(1)
			require.True(t, m.previewRetire.Running())
			switch cancel {
			case "resize":
				m.SetSize(40, 20)
			case "expand":
				m.SetExpandedImmediately(true)
			case "hide":
				m.StopAnimation()
				m.ResumeAnimation()
			}
			require.False(t, m.previewRetire.Running())
			require.Zero(t, ar.ActiveCount())
			m.SetExpandedImmediately(false)
			require.Equal(t, 1, m.Height())
		})
	}
}
