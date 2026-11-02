package messages

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// vtstAppendRenderBound is the O(1) allowance for one append: the new item and
// at most one neighbour whose separator or height may change. Items below the
// viewport are never rendered, so the reader's frame costs nothing.
const vtstAppendRenderBound = 2

// vtstFrameTop returns the first rows of the frame with the last two columns
// removed. The scrollbar gutter tracks total height, so it is not frame content.
func vtstFrameTop(m *model, rows int) string {
	lines := strings.Split(ansi.Strip(m.View()), "\n")
	out := make([]string, 0, min(rows, len(lines)))
	for _, line := range lines[:min(rows, len(lines))] {
		out = append(out, ansi.Truncate(line, max(0, ansi.StringWidth(line)-2), ""))
	}
	return strings.Join(out, "\n")
}

// vtstCounts is a snapshot of the work counters a single step is charged with.
type vtstCounts struct {
	renders, rebuilds, visits, facade uint64
}

func vtstSnap(m *model) vtstCounts {
	s := m.layoutWorkStats()
	return vtstCounts{renders: vprlRenders(m), rebuilds: s.slotRebuilds, visits: s.itemVisits, facade: s.facadeEntries}
}

func (c vtstCounts) since(prev vtstCounts) vtstCounts {
	return vtstCounts{renders: c.renders - prev.renders, rebuilds: c.rebuilds - prev.rebuilds, visits: c.visits - prev.visits, facade: c.facade - prev.facade}
}

// vtstWheelUpTo scrolls up with real wheel messages until the offset is at or
// above target, which sets userHasScrolled exactly as a user does.
func vtstWheelUpTo(t *testing.T, m *model, target int) {
	t.Helper()
	for range 100_000 {
		if m.scrollOffset <= target {
			break
		}
		_, _ = m.Update(messages.WheelCoalescedMsg{X: 5, Y: 5, Delta: -1})
	}
	require.True(t, m.userHasScrolled, "a wheel-up must set userHasScrolled")
	require.LessOrEqual(t, m.scrollOffset, target)
}

// vtstAppendScenario loads n real items, optionally resizes, scrolls the reader
// to mid-history with the wheel, then appends new messages and streamed chunks
// at the bottom. Each append must cost O(1) and leave the reader's frame alone.
func vtstAppendScenario(t *testing.T, n, resizeTo int) {
	t.Helper()
	m := vprlLoad(t, 120, vprlRealSession(t, n))
	t.Cleanup(m.StopAnimations)
	_ = m.View()
	if resizeTo > 0 {
		m.SetSize(resizeTo, 40)
		_ = m.View()
	}
	vtstWheelUpTo(t, m, m.totalScrollableHeight()/2)
	_ = m.View()
	require.True(t, m.userHasScrolled)

	offset := m.scrollOffset
	top := vtstFrameTop(m, 12)
	require.NotEmpty(t, strings.TrimSpace(top))

	step := func(name string, act func()) {
		t.Helper()
		before := vtstSnap(m)
		act()
		_ = m.View()
		cost := vtstSnap(m).since(before)
		t.Logf("%s messages=%d renders=%d itemVisits=%d slotRebuilds=%d facadeEntries=%d", name, n, cost.renders, cost.visits, cost.rebuilds, cost.facade)
		require.LessOrEqual(t, cost.renders, uint64(vtstAppendRenderBound), "%s renders O(1) items", name)
		require.LessOrEqual(t, cost.visits, uint64(vtstAppendRenderBound), "%s measures O(1) slots", name)
		require.Zero(t, cost.rebuilds, "%s needs no full slot index rebuild", name)
		require.Equal(t, offset, m.scrollOffset, "%s must not move the reader", name)
		require.Equal(t, top, vtstFrameTop(m, 12), "%s must not change the reader's visible frame", name)
	}

	for k := range 3 {
		step(fmt.Sprintf("append-user-%d", k), func() {
			m.AddUserMessage(fmt.Sprintf("appended prompt %d while the reader is scrolled up", k))
		})
	}
	// The first chunk of a new reply creates its message; later chunks extend it.
	for k := range 5 {
		step(fmt.Sprintf("stream-chunk-%d", k), func() {
			m.AppendToLastMessage("root", fmt.Sprintf("streamed chunk %d of the live reply\n", k))
		})
	}
}

func TestLayoutAppendKeepsScrolledReaderFrame1000(t *testing.T) {
	vtstAppendScenario(t, 1000, 0)
}

func TestLayoutAppendKeepsScrolledReaderFrameAfterResize6000(t *testing.T) {
	vtstAppendScenario(t, 6000, 90)
}

