package tui

import (
	"image/color"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func splitTestRoot(t *testing.T) *appModel {
	t.Helper()
	root, _, _ := frozenClockRoot(t, 120, 40)
	root.chatPage.SetRoutingID("profile")
	root.editors["profile"] = root.editor
	for _, id := range []string{"second", "third"} {
		sess := session.New(session.WithID(id))
		sess.Title = "会話 λ " + id
		a := app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
		_, err := root.supervisor.AddSession(t.Context(), a, sess, "", nil)
		require.NoError(t, err)
		page, state, ed, application := root.chatPage, root.sessionState, root.editor, root.application
		root.initSessionComponents(id, a, sess)
		root.chatPage.Init()
		root.chatPage, root.sessionState, root.editor, root.application = page, state, ed, application
	}
	tabs, active := root.supervisor.GetTabs()
	root.setTabs(tabs, active)
	root.handleWindowResize(120, 40)
	root.View()
	t.Cleanup(func() {
		for _, p := range root.chatPages {
			chat.Cleanup(p)
		}
		root.ar.Stop()
	})
	return root
}

func TestSplitCanonicalMoveRemoveSingleKeepOwners(t *testing.T) {
	root := splitTestRoot(t)
	pages := map[string]chat.Page{}
	editors := map[string]editor.Editor{}
	for id, p := range root.chatPages {
		pages[id] = p
		editors[id] = root.editors[id]
	}
	root.editor.SetValue("first draft λ")
	root.splitPane("second", "profile", splitRight)
	require.Equal(t, "second", root.paneFocus())
	require.Equal(t, []string{"profile", "second"}, root.panes.Sessions())
	root.editor.SetValue("second draft 界")
	root.splitPane("third", "profile", splitBottom)
	root.splitPane("second", "third", splitLeft)
	require.Len(t, root.panes.Sessions(), 3)
	root.removePane("second")
	require.False(t, root.panes.Contains("second"))
	require.Equal(t, 3, root.supervisor.Count())
	root.handleSwitchTab("second")
	require.Equal(t, "second draft 界", root.editor.Value())
	root.singlePane()
	require.Len(t, root.panes.Sessions(), 1)
	for id, p := range pages {
		require.Same(t, p, root.chatPages[id])
		require.Same(t, editors[id], root.editors[id])
		require.NotNil(t, root.supervisor.GetRunner(id))
	}
	root.handleSwitchTab("profile")
	require.Equal(t, "first draft λ", root.editor.Value())
}

type splitRecordingPage struct {
	chat.Page

	updates    []tea.Msg
	bottom     int
	generation uint64
}

func (p *splitRecordingPage) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	p.updates = append(p.updates, msg)
	_, cmd := p.Page.Update(msg)
	return p, cmd
}

func (p *splitRecordingPage) ScrollToBottom() tea.Cmd { p.bottom++; return p.Page.ScrollToBottom() }

func (p *splitRecordingPage) VisualGeneration() uint64 {
	return p.Page.VisualGeneration() + p.generation
}

func (p *splitRecordingPage) MeasureSplitShell(w, h int) chat.SplitShellGeometry {
	return p.Page.(chat.SplitPresentation).MeasureSplitShell(w, h)
}

func (p *splitRecordingPage) SetSplitPresentation(g *chat.SplitPresentationGeometry) tea.Cmd {
	return p.Page.(chat.SplitPresentation).SetSplitPresentation(g)
}

func (p *splitRecordingPage) TranscriptView() string {
	return p.Page.(chat.SplitPresentation).TranscriptView()
}

func (p *splitRecordingPage) SidebarView() string {
	return p.Page.(chat.SplitPresentation).SidebarView()
}

func (p *splitRecordingPage) SidebarHandleView() string {
	return p.Page.(chat.SplitPresentation).SidebarHandleView()
}

func (p *splitRecordingPage) SidebarVisualGeneration() uint64 {
	return p.Page.(chat.SplitPresentation).SidebarVisualGeneration()
}

func (p *splitRecordingPage) SetPresentationVisible(v bool) tea.Cmd {
	return p.Page.(chat.PresentationVisibility).SetPresentationVisible(v)
}

func TestSplitFocusPreservesScrollAndWheelDoesNotStealComposer(t *testing.T) {
	root := splitTestRoot(t)
	first := &splitRecordingPage{Page: root.chatPage}
	root.chatPages["profile"], root.chatPage = first, first
	root.splitPane("second", "profile", splitRight)
	root.editor.SetValue("second draft")
	r := root.paneGeometry.Panes["profile"]
	root.Update(messages.WheelCoalescedMsg{X: r.X + 2, Y: r.Y + 2, Delta: -4})
	require.Equal(t, "second", root.paneFocus())
	require.Equal(t, "second draft", root.editor.Value())
	require.IsType(t, messages.WheelCoalescedMsg{}, first.updates[len(first.updates)-1])
	root.Update(tea.MouseClickMsg{X: r.X + 1, Y: r.Y, Button: tea.MouseLeft})
	require.Equal(t, "profile", root.paneFocus())
	require.Zero(t, first.bottom, "pane focus never invokes ScrollToBottom")
	root.handleSwitchTab("second")
	require.Equal(t, "second draft", root.editor.Value())
}

