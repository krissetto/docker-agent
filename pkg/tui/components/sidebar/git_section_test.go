package sidebar

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/review"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func gitSidebar(t *testing.T) *model {
	t.Helper()
	m := New(animation.NewRuntime(), t.Context(), service.NewSessionState(session.New())).(*model)
	m.width, m.height = 44, 60
	return m
}

func gitSnapshot(state review.LoadState) *review.Snapshot {
	return &review.Snapshot{Generation: 1, Repositories: []review.Repository{
		{ID: "r1", Name: "docker-agent", Worktrees: []review.Worktree{{ID: "w1", Branch: "main", Current: true, State: state, Files: []review.FileChange{
			{Path: "pkg/tui/panel.go", Group: review.GroupUnstaged, Status: "M", Added: 3, Removed: 1},
			{Path: "notes.md", Group: review.GroupUntracked, Status: "?"},
		}}}},
		{ID: "r2", Name: "nested", Kind: review.RepoNested, Worktrees: []review.Worktree{{ID: "w2", Branch: "dev", State: review.StateUnscanned}}},
	}}
}

func gitView(m *model) string {
	return ansi.Strip(strings.Join(m.renderSections(m.contentWidth(false)), "\n"))
}

func drainGit(cmd tea.Cmd) []GitRequestMsg {
	if cmd == nil {
		return nil
	}
	var out []GitRequestMsg
	switch msg := cmd().(type) {
	case GitRequestMsg:
		out = append(out, msg)
	case tea.BatchMsg:
		for _, c := range msg {
			out = append(out, drainGit(c)...)
		}
	}
	return out
}

func gitLine(m *model, label string) string {
	for i, line := range m.git.lines(m.contentWidth(false)) {
		if strings.Contains(ansi.Strip(line), label) {
			return strconv.Itoa(i)
		}
	}
	return ""
}

func TestGitSectionStartsCollapsedAndRequestsOnlyOnExpand(t *testing.T) {
	m := gitSidebar(t)
	assert.NotContains(t, gitView(m), "Git changes", "hidden until the host says the workspace is reviewable")
	m.SetGitChanges(GitChanges{Available: true})
	view := gitView(m)
	assert.Contains(t, view, "Git changes")
	assert.Contains(t, view, "›", "collapsed indicator is always visible")

	reqs := drainGit(m.ActivateGitLine("0"))
	require.Equal(t, []GitRequestMsg{{Action: GitLoad}}, reqs, "the first expand asks for discovery; nothing ran before")
	assert.Contains(t, gitView(m), "not loaded", "without a scan in flight it never shows a spinner")
	m.SetGitChanges(GitChanges{Available: true, Loading: true})
	assert.Contains(t, gitView(m), "scanning…")

	m.SetGitChanges(GitChanges{Available: true, Snapshot: gitSnapshot(review.StateUnscanned)})
	view = gitView(m)
	assert.Contains(t, view, "docker-agent ⎇ main")
	assert.Contains(t, view, "nested ⎇ dev")
	assert.NotContains(t, view, "Unstaged", "repositories stay collapsed")
	assert.Contains(t, view, "├ ", "rows use the subagent tree guides")
	assert.Contains(t, view, "└ ")
}

func TestGitSectionLazyWorktreeAndRetainedExpansion(t *testing.T) {
	m := gitSidebar(t)
	m.SetGitChanges(GitChanges{Available: true})
	m.ActivateGitLine("0")
	m.SetGitChanges(GitChanges{Available: true, Snapshot: gitSnapshot(review.StateUnscanned)})

	reqs := drainGit(m.ActivateGitLine(gitLine(m, "docker-agent")))
	assert.Equal(t, []GitRequestMsg{{Action: GitLoadWorktree, WorktreeID: "w1"}}, reqs, "status loads only for the expanded worktree")

	m.SetGitChanges(GitChanges{Available: true, Snapshot: gitSnapshot(review.StateReady)})
	view := gitView(m)
	assert.Contains(t, view, "Unstaged", "the expansion survives the async snapshot")
	assert.Contains(t, view, "2 changes")
	assert.NotContains(t, view, "panel.go", "groups stay collapsed")

	m.ActivateGitLine(gitLine(m, "Unstaged"))
	assert.Contains(t, gitView(m), "M pkg/tui/panel.go")
	open := drainGit(m.ActivateGitLine(gitLine(m, "panel.go")))
	assert.Equal(t, []GitRequestMsg{{Action: GitOpen, WorktreeID: "w1", Path: "pkg/tui/panel.go", Group: review.GroupUnstaged}}, open)
}

