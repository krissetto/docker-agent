package messages

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

const themeScrollHeight = 24

// themeScrollFixture builds a work-heavy compact transcript of n items. Each turn
// is a user prompt, two completed tool calls (one Work block) and an answer; every
// seventh answer is long. n must be a multiple of four.
func themeScrollFixture(t *testing.T, n int) *model {
	t.Helper()
	return themeScrollFixtureOn(t, animation.NewRuntime(), n)
}

// themeScrollFixtureOn is themeScrollFixture on a given animation runtime, so a
// test can drive disclosure ticks deterministically.
func themeScrollFixtureOn(t *testing.T, ar *animation.Runtime, n int) *model {
	t.Helper()
	require.Zero(t, n%4)
	m := NewScrollableView(ar, 100, themeScrollHeight, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	m.SetCompactWorkLog(true)
	long := strings.Repeat("word ", 300) + "end"
	for turn := range n / 4 {
		m.addMessage(types.User(fmt.Sprintf("request %d", turn)))
		for k := range 2 {
			call := tools.ToolCall{ID: fmt.Sprintf("call-%d-%d", turn, k), Function: tools.FunctionCall{Name: "shell", Arguments: fmt.Sprintf(`{"cmd":"step %d"}`, turn)}}
			m.addMessage(types.ToolCallMessage("root", call, tools.Tool{Name: "shell"}, types.ToolStatusCompleted))
		}
		body := fmt.Sprintf("answer %d", turn)
		if turn%7 == 6 {
			body = fmt.Sprintf("## answer %d\n\n%s", turn, long)
		}
		m.addMessage(types.Agent(types.MessageTypeAssistant, "root", body))
	}
	require.Len(t, m.messages, n)
	_ = m.View()
	return m
}

// themeScrollPlace puts the reader at top, middle or bottom with the real
// scroll state a user would have.
func themeScrollPlace(t *testing.T, m *model, pos string) {
	t.Helper()
	switch pos {
	case "top":
		m.userHasScrolled = true
		m.setScrollOffset(0)
	case "middle":
		m.userHasScrolled = true
		m.setScrollOffset(m.layoutTotal() / 2)
	default:
		_ = m.scrollToBottom()
	}
	_ = m.View()
}

// themeScrollCost is the work one event performed, as counter deltas, and the
// slots whose lines its View produced again.
type themeScrollCost struct {
	renders, blockViews, viewCacheInvalidations, projectionRebuilds, itemVisits, sizeCalls uint64
	anchorLookups, exchangeVisits                                                          uint64
	rendered                                                                               []int        // slots whose lines were produced again, ascending
	allowed                                                                                map[int]bool // slots on screen, plus the anchor of every on-screen child
	bound                                                                                  int          // allowed slots plus the children of their anchors
	blocks                                                                                 int          // allowed slots that are Work block anchors
}

type themeScrollSnap struct {
	renders uint64
	stats   layoutStats
}

func themeScrollSnapshot(m *model) themeScrollSnap {
	return themeScrollSnap{renders: vprlRenders(m), stats: m.layoutWorkStats()}
}

// themeScrollLines maps each materialized slot to the address of its first line.
// A slot whose lines are produced again gets a new backing array, so its address
// changes.
func themeScrollLines(m *model) map[int]*string {
	out := make(map[int]*string, len(m.renderedItems)+len(m.uncached))
	for i, e := range m.renderedItems {
		if len(e.lines) > 0 {
			out[i] = &e.lines[0]
		}
	}
	for i, e := range m.uncached {
		if len(e.lines) > 0 {
			out[i] = &e.lines[0]
		}
	}
	return out
}

// themeScrollViewport returns the slots the viewport may render. A slot is on
// screen when its rows overlap the viewport; a hidden child is on screen when its
// own row is, and then its block anchor is the required wrapper. No margin is
// added: the set follows the viewport at the time.
func themeScrollViewport(m *model) map[int]bool {
	allowed := make(map[int]bool)
	top, bottom := m.scrollOffset, m.scrollOffset+m.height
	for i := range m.views {
		if m.hiddenSlot(i) {
			if anchor, ok := m.workSourceToHeader[i]; ok {
				if y := m.slotOffset(i); y >= top && y < bottom {
					allowed[anchor] = true
				}
			}
			continue
		}
		start, end := m.slotOffset(i), m.displayEnd(i)
		if end > start && end > top && start < bottom {
			allowed[i] = true
		}
	}
	return allowed
}

// themeScrollMeasure runs one event and the View that follows it. The viewport is
// taken before the event (the settled state the event starts from) and after the
// View, and both sets are allowed.
func themeScrollMeasure(m *model, event func()) themeScrollCost {
	beforeAllowed := themeScrollViewport(m)
	beforeLines := themeScrollLines(m)
	before := themeScrollSnapshot(m)
	event()
	_ = m.View()
	after := themeScrollSnapshot(m)
	afterAllowed := themeScrollViewport(m)
	afterLines := themeScrollLines(m)

	allowed := beforeAllowed
	for i := range afterAllowed {
		allowed[i] = true
	}
	bound, blocks := len(allowed), 0
	for child, anchor := range m.workSourceToHeader {
		if child != anchor && allowed[anchor] {
			bound++
		}
	}
	for i := range allowed {
		if m.workHeaderBySource[i] != nil {
			blocks++
		}
	}
	var rendered []int
	for i, lines := range afterLines {
		if old, ok := beforeLines[i]; !ok || old != lines {
			rendered = append(rendered, i)
		}
	}
	slices.Sort(rendered)
	return themeScrollCost{
		renders:                after.renders - before.renders,
		blockViews:             after.stats.blockViews - before.stats.blockViews,
		viewCacheInvalidations: after.stats.viewCacheInvalidations - before.stats.viewCacheInvalidations,
		projectionRebuilds:     after.stats.projectionRebuilds - before.stats.projectionRebuilds,
		itemVisits:             after.stats.itemVisits - before.stats.itemVisits,
		sizeCalls:              after.stats.sizeCalls - before.stats.sizeCalls,
		anchorLookups:          after.stats.anchorLookups - before.stats.anchorLookups,
		exchangeVisits:         after.stats.exchangeVisits - before.stats.exchangeVisits,
		rendered:               rendered,
		allowed:                allowed,
		bound:                  bound,
		blocks:                 blocks,
	}
}

// themeScrollEvents runs the three events on a loaded transcript: a theme change,
// a resize, and an isolated real wheel step after the resize.
func themeScrollEvents(t *testing.T, n int, pos string) map[string]themeScrollCost {
	t.Helper()
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	m := themeScrollFixture(t, n)
	themeScrollPlace(t, m, pos)
	out := map[string]themeScrollCost{}
	out["theme"] = themeScrollMeasure(m, func() {
		styles.ApplyTheme(vprlTheme(original))
		_, _ = m.Update(messages.ThemeChangedMsg{})
	})
	out["resize"] = themeScrollMeasure(m, func() { m.SetSize(60, themeScrollHeight) })
	delta := -1
	if pos == "top" {
		delta = 1
	}
	out["scroll"] = themeScrollMeasure(m, func() {
		_, _ = m.Update(messages.WheelCoalescedMsg{X: 5, Y: 5, Delta: delta})
	})
	return out
}

// TestThemeScrollRendersOnlyTheViewport runs theme, width and scroll events on 1000
// and 6000 work-heavy items at the top, middle and bottom. What the event produces
// again must be on screen or a required block anchor, and every counter is bounded
// by what is on screen. Nothing is compared across history sizes: the bound is a
// function of the viewport alone.
func TestThemeScrollRendersOnlyTheViewport(t *testing.T) {
	for _, pos := range []string{"top", "middle", "bottom"} {
		t.Run(pos, func(t *testing.T) {
			for _, n := range []int{1000, 6000} {
				t.Run(fmt.Sprintf("items=%d", n), func(t *testing.T) {
					results := themeScrollEvents(t, n, pos)
					for _, event := range []string{"theme", "resize", "scroll"} {
						cost := results[event]
						for _, i := range cost.rendered {
							require.True(t, cost.allowed[i], "%s %s produced slot %d, which is neither on screen nor a block anchor of an on-screen child", pos, event, i)
						}
						require.LessOrEqual(t, len(cost.rendered), cost.bound, "%s %s rendered slots are bounded by the viewport", pos, event)
						require.LessOrEqual(t, cost.renders, uint64(cost.bound), "%s %s message renders are bounded by the viewport", pos, event)
						require.LessOrEqual(t, cost.anchorLookups, uint64(cost.blocks), "%s %s anchor lookups are bounded by on-screen blocks", pos, event)
						require.LessOrEqual(t, cost.exchangeVisits, cost.anchorLookups, "%s %s: a lookup touches at most one exchange", pos, event)
						t.Logf("%s %-7s n=%-5d rendered=%d bound=%d blocks=%d renders=%d anchorLookups=%d exchangeVisits=%d blockViews=%d",
							pos, event, n, len(cost.rendered), cost.bound, cost.blocks, cost.renders, cost.anchorLookups, cost.exchangeVisits, cost.blockViews)
					}
					require.NotEmpty(t, results["theme"].rendered, "a theme change restyles on-screen content")
					require.NotEmpty(t, results["resize"].rendered, "a resize re-lays out on-screen content")
				})
			}
		})
	}
}

// TestWorkItemLookupIndexIsStructuralOnly: width, theme and scroll keep the lookup
// index as it is; a structural change rebuilds it.
func TestWorkItemLookupIndexIsStructuralOnly(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	m := themeScrollFixture(t, 1000)
	themeScrollPlace(t, m, "bottom")
	index := reflect.ValueOf(m.workExchangeBySource).Pointer()
	require.NotZero(t, index, "the projection has an index")

	styles.ApplyTheme(vprlTheme(original))
	_, _ = m.Update(messages.ThemeChangedMsg{})
	_ = m.View()
	m.SetSize(60, themeScrollHeight)
	_ = m.View()
	_, _ = m.Update(messages.WheelCoalescedMsg{X: 5, Y: 5, Delta: -1})
	_ = m.View()
	require.Equal(t, index, reflect.ValueOf(m.workExchangeBySource).Pointer(), "width, theme and scroll keep the index")

	m.addMessage(types.User("one more request"))
	_ = m.View()
	require.NotEqual(t, index, reflect.ValueOf(m.workExchangeBySource).Pointer(), "a structural change rebuilds the index")
}

// scanWorkItemsForAnchor is the scan the index replaced: the first exchange whose
// first visible source is anchor, with its visible sources in order.
func scanWorkItemsForAnchor(m *model, anchor int) []int {
	if !m.workEnabled {
		return nil
	}
	for _, exchange := range m.workProjection.exchanges {
		visibleAnchor := -1
		for _, source := range exchange.work {
			if source >= m.historyStart {
				visibleAnchor = source
				break
			}
		}
		if visibleAnchor != anchor {
			continue
		}
		var out []int
		for _, source := range exchange.work {
			if source >= m.historyStart && source >= 0 && source < len(m.views) {
				out = append(out, source)
			}
		}
		return out
	}
	return nil
}

// TestWorkItemLookupMatchesScanOracle compares the index with the scan it replaced,
// for every slot and several history starts, including starts that hide an anchor
// without any projection sync (history paging does not dirty the projection).
func TestWorkItemLookupMatchesScanOracle(t *testing.T) {
	m := themeScrollFixture(t, 200)
	saved := m.historyStart
	t.Cleanup(func() { m.historyStart = saved })
	for _, start := range []int{0, 1, 2, 3, 17, 150, 197} {
		m.historyStart = start
		for anchor := -1; anchor <= len(m.views); anchor++ {
			var got []int
			for _, item := range m.workItemsForAnchor(anchor) {
				got = append(got, item.SourceIndex)
			}
			want := scanWorkItemsForAnchor(m, anchor)
			require.True(t, slices.Equal(want, got), "historyStart=%d anchor=%d: want %v got %v", start, anchor, want, got)
		}
	}
}

// themeScrollLastBlock is the anchor of the Work block of the last turn: its
// first tool call, three items from the end.
func themeScrollLastBlock(m *model) int { return len(m.messages) - 3 }

// themeScrollWorkHeader is the screen row of the last turn's Work block header, or
// -1 when the block is not on screen.
func themeScrollWorkHeader(m *model) int {
	y := m.workRangeStart(themeScrollLastBlock(m)) - m.scrollOffset
	if y < 0 || y >= m.height {
		return -1
	}
	return y
}

// themeScrollLineAt returns the plain-text frame row y.
func themeScrollLineAt(frame string, y int) string {
	lines := strings.Split(ansi.Strip(frame), "\n")
	if y < 0 || y >= len(lines) {
		return ""
	}
	return lines[y]
}

// TestOffscreenWorkBlockUpdatesOnlyWhenShown: a child of a Work block that is far
// above the reader changes while it is offscreen. Nothing renders the block until
// the real wheel brings it on screen, and then the block shows the new state. The
// state is the tool count in the collapsed header, which is on screen without
// expanding the block.
func TestOffscreenWorkBlockUpdatesOnlyWhenShown(t *testing.T) {
	const n = 1000
	m := themeScrollFixture(t, n)
	// Turn 249 is items 996..999: user, two tools, answer. Its block starts at the
	// first tool call, n-3. Its second tool starts running, so the header counts one.
	anchor := n - 3
	require.NotNil(t, m.workHeaderBySource[anchor], "the last turn has a Work block")
	running := tools.ToolCall{ID: "call-249-1", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"step 249"}`}}
	m.AddOrUpdateToolCall("root", running, tools.Tool{Name: "shell"}, types.ToolStatusRunning)
	themeScrollPlace(t, m, "bottom")
	y := themeScrollWorkHeader(m)
	require.GreaterOrEqual(t, y, 0, "the Work block header is on screen at the bottom")
	require.Contains(t, themeScrollLineAt(m.View(), y), "(1 tool)")

	// Scroll up with real wheel steps until the block is two screens above the reader.
	for range 100_000 {
		if m.slotOffset(anchor)+m.geom.Height(anchor)+2*themeScrollHeight <= m.scrollOffset {
			break
		}
		_, _ = m.Update(messages.WheelCoalescedMsg{X: 5, Y: 5, Delta: -1})
	}
	_ = m.View()
	require.False(t, m.materialized(anchor), "a block two screens above the reader is not materialized")

	// The second tool completes while the block is offscreen: no Work block renders.
	before := m.layoutWorkStats()
	m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: "call-249-1", Response: "done", Result: &tools.ToolCallResult{Output: "done"}}, types.ToolStatusCompleted)
	_ = m.View()
	after := m.layoutWorkStats()
	require.Zero(t, after.blockViews-before.blockViews, "an offscreen block change renders no Work block")
	require.False(t, m.materialized(anchor), "an offscreen block change keeps the block unmaterialized")

	// The real wheel brings the block on screen. A wheel step lays out what it
	// reveals, so the render is counted across the steps, not only the final View.
	before = m.layoutWorkStats()
	for range 100_000 {
		if y := themeScrollWorkHeader(m); y >= 0 && y+3 < m.height {
			break
		}
		_, _ = m.Update(messages.WheelCoalescedMsg{X: 5, Y: 5, Delta: 1})
	}
	_ = m.View()
	after = m.layoutWorkStats()
	require.Positive(t, after.blockViews-before.blockViews, "a block that comes on screen is rendered")
	require.True(t, m.materialized(anchor), "a block on screen is materialized")
	y = themeScrollWorkHeader(m)
	require.Contains(t, themeScrollLineAt(m.View(), y), "(2 tools)", "the revealed header shows the completed tool")
}

// TestThemeChangeRestylesVisibleContent: after a theme change the visible frame is
// restyled, with the same text in the same place, and the visible Work block is
// still clickable and expands and collapses on a real click.
func TestThemeChangeRestylesVisibleContent(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	const n = 1000
	m := themeScrollFixture(t, n)
	themeScrollPlace(t, m, "bottom")
	before := m.View()

	styles.ApplyTheme(vprlTheme(original))
	_, _ = m.Update(messages.ThemeChangedMsg{})
	after := m.View()
	require.NotEqual(t, before, after, "a theme change restyles the visible frame")
	require.Equal(t, ansi.Strip(before), ansi.Strip(after), "a theme change keeps the text and layout")
	require.Contains(t, after, "38;2;17;17;17", "visible text carries the new theme's foreground")

	// A visible Work block still toggles on a real click after the theme change.
	anchor := n - 3
	y := themeScrollWorkHeader(m)
	require.GreaterOrEqual(t, y, 0, "the Work block header is on screen")
	wasExpanded := m.workHeaderBySource[anchor].IsExpanded()
	_, _ = m.handleMouseClick(tea.MouseClickMsg{X: 4, Y: y, Button: tea.MouseLeft})
	_ = m.View()
	require.NotEqual(t, wasExpanded, m.workHeaderBySource[anchor].IsExpanded(), "a click on the restyled header toggles the block")
	_, _ = m.handleMouseClick(tea.MouseClickMsg{X: 4, Y: themeScrollWorkHeader(m), Button: tea.MouseLeft})
	_ = m.View()
	require.Equal(t, wasExpanded, m.workHeaderBySource[anchor].IsExpanded(), "a second click restores the block")
}

// revealFixture is a work-heavy transcript whose Work blocks hold a completed tool
// call and a reasoning child with text: user, tool call, reasoning, answer. The
// last turn's block anchors at n-3, as in themeScrollFixture.
func revealFixture(t *testing.T, ar *animation.Runtime, n int) *model {
	t.Helper()
	require.Zero(t, n%4)
	m := NewScrollableView(ar, 100, themeScrollHeight, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	m.SetCompactWorkLog(true)
	for turn := range n / 4 {
		m.addMessage(types.User(fmt.Sprintf("request %d", turn)))
		call := tools.ToolCall{ID: fmt.Sprintf("reveal-%d", turn), Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"ls"}`}}
		m.addMessage(types.ToolCallMessage("root", call, tools.Tool{Name: "shell"}, types.ToolStatusCompleted))
		m.addMessage(types.Agent(types.MessageTypeAssistantReasoningBlock, "root", fmt.Sprintf("Thinking paragraph %d", turn)))
		m.addMessage(types.Agent(types.MessageTypeAssistant, "root", fmt.Sprintf("answer %d", turn)))
	}
	require.Len(t, m.messages, n)
	_ = m.View()
	return m
}