func TestSplitVisibleStreamsTicksAndAggregateCache(t *testing.T) {
	root := splitTestRoot(t)
	first := &splitRecordingPage{Page: root.chatPage}
	root.chatPages["profile"], root.chatPage = first, first
	root.splitPane("second", "profile", splitRight)
	root.Update(messages.RoutedMsg{SessionID: "profile", Inner: runtime.StreamStarted("profile", "root")})
	root.Update(messages.RoutedMsg{SessionID: "profile", Inner: runtime.AgentChoice("root", "profile", "VISIBLE BACKGROUND λ界")})
	require.Contains(t, ansi.Strip(root.View().Content), "VISIBLE BACKGROUND")
	require.Equal(t, "second", root.paneFocus())
	before := len(first.updates)
	root.tickVisiblePanes(animation.TickMsg{})
	require.Greater(t, len(first.updates), before)
	root.View()
	require.True(t, root.visiblePaneCacheValid())
	first.generation++
	require.False(t, root.visiblePaneCacheValid(), "unfocused visible generations fence root cache")
	root.View()
	require.True(t, root.visiblePaneCacheValid())
	root.removePane("profile")
	before = len(first.updates)
	root.tickVisiblePanes(animation.TickMsg{})
	require.Len(t, first.updates, before, "hidden page is not ticked")
	root.Update(messages.RoutedMsg{SessionID: "profile", Inner: runtime.AgentChoice("root", "profile", " hidden suffix")})
	require.Len(t, first.updates, before+1, "hidden content still accumulates")
}

func TestSplitShrinkGrowAndHeightOnlyWarmHistory(t *testing.T) {
	root := splitTestRoot(t)
	for _, id := range []string{"profile", "second"} {
		sess, _, _ := mixedHistorySession(40)
		root.supervisor.GetRunner(id).App.Session().Messages = sess.Messages
		root.chatPages[id].Init()
	}
	root.splitPane("second", "profile", splitRight)
	root.View()
	tree := root.panes.root
	root.handleWindowResize(40, 20)
	require.True(t, root.paneGeometry.Compact)
	require.Empty(t, root.paneGeometry.Dividers)
	require.Same(t, tree, root.panes.root)
	root.handleWindowResize(120, 40)
	require.False(t, root.paneGeometry.Compact)
	require.Len(t, root.paneGeometry.Panes, 2)
	for _, h := range []int{40, 44, 40} {
		root.handleWindowResize(120, h)
		root.View()
	}
	expected := map[string][3]uint64{}
	for id := range root.paneGeometry.Panes {
		a, b, c := root.chatPages[id].(resizeCacheReporter).ResizeCacheStats()
		expected[id] = [3]uint64{a, b, c}
	}
	for i := range 8 {
		root.handleWindowResize(120, 40+(i%2)*4)
		root.View()
	}
	for id := range root.paneGeometry.Panes {
		a, b, c := root.chatPages[id].(resizeCacheReporter).ResizeCacheStats()
		expectedStats := expected[id]
		actualStats := [3]uint64{a, b, c}
		require.Equal(t, expectedStats, actualStats, "height-only pane resize must reuse warmed markdown")
		t.Logf("pane=%s height-only rebuilds/misses/renders delta=0/0/0", id)
	}
}

func TestSplitPalettePreservesDraftAndUnsupportedIsAtomic(t *testing.T) {
	root := splitTestRoot(t)
	root.editor.SetValue("keep live draft")
	cmd := root.openPanes()
	root.Update(cmd())
	require.True(t, root.dialogMgr.Open())
	root.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
	require.Equal(t, "keep live draft", root.editor.Value(), "picker typing is not composer typing")
	root.updateDialogCmd(dialog.HideDialogMsg{})
	before := root.paneLayout().Sessions()
	root.chatPages["second"] = &mockChatPage{}
	msgs := collectMsgs(root.splitPane("second", "profile", splitRight))
	require.NotEmpty(t, msgs, "unsupported seam has user feedback")
	require.Equal(t, before, root.paneLayout().Sessions())
	root.leanMode = true
	for _, category := range root.commandCategories() {
		for _, item := range category.Commands {
			require.NotEqual(t, "/panes", item.SlashCommand)
		}
	}
	require.NotNil(t, root.openPanes(), "lean rejection is explicit, never chat fallthrough")
}

func TestSplitUnicodeCellClipping(t *testing.T) {
	for _, width := range []int{1, 2, 3, 24, 49} {
		text := paneClipped("界λ🙂é long\nsecond", width, 3)
		lines := strings.Split(text, "\n")
		require.Len(t, lines, 3)
		for _, line := range lines {
			require.Equal(t, width, ansi.StringWidth(line))
		}
	}
}

