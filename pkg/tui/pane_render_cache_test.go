package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func assertFreshPaneComposition(t *testing.T, root *appModel) {
	t.Helper()
	cached := root.composePanes()
	root.paneRenderCache = paneRenderCache{}
	root.paneTitleCache = nil
	root.paneDimCache = nil
	require.Equal(t, cached, root.composePanes(), "cached spans must match a fresh component composition")
}

func TestPanePreparationReusesUnchangedVisibleComponents(t *testing.T) {
	root, _, scheduler := paneReplayRoot(t)
	root.composePanes()
	builds := func(id string) uint64 { return root.paneRenderCache.parts["transcript:"+id].builds }
	before := map[string]uint64{}
	for _, id := range root.panes.Sessions() {
		before[id] = builds(id)
	}
	// A routed stream modifies exactly one visible pane. The other authoritative
	// views are still read, but their ANSI clipping/padding is not repeated.
	generation, _ := root.supervisor.RouteGeneration("second")
	root.Update(messages.RoutedMsg{SessionID: "second", RouteGeneration: generation, Inner: runtime.AgentChoice("root", "second", "\nnew visible stream λ界")})
	root.composePanes()
	require.Greater(t, builds("second"), before["second"])
	require.Equal(t, before["profile"], builds("profile"))
	require.Equal(t, before["third"], builds("third"))
	// A real spinner frame/sidebar update leaves unrelated transcripts prepared.
	// The active transcript may legitimately animate its own working indicator.
	root.Update(runtime.StreamStarted("profile", "root"))
	root.composePanes()
	for _, id := range root.panes.Sessions() {
		before[id] = builds(id)
	}
	settlePaneReplay(root, scheduler)
	root.composePanes()
	for _, id := range []string{"second", "third"} {
		require.Equal(t, before[id], builds(id), id)
	}
	assertFreshPaneComposition(t, root)
}

func TestPanePreparationInvalidationAndBoundedOwnership(t *testing.T) {
	root, _, _ := paneReplayRoot(t)
	root.composePanes()
	for _, step := range []struct {
		name string
		run  func()
	}{
		{"scroll", func() {
			r := root.paneGeometry.Panes["second"]
			root.Update(messages.WheelCoalescedMsg{X: r.X + 2, Y: r.Y + 2, Delta: -3})
		}},
		{"focus", func() { root.handleSwitchTab("second") }},
		{"dim", func() { root.dimInactivePanes = true }},
		{"resize", func() { root.Update(tea.WindowSizeMsg{Width: 170, Height: 52}) }},
		{"draft geometry", func() { root.editor.SetValue(strings.Repeat("draft λ界 ", 100)); root.resizeAll() }},
	} {
		t.Run(step.name, func(t *testing.T) { step.run(); assertFreshPaneComposition(t, root) })
	}
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	theme := *original
	theme.Colors.TextMuted = "#123456"
	styles.ApplyTheme(&theme)
	root.Update(messages.ThemeChangedMsg{})
	assertFreshPaneComposition(t, root)
	root.singlePane()
	root.composePanes()
	for key := range root.paneRenderCache.parts {
		if strings.HasPrefix(key, "transcript:") {
			require.Equal(t, "transcript:"+root.paneFocus(), key)
		}
	}
	require.Empty(t, root.paneTitleCache, "single-pane view owns no pane headings")
	require.Len(t, root.paneRenderCache.rows, root.contentHeight)
	root.closeTab("third")
	require.NotContains(t, root.paneRenderCache.parts, "transcript:third")
}

func TestPanePreparedSpansPreserveTerminalSemantics(t *testing.T) {
	for _, raw := range []string{"", "λ界 👩‍💻\nsecond", "\x1b[31mred\x1b[m  ", "\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\", "\x1b[48;2;10;20;30m colored padding   \x1b[m"} {
		var cache paneRenderCache
		prepared := cache.prepare("pane", raw, 12, 4)
		rows := make([][]paneRowSpan, 4)
		for y, span := range prepared {
			span.x = 2
			rows[y] = []paneRowSpan{span}
		}
		got := cache.render(rows, 20)
		for y := range rows {
			for i := range rows[y] {
				rows[y][i].prepared = false
			}
		}
		require.Equal(t, renderPaneRows(rows, 20), got)
		before := cache.parts["pane"].builds
		cache.prepare("pane", raw, 12, 4)
		require.Equal(t, before, cache.parts["pane"].builds)
		cache.prepare("pane", raw, 11, 4)
		require.Greater(t, cache.parts["pane"].builds, before)
	}
}

func TestShellFrameCacheReadsExactContentAndGeometry(t *testing.T) {
	var cache shellFrameCache
	renders := 0
	render := func() string { renders++; return "framed" }
	require.Equal(t, "framed", cache.render("raw", 80, 2, render))
	require.Equal(t, "framed", cache.render("raw", 80, 2, render))
	require.Equal(t, 1, renders)
	cache.render("replacement page", 80, 2, render)
	cache.render("replacement page", 81, 2, render)
	cache.render("replacement page", 81, 3, render)
	require.Equal(t, 4, renders)
}
