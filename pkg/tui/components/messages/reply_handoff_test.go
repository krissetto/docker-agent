package messages

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type replyHandoffScheduler struct{ now time.Time }

func (s *replyHandoffScheduler) Now() time.Time { return s.now }
func (s *replyHandoffScheduler) Tick(delay time.Duration, create func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		s.now = s.now.Add(delay)
		return create(s.now)
	}
}

func newReplyHandoffTest(t *testing.T) (*animation.Runtime, *replyHandoff) {
	t.Helper()
	scheduler := &replyHandoffScheduler{now: time.Unix(1, 0)}
	runtime := animation.NewRuntimeWithScheduler(scheduler)
	handoff := newReplyHandoff(runtime)
	t.Cleanup(runtime.Stop)
	return runtime, handoff
}

func tickReplyHandoff(t *testing.T, runtime *animation.Runtime, handoff *replyHandoff) {
	t.Helper()
	cmd := runtime.Continue()
	require.NotNil(t, cmd, "finite handoff owns one shared runtime tick")
	msg, ok := cmd().(animation.TickMsg)
	require.True(t, ok)
	accepted, acceptedOK := runtime.Accept(msg)
	require.True(t, acceptedOK)
	_, _ = handoff.Tick()
	_ = accepted
}

func advanceReplyHandoffTicks(t *testing.T, runtime *animation.Runtime, handoff *replyHandoff, ticks int) {
	t.Helper()
	for range ticks {
		tickReplyHandoff(t, runtime, handoff)
	}
}

func finishReplyHandoffPhase(t *testing.T, runtime *animation.Runtime, handoff *replyHandoff) {
	t.Helper()
	remaining := handoff.transition.Duration() - handoff.transition.Elapsed()
	frames := int(remaining / animation.TickRate)
	if remaining%animation.TickRate != 0 {
		frames++
	}
	for range frames {
		tickReplyHandoff(t, runtime, handoff)
	}
}

func handoffRow(source *types.Message, index, local int, component replyHandoffComponentID, y, height float64) replyHandoffLayoutRow {
	return replyHandoffLayoutRow{Source: source, SourceIndex: index, LocalRow: local, Component: component, Y: y, Height: height}
}