func TestSplitComposerCursorAndAttachmentRemainOnCanonicalEditor(t *testing.T) {
	root := splitTestRoot(t)
	first := installPaneRecorder(root, "profile")
	root.editor.SetValue("abcd")
	root.updateEditorCmd(tea.KeyPressMsg{Code: tea.KeyLeft})
	root.updateEditorCmd(tea.KeyPressMsg{Code: tea.KeyLeft})
	root.splitPane("second", "profile", splitRight)
	root.editor.SetValue("other")
	root.handleSwitchTab("profile")
	root.updateEditorCmd(tea.KeyPressMsg{Code: 'X', Text: "X"})
	require.Equal(t, "abXcd", root.editor.Value(), "switch preserves insertion cursor")
	root.updateEditorCmd(tea.PasteMsg{Content: "one\ntwo\nthree\nfour\nfive\nsix\nattached λ"})
	require.True(t, root.editor.HasContextBar(), "large paste creates canonical composer attachment")
	attachmentPath := filepath.Join(t.TempDir(), "attachment.txt")
	require.NoError(t, os.WriteFile(attachmentPath, []byte("file attachment λ界"), 0o600))
	require.NoError(t, root.editor.AttachFile(attachmentPath))
	cmd := root.stampEditorSend(root.editor.SendContent())
	root.handleSwitchTab("second")
	for _, msg := range collectMsgs(cmd) {
		root.Update(msg)
	}
	require.Len(t, first.sends, 1)
	require.NotEmpty(t, first.sends[0].Attachments)
	require.Equal(t, attachmentPath, first.sends[0].Attachments[0].FilePath)
	require.Contains(t, first.sends[0].Content, "attached λ", "large paste expands into content while file attachments retain their path")
	require.Equal(t, "other", root.editor.Value())
}

func TestPaneHeadingAndDimCacheFollowIdentityFocusTheme(t *testing.T) {
	root := splitTestRoot(t)
	root.sessionStates["profile"].SetCurrentAgentName("canonical-agent")
	root.splitPane("second", "profile", splitRight)
	title := root.paneTitle("profile", 70)
	require.Contains(t, ansi.Strip(title), "canonical-agent")
	require.Equal(t, 70, ansi.StringWidth(title))
	root.dimInactivePanes = true
	input := "\x1b[38;2;100;150;200mtext\x1b[m"
	dimmed := root.paneTranscript("profile", input)
	require.NotEqual(t, input, dimmed)
	require.Equal(t, "text", ansi.Strip(dimmed))
	require.Equal(t, dimmed, root.paneTranscript("profile", input))
	originalTheme := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(originalTheme) })
	styles.ApplyTheme(styles.DefaultTheme())
	root.paneTranscript("profile", input)
	require.Equal(t, styles.ThemeGeneration(), root.paneDimCache["profile"].theme)
	root.handleSwitchTab("profile")
	require.Equal(t, input, root.paneTranscript("profile", input))
	root.handleSwitchTab("second")
	root.dimInactivePanes = false
	require.Equal(t, input, root.paneTranscript("profile", input))
}

func TestPaneRowCompositionMatchesCellLayerComposition(t *testing.T) {
	for _, input := range []string{"plain text", "界λ🙂e", "\x1b[38;2;100;150;200mshort", "\x1b[48;2;20;30;40mshort", "\x1b[38;2;100;150;200mred界\x1b[0m", "\x1b[48;2;20;30;40mbackground\x1b[0m", "\x1b]8;;https://example.invalid\x1b\\link界\x1b]8;;\x1b\\"} {
		rows := [][]paneRowSpan{{{x: 1, width: 8, content: ansi.Truncate(input, 8, "")}, {x: 10, width: 6, content: "right"}}, {{x: 1, width: 15, content: "lower"}}}
		got := renderPaneRows(rows, 18)
		old := lipgloss.NewCompositor(
			lipgloss.NewLayer(paneClipped("", 18, 2)),
			lipgloss.NewLayer(paneClipped(input, 8, 1)).X(1),
			lipgloss.NewLayer(paneClipped("right", 6, 1)).X(10),
			lipgloss.NewLayer(paneClipped("lower", 15, 1)).X(1).Y(1),
		).Render()
		require.Equal(t, ansi.Strip(old), ansi.Strip(got))
		require.Equal(t, chromeCells(old), chromeCells(got), "foreground/background runs are preserved")
		for line := range strings.SplitSeq(got, "\n") {
			require.LessOrEqual(t, ansi.StringWidth(line), 18)
		}
	}
}

