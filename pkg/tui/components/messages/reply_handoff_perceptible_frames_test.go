package messages

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// Counts distinct raw painted frames across one phase's ticks.
func distinctFramesDuring(t *testing.T, m *model, phase replyHandoffPhase) int {
	t.Helper()
	seen := make(map[string]struct{})
	for i := 0; m.replyHandoff.Phase() == phase && i < 100; i++ {
		acceptAndUpdateHandoffTick(t, m.ar, m)
		seen[m.View()] = struct{}{}
	}
	return len(seen)
}

// Opacity changes alter the ANSI output, so raw frames track fade steps.
func TestReplyHandoffFadePhasesRenderPerceptibleFrames(t *testing.T) {
	m := newFourthReplyFixture(t)
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "fourth reply prose"))
	require.Equal(t, replyHandoffFadeOut, m.replyHandoff.Phase(), "the fourth reply retires the oldest reply")

	const minFrames = 5
	require.GreaterOrEqual(t, distinctFramesDuring(t, m, replyHandoffFadeOut), minFrames, "fade-out paints several frames")
	require.Equal(t, replyHandoffReflow, m.replyHandoff.Phase(), "fade-out hands off to reflow")
	distinctFramesDuring(t, m, replyHandoffReflow)
	require.Equal(t, replyHandoffFadeIn, m.replyHandoff.Phase(), "reflow hands off to the incoming fade")
	require.GreaterOrEqual(t, distinctFramesDuring(t, m, replyHandoffFadeIn), minFrames, "fade-in paints several frames")
	require.Equal(t, replyHandoffIdle, m.replyHandoff.Phase())
}

func newTallOldestReplyFixture(t *testing.T) *model {
	t.Helper()
	ar := animation.NewRuntimeWithScheduler(&replyHandoffScheduler{now: testHandoffTime()})
	m := NewScrollableView(ar, 84, 50, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	m.SetCompactWorkLog(true)
	m.SetVisibleReplies(msgtypes.VisibleRepliesThree)
	m.suppressReplyHandoff = true
	m.AddUserMessage("request")
	// Single newlines join into one markdown paragraph; blank lines keep six rows.
	var tall []string
	for i := range 6 {
		tall = append(tall, fmt.Sprintf("tall oldest paragraph %d", i))
	}
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", strings.Join(tall, "\n\n")))
	m.committedMessageCount = len(m.messages)
	for i := 1; i < 3; i++ {
		m.addMessage(types.Agent(types.MessageTypeAssistant, "root", fmt.Sprintf("seed reply %d prose", i)))
		m.committedMessageCount = len(m.messages)
	}
	m.suppressReplyHandoff = false
	m.View()
	m.syncWorkProjection()
	require.Len(t, m.workProjection.exchanges, 1)
	require.Len(t, m.workProjection.exchanges[0].response, 3, "precondition: three Last3 replies visible")
	require.Equal(t, replyHandoffIdle, m.replyHandoff.Phase())
	return m
}

func screenRowOf(m *model, needle string) int {
	for row, line := range strings.Split(ansi.Strip(m.View()), "\n") {
		if strings.Contains(line, needle) {
			return row
		}
	}
	return -1
}

// Counts whole terminal rows the survivor occupies, not per-frame changes.
func TestReplyHandoffReflowGlidesSurvivingReplyAcrossRows(t *testing.T) {
	m := newTallOldestReplyFixture(t)
	const survivor = "seed reply 1 prose"
	startRow := screenRowOf(m, survivor)
	require.GreaterOrEqual(t, startRow, 0, "survivor is visible before the handoff")

	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "fourth reply prose"))
	require.Equal(t, replyHandoffFadeOut, m.replyHandoff.Phase())
	for range 100 {
		if m.replyHandoff.Phase() != replyHandoffFadeOut {
			break
		}
		acceptAndUpdateHandoffTick(t, m.ar, m)
	}
	require.Equal(t, replyHandoffReflow, m.replyHandoff.Phase())

	rows := []int{startRow}
	for range 100 {
		if m.replyHandoff.Phase() != replyHandoffReflow {
			break
		}
		acceptAndUpdateHandoffTick(t, m.ar, m)
		rows = append(rows, screenRowOf(m, survivor))
	}
	require.Equal(t, replyHandoffFadeIn, m.replyHandoff.Phase(), "reflow hands off to the incoming fade")

	travel := startRow - rows[len(rows)-1]
	require.GreaterOrEqual(t, travel, 5, "fixture gives the survivor at least five rows of travel")
	distinct := make(map[int]struct{})
	for i, row := range rows {
		require.GreaterOrEqual(t, row, 0, "survivor stays visible at tick %d", i)
		if i > 0 {
			require.LessOrEqual(t, row, rows[i-1], "survivor never moves down at tick %d", i)
		}
		distinct[row] = struct{}{}
	}
	require.GreaterOrEqual(t, len(distinct), 5, "reflow visits at least five distinct rows")
}