// workRevealTicks advances the disclosure animation by its real ticks until no
// block animates, and returns the most blocks that animated during one tick.
func workRevealTicks(t *testing.T, m *model, ar *animation.Runtime) (maxAnimating int, perTick []uint64) {
	t.Helper()
	for range 200 {
		animating := 0
		for _, block := range m.workBlocks {
			if block.NeedsTick() {
				animating++
			}
		}
		if ar.ActiveCount() == 0 {
			break
		}
		maxAnimating = max(maxAnimating, animating)
		cmd := ar.Continue()
		require.NotNil(t, cmd)
		tick, ok := ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		before := m.layoutWorkStats()
		_, _ = m.Update(tick)
		_ = m.View()
		perTick = append(perTick, m.layoutWorkStats().blockViews-before.blockViews)
	}
	return maxAnimating, perTick
}

// TestRealWorkClickRevealsBodyAtBottomReader is positive coverage of a real click
// on a collapsed Work block under the bottom reader: the disclosure reveal runs over
// real ticks, and once it settles the body is laid out and painted. Each tick
// renders only the animating block (base-semantics height re-measure, viewport
// bounded). It is not a regression detector. Observed proof: with the height-dirty
// change removed (paint-only invalidation on ticks) the test also passes, with the
// same blockViews per tick (scratch/vprl/logs/vmouse/before-private.log), so it does
// not distinguish the two.
func TestRealWorkClickRevealsBodyAtBottomReader(t *testing.T) {
	const n = 1000
	// The reasoning child renders its text when the block is expanded; tool children
	// without output render no body, so they cannot show the bug.
	const marker = "Thinking paragraph 249"
	ar := animation.NewRuntimeWithScheduler(&disclosureClock{now: time.Unix(1, 0)})
	m := revealFixture(t, ar, n)
	themeScrollPlace(t, m, "bottom")
	anchor := n - 3
	block := m.workHeaderBySource[anchor]
	require.NotNil(t, block, "the last turn has a Work block")
	require.False(t, block.Expanded(), "the block starts collapsed")
	y := themeScrollWorkHeader(m)
	require.GreaterOrEqual(t, y, 0, "the Work header is on screen at the bottom")

	_, _ = m.handleMouseClick(tea.MouseClickMsg{X: 4, Y: y, Button: tea.MouseLeft})
	require.True(t, block.Expanded(), "the real click expands the block")
	_ = m.View()
	require.NotContains(t, ansi.Strip(m.View()), marker, "before the reveal ticks the body is not painted")

	maxAnimating, perTick := workRevealTicks(t, m, ar)
	require.NotEmpty(t, perTick, "the disclosure animates over ticks")
	require.False(t, block.NeedsTick(), "the disclosure settled")
	require.Greater(t, block.Height(), 1, "the settled block includes its body")
	require.NotEmpty(t, block.Rows(), "the settled block has child rows")
	require.Contains(t, ansi.Strip(m.View()), marker, "the settled body is painted")

	require.Equal(t, 1, maxAnimating, "one block animates")
	t.Logf("reveal: ticks=%d maxAnimatingBlocks=%d blockViewsPerTick=%v", len(perTick), maxAnimating, perTick)
	// One animating block costs one measure (View, Height and Rows: five counted
	// views) plus one paint View. The bound is per animating block, never per history.
	for i, views := range perTick {
		require.LessOrEqual(t, views, uint64(6*maxAnimating), "tick %d renders only the animating block", i)
	}
}