// vtstWorkSession returns exactly n session items in four-item cycles: a user
// prompt, a shell tool call, its result and an assistant answer. Each cycle is
// one work exchange when compact Work is on.
func vtstWorkSession(t *testing.T, n int) *session.Session {
	t.Helper()
	sess := session.New(session.WithID("vtst-work"))
	for i := range n {
		id := strconv.Itoa(i)
		var item session.Item
		switch i % 4 {
		case 0:
			item = session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "work prompt " + id}})
		case 1:
			call := tools.ToolCall{ID: "call-" + id, Type: "function", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"go test ./..."}`}}
			item = session.NewMessageItem(&session.Message{AgentName: "root", Message: chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call}}})
		case 2:
			item = session.NewMessageItem(&session.Message{AgentName: "root", Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: "call-" + strconv.Itoa(i-1), Content: "ok  \tbilling/ledger\t0.42s\n"}})
		default:
			item = session.NewMessageItem(&session.Message{AgentName: "root", Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "Done with item " + id + "."}})
		}
		sess.Messages = append(sess.Messages, item)
	}
	require.Len(t, sess.Messages, n)
	return sess
}

// vtstLoadWork loads sess with compact Work set before the replay, so every
// slot is built in that mode from the start.
func vtstLoadWork(t *testing.T, sess *session.Session, compact bool) *model {
	t.Helper()
	m := NewScrollableView(animation.NewRuntime(), 120, 40, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	_ = m.SetCompactWorkLog(compact)
	BeginReplay(m, PrepareReplay(sess), nil)
	drainRetainedReplay(t, m)
	finishHistoryReplay(t, m)
	_ = m.View()
	return m
}

// vtstAccountingPath is the optional markdown report. It is written only when
// VTST_SCRATCH names a directory, so a plain run writes nothing.
func vtstAccountingPath() string {
	dir := os.Getenv("VTST_SCRATCH")
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "compact-accounting.md")
}

// TestLayoutCompactWorkToggleAccounting toggles SetCompactWorkLog on a loaded
// work transcript and records what the toggle costs: renders, measured slots,
// full slot rebuilds, and the work slots present before the toggle. Renders are
// checked before any explicit demand. Geometry is then compared after an explicit
// full materialization on both models, because initial load is lazy and offscreen
// slots hold placeholder estimates until demanded.
func TestLayoutCompactWorkToggleAccounting(t *testing.T) {
	var report strings.Builder
	report.WriteString("# Compact Work toggle accounting\n\n")
	report.WriteString("Fixture: exact session of N items in four-item cycles (user prompt, shell tool call, tool result, answer). The tool result folds into its call, so N views = 3 per cycle. Width 120, height 40, reader at bottom. Each row loads in the opposite Work mode and toggles once.\n\n")
	report.WriteString("Counts are taken before any explicit demand. Totals and frames are compared only after ensureAllItemsRendered on both models.\n\n")
	report.WriteString("| session items | views (N) | toggle | renders | itemVisits | slotRebuilds | slotLoops | work anchors before toggle | hidden work children before toggle | visible slots | total height after demand | fresh total after demand |\n")
	report.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	t.Cleanup(func() {
		if path := vtstAccountingPath(); path != "" {
			require.NoError(t, os.WriteFile(path, []byte(report.String()), 0o644))
		}
	})
	for _, n := range []int{1000, 6000} {
		t.Run(fmt.Sprintf("messages=%d", n), func(t *testing.T) {
			sess := vtstWorkSession(t, n)
			for _, enable := range []bool{true, false} {
				name := map[bool]string{true: "compact on", false: "compact off"}[enable]
				t.Run(name, func(t *testing.T) {
					m := vtstLoadWork(t, sess, !enable)
					var anchors, hidden int
					for _, block := range m.workHeaderBySource {
						if block != nil {
							anchors++
						}
					}
					for source, anchor := range m.workSourceToHeader {
						if source != anchor {
							hidden++
						}
					}
					before := m.layoutWorkStats()
					renders := vprlRenders(m)
					_ = m.SetCompactWorkLog(enable)
					_ = m.View()
					after := m.layoutWorkStats()
					cost := vtstCounts{renders: vprlRenders(m) - renders, rebuilds: after.slotRebuilds - before.slotRebuilds, visits: after.itemVisits - before.itemVisits}
					slotLoops := after.slotLoops - before.slotLoops
					visible := m.visibleSlotCount()
					require.LessOrEqual(t, cost.renders, uint64(visible), "renders only visible slots (before any demand)")
					require.LessOrEqual(t, cost.rebuilds, uint64(1), "rebuilds the slot index at most once")

					// Explicit full demand on both models, then compare geometry and frame.
					// Both readers are pinned to the bottom after demand: the lazy load and the
					// toggle leave the raw offset at different rows, so the anchor is compared
					// at a common position.
					m.ensureAllItemsRendered()
					_ = m.scrollToBottom()
					_ = m.View()
					fresh := vtstLoadWork(t, sess, enable)
					fresh.ensureAllItemsRendered()
					_ = fresh.scrollToBottom()
					_ = fresh.View()
					t.Logf("toggle=%s messages=%d views=%d renders=%d itemVisits=%d slotRebuilds=%d slotLoops=%d anchorsBefore=%d hiddenBefore=%d visible=%d totalAfterDemand=%d freshAfterDemand=%d",
						name, n, len(m.views), cost.renders, cost.visits, cost.rebuilds, slotLoops, anchors, hidden, visible, m.totalHeight, fresh.totalHeight)
					fmt.Fprintf(&report, "| %d | %d | %s | %d | %d | %d | %d | %d | %d | %d | %d | %d |\n",
						n, len(m.views), name, cost.renders, cost.visits, cost.rebuilds, slotLoops, anchors, hidden, visible, m.totalHeight, fresh.totalHeight)
					require.Equal(t, fresh.totalHeight, m.totalHeight, "total height after full demand must match a fresh load in the same mode")
					require.Equal(t, fresh.scrollOffset, m.scrollOffset, "reader anchor must match a fresh load in the same mode")
					require.Equal(t, vtstFrameTop(fresh, m.height), vtstFrameTop(m, m.height), "visible frame must match a fresh load in the same mode")
				})
			}
		})
	}
}
