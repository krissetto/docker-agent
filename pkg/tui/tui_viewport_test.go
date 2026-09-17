package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/components/completion"
	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestSingleVisiblePaneHasNoHeaderAndFirstRowRoutesToTranscript(t *testing.T) {
	root := splitTestRoot(t)
	page := installPaneRecorder(root, "profile")
	for _, split := range []bool{false, true} {
		if split {
			root.handleWindowResize(120, 40)
			root.splitPane("second", "profile", splitRight)
			root.handleSwitchTab("profile")
		}
		tree := root.panes.root
		for _, size := range [][2]int{{24, 12}, {120, 40}, {24, 12}} {
			root.handleWindowResize(size[0], size[1])
			count := len(root.paneGeometry.Panes)
			require.Equal(t, count > 1, root.paneHeaderHeight() > 0)
			require.Same(t, tree, root.panes.root)
			if count != 1 {
				continue
			}
			require.NotContains(t, ansi.Strip(root.composePanes()), "Send to")
			r := root.paneGeometry.Panes["profile"]
			transcript := root.chatPages["profile"].(chat.SplitPresentation).TranscriptView()
			require.Equal(t, r.H, lipgloss.Height(transcript), "no invisible heading row is reserved")
			page.updates = nil
			root.forwardPanePointer(tea.MouseClickMsg{X: r.X, Y: r.Y, Button: tea.MouseLeft}, r.X, r.Y, false)
			require.NotEmpty(t, page.updates, "the former header row now belongs to transcript input")
		}
	}
}

func TestSinglePaneDormancyAndPauseUseExistingNoticeSeam(t *testing.T) {
	root := splitTestRoot(t)
	root.composingPaneStatuses = map[string]runtime.SessionStatus{"profile": {Dormant: true}}
	require.Contains(t, ansi.Strip(root.renderMessageBar()), "Restored · paused · /resume")
	require.NotContains(t, ansi.Strip(root.composePanes()), "Restored")
	root.composingPaneStatuses = nil
	root.sessionState.SetPauseState(service.PausePaused)
	require.Contains(t, ansi.Strip(root.renderMessageBar()), "paused")
	require.Zero(t, root.ar.ActiveCount(), "passive truth acquires no lease")
}

// viewportRecordingEditor wraps the real editor without replacing its layout,
// viewport capability, content, focus, or input behavior.
type viewportRecordingEditor struct {
	editor.Editor
	editor.ViewportLayout

	clicks []tea.MouseClickMsg
}

func (e *viewportRecordingEditor) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if click, ok := msg.(tea.MouseClickMsg); ok {
		e.clicks = append(e.clicks, click)
	}
	updated, cmd := e.Editor.Update(msg)
	e.Editor = updated.(editor.Editor)
	return e, cmd
}

func TestRootComposerAndCompletionFitBeforeFinalClipping(t *testing.T) {
	for _, shell := range []struct {
		name string
		lean bool
	}{{"full", false}, {"root-lean", true}} {
		t.Run(shell.name, func(t *testing.T) {
			root := splitTestRoot(t)
			root.leanMode = shell.lean
			recorder := &viewportRecordingEditor{Editor: root.editor, ViewportLayout: root.editor.(editor.ViewportLayout)}
			root.editor = recorder
			draft := strings.Repeat("wrapped 界 e\u0301 text ", 20) + "END"
			root.editor.SetValue(draft)
			root.editor.Focus()
			var items []completion.Item
			for i := range 24 {
				items = append(items, completion.Item{Label: fmt.Sprintf("item%02d", i), Description: strings.Repeat("long description 界 ", 30), Value: draft + strings.Repeat(" preview\n", 30)})
			}
			root.updateCompletionsCmd(completion.OpenMsg{Items: items})
			root.updateEditorCmd(completion.SelectionChangedMsg{Value: items[0].Value})
			root.viewCacheValid = false // direct fixture setup changes shell/editor outside Update
			for _, size := range [][2]int{{120, 40}, {40, 10}, {16, 7}, {2, 2}, {1, 1}, {80, 24}} {
				t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
					root.handleWindowResize(size[0], size[1])
					require.False(t, root.viewCacheValid, "accepted dimensions invalidate even an empty transcript frame")
					editorView := root.editor.View()
					_, height := root.editor.GetSize()
					require.Equal(t, height, lipgloss.Height(editorView), "preview cannot extend the allocated textarea")
					require.LessOrEqual(t, lipgloss.Width(editorView), size[0])
					require.LessOrEqual(t, root.editorTop()+height+root.contextHeight+root.messageBarHeight(), size[1], "cursor-bearing editor fits before final compositor clipping")
					textY := root.editorTop() + root.editorFrame().GetPaddingTop()
					textX := root.editorFrame().GetMarginLeft() + root.editorFrame().GetPaddingLeft()
					require.Equal(t, regionEditor, root.hitTestRegion(textY))
					require.False(t, root.composerResizeHit(textX, textY))
					before := len(recorder.clicks)
					root.handleMouseClick(tea.MouseClickMsg{X: textX, Y: textY, Button: tea.MouseLeft})
					require.Len(t, recorder.clicks, before+1)
					require.Equal(t, 0, recorder.clicks[before].X)
					require.Equal(t, 0, recorder.clicks[before].Y, "real editor receives the first text cell, not frame-relative padding")
					popup := root.completions.View()
					if popup != "" {
						require.LessOrEqual(t, lipgloss.Height(popup), root.editorTop()-root.editor.BannerHeight())
						require.LessOrEqual(t, lipgloss.Width(popup), size[0])
					}
					frame := root.View().Content
					require.Equal(t, size[1], lipgloss.Height(frame))
					require.Equal(t, frame, root.View().Content, "unchanged frame keeps its cache")
					require.Equal(t, frame, root.composeView().Content, "cached and fresh compositions agree after tiny resize")
					root.handleWindowResize(size[0], size[1])
					require.True(t, root.viewCacheValid, "identical dimensions retain the warm aggregate cache")
					require.Equal(t, frame, root.View().Content)
					require.Equal(t, draft, root.editor.Value())
				})
			}
		})
	}
}
