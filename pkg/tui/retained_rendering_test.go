package tui

import (
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/components/agentidentity"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// This adapter forces the real component's fresh path, not just the root cache.
// Keep the optional viewport capability so root clipping uses identical geometry.
type freshRenderingEditor struct {
	editor.Editor
	editor.ViewportLayout
	fresh editor.RetainedRendering
}

func (e freshRenderingEditor) View() string                { return e.fresh.FreshView() }
func (e freshRenderingEditor) BannerView(width int) string { return e.fresh.FreshBannerView(width) }

func requireFreshRetainedView(t *testing.T, root *appModel) {
	t.Helper()
	cached := root.View()
	original := root.editor
	fresh, ok := original.(editor.RetainedRendering)
	require.True(t, ok, "fixture must expose the real editor's forced-fresh renderer")
	before := fresh.RenderDiagnostics()
	root.editor = freshRenderingEditor{Editor: original, ViewportLayout: original.(editor.ViewportLayout), fresh: fresh}
	defer func() { root.editor = original }()
	root.viewCacheValid = false
	root.paneRenderCache = paneRenderCache{}
	root.paneTitleCache = nil
	root.paneDimCache = nil
	root.tabFrameCache = shellFrameCache{}
	root.contextFrameCache = shellFrameCache{}
	require.Equal(t, cached, root.View(), "compare complete tea.View, including cursor/colors/terminal metadata; no ANSI normalization")
	require.Greater(t, fresh.RenderDiagnostics().Body, before.Body, "fresh root must execute editor body renderer")
	require.Greater(t, fresh.RenderDiagnostics().Textarea, before.Textarea, "fresh root must execute Bubbles artifact renderer")
}

func TestRetainedRootTransitionsMatchForcedFreshCompleteView(t *testing.T) {
	originalTheme := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(originalTheme) })
	root, _, _ := paneReplayRoot(t)
	root.focusedPanel = PanelEditor
	root.editor.Focus()
	for _, step := range []struct {
		name string
		run  func()
	}{
		{"initial", func() {}},
		{"text and graphemes", func() { root.editor.SetValue("Cafe\u0301 界 👩‍💻"); root.resizeAll(); root.Update(struct{}{}) }},
		{"cursor", func() { root.Update(tea.KeyPressMsg{Code: tea.KeyLeft}) }},
		{"paste", func() { root.Update(tea.PasteMsg{Content: " pasted λ"}) }},
		{"search", func() { root.editor.EnterHistorySearch(); root.Update(struct{}{}) }},
		{"search text", func() { root.Update(tea.KeyPressMsg{Code: 'a', Text: "a"}) }},
		{"exit search", func() { root.Update(tea.KeyPressMsg{Code: tea.KeyEscape}) }},
		{"resize", func() { root.Update(tea.WindowSizeMsg{Width: 130, Height: 42}) }},
		{"theme", func() {
			theme := *originalTheme
			theme.Colors.TextMuted = "#123456"
			theme.Colors.EditorBg = "#182838"
			styles.ApplyTheme(&theme)
			root.Update(messages.ThemeChangedMsg{})
		}},
		{"blur", func() { root.editor.Blur(); root.focusedPanel = PanelContent; root.Update(struct{}{}) }},
		{"pane focus", func() { root.handleSwitchTab("second") }},
		{"dim", func() { root.dimInactivePanes = true; root.Update(struct{}{}) }},
		{"pane removal", func() { root.removePane("third") }},
		{"single pane", func() { root.singlePane() }},
		{"narrow", func() { root.Update(tea.WindowSizeMsg{Width: 12, Height: 10}) }},
	} {
		t.Run(step.name, func(t *testing.T) { step.run(); requireFreshRetainedView(t, root) })
	}
}

func TestRetainedSpinnerSkipsUnrelatedRegions(t *testing.T) {
	root, _, scheduler := paneReplayRoot(t)
	root.editor.SetValue(strings.Repeat("draft 界 ", 8))
	root.resizeAll()
	root.Update(runtime.StreamStarted("profile", "root"))
	tabs, active := root.supervisor.GetTabs()
	for i := range tabs {
		if tabs[i].SessionID == "profile" {
			tabs[i].Activity = messages.TabActivityRunning
		}
	}
	root.Update(messages.TabsUpdatedMsg{Tabs: tabs, ActiveIdx: active})
	// The active transcript can legitimately animate its own pending spinner;
	// unrelated authoritative snapshots must retain their prepared spans.
	settlePaneReplay(root, scheduler)
	root.View()
	ed := root.editor.(editor.RetainedRendering)
	beforeEditor := ed.RenderDiagnostics()
	beforeTitle := root.paneTitleCache["profile"]
	builds := map[string]uint64{}
	for _, id := range []string{"second", "third"} {
		builds[id] = root.paneRenderCache.parts["transcript:"+id].builds
	}
	scheduler.step = 80 * time.Millisecond
	changed := false
	for range 12 {
		require.NotEmpty(t, scheduler.pending)
		cmd := scheduler.pending[0]
		scheduler.pending = scheduler.pending[1:]
		msg := cmd()
		require.IsType(t, animation.TickMsg{}, msg)
		root.Update(msg)
		root.View()
		after := root.paneTitleCache["profile"]
		changed = changed || after.layout.Renders() > beforeTitle.layout.Renders()
	}
	require.True(t, changed, "real scheduler must advance the visible activity artifact")
	afterEditor := ed.RenderDiagnostics()
	require.Equal(t, beforeEditor.Textarea, afterEditor.Textarea)
	require.Equal(t, beforeEditor.Search, afterEditor.Search)
	require.Equal(t, beforeEditor.Banner, afterEditor.Banner)
	require.Equal(t, beforeEditor.Composition, afterEditor.Composition, "spinner retains editor composition")
	require.Greater(t, afterEditor.Body, beforeEditor.Body, "complete source frame style is authoritatively painted")
	for id, count := range builds {
		require.Equal(t, count, root.paneRenderCache.parts["transcript:"+id].builds, id)
	}
	requireFreshRetainedView(t, root)
}