func TestTiledSidebarGeometryDoesNotFollowPerTabPreferences(t *testing.T) {
	root := splitTestRoot(t)
	root.hideSidebar = false
	for _, id := range []string{"profile", "second", "third"} {
		runner := root.supervisor.GetRunner(id)
		root.createSessionComponents(id, runner.App, runner.App.Session())
		root.chatPages[id].Init()
	}
	root.chatPage, root.editor, root.sessionState = root.chatPages["profile"], root.editors["profile"], root.sessionStates["profile"]
	first := chat.SidebarSettings{Collapsed: false, PreferredWidth: 30}
	second := chat.SidebarSettings{Collapsed: true, PreferredWidth: 42}
	root.chatPages["profile"].SetSidebarSettings(first)
	root.chatPages["second"].SetSidebarSettings(second)
	root.resizeAll()
	root.splitPane("second", "profile", splitRight)
	bounds, shell := root.paneBounds, root.paneShell
	root.handleSwitchTab("profile")
	require.Equal(t, bounds, root.paneBounds)
	require.Equal(t, shell, root.paneShell)
	root.handleSwitchTab("second")
	require.Equal(t, bounds, root.paneBounds)
	require.Equal(t, first, root.chatPages["profile"].GetSidebarSettings())
	require.Equal(t, second, root.chatPages["second"].GetSidebarSettings())
	root.singlePane()
	_, active := root.chatPage.(chat.SplitSidebarPresentation).SplitSidebarSettings()
	require.False(t, active)
	require.Equal(t, second, root.chatPage.GetSidebarSettings())
}

func TestPaneHeaderOwnsActivityWithoutSharedSeparator(t *testing.T) {
	root := splitTestRoot(t)
	root.splitPane("second", "profile", splitRight)
	root.sessionStates["profile"].SetPauseState(service.PausePaused)
	require.Contains(t, ansi.Strip(root.paneTitle("profile", 60)), "paused")
	root.paneOutcomes = map[string]runtime.TurnOutcome{"second": runtime.TurnCompleted}
	require.Contains(t, ansi.Strip(root.paneTitle("second", 60)), "done")
	require.NotEmpty(t, root.renderResizeHandle(root.width))
	require.Equal(t, regionTabBar, root.hitTestRegion(root.contentHeight+1))
}

func TestTiledSidebarExplicitOverrideAndNestedFocus(t *testing.T) {
	root := splitTestRoot(t)
	root.splitPane("second", "profile", splitRight)
	root.splitPane("third", "second", splitBottom)
	before := root.panes.root
	settings := chat.SidebarSettings{Collapsed: true, PreferredWidth: 31}
	root.chatPage.(chat.SplitSidebarPresentation).SetSplitSidebarSettings(&settings)
	root.syncPaneSidebarSettings()
	root.resizePanes()
	for _, id := range root.panes.Sessions() {
		got, active := root.chatPages[id].(chat.SplitSidebarPresentation).SplitSidebarSettings()
		require.True(t, active)
		require.Equal(t, settings, got)
	}
	root.handleSwitchTab("profile")
	require.Same(t, before, root.panes.root)
	root.handleWindowResize(40, 18)
	require.True(t, root.paneGeometry.Compact)
	root.handleWindowResize(120, 40)
	require.False(t, root.paneGeometry.Compact)
	require.Same(t, before, root.panes.root)
}

// The inherited compositor dropped U+0301 from "界λ🙂e\u0301", rendering
// " 界λ🙂e   right\n lower". Preserve the complete grapheme instead; this
// explicitly supersedes only that lossy case, not the equivalence test above.
func TestPaneRowCompositionPreservesCombiningGraphemeBytes(t *testing.T) {
	rows := [][]paneRowSpan{{{x: 1, width: 8, content: "界λ🙂e\u0301"}, {x: 10, width: 6, content: "right"}}, {{x: 1, width: 15, content: "lower"}}}
	got := renderPaneRows(rows, 18)
	require.Equal(t, " 界λ🙂e\u0301   right\n lower", got)
	require.Equal(t, 10, ansi.StringWidth(strings.Split(got, "right")[0]))
	require.NotContains(t, got, " \u0301", "combining marks must not migrate onto padding")
}

func TestPaneRowCompositionGraphemeClipEdgesAndStyleBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		width             int
	}{
		{name: "composed", input: "éX", width: 1, want: " é right"},
		{name: "decomposed", input: "e\u0301X", width: 1, want: " e\u0301 right"},
		{name: "wide fits", input: "界X", width: 2, want: " 界 right"},
		{name: "wide clipped", input: "界X", width: 1, want: "   right"},
		{name: "cluster at edge", input: "界e\u0301X", width: 3, want: " 界e\u0301 right"},
		{name: "emoji cluster", input: "👩‍💻X", width: 2, want: " 👩‍💻 right"},
		{name: "emoji clipped", input: "👩‍💻X", width: 1, want: "   right"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, decoration := range []struct{ start, end string }{
				{},
				{start: "\x1b[38;2;100;150;200m", end: "\x1b[0m"},
				{start: "\x1b]8;;https://example.invalid\x1b\\", end: ansi.ResetHyperlink()},
			} {
				content := ansi.Truncate(decoration.start+tc.input+decoration.end, tc.width, "")
				rows := [][]paneRowSpan{{{x: 1, width: tc.width, content: content}, {x: tc.width + 2, width: 5, content: "right"}}}
				got := renderPaneRows(rows, tc.width+7)
				require.Equal(t, tc.want, ansi.Strip(got), "exact grapheme bytes survive style/hyperlink clipping")
				before, _, found := strings.Cut(got, "right")
				require.True(t, found)
				require.Equal(t, tc.width+2, ansi.StringWidth(before), "neighbor starts at its exact cell")
				require.NotContains(t, got, " \u0301")
				cells := chromeCells(got)
				for _, cell := range cells[len(cells)-5:] {
					require.Nil(t, cell.fg, "left pane SGR cannot color neighboring pane")
					require.Nil(t, cell.bg)
				}
				if strings.Contains(before, "\x1b]8;;https://example.invalid") {
					require.Greater(t, strings.LastIndex(before, ansi.ResetHyperlink()), strings.LastIndex(before, "\x1b]8;;https://example.invalid"), "hyperlink closes before the neighbor")
				}
			}
		})
	}
}