// TestWorkToolChildrenWithoutOutputExpandToHeaderOnly pins the body of a Work block
// whose children are completed tool calls without output. Such a child renders no
// text, so the expanded block is its header alone, on a real click as well as on a
// direct expand. The block's tool-call children are the same in base and candidate:
// the tool views and the workblock body code are unchanged.
func TestWorkToolChildrenWithoutOutputExpandToHeaderOnly(t *testing.T) {
	const n = 1000
	ar := animation.NewRuntimeWithScheduler(&disclosureClock{now: time.Unix(1, 0)})
	m := themeScrollFixtureOn(t, ar, n)
	themeScrollPlace(t, m, "bottom")
	anchor := n - 3
	block := m.workHeaderBySource[anchor]
	require.NotNil(t, block)

	items := m.workItemsForAnchor(anchor)
	require.Len(t, items, 2, "the lookup returns both tool children of the block")
	for _, item := range items {
		require.Empty(t, ansi.Strip(item.View.View()), "a completed tool call without output renders no text")
	}

	y := themeScrollWorkHeader(m)
	require.GreaterOrEqual(t, y, 0)
	_, _ = m.handleMouseClick(tea.MouseClickMsg{X: 4, Y: y, Button: tea.MouseLeft})
	_, _ = workRevealTicks(t, m, ar)
	require.False(t, block.NeedsTick())
	require.Equal(t, 1, block.Height(), "the expanded block is its header when no child has text")
	require.Empty(t, block.Rows(), "no child row is laid out without text")
}