func TestReplyHandoffFadeOutReflowAndOneIncomingFade(t *testing.T) {
	runtime, handoff := newReplyHandoffTest(t)
	boundary := types.User("human")
	oldReply := types.Agent(types.MessageTypeAssistant, "root", "retire")
	retained := types.Agent(types.MessageTypeAssistant, "root", "keep")
	incoming := types.Agent(types.MessageTypeAssistant, "root", "incoming")
	incomingPart := types.Agent(types.MessageTypeAssistant, "root", "part")
	exchange := replyHandoffExchangeID{Boundary: boundary, Generation: 9}
	retiringID := responseHandoffComponent(exchange, 1, oldReply)
	keptID := responseHandoffComponent(exchange, 2, retained)
	incomingID := responseHandoffComponent(exchange, 3, incoming)
	incomingPartID := responseHandoffComponent(exchange, 3, incomingPart)
	old := []replyHandoffLayoutRow{
		handoffRow(oldReply, 4, 0, retiringID, 0, 2),
		handoffRow(retained, 5, 0, keptID, 2, 2),
	}
	target := []replyHandoffLayoutRow{
		handoffRow(retained, 6, 0, keptID, 0, 2),
		handoffRow(incoming, 7, 0, incomingID, 2, 1),
		handoffRow(incomingPart, 8, 1, incomingPartID, 3, 1),
	}
	cmd := handoff.Begin(old, target, []replyHandoffComponentID{retiringID}, &incomingID)
	require.Nil(t, cmd)
	require.Equal(t, replyHandoffFadeOut, handoff.Phase())
	require.EqualValues(t, 1, runtime.ActiveCount())
	fadeStart := handoff.Rows()
	require.Len(t, fadeStart, 2)
	require.InDelta(t, 0.0, fadeStart[0].Y, 1e-9)
	require.InDelta(t, 2.0, fadeStart[0].Height, 1e-9)
	require.InDelta(t, 1.0, fadeStart[0].Opacity, 1e-9)
	require.NotContains(t, sourceMessages(fadeStart), incoming, "incoming source stays concealed before its component fade")

	advanceReplyHandoffTicks(t, runtime, handoff, 3)
	fading := handoff.Rows()
	require.Equal(t, replyHandoffFadeOut, handoff.Phase())
	require.Less(t, opacityForSource(fading, oldReply), 1.0)
	require.Greater(t, opacityForSource(fading, oldReply), 0.0)
	require.InDelta(t, 2.0, yForSource(fading, retained), 1e-9, "retained text stays in its old position during fade-out")

	finishReplyHandoffPhase(t, runtime, handoff)
	require.Equal(t, replyHandoffReflow, handoff.Phase())
	var intermediate []float64
	for range 4 {
		tickReplyHandoff(t, runtime, handoff)
		intermediate = append(intermediate, yForSource(handoff.Rows(), retained))
	}
	require.True(t, strictlyMonotonicToward(intermediate, 2, 0), "retained source follows several real intermediate positions: %v", intermediate)
	for _, row := range handoff.Rows() {
		if row.Spacer {
			require.Nil(t, row.Source)
			require.False(t, row.Interactive)
		}
	}
	var spacerY []float64
	for _, row := range handoff.Rows() {
		if row.Spacer && row.Component == incomingID {
			require.InDelta(t, 0.0, row.Opacity, 1e-9)
			require.False(t, row.Interactive)
			spacerY = append(spacerY, row.Y)
		}
	}
	require.Equal(t, []float64{2, 3}, spacerY, "each incoming source row reserves its own final destination slot without flashing")

	finishReplyHandoffPhase(t, runtime, handoff)
	require.Equal(t, replyHandoffFadeIn, handoff.Phase())
	require.InDelta(t, 0.0, opacityForSource(handoff.Rows(), incoming), 1e-9, "incoming component begins at zero opacity")
	handoff.UpdateTarget([]replyHandoffLayoutRow{
		handoffRow(retained, 6, 0, keptID, 0, 2),
		handoffRow(incoming, 7, 0, incomingID, 2, 2),
		handoffRow(incomingPart, 8, 1, incomingPartID, 4, 1),
	})
	incoming.Content += " more streamed text"
	before := runtime.Now()
	deadline := before + handoff.transition.Duration() - handoff.transition.Elapsed()
	for range 100 {
		incoming.Content += " token"
		if handoff.Active() {
			tickReplyHandoff(t, runtime, handoff)
			rows := handoff.Rows()
			require.Greater(t, opacityForSource(rows, incoming), 0.0)
			require.LessOrEqual(t, opacityForSource(rows, incoming), 1.0)
			require.InDelta(t, opacityForSource(rows, incoming), opacityForSource(rows, incomingPart), 1e-9, "all source parts in one component share opacity")
		}
		require.Contains(t, incoming.Content, "token", "every chunk mutates the authoritative source immediately")
	}
	require.Equal(t, 100, strings.Count(incoming.Content, "token"), "all chunks are ingested without buffering")
	require.False(t, handoff.Active(), "fade ends despite continued token appends")
	require.GreaterOrEqual(t, runtime.Now(), deadline)
	require.LessOrEqual(t, runtime.Now(), deadline+animation.TickRate, "stream appends never restart or extend the fixed deadline")
	require.Equal(t, int32(0), runtime.ActiveCount(), "finite phase chain releases its lease")
	require.InDelta(t, 1.0, opacityForSource(handoff.Rows(), incoming), 1e-9, "settled streaming source is full opacity")
}

