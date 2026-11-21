package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/gitreview"
	view "github.com/docker/docker-agent/pkg/tui/components/review"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

func TestFilesInspectorEscapeClearsSidebarSelectionAndRestoresFocus(t *testing.T) {
	for _, panel := range []FocusedPanel{PanelEditor, PanelContent, PanelSidebarGit} {
		t.Run(string(panel), func(t *testing.T) {
			h := newReviewHarness(t, 140)
			h.pump(browseRequest(h, "s1", sidebar.GitBrowse))
			h.m.focusedPanel = panel
			h.page.gitFocused = panel == PanelSidebarGit
			wantPanel := panel
			if panel == PanelSidebarGit {
				wantPanel = PanelContent
			}

			for _, path := range []string{"README.md", "other.go"} {
				_, cmd := h.m.Update(sidebar.GitRequestMsg{Owner: "s1", Action: sidebar.GitOpenFile, WorktreeID: "w1", Path: path})
				h.pump(cmd)
				require.Equal(t, path, h.page.git.Open.Path)
				require.NotNil(t, h.page.overlay)
				require.False(t, h.page.GitFocused(), "only the inspector has keyboard focus")
				require.True(t, h.m.reviewModel().Focused())

				h.key("esc")
				require.Nil(t, h.page.overlay)
				require.Empty(t, h.page.git.Open.Path, "a retained Files reader must not keep sidebar selection or sibling dimming")
				require.Equal(t, wantPanel, h.m.focusedPanel)
				if wantPanel == PanelEditor {
					require.True(t, h.editor.focused)
				} else {
					require.True(t, h.page.focused)
				}
				ws := h.m.gitReviews["s1"]
				key, ok := ws.models[view.ModeFiles].OpenKey()
				require.True(t, ok, "closing preserves the cached reader")
				require.Equal(t, path, key.Path)
				h.pump(h.m.pushGitSidebar(ws))
				require.Empty(t, h.page.git.Open.Path, "a later sidebar refresh must not restore stale selection")
			}
		})
	}
}

type filesFocusProvider struct{ *fakeReviewProvider }

func (p filesFocusProvider) Directory(_ context.Context, worktree, path string) (gitreview.Directory, error) {
	return gitreview.Directory{WorktreeID: worktree, Path: path, State: gitreview.StateReady, Entries: []gitreview.DirectoryEntry{{Name: "README.md"}, {Name: "other.go"}}}, nil
}

func TestFilesSidebarClickEscapeRestoresFilenameANSI(t *testing.T) {
	h := newReviewHarness(t, 220)
	h.m.gitReviewFactory = func(*app.App, string, string) (gitreview.Provider, *view.Snapshot, error) {
		return filesFocusProvider{h.provider}, nil, nil
	}
	page := withRealPage(t, h)
	h.pump(h.m.syncGitReview())
	h.clickText("Files")

	filenameRows := func() []string {
		t.Helper()
		_, cmd := h.m.Update(tea.MouseMotionMsg{X: 2, Y: 2})
		h.pump(cmd)
		h.settle()
		var rows []string
		for _, row := range strings.Split(page.(chat.SplitPresentation).SidebarView(), "\n") {
			plain := ansi.Strip(row)
			if strings.Contains(plain, "README.md") || strings.Contains(plain, "other.go") {
				rows = append(rows, row)
			}
		}
		require.Len(t, rows, 2, "both Files rows are rendered")
		return rows
	}
	before := filenameRows()
	for _, path := range []string{"README.md", "other.go"} {
		h.clickText(path)
		require.True(t, h.m.activeReview().inspecting)
		require.NotEqual(t, before, filenameRows(), "the open file is visibly selected and its sibling dimmed")
		h.key("esc")
		require.Equal(t, before, filenameRows(), "Escape restores the exact original filename ANSI, not merely its text")
	}
}
