package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
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
	require.Nil(t, root.openPanes())
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