func TestReplyHandoffWithoutRetirementStartsIncomingFadeImmediately(t *testing.T) {
	runtime, handoff := newReplyHandoffTest(t)
	boundary := types.User("human")
	incoming := types.Agent(types.MessageTypeAssistant, "root", "reply")
	component := responseHandoffComponent(replyHandoffExchangeID{Boundary: boundary}, 2, incoming)
	cmd := handoff.Begin(nil, []replyHandoffLayoutRow{handoffRow(incoming, 0, 0, component, 0, 1)}, nil, &component)
	require.Nil(t, cmd)
	require.Equal(t, replyHandoffFadeIn, handoff.Phase())
	require.InDelta(t, 0.0, opacityForSource(handoff.Rows(), incoming), 1e-9)
	require.EqualValues(t, 1, runtime.ActiveCount())
	finishReplyHandoffPhase(t, runtime, handoff)
	require.False(t, handoff.Active())
	require.Zero(t, runtime.ActiveCount())
}

func TestReplyHandoffUpdatesGrowingTargetWithoutRestartAndQueuesEviction(t *testing.T) {
	runtime, handoff := newReplyHandoffTest(t)
	boundary := types.User("exchange")
	first := types.Agent(types.MessageTypeAssistant, "root", "first")
	second := types.Agent(types.MessageTypeAssistant, "root", "second")
	third := types.Agent(types.MessageTypeAssistant, "root", "third")
	exchange := replyHandoffExchangeID{Boundary: boundary, Generation: 2}
	firstID := responseHandoffComponent(exchange, 1, first)
	secondID := responseHandoffComponent(exchange, 2, second)
	thirdID := responseHandoffComponent(exchange, 3, third)
	initial := []replyHandoffLayoutRow{handoffRow(first, 0, 0, firstID, 0, 1), handoffRow(second, 1, 0, secondID, 1, 1)}
	handoff.Begin(initial, initial, nil, &secondID)
	require.Equal(t, replyHandoffFadeIn, handoff.Phase())
	start := runtime.Now()
	for range 4 {
		second.Content += " chunk"
		handoff.UpdateTarget([]replyHandoffLayoutRow{
			handoffRow(first, 0, 0, firstID, 0, 1),
			handoffRow(second, 1, 0, secondID, 1, 2),
		})
		tickReplyHandoff(t, runtime, handoff)
		require.InDelta(t, 1.0, opacityForSource(handoff.Rows(), first), 1e-9, "existing retained prose is not faded")
		require.Greater(t, opacityForSource(handoff.Rows(), second), 0.0)
	}
	require.Equal(t, start+4*animation.TickRate, runtime.Now())
	newTarget := []replyHandoffLayoutRow{handoffRow(second, 1, 0, secondID, 0, 2), handoffRow(third, 2, 0, thirdID, 2, 1)}
	handoff.Begin(initial, newTarget, []replyHandoffComponentID{firstID}, &thirdID)
	require.Equal(t, replyHandoffFadeIn, handoff.Phase(), "queued eviction cannot restart the currently active fade")
	finishReplyHandoffPhase(t, runtime, handoff)
	require.Equal(t, replyHandoffFadeOut, handoff.Phase(), "coalesced target runs as a later bounded batch")
	finishReplyHandoffPhase(t, runtime, handoff)
	require.Equal(t, replyHandoffReflow, handoff.Phase())
	finishReplyHandoffPhase(t, runtime, handoff)
	require.Equal(t, replyHandoffFadeIn, handoff.Phase())
	finishReplyHandoffPhase(t, runtime, handoff)
	require.False(t, handoff.Active())
	require.Equal(t, int32(0), runtime.ActiveCount())
	require.Contains(t, sourceMessages(handoff.Rows()), second)
	require.Contains(t, sourceMessages(handoff.Rows()), third)
}