// toolResultFixture is a work-heavy transcript whose tool calls completed with a
// result: user, two tool calls with output, answer. The last turn's block anchors
// at n-3, as in themeScrollFixture.
func toolResultFixture(t *testing.T, ar *animation.Runtime, n int) *model {
	t.Helper()
	require.Zero(t, n%4)
	m := NewScrollableView(ar, 100, themeScrollHeight, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	m.SetCompactWorkLog(true)
	for turn := range n / 4 {
		m.addMessage(types.User(fmt.Sprintf("request %d", turn)))
		for k := range 2 {
			call := tools.ToolCall{ID: fmt.Sprintf("result-%d-%d", turn, k), Function: tools.FunctionCall{Name: "shell", Arguments: fmt.Sprintf(`{"cmd":"go test ./pkg/%d"}`, turn)}}
			msg := types.ToolCallMessage("root", call, tools.Tool{Name: "shell"}, types.ToolStatusCompleted)
			msg.Content = fmt.Sprintf("result %d-%d", turn, k)
			m.addMessage(msg)
		}
		m.addMessage(types.Agent(types.MessageTypeAssistant, "root", fmt.Sprintf("answer %d", turn)))
	}
	require.Len(t, m.messages, n)
	_ = m.View()
	return m
}

// TestRealWorkClickShowsToolResultsAtBottomReader: a real click expands a Work block
// of completed tool calls with output under the bottom reader. Once the reveal has
// settled, the tool results must be laid out and painted.
func TestRealWorkClickShowsToolResultsAtBottomReader(t *testing.T) {
	const n = 1000
	const marker = "result 249-1"
	ar := animation.NewRuntimeWithScheduler(&disclosureClock{now: time.Unix(1, 0)})
	m := toolResultFixture(t, ar, n)
	themeScrollPlace(t, m, "bottom")
	anchor := n - 3
	block := m.workHeaderBySource[anchor]
	require.NotNil(t, block, "the last turn has a Work block")
	require.NotContains(t, ansi.Strip(m.View()), marker, "the collapsed block does not paint its results")

	y := themeScrollWorkHeader(m)
	require.GreaterOrEqual(t, y, 0, "the Work header is on screen at the bottom")
	_, _ = m.handleMouseClick(tea.MouseClickMsg{X: 4, Y: y, Button: tea.MouseLeft})
	require.True(t, block.Expanded(), "the real click expands the block")

	_, _ = workRevealTicks(t, m, ar)
	require.False(t, block.NeedsTick(), "the disclosure settled")
	require.Greater(t, block.Height(), 1, "the settled block includes its children")
	require.Contains(t, ansi.Strip(m.View()), marker, "the settled tool result is painted")
}

// hotPathCost is what one user event moved, as counter deltas.
type hotPathCost struct {
	workRangeScans, workIndexVisits, identityLookups, identityCatchupEntries, blockViews, targetBodyRenders, themeUpdates uint64
	blocks                                                                                                                int // Work blocks on screen across the event
}

// hotPathEventNames are the real user paths the hot-path counters are measured on.
var hotPathEventNames = []string{"hover", "click", "wheel", "copy", "handoff", "width", "theme", "stream", "scroll"}

// hotPathRow is the first screen row that lies on a Work block, or 0. It reads the
// slot index only, so it moves none of the counters an event is measured by.
func hotPathRow(m *model) int {
	for y := range m.height {
		if _, _, _, ok := m.workBlockAt(m.scrollOffset + y); ok {
			return y
		}
	}
	return 0
}

func hotPathBlocksOnScreen(m *model) int {
	n := 0
	for i := range themeScrollViewport(m) {
		if m.workHeaderBySource[i] != nil {
			n++
		}
	}
	return n
}

// hotPathEvent returns the event driven through its real entry point.
func hotPathEvent(t *testing.T, m *model, name string, original *styles.Theme) func() {
	t.Helper()
	row := hotPathRow(m)
	switch name {
	case "hover":
		return func() { m.handleMouseMotion(tea.MouseMotionMsg{X: 5, Y: row}) }
	case "click":
		return func() { _, _ = m.handleMouseClick(tea.MouseClickMsg{X: 2, Y: row, Button: tea.MouseLeft}) }
	case "wheel":
		return func() { _, _ = m.Update(messages.WheelCoalescedMsg{X: 5, Y: row, Delta: -1}) }
	case "copy":
		return func() {
			last := len(m.views) - 1
			if m.selectSourceSpan(last-3, 2, last, 3) {
				_ = m.extractSelectedText()
			}
		}
	case "handoff":
		return func() { m.snapshotPendingReplyHandoff(nil) }
	case "width":
		return func() { m.SetSize(60, themeScrollHeight) }
	case "stream":
		return func() { m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "streamed reply")) }
	case "theme":
		return func() {
			styles.ApplyTheme(vprlTheme(original))
			_, _ = m.Update(messages.ThemeChangedMsg{})
		}
	default:
		delta := -1
		if m.scrollOffset == 0 {
			delta = 1
		}
		return func() { _, _ = m.Update(messages.WheelCoalescedMsg{X: 5, Y: 5, Delta: delta}) }
	}
}