func TestPaneRowCompositionSidebarOverlayMatchesOldCells(t *testing.T) {
	root := splitTestRoot(t)
	root.hideSidebar = false
	for _, id := range []string{"profile", "second", "third"} {
		runner := root.supervisor.GetRunner(id)
		root.createSessionComponents(id, runner.App, runner.App.Session())
		root.chatPages[id].Init()
	}
	root.chatPage, root.editor, root.sessionState = root.chatPages["profile"], root.editors["profile"], root.sessionStates["profile"]
	root.handleWindowResize(160, 50)
	root.splitPane("second", "profile", splitRight)
	root.splitPane("third", "second", splitBottom)
	for _, position := range []messages.SidebarPosition{messages.SidebarTop, messages.SidebarBottom} {
		root.applyLayoutSettings(messages.LayoutSettings{SidebarPosition: position})
		layers := []*lipgloss.Layer{lipgloss.NewLayer(paneClipped("", root.width, root.contentHeight))}
		add := func(content string, r splitRect) {
			if r.W > 0 && r.H > 0 {
				layers = append(layers, lipgloss.NewLayer(paneClipped(content, r.W, r.H)).X(r.X).Y(r.Y))
			}
		}
		for _, id := range root.paneLayout().Sessions() {
			r, visible := root.paneGeometry.Panes[id]
			if !visible {
				continue
			}
			page := root.chatPages[id].(chat.SplitPresentation)
			header := root.paneHeaderHeight()
			if header > 0 {
				add(root.paneTitle(id, r.W), splitRect{X: r.X, Y: r.Y + r.H - header, W: r.W, H: header})
			}
			add(root.paneTranscript(id, page.TranscriptView()), splitRect{X: r.X, Y: r.Y, W: r.W, H: r.H - header})
		}
		for _, divider := range root.paneGeometry.Dividers {
			glyph := strings.Repeat("─", divider.Rect.W)
			if divider.Axis == splitColumns {
				glyph = strings.Repeat("│\n", divider.Rect.H)
			}
			add(styles.MutedStyle.Render(glyph), divider.Rect)
		}
		page := root.chatPage.(chat.SplitPresentation)
		add(page.SidebarView(), paneRect(root.paneShell.Sidebar))
		add(page.SidebarHandleView(), paneRect(root.paneShell.SidebarHandle))
		old := lipgloss.NewCompositor(layers...).Render()
		got := root.composePanes()
		require.Equal(t, ansi.Strip(old), ansi.Strip(got), "nested panes and %s sidebar overlay positions", position)
		require.Equal(t, chromeCells(old), chromeCells(got), "nested pane/sidebar overlay styles")
	}
}

func TestPaneTrailingDefaultPaddingPreservesANSIAndOSC(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{input: "text   ", want: "text"},
		{input: "text\x1b[m   \x1b[m", want: "text\x1b[m\x1b[m"},
		{input: "\x1b[48;2;20;30;40mtext   \x1b[m  ", want: "\x1b[48;2;20;30;40mtext   \x1b[m"},
		{input: "\x1b[38;2;20;30;40mtext   \x1b[m  ", want: "\x1b[38;2;20;30;40mtext   \x1b[m"},
		{input: "\x1b[48:2::20:30:40mtext   \x1b[49m  ", want: "\x1b[48:2::20:30:40mtext   \x1b[49m"},
		{input: "\x1b]8;;https://example.invalid\x1b\\link \x1b]8;;\x1b\\  ", want: "\x1b]8;;https://example.invalid\x1b\\link \x1b]8;;\x1b\\"},
		{input: "e\u0301   \x1b[m", want: "e\u0301\x1b[m"},
	} {
		require.Equal(t, tc.want, trimPaneDefaultPadding(tc.input))
	}
}

func TestPaneTrailingMalformedExtendedColorsStayOpaque(t *testing.T) {
	for _, prefix := range []string{"38", "48", "58"} {
		for _, channels := range []string{";2;4", ":2:4", ";2;0", ":2:0", ";5", ":5"} {
			input := "\x1b[" + prefix + channels + "mtext   \x1b[0m  "
			require.Equal(t, strings.TrimSuffix(input, "  "), trimPaneDefaultPadding(input), "malformed color padding remains opaque until explicit reset")
		}
	}
}