func TestReplyHandoffSettingsReflowConsumesQueuedIncomingBatch(t *testing.T) {
	runtime, handoff := newReplyHandoffTest(t)
	boundary := types.User("human")
	oldReply := types.Agent(types.MessageTypeAssistant, "root", "old")
	newReply := types.Agent(types.MessageTypeAssistant, "root", "new")
	exchange := replyHandoffExchangeID{Boundary: boundary}
	oldID := responseHandoffComponent(exchange, 1, oldReply)
	newID := responseHandoffComponent(exchange, 2, newReply)
	old := []replyHandoffLayoutRow{handoffRow(oldReply, 0, 0, oldID, 0, 1)}
	settingsTarget := []replyHandoffLayoutRow{}
	handoff.Begin(old, settingsTarget, []replyHandoffComponentID{oldID}, nil)
	require.Equal(t, replyHandoffFadeOut, handoff.Phase())
	queuedTarget := []replyHandoffLayoutRow{handoffRow(newReply, 1, 0, newID, 0, 1)}
	handoff.Begin(old, queuedTarget, nil, &newID)
	finishReplyHandoffPhase(t, runtime, handoff)
	require.Equal(t, replyHandoffReflow, handoff.Phase())
	finishReplyHandoffPhase(t, runtime, handoff)
	require.Equal(t, replyHandoffFadeIn, handoff.Phase(), "queued incoming begins after settings-only retirement reflow")
	require.InDelta(t, 0.0, opacityForSource(handoff.Rows(), newReply), 1e-9)
	finishReplyHandoffPhase(t, runtime, handoff)
	require.False(t, handoff.Active())
	require.InDelta(t, 1.0, opacityForSource(handoff.Rows(), newReply), 1e-9)
	require.Zero(t, runtime.ActiveCount())
}

func TestReplyHandoffReflowMovesRetiringSourceOnceIntoExpandedWork(t *testing.T) {
	runtime, handoff := newReplyHandoffTest(t)
	boundary := types.User("human")
	retiring := types.Agent(types.MessageTypeAssistant, "root", "older prose")
	retained := types.Agent(types.MessageTypeAssistant, "root", "latest")
	exchange := replyHandoffExchangeID{Boundary: boundary}
	retiringID := responseHandoffComponent(exchange, 1, retiring)
	retainedID := responseHandoffComponent(exchange, 2, retained)
	old := []replyHandoffLayoutRow{handoffRow(retiring, 0, 0, retiringID, 0, 2), handoffRow(retained, 1, 0, retainedID, 2, 1)}
	target := []replyHandoffLayoutRow{handoffRow(retiring, 2, 0, retiringID, 4, 2), handoffRow(retained, 1, 0, retainedID, 0, 1)}
	handoff.Begin(old, target, []replyHandoffComponentID{retiringID}, nil)
	finishReplyHandoffPhase(t, runtime, handoff)
	require.Equal(t, replyHandoffReflow, handoff.Phase())
	rows := handoff.Rows()
	count := 0
	var migrated replyHandoffFrameRow
	for _, row := range rows {
		if row.Source != retiring {
			continue
		}
		count++
		migrated = row
		require.False(t, row.Spacer)
		require.False(t, row.Interactive)
		require.InDelta(t, 0.0, row.Opacity, 1e-9, "retiring content does not flash at reflow start")
	}
	require.Equal(t, 1, count, "a migrated source has one paint/hit row, not an old and Work copy")
	require.InDelta(t, 0.0, migrated.Y, 1e-9)
	tickReplyHandoff(t, runtime, handoff)
	rows = handoff.Rows()
	require.Greater(t, yForSource(rows, retiring), 0.0, "migrated source follows its actual Work destination")
	require.Greater(t, opacityForSource(rows, retiring), 0.0)
	require.True(t, interactiveForSource(rows, retiring))
	require.Equal(t, 1, countSource(rows, retiring), "moving source is never painted or hittable twice")
}