// hotPathMeasure runs one event and the View after it, reporting counter deltas.
func hotPathMeasure(m *model, event func()) hotPathCost {
	blocks := hotPathBlocksOnScreen(m)
	before := m.stats
	event()
	_ = m.View()
	after := m.stats
	return hotPathCost{
		workRangeScans:         after.workRangeScans - before.workRangeScans,
		workIndexVisits:        after.workIndexVisits - before.workIndexVisits,
		identityLookups:        after.identityLookups - before.identityLookups,
		identityCatchupEntries: after.identityCatchupEntries - before.identityCatchupEntries,
		blockViews:             after.blockViews - before.blockViews,
		targetBodyRenders:      after.targetBodyRenders - before.targetBodyRenders,
		themeUpdates:           after.themeUpdates - before.themeUpdates,
		blocks:                 max(blocks, hotPathBlocksOnScreen(m)),
	}
}

// TestHotPathEventsDoNotScanWorkRanges drives every hot user path on 1k and 6k
// work-heavy transcripts at the top, middle and bottom. No event may walk the work
// ranges. Block renders and target reads stay bounded by the blocks the event
// touches, not by history. The counters are read before any explicit demand.
func TestHotPathEventsDoNotScanWorkRanges(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	for _, pos := range []string{"top", "middle", "bottom"} {
		small := themeScrollFixture(t, 1000)
		large := themeScrollFixture(t, 6000)
		for _, name := range hotPathEventNames {
			costs := map[int]hotPathCost{}
			for n, m := range map[int]*model{1000: small, 6000: large} {
				themeScrollPlace(t, m, pos)
				costs[n] = hotPathMeasure(m, hotPathEvent(t, m, name, original))
				cost := costs[n]
				if name == "stream" {
					// A new message dirties the work projection, so one structural rebuild
					// walks each block once. It is not a hover, wheel or epoch read, and its
					// cost is bounded by the blocks it rebuilds.
					require.LessOrEqual(t, cost.workRangeScans, uint64(2*len(m.workHeaderBySource)+2), "%s %s n=%d: the structural rebuild walks each block at most twice", pos, name, n)
				} else {
					require.Zero(t, cost.workRangeScans, "%s %s n=%d: no event walks the work ranges", pos, name, n)
				}
				require.Zero(t, cost.identityCatchupEntries, "%s %s n=%d: production writes keep the identity index exact, so no catch-up runs", pos, name, n)
				require.LessOrEqual(t, cost.blockViews, uint64(5*(cost.blocks+1)), "%s %s n=%d: block renders are bounded by the blocks on screen", pos, name, n)
				require.LessOrEqual(t, cost.targetBodyRenders, uint64(2), "%s %s n=%d: at most one hit target is read per event", pos, name, n)
				t.Logf("%s %-7s n=%-5d scans=%d visits=%d identity=%d blockViews=%d targetBodies=%d themeUpdates=%d blocks=%d",
					pos, name, n, cost.workRangeScans, cost.workIndexVisits, cost.identityLookups, cost.blockViews, cost.targetBodyRenders, cost.themeUpdates, cost.blocks)
			}
		}
	}
}