func TestDegenerateTerminalKeepsFocusedLeafVisibleWithoutChangingTree(t *testing.T) {
	root := splitTestRoot(t)
	root.splitPane("second", "profile", splitRight)
	tree := root.panes.root
	root.handleWindowResize(1, 1)
	frame := ansi.Strip(root.View().Content)
	require.Equal(t, ansi.Strip(root.editor.View()), frame, "the only cell belongs to the editor, never a replacement header")
	require.Equal(t, 1, ansi.StringWidth(frame))
	require.True(t, root.paneGeometry.Compact)
	require.Equal(t, "second", root.paneFocus())
	require.Same(t, tree, root.panes.root)
	root.handleWindowResize(120, 40)
	require.False(t, root.paneGeometry.Compact)
	require.Same(t, tree, root.panes.root)
}

func TestPaneHeaderUsesOneImmutableStatusSnapshotDuringComposition(t *testing.T) {
	root := splitTestRoot(t)
	root.composingPaneStatuses = map[string]runtime.SessionStatus{"profile": {SessionID: "profile", Dormant: true, Pending: 2}}
	first := ansi.Strip(root.paneTitle("profile", 120))
	require.Contains(t, first, "Restored · paused")
	// Simulate a newer canonical head becoming observable while composing.
	// The published frame must remain coherent with its captured key.
	root.viewPaneStatuses = root.composingPaneStatuses
	root.composingPaneStatuses = map[string]runtime.SessionStatus{"profile": {SessionID: "profile", Dormant: false, Pending: 2}}
	second := ansi.Strip(root.paneTitle("profile", 120))
	require.NotContains(t, second, "Restored · paused")
	require.True(t, root.viewPaneStatuses["profile"].Dormant, "previous frame key remains immutable")
	root.composingPaneStatuses = nil
}

func TestPaneHeadersSeparateRecipientAndTranscriptFromTabs(t *testing.T) {
	for _, size := range [][2]int{{120, 40}, {64, 24}, {24, 12}, {1, 1}} {
		root := splitTestRoot(t)
		root.sessionStates["profile"].SetCurrentAgentName("reviewer")
		root.handleWindowResize(size[0], size[1])
		frame := root.View().Content
		rows := strings.Split(frame, "\n")
		require.Len(t, rows, size[1])
		for _, row := range rows {
			require.Equal(t, size[0], ansi.StringWidth(row))
		}
		require.Zero(t, root.paneHeaderHeight())
		require.NotContains(t, ansi.Strip(frame), "Send to", "single visible pane never has a heading")
		require.NotContains(t, ansi.Strip(frame), "Composer")
		if root.contentHeight > paneMinHeight {
			require.Contains(t, ansi.Strip(rows[root.contentHeight]), "─", "the restored separator separates transcript/sidebar from tabs")
			for _, rect := range root.paneGeometry.Panes {
				require.LessOrEqual(t, rect.Y+rect.H, root.contentHeight)
			}
		}
	}
}

func TestPaneHeaderSurfaceTracksFocusAndThemeWithoutChangingOwners(t *testing.T) {
	root := splitTestRoot(t)
	root.sessionStates["profile"].SetCurrentAgentName("reviewer")
	root.splitPane("second", "profile", splitRight)
	page, savedEditor := root.chatPages["profile"], root.editors["profile"]
	inactive := root.paneTitle("profile", 60)
	root.handleSwitchTab("profile")
	focused := root.paneTitle("profile", 60)
	require.NotContains(t, ansi.Strip(inactive), "Send to")
	wide := ansi.Strip(root.paneTitle("profile", 60) + root.paneTitle("second", 60))
	require.NotContains(t, wide, "Send to")
	require.NotEqual(t, inactive, focused)
	require.Contains(t, ansi.Strip(focused), "reviewer")
	require.Equal(t, ansi.Strip(inactive), ansi.Strip(focused), "surface contrast identifies focus without a label prefix")
	for _, cell := range chromeCells(focused) {
		require.NotNil(t, cell.bg, "header background spans every cell")
	}
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	theme := *original
	theme.Colors.EditorBg = "#203040"
	styles.ApplyTheme(&theme)
	require.NotEqual(t, focused, root.paneTitle("profile", 60))
	require.Same(t, page, root.chatPages["profile"])
	require.Same(t, savedEditor, root.editors["profile"])
}

func TestSharedSidebarAndPaneHeaderGeometryAcrossWidthsFocusAndTheme(t *testing.T) {
	root := splitTestRoot(t)
	root.hideSidebar = false
	for _, id := range []string{"profile", "second", "third"} {
		runner := root.supervisor.GetRunner(id)
		root.createSessionComponents(id, runner.App, runner.App.Session())
		root.chatPages[id].Init()
	}
	root.chatPage, root.editor, root.sessionState = root.chatPages["profile"], root.editors["profile"], root.sessionStates["profile"]
	root.handleWindowResize(160, 44)
	root.splitPane("second", "profile", splitRight)
	for _, position := range []messages.SidebarPosition{messages.SidebarRight, messages.SidebarTop, messages.SidebarBottom} {
		root.applyLayoutSettings(messages.LayoutSettings{SidebarPosition: position})
		for _, size := range [][2]int{{160, 44}, {80, 30}, {24, 12}, {1, 1}} {
			root.handleWindowResize(size[0], size[1])
			before := root.paneBounds
			root.handleSwitchTab("profile")
			require.Equal(t, before, root.paneBounds, "focus does not change effective tiled sidebar geometry")
			root.handleSwitchTab("second")
			require.Equal(t, before, root.paneBounds)
			frame := root.View().Content
			require.Len(t, strings.Split(frame, "\n"), size[1])
			for line := range strings.SplitSeq(frame, "\n") {
				require.Equal(t, size[0], ansi.StringWidth(line))
			}
			if position != messages.SidebarRight {
				require.LessOrEqual(t, root.paneShell.Sidebar.Height, 2)
			}
			if root.contentHeight > paneMinHeight {
				rows := strings.Split(ansi.Strip(frame), "\n")
				require.Contains(t, rows[root.contentHeight], "─")
			}
		}
	}
}