func TestReplyHandoffComponentIdentityUsesStableSourceFallback(t *testing.T) {
	exchange := replyHandoffExchangeID{Orphan: 17, Generation: 3}
	first := types.Agent(types.MessageTypeAssistant, "root", "one")
	second := types.Agent(types.MessageTypeAssistant, "root", "one")
	require.NotSame(t, responseHandoffComponent(exchange, 0, first).Source, responseHandoffComponent(exchange, 0, second).Source, "equal text and sender do not imply group identity")
	require.Equal(t, responseHandoffComponent(exchange, 9, first), responseHandoffComponent(exchange, 9, second), "explicit group ID survives source-part changes")
}

func sourceMessages(rows []replyHandoffFrameRow) []*types.Message {
	var messages []*types.Message
	for _, row := range rows {
		if row.Source != nil {
			messages = append(messages, row.Source)
		}
	}
	return messages
}

func countSource(rows []replyHandoffFrameRow, source *types.Message) int {
	count := 0
	for _, row := range rows {
		if row.Source == source {
			count++
		}
	}
	return count
}

func interactiveForSource(rows []replyHandoffFrameRow, source *types.Message) bool {
	for _, row := range rows {
		if row.Source == source {
			return row.Interactive
		}
	}
	return false
}

func opacityForSource(rows []replyHandoffFrameRow, source *types.Message) float64 {
	for _, row := range rows {
		if row.Source == source {
			return row.Opacity
		}
	}
	return -1
}

func yForSource(rows []replyHandoffFrameRow, source *types.Message) float64 {
	for _, row := range rows {
		if row.Source == source {
			return row.Y
		}
	}
	return -1
}

func strictlyMonotonicToward(values []float64, from, to float64) bool {
	if len(values) < 2 {
		return false
	}
	previous := from
	for _, value := range values {
		if to < from && (value >= previous || value <= to) {
			return false
		}
		if to > from && (value <= previous || value >= to) {
			return false
		}
		previous = value
	}
	return true
}

func TestReplyHandoffBurstFadesEveryNewVisibleComponentAndDropsStaleRows(t *testing.T) {
	ar, h := newReplyHandoffTest(t)
	first, second, third := types.Agent(types.MessageTypeAssistant, "root", "first"), types.Agent(types.MessageTypeAssistant, "root", "second"), types.Agent(types.MessageTypeAssistant, "root", "third")
	exchange := replyHandoffExchangeID{Orphan: 1}
	firstID := responseHandoffComponent(exchange, 1, first)
	secondID := responseHandoffComponent(exchange, 2, second)
	thirdID := responseHandoffComponent(exchange, 3, third)
	firstRow := handoffRow(first, 0, 0, firstID, 0, 1)
	secondRow := handoffRow(second, 1, 0, secondID, 1, 1)
	thirdRow := handoffRow(third, 2, 0, thirdID, 2, 1)
	h.Begin(nil, []replyHandoffLayoutRow{firstRow}, nil, &firstID)
	h.Begin(nil, []replyHandoffLayoutRow{firstRow, secondRow}, nil, &secondID)
	h.Begin(nil, []replyHandoffLayoutRow{secondRow, thirdRow}, []replyHandoffComponentID{firstID}, &thirdID)
	finishReplyHandoffPhase(t, ar, h)
	finishReplyHandoffPhase(t, ar, h)
	finishReplyHandoffPhase(t, ar, h)
	require.Equal(t, replyHandoffFadeIn, h.Phase())
	require.InDelta(t, 0, opacityForSource(h.Rows(), second), 1e-9)
	require.InDelta(t, 0, opacityForSource(h.Rows(), third), 1e-9)
	finishReplyHandoffPhase(t, ar, h)
	require.NotContains(t, sourceMessages(h.Rows()), first, "latest target does not resurrect a retired source")
	require.InDelta(t, 1, opacityForSource(h.Rows(), second), 1e-9)
	require.InDelta(t, 1, opacityForSource(h.Rows(), third), 1e-9)
	require.Zero(t, ar.ActiveCount())
}
