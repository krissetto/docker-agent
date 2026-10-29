package chat

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/review"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type fakeOverlay struct {
	w, h, x, y int
	clicks     int
	wheels     int
}

func (o *fakeOverlay) View() string {
	return strings.TrimSuffix(strings.Repeat("DIFF INSPECTOR\n", max(1, o.h)), "\n")
}
func (o *fakeOverlay) SetSize(w, h int)     { o.w, o.h = w, h }
func (o *fakeOverlay) SetPosition(x, y int) { o.x, o.y = x, y }
func (o *fakeOverlay) HandleMouse(msg tea.MouseMsg) (bool, tea.Cmd) {
	m := msg.Mouse()
	if m.X < o.x || m.X >= o.x+o.w || m.Y < o.y || m.Y >= o.y+o.h {
		return false, nil
	}
	if _, click := msg.(tea.MouseClickMsg); click {
		o.clicks++
	}
	if _, wheel := msg.(tea.MouseWheelMsg); wheel {
		o.wheels++
	}
	return true, nil
}

func gitTestPage(t *testing.T) *chatPage {
	t.Helper()
	sess := session.New(session.WithID("git-page"))
	a, _ := newSessionTestApp(t, sess, nil, nil)
	ar := animation.NewRuntimeWithScheduler(&referenceHoverScheduler{now: time.Unix(1, 0)})
	p := New(ar, t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	t.Cleanup(func() { Cleanup(p); ar.Stop() })
	p.SetSize(160, 40)
	p.resetProjection(runtime.SessionSnapshot{Session: sess})
	return p
}

func gitRequests(cmd tea.Cmd) []sidebar.GitRequestMsg {
	if cmd == nil {
		return nil
	}
	var out []sidebar.GitRequestMsg
	switch msg := cmd().(type) {
	case sidebar.GitRequestMsg:
		out = append(out, msg)
	case tea.BatchMsg:
		for _, c := range msg {
			out = append(out, gitRequests(c)...)
		}
	}
	return out
}

func (p *chatPage) clickSidebarLine(t *testing.T, label string) tea.Cmd {
	t.Helper()
	settleSidebarGesture(t, p)
	x := styles.AppPadding + p.computeSidebarLayout().sidebarStartX + sidebar.DefaultLayoutConfig().PaddingLeft + 3
	y := renderedLineContaining(t, p.sidebar.View(), label)
	_, cmd := p.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	return cmd
}

func TestGitSidebarClicksOpenFileAndInspectorKeepsSidebar(t *testing.T) {
	p := gitTestPage(t)
	p.SetGitChanges(sidebar.GitChanges{Available: true})
	reqs := gitRequests(p.clickSidebarLine(t, "Files"))
	require.Equal(t, []sidebar.GitRequestMsg{{Action: sidebar.GitBrowse}}, reqs, "the Files label opens one folder listing, never a repository scan")

	snap := &review.Snapshot{Generation: 1, Repositories: []review.Repository{{ID: "r", Name: "work", Worktrees: []review.Worktree{{ID: "w", Branch: "main", State: review.StateReady, Files: []review.FileChange{{Path: "main.go", Group: review.GroupUnstaged, Status: "M", Added: 1}}}}}}}
	p.SetGitChanges(sidebar.GitChanges{Available: true, Snapshot: snap})
	section, ok := p.gitSidebar()
	require.True(t, ok)
	section.SetGitMode(review.ModeChanges)
	section.RevealGit()
	p.clickSidebarLine(t, "work")
	p.clickSidebarLine(t, "work  ⎇ main")
	p.clickSidebarLine(t, "Unstaged")
	reqs = gitRequests(p.clickSidebarLine(t, "main.go"))
	require.Equal(t, []sidebar.GitRequestMsg{{Action: sidebar.GitOpen, WorktreeID: "w", Path: "main.go", Group: review.GroupUnstaged}}, reqs)

	overlay := &fakeOverlay{}
	p.SetContentOverlay(overlay)
	view := ansi.Strip(p.View())
	assert.Contains(t, view, "DIFF INSPECTOR", "the inspector replaces the transcript area")
	assert.Contains(t, view, "work  ⎇ main", "the normal sidebar stays")
	sl := p.computeSidebarLayout()
	assert.Equal(t, sl.chatWidth, overlay.w, "sized exactly like the transcript")

	p.Update(tea.MouseClickMsg{X: overlay.x + 2, Y: 3, Button: tea.MouseLeft})
	assert.Equal(t, 1, overlay.clicks, "clicks inside the transcript area go to the inspector")
	p.Update(msgtypes.WheelCoalescedMsg{X: overlay.x + 2, Y: 3, Delta: 1})
	assert.Equal(t, 1, overlay.wheels, "wheels inside the transcript area go to the inspector")
	clicks, wheels := overlay.clicks, overlay.wheels
	sl = p.computeSidebarLayout()
	sidebarX := styles.AppPadding + sl.sidebarStartX + 2
	_, sidebarCmd := p.Update(msgtypes.WheelCoalescedMsg{X: sidebarX, Y: 2, Delta: 1})
	_ = sidebarCmd
	assert.Equal(t, clicks, overlay.clicks, "sidebar clicks are not consumed by the inspector")
	assert.Equal(t, wheels, overlay.wheels, "sidebar wheels are not consumed by the inspector")
	p.clickSidebarLine(t, "Unstaged")
	assert.Equal(t, clicks, overlay.clicks, "sidebar clicks still reach the sidebar")

	p.SetContentOverlay(nil)
	assert.NotContains(t, ansi.Strip(p.View()), "DIFF INSPECTOR")
}

func TestGitSidebarCompactModeHitTargets(t *testing.T) {
	p := gitTestPage(t)
	p.SetGitChanges(sidebar.GitChanges{Available: true, Snapshot: &review.Snapshot{Repositories: []review.Repository{{ID: "r", Name: "repo", Worktrees: []review.Worktree{{ID: "w", State: review.StateReady}}}}}})
	view := ansi.Strip(p.sidebar.View())
	lines := strings.Split(view, "\n")
	headerY := -1
	for i, line := range lines {
		if strings.Contains(line, "Files - Git") {
			headerY = i
			break
		}
	}
	require.GreaterOrEqual(t, headerY, 0)
	base := styles.AppPadding + p.computeSidebarLayout().sidebarStartX + sidebar.DefaultLayoutConfig().PaddingLeft
	filesX, gitX := base+1, base+ansi.StringWidth("Files - ")+1
	_, files := p.handleSidebarClickType(filesX, headerY)
	_, git := p.handleSidebarClickType(gitX, headerY)
	assert.Equal(t, "mode:browse", files, "the collapsed Files label opens Files mode")
	assert.Equal(t, "mode:changes", git, "the collapsed Git label opens Changes mode")
	p.clickSidebarLine(t, "Files - Git")
	settleSidebarGesture(t, p)
	_, files = p.handleSidebarClickType(filesX, headerY)
	_, git = p.handleSidebarClickType(gitX, headerY)
	assert.Equal(t, "mode:browse", files)
	assert.Equal(t, "mode:changes", git)

	requests := gitRequests(p.clickSidebarLine(t, "Files"))
	assert.NotContains(t, requests, sidebar.GitRequestMsg{Action: sidebar.GitBrowse}, "the active Files label collapses instead of requesting the mode again")
	section, ok := p.gitSidebar()
	require.True(t, ok)
	section.SetGitMode(review.ModeFiles)
	view = ansi.Strip(p.sidebar.View())
	assert.Contains(t, view, "Files - Git")
	assert.NotContains(t, view, "Changes(0)")
	assert.NotContains(t, view, "Browse")
	_, cmd := p.handleMouseClick(tea.MouseClickMsg{X: filesX, Y: headerY, Button: tea.MouseLeft})
	assert.Empty(t, gitRequests(cmd), "the selected Files label opens details without another mode request")
	_, cmd = p.handleMouseClick(tea.MouseClickMsg{X: gitX, Y: headerY, Button: tea.MouseLeft})
	assert.Contains(t, gitRequests(cmd), sidebar.GitRequestMsg{Action: sidebar.GitShowChanges}, "Git label switches mode while the header remains available")
}