func TestInactivePaneRenderedContrastDarkLightAndDisabled(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	for _, palette := range []struct{ name, background, text, surface string }{
		{name: "dark", background: "#101820", text: "#e8edf2", surface: "#25313d"},
		{name: "light", background: "#f4f5f6", text: "#202830", surface: "#dce1e6"},
	} {
		t.Run(palette.name, func(t *testing.T) {
			theme := *original
			theme.Colors.Background, theme.Colors.TextPrimary = palette.background, palette.text
			theme.Colors.EditorBg, theme.Colors.CardBg = palette.surface, palette.surface
			styles.ApplyTheme(&theme)
			root := splitTestRoot(t)
			root.splitPane("second", "profile", splitRight)
			root.dimInactivePanes = true
			transcript := styles.BaseStyle.Render("Readable inactive transcript λ界")
			focused := root.paneTranscript("second", transcript)
			inactive := root.paneTranscript("profile", transcript)
			a, b, c := root.chatPages["profile"].(resizeCacheReporter).ResizeCacheStats()
			for range 12 {
				require.Equal(t, inactive, root.paneTranscript("profile", transcript))
			}
			afterA, afterB, afterC := root.chatPages["profile"].(resizeCacheReporter).ResizeCacheStats()
			require.Equal(t, [3]uint64{a, b, c}, [3]uint64{afterA, afterB, afterC}, "cached dim presentation never reflows history")
			require.Zero(t, root.ar.ActiveCount())
			require.Equal(t, transcript, focused)
			require.Equal(t, ansi.Strip(focused), ansi.Strip(inactive))
			focusCell, inactiveCell := chromeCells(focused)[0], chromeCells(inactive)[0]
			distance := func(c color.Color) float64 {
				r, g, b := styles.ColorToRGB(c)
				br, bg, bb := styles.ColorToRGB(styles.Background)
				return math.Sqrt((r-br)*(r-br) + (g-bg)*(g-bg) + (b-bb)*(b-bb))
			}
			require.Less(t, distance(inactiveCell.fg), distance(focusCell.fg)*0.40, "rendered RGB distance must be strongly reduced")
			require.Greater(t, distance(inactiveCell.fg), distance(focusCell.fg)*0.30, "body text retains the intended 35% contrast rather than disappearing")
			root.dimInactivePanes = false
			headerFull := root.paneTitle("profile", 60)
			root.dimInactivePanes = true
			headerDim := root.paneTitle("profile", 60)
			require.Equal(t, ansi.Strip(headerFull), ansi.Strip(headerDim))
			require.NotEqual(t, chromeCells(headerFull)[0].bg, chromeCells(headerDim)[0].bg, "inactive full-width header surface dims with its transcript")
			root.dimInactivePanes = false
			require.Equal(t, transcript, root.paneTranscript("profile", transcript))
			root.dimInactivePanes = true
			root.handleSwitchTab("profile")
			require.Equal(t, transcript, root.paneTranscript("profile", transcript), "focus immediately restores original ANSI")
			for _, width := range []int{1, 2, 3, 24} {
				require.Equal(t, width, ansi.StringWidth(root.paneTitle("second", width)))
			}
			root.singlePane()
			require.Equal(t, transcript, root.paneTranscript("second", transcript), "single-view presentation is never dimmed")
			t.Logf("%s focused=%q inactive=%q header=%q", palette.name, focused, inactive, headerDim)
		})
	}
}

