package messages

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// sizedCountedView wraps its content to the width it was sized to, so a width
// change really changes heights and the layout must settle estimates.
type sizedCountedView struct {
	content string
	width   int
	renders int
}

func (v *sizedCountedView) SetSize(w, _ int) tea.Cmd               { v.width = w; return nil }
func (*sizedCountedView) Init() tea.Cmd                            { return nil }
func (v *sizedCountedView) Update(tea.Msg) (layout.Model, tea.Cmd) { return v, nil }
func (v *sizedCountedView) View() string                           { v.renders++; return wrapRunes(v.content, v.width) }

func wrapRunes(s string, width int) string {
	width = max(1, width)
	r := []rune(s)
	var lines []string
	for start := 0; start < len(r); start += width {
		lines = append(lines, string(r[start:min(len(r), start+width)]))
	}
	if len(lines) == 0 {
		lines = []string{""}
	}
	return strings.Join(lines, "\n")
}

// identityModel builds a transcript of n sized items and lays it out at 80x24.
func identityModel(t *testing.T, n int) *model {
	t.Helper()
	m := NewScrollableView(animation.NewRuntime(), 80, 24, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	for i := range n {
		view := &sizedCountedView{content: fmt.Sprintf("item %d %s", i, strings.Repeat("wrapped text ", i%9+1))}
		m.messages = append(m.messages, types.User(view.content))
		m.views = append(m.views, view)
	}
	_ = m.View()
	return m
}

func identityRenders(m *model) []int {
	out := make([]int, len(m.views))
	for i, v := range m.views {
		out[i] = v.(*sizedCountedView).renders
	}
	return out
}

// identityVisible returns the slots whose rows intersect the viewport.
func identityVisible(m *model) []int {
	if len(m.views) == 0 {
		return nil
	}
	first := m.slotAt(m.scrollOffset)
	last := min(m.slotAt(max(m.scrollOffset, m.scrollOffset+m.height-1)), len(m.views)-1)
	var out []int
	for i := first; i <= last; i++ {
		out = append(out, i)
	}
	return out
}

func containsSlot(set []int, i int) bool {
	return slices.Contains(set, i)
}

// TestLayoutRenderIdentities logs, for each event, every item that rendered and
// its class. Classes: visible-after (shown after the event), visible-before-only
// (shown before, not after), anchor (the reader's top row after the event, which
// is also visible-after), tail (the last item), settling (anything else: an
// estimate correction that was measured offscreen). Run with -v; the logs are
// the evidence in scratch/vprl/logs/render-identities.txt.
func TestLayoutRenderIdentities(t *testing.T) {
	for _, n := range []int{100, 1000, 6000} {
		for _, pos := range []string{"top", "middle", "bottom"} {
			t.Run(fmt.Sprintf("n=%d/%s", n, pos), func(t *testing.T) {
				m := identityModel(t, n)
				total := m.totalHeight
				switch pos {
				case "top":
					m.userHasScrolled = true
					m.setScrollOffset(0)
				case "middle":
					m.userHasScrolled = true
					m.setScrollOffset(total / 2)
				default:
					_ = m.scrollToBottom()
				}
				_ = m.View()
				events := []struct {
					name string
					act  func()
				}{
					{"shrink-80to60", func() { m.SetSize(60, 24) }},
					{"grow-60to80", func() { m.SetSize(80, 24) }},
					{"theme", func() { _, _ = m.Update(messages.ThemeChangedMsg{}) }},
					{"append-one", func() {
						view := &sizedCountedView{content: fmt.Sprintf("appended %d", n)}
						m.messages = append(m.messages, types.User(view.content))
						m.views = append(m.views, view)
					}},
				}
				for _, ev := range events {
					beforeVisible := identityVisible(m)
					before := identityRenders(m)
					ev.act()
					_ = m.View()
					after := identityRenders(m)
					afterVisible := identityVisible(m)
					anchor := m.slotAt(m.scrollOffset)
					tail := len(m.views) - 1
					var rendered []string
					counts := map[string]int{}
					for i := range after {
						if i < len(before) && after[i] == before[i] {
							continue
						}
						class := "settling"
						switch {
						case containsSlot(afterVisible, i):
							class = "visible-after"
						case containsSlot(beforeVisible, i):
							class = "visible-before-only"
						}
						if i == tail {
							class = "tail"
						}
						if i == anchor && containsSlot(afterVisible, i) {
							class = "anchor"
						}
						counts[class]++
						rendered = append(rendered, fmt.Sprintf("%d:%s", i, class))
					}
					t.Logf("event=%s pos=%s n=%d visibleBefore=%d visibleAfter=%d rendered=%d visible-after=%d anchor=%d tail=%d visible-before-only=%d settling=%d items=%v",
						ev.name, pos, n, len(beforeVisible), len(afterVisible), len(rendered), counts["visible-after"]+counts["anchor"], counts["anchor"], counts["tail"], counts["visible-before-only"], counts["settling"], rendered)
					require.Zero(t, counts["settling"], "%s renders only the items the viewport needs", ev.name)
				}
			})
		}
	}
}

// identityUnion returns the slots in either set, without duplicates.
func identityUnion(a, b []int) []int {
	out := append([]int(nil), a...)
	for _, i := range b {
		if !containsSlot(out, i) {
			out = append(out, i)
		}
	}
	return out
}