// TestMessageIdentityIndexStaysExact: every production change to the transcript keeps
// the identity index exact through its hook, without a repair: an ordinary append
// (one entry written), a tail spinner insert, a removal, a same-length replacement, a
// session load and a reset. A direct write is not repaired, so it does not resolve.
func TestMessageIdentityIndexStaysExact(t *testing.T) {
	m := themeScrollFixture(t, 40)
	requireExact := func() {
		t.Helper()
		require.Equal(t, len(m.messages), m.messageIndexLen, "every change went through its hook")
		for i, msg := range m.messages {
			require.Equal(t, i, m.messagePointerIndex(msg))
		}
	}
	requireExact()

	// Ordinary append: exactly one entry is written.
	before := m.stats.identityEntries
	m.addMessage(types.User("late"))
	require.Equal(t, before+1, m.stats.identityEntries, "an ordinary append writes one index entry")
	requireExact()

	// Tail spinner insert: the spinner stays last, the new message goes before it.
	m.addMessage(types.Spinner())
	spinner := m.messages[len(m.messages)-1]
	user := types.User("before the spinner")
	m.addMessage(user)
	require.Equal(t, len(m.messages)-1, m.messagePointerIndex(spinner), "the spinner stays pinned at the tail")
	require.Equal(t, len(m.messages)-2, m.messagePointerIndex(user), "the new message is inserted before the spinner")
	requireExact()

	// Removal: the removed pointer stops resolving and the rest keep their positions.
	m.removeSpinner()
	require.Equal(t, -1, m.messagePointerIndex(spinner), "a removed pointer no longer resolves")
	requireExact()

	// Same-length replacement: the tail pointer is replaced by a new one at the same
	// length. The old pointer is gone and the new one resolves.
	m.addMessage(types.Spinner())
	old := m.messages[len(m.messages)-1]
	length := len(m.messages)
	m.removeSpinner()
	m.addMessage(types.Spinner())
	replacement := m.messages[len(m.messages)-1]
	require.Len(t, m.messages, length, "the replacement keeps the length")
	require.Equal(t, -1, m.messagePointerIndex(old), "the replaced pointer no longer resolves")
	require.Equal(t, length-1, m.messagePointerIndex(replacement), "the new pointer resolves at the same position")
	requireExact()

	// Canonical replay: a session load rebuilds the transcript through the replay path,
	// and every loaded message is indexed exactly.
	sess := session.New(session.WithID("identity"))
	for i := range 12 {
		sess.AddMessage(session.UserMessage(fmt.Sprintf("replayed %d", i)))
	}
	m.LoadFromSession(sess, nil)
	require.Equal(t, -1, m.messagePointerIndex(replacement), "a message from before the load no longer resolves")
	requireExact()

	// Reset: the transcript is rebuilt from the session and the index follows it.
	sess.AddMessage(session.UserMessage("recovered"))
	m.ResetFromSession(sess, nil)
	requireExact()

	// Direct appends bypass the hooks. The first lookup catches up that unindexed tail,
	// and only that tail: two new messages are two entries, and a later lookup adds none.
	first, second := types.User("direct 1"), types.User("direct 2")
	m.messages = append(m.messages, first, second)
	m.views = append(m.views, m.createMessageView(first), m.createMessageView(second))
	catchup := m.stats.identityCatchupEntries
	require.Equal(t, len(m.messages)-2, m.messagePointerIndex(first))
	require.Equal(t, catchup+2, m.stats.identityCatchupEntries, "exactly the two unindexed messages are caught up")
	require.Equal(t, len(m.messages)-1, m.messagePointerIndex(second))
	require.Equal(t, catchup+2, m.stats.identityCatchupEntries, "a later lookup catches up nothing")
	requireExact()

	// Same-length replacement without a hook: nothing is repaired, so neither the old
	// nor the new pointer resolves.
	lastPos := len(m.messages) - 1
	replaced := m.messages[lastPos]
	swapped := types.User("swapped")
	m.messages[lastPos] = swapped
	catchup = m.stats.identityCatchupEntries
	require.Equal(t, -1, m.messagePointerIndex(swapped), "an unhooked replacement is not repaired")
	require.Equal(t, -1, m.messagePointerIndex(replaced), "the replaced pointer no longer resolves")
	require.Equal(t, catchup, m.stats.identityCatchupEntries, "a same-length change catches up nothing")

	// Shrink without a hook: removed pointers resolve to -1, and nothing is repaired.
	removed := m.messages[len(m.messages)-1]
	m.messages = m.messages[:len(m.messages)-2]
	catchup = m.stats.identityCatchupEntries
	require.Equal(t, -1, m.messagePointerIndex(removed), "a truncated pointer does not resolve")
	require.Equal(t, catchup, m.stats.identityCatchupEntries, "a shrink catches up nothing")
}