func TestPureBaseThemesRenderCanonicalPaneShellAndDialog(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	for _, ref := range []string{"black-cyan", "black-amber", "black-violet", "white-cobalt", "white-teal", "white-rose"} {
		t.Run(ref, func(t *testing.T) {
			theme, err := styles.LoadTheme(ref)
			require.NoError(t, err)
			styles.ApplyTheme(theme)
			root, _, _ := wallClockRoot(t, 160, 44)
			for _, id := range []string{"second", "third"} {
				sess := session.New(session.WithID(id))
				application := app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
				_, err := root.supervisor.AddSession(t.Context(), application, sess, "", nil)
				require.NoError(t, err)
			}
			t.Cleanup(func() {
				for _, page := range root.chatPages {
					chat.Cleanup(page)
				}
			})
			root.hideSidebar = false
			for _, id := range []string{"profile", "second", "third"} {
				runner := root.supervisor.GetRunner(id)
				runner.App.Session().AddMessage(session.UserMessage("Readable transcript λ界 " + id))
				root.createSessionComponents(id, runner.App, runner.App.Session())
				root.chatPages[id].Init()
			}
			root.chatPage, root.editor, root.sessionState = root.chatPages["profile"], root.editors["profile"], root.sessionStates["profile"]
			root.handleWindowResize(160, 44)
			root.splitPane("second", "profile", splitRight)
			_, teamCmd := root.Update(runtime.TeamInfo([]runtime.AgentDetails{{Name: "reviewer", Provider: "provider", Model: "fixture alias", ModelID: "canonical-model", ModelName: "Friendly Model", ThinkingMode: "effort", ThinkingLevel: "high", CanCycleThinking: true}}, "reviewer"))
			_, agentCmd := root.Update(runtime.AgentInfo("reviewer", "provider/canonical-model", "Reviewer", ""))
			settleRootChrome(t, root, tea.Batch(teamCmd, agentCmd))
			root.dimInactivePanes = true
			frame := root.View().Content
			plain := ansi.Strip(frame)
			require.NotContains(t, plain, "Send to")
			require.Contains(t, plain, "Readable transcript")
			// Exercise real transcript selection, not a style assembled from raw
			// YAML: SelectionStyle uses ApplyTheme's effective foreground.
			pane := root.paneGeometry.Panes[root.paneFocus()]
			selected := false
			for row, line := range strings.Split(plain, "\n") {
				if row < pane.Y || row >= pane.Y+pane.H-root.paneHeaderHeight() {
					continue
				}
				segment := ansi.Cut(line, pane.X, pane.X+pane.W)
				before, _, found := strings.Cut(segment, "Readable transcript")
				if !found {
					continue
				}
				x := pane.X + ansi.StringWidth(before)
				_, clickCmd := root.Update(tea.MouseClickMsg{X: x, Y: row, Button: tea.MouseLeft})
				_, motionCmd := root.Update(tea.MouseMotionMsg{X: x + 8, Y: row, Button: tea.MouseLeft})
				selection := root.View().Content
				for _, cell := range chromeCells(selection) {
					if cell.bg != nil && color.NRGBAModel.Convert(cell.bg) == color.NRGBAModel.Convert(styles.Selected) {
						require.Equal(t, color.NRGBAModel.Convert(styles.SelectedFg), color.NRGBAModel.Convert(cell.fg))
						selected = true
					}
				}
				_, releaseCmd := root.Update(tea.MouseReleaseMsg{X: x + 8, Y: row, Button: tea.MouseLeft})
				root.chatPage.(chat.PresentationSelection).ClearPresentationSelection()
				settleRootChrome(t, root, tea.Batch(clickCmd, motionCmd, releaseCmd))
				require.False(t, root.chatPage.IsSelecting(), "mouseup and clearing retire selection anchors")
				require.Zero(t, root.ar.ActiveCount())
				break
			}
			require.True(t, selected, "real transcript selection paints the effective selection background")
			page := root.chatPage.(chat.SplitPresentation)
			sidebar := ansi.Strip(page.SidebarView())
			require.Contains(t, sidebar, "Friendly Model")
			require.Contains(t, sidebar, "provider")
			require.Contains(t, sidebar, "high")
			require.Contains(t, sidebar, "reviewer")
			sidebarRows := strings.Split(sidebar, "\n")
			identityRow, modelRow, providerRow := -1, -1, -1
			for row, text := range sidebarRows {
				if strings.Contains(text, "reviewer") {
					identityRow = row
				}
				if strings.Contains(text, "Friendly Model") {
					modelRow = row
				}
				if strings.Contains(text, "provider") && strings.Contains(text, "high") {
					providerRow = row
				}
			}
			require.Positive(t, identityRow, "usage block precedes the canonical identity")
			require.Contains(t, strings.Join(sidebarRows[:identityRow], "\n"), "$0.00", "usage remains above identity")
			require.Equal(t, identityRow+1, modelRow)
			require.Equal(t, modelRow+1, providerRow)
			require.NotContains(t, sidebarRows[len(sidebarRows)-1], "reviewer", "identity is not pinned to the footer")
			for line := range strings.SplitSeq(frame, "\n") {
				require.Equal(t, 160, ansi.StringWidth(line))
			}
			root.dimInactivePanes = false
			undimmed := root.composePanes()
			root.dimInactivePanes = true
			require.NotEqual(t, undimmed, root.composePanes())
			for _, position := range []messages.SidebarPosition{messages.SidebarTop, messages.SidebarBottom} {
				root.applyLayoutSettings(messages.LayoutSettings{SidebarPosition: position})
				require.LessOrEqual(t, root.paneShell.Sidebar.Height, 2)
			}
			dialogModel := dialog.NewPermissionsDialog(nil, false)
			dialogModel.SetSize(100, 30)
			require.NotEmpty(t, ansi.Strip(dialogModel.View()))
			t.Logf("theme=%s frame=%q sidebar=%q dialog=%q", ref, frame, page.SidebarView(), dialogModel.View())
		})
	}
}