func TestGitSectionKeyboardTraversal(t *testing.T) {
	m := gitSidebar(t)
	m.SetGitChanges(GitChanges{Available: true, Snapshot: gitSnapshot(review.StateReady)})
	press := func(keys ...string) []GitRequestMsg {
		var out []GitRequestMsg
		for _, k := range keys {
			r, _ := utf8.DecodeRuneInString(k)
			msg := tea.KeyPressMsg{Code: r, Text: k}
			switch k {
			case "enter":
				msg = tea.KeyPressMsg{Code: tea.KeyEnter}
			case "right":
				msg = tea.KeyPressMsg{Code: tea.KeyRight}
			case "left":
				msg = tea.KeyPressMsg{Code: tea.KeyLeft}
			case "down":
				msg = tea.KeyPressMsg{Code: tea.KeyDown}
			}
			_, cmd := m.GitKey(msg)
			out = append(out, drainGit(cmd)...)
		}
		return out
	}
	m.RevealGit()
	assert.True(t, m.GitFocused())
	assert.Contains(t, gitView(m), "docker-agent", "reveal opens the section only")
	assert.NotContains(t, gitView(m), "Unstaged", "and never repositories")
	press("down", "right", "down", "right", "down")
	assert.Contains(t, gitView(m), "panel.go")
	open := press("enter")
	require.Len(t, open, 1)
	assert.Equal(t, GitOpen, open[0].Action)
	press("left", "left")
	assert.NotContains(t, gitView(m), "panel.go", "left collapses back up the tree")
	assert.Equal(t, []GitRequestMsg{{Action: GitRefresh}}, press("r"))
}

func TestGitSectionClickTargetsInSidebar(t *testing.T) {
	m := gitSidebar(t)
	m.SetGitChanges(GitChanges{Available: true})
	m.renderSections(m.contentWidth(false))
	require.GreaterOrEqual(t, m.gitStart, 0)
	m.placement = nil
	result, payload := m.HandleClickType(m.layoutCfg.PaddingLeft+1, m.gitStart-m.scrollview.ScrollOffset())
	assert.Equal(t, ClickGit, result)
	assert.Equal(t, "0", payload)
}

func TestGitSectionSelectedAndHoveredRowsStayDistinct(t *testing.T) {
	m := gitSidebar(t)
	open := review.DocumentKey{WorktreeID: "w1", Path: "pkg/tui/panel.go", Group: review.GroupUnstaged}
	m.SetGitChanges(GitChanges{Available: true, Snapshot: gitSnapshot(review.StateReady), Open: open})
	m.git.expanded = map[string]bool{}
	m.git.expanded[gitSectionKey] = true
	m.git.expanded["wt:w1"] = true
	m.git.expanded["grp:w1:"+strconv.Itoa(int(review.GroupUnstaged))] = true
	m.git.rebuild()
	line := gitLine(m, "panel.go")
	require.NotEmpty(t, line)
	index, _ := strconv.Atoi(line)
	row := m.git.rows[index-2]
	selected := m.git.renderRow(row, 42, false, false)
	assert.Contains(t, selected, styles.SelectionStyle.Render("pkg/tui/panel.go"), "open file remains visibly selected without keyboard focus")
	m.git.setHovered(index)
	hovered := m.git.renderRow(row, 42, false, true)
	assert.Contains(t, ansi.Strip(hovered), "▌", "selected state remains explicit on hover")
	assert.NotContains(t, hovered, styles.SelectionStyle.Render("pkg/tui/panel.go"), "hover background differs from selection background")
}

func TestGitWatchScopeIncludesOnlyExpandedWorktrees(t *testing.T) {
	m := gitSidebar(t)
	m.SetGitChanges(GitChanges{Owner: "s1", Available: true, Snapshot: gitSnapshot(review.StateReady)})
	m.git.expanded = map[string]bool{}
	m.git.expanded[gitSectionKey] = true
	m.git.expanded["wt:w1"] = true
	m.git.rebuild()
	watch := m.GitWatchState()
	assert.True(t, watch.Open)
	assert.Equal(t, []string{"w1"}, watch.ExpandedWorktrees)
	m.git.expanded[gitSectionKey] = false
	assert.False(t, m.GitWatchState().Open)
}