func TestPaneColorSnapshotPreservesEncoding(t *testing.T) {
	for _, value := range []string{"1", "21", "#123456", "invalid"} {
		c := lipgloss.Color(value)
		require.Equal(t, lipgloss.NewStyle().Foreground(c).Render("λ界"), lipgloss.NewStyle().Foreground(snapshotPaneColor(c).color()).Render("λ界"), value)
	}
}

// Keep a small pre-retention oracle: forced fresh alone would not detect a
// shared retained/fresh renderer changing the historical hyperlink/frame bytes.
func TestPaneTitleRetainedMatchesOriginalBytes(t *testing.T) {
	root, _, _ := paneReplayRoot(t)
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	for _, palette := range []string{"default", "default-light", "indexed"} {
		theme := *original
		if palette == "indexed" {
			theme.Colors.TextPrimary, theme.Colors.TextMuted, theme.Colors.EditorBg = "21", "7", "4"
		} else {
			loaded, err := styles.LoadTheme(palette)
			require.NoError(t, err)
			theme = *loaded
		}
		styles.ApplyTheme(&theme)
		for _, dim := range []bool{false, true} {
			root.dimInactivePanes = dim
			for _, id := range root.panes.Sessions() {
				for _, width := range []int{0, 1, 3, 4, 12, 48, 120} {
					require.Equal(t, originalPaneTitle(root, id, width), root.paneTitle(id, width), "palette=%s dim=%v id=%s width=%d", palette, dim, id, width)
				}
			}
		}
	}
}

func originalPaneTitle(root *appModel, id string, width int) string {
	name, nodeID := root.tabAgentIdentity(messages.TabInfo{SessionID: id})
	if name == "" {
		name = "agent"
	}
	ref := lifecycle.InputReference{Kind: lifecycle.InputReferenceNode, ID: nodeID, Name: name, Agent: name, DisplayID: paneDisplayNodeID(nodeID)}
	title := "New Session"
	if state := root.sessionStates[id]; state != nil && state.SessionTitle() != "" {
		title = state.SessionTitle()
	}
	activity := root.paneActivity(id)
	count := ""
	if children := root.paneSubagentCount(id, subagent.NodeID(nodeID)); children > 0 {
		count = " (" + strconv.Itoa(children) + ")"
	}
	budget := max(0, width-1-ansi.StringWidth(activity)-ansi.StringWidth(count)-5)
	label := " " + agentidentity.Label(ref, budget) + styles.MutedStyle.Render(count) + " " + activity + styles.MutedStyle.Render(" · "+title)
	if width <= 3 {
		label = "●"
	}
	background := styles.CardBg
	if id == root.paneFocus() {
		background = styles.EditorBg
	}
	heading := styles.RenderComposite(lipgloss.NewStyle().Foreground(styles.TextPrimary).Background(background), paneClipped(label, width, 1))
	if root.dimInactivePanes && root.panesEnabled() && id != root.paneFocus() {
		context := styles.NewFadeContext()
		heading = styles.FadeLineCtx(heading, inactivePaneContrast, &context)
	}
	return heading
}

func TestPaneTitlePreservesEvolvedSourceStyles(t *testing.T) {
	root, _, _ := paneReplayRoot(t)
	original := styles.MutedStyle
	t.Cleanup(func() { styles.MutedStyle = original })
	for _, style := range []lipgloss.Style{
		original.Bold(true),
		original.Background(lipgloss.Color("21")),
		original.Padding(0, 2),
		original.Transform(strings.ToUpper),
		original.Bold(true).Background(lipgloss.Color("#102030")).Padding(0, 1).Transform(strings.ToUpper),
	} {
		styles.MutedStyle = style
		for _, id := range root.panes.Sessions() {
			for _, width := range []int{3, 12, 48, 120} {
				require.Equal(t, originalPaneTitle(root, id, width), root.paneTitle(id, width))
			}
		}
	}
}
