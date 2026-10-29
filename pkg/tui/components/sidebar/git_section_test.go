package sidebar

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
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
		{ID: "r1", Name: "docker-agent", Worktrees: []review.Worktree{{ID: "w1", Path: ".", Branch: "main", Current: true, State: state, CountsState: state, Files: []review.FileChange{
			{Path: "pkg/tui/panel.go", Group: review.GroupUnstaged, Status: "M", Added: 3, Removed: 1},
			{Path: "notes.md", Group: review.GroupUntracked, Status: "?"},
		}}}},
		{ID: "r2", Name: "nested", Kind: review.RepoNested, Worktrees: []review.Worktree{{ID: "w2", Path: "worktrees/dev", Branch: "dev", Linked: true, State: review.StateUnscanned}}},
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
	assert.Contains(t, view, "Files - Git")
	assert.Contains(t, view, "›", "collapsed indicator is always visible")
	assert.NotContains(t, view, "Changes")
	assert.NotContains(t, view, "Browse")

	reqs := drainGit(m.ActivateGitLine("0"))
	require.Equal(t, []GitRequestMsg{{Action: GitLoad}}, reqs, "the first expand asks for discovery; nothing ran before")
	assert.Contains(t, gitView(m), "not loaded", "without a scan in flight it never shows a spinner")
	m.SetGitChanges(GitChanges{Available: true, Loading: true})
	assert.Contains(t, gitView(m), "⠋")
	assert.NotContains(t, gitView(m), "scanning", "the animation itself signals batch progress")

	m.SetGitChanges(GitChanges{Available: true, Snapshot: gitSnapshot(review.StateUnscanned)})
	view = gitView(m)
	assert.Contains(t, view, "Files - Git · 2")
	assert.Contains(t, view, "docker-agent")
	assert.Contains(t, view, "nested")
	assert.NotContains(t, view, "Unstaged", "repositories stay collapsed")
	assert.NotContains(t, view, "├ ", "top-level repositories sit flush like root subagents")
	assert.NotContains(t, view, "└ ")
}

func TestGitCompactRowModesAndEmphasis(t *testing.T) {
	m := gitSidebar(t)
	m.SetGitChanges(GitChanges{Available: true, Snapshot: gitSnapshot(review.StateReady)})
	lines := m.git.lines(m.contentWidth(false))
	require.Len(t, lines, 1, "the collapsed section is a single row")
	assert.Contains(t, ansi.Strip(lines[0]), "Files - Git")
	assert.NotContains(t, ansi.Strip(lines[0]), "Changes")
	assert.NotContains(t, ansi.Strip(lines[0]), "Browse")
	assert.Contains(t, ansi.Strip(lines[0]), "· 2", "the compact badge counts repository groups")
	assert.NotContains(t, ansi.Strip(lines[0]), "Changes")

	requests := drainGit(m.ActivateGitLine("mode:browse"))
	assert.Equal(t, review.ModeFiles, m.git.data.Mode)
	assert.Contains(t, requests, GitRequestMsg{Action: GitBrowse})
	assert.True(t, m.git.open(), "selecting Files expands directly into Browse")
	lines = m.git.lines(m.contentWidth(false))
	assert.Contains(t, ansi.Strip(lines[0]), "Files - Git · 2")
	assert.NotContains(t, ansi.Strip(lines[0]), "Changes")
	assert.NotContains(t, strings.Join(lines, "\n"), "Browse")
	_ = drainGit(m.ActivateGitLine("mode:browse"))
	assert.False(t, m.git.open(), "the active tab collapses the section")

	lines = m.git.lines(m.contentWidth(false))
	require.Len(t, lines, 1)
	assert.Contains(t, ansi.Strip(lines[0]), "Files - Git · 2")

	_ = drainGit(m.ActivateGitLine("mode:browse"))
	assert.True(t, m.git.open(), "the selected compact label opens the section")
	_ = drainGit(m.ActivateGitLine("0"))
	requests = drainGit(m.ActivateGitLine("mode:changes"))
	assert.Contains(t, requests, GitRequestMsg{Action: GitShowChanges})
	assert.Equal(t, review.ModeChanges, m.git.data.Mode)
	assert.True(t, m.git.open(), "selecting Git expands directly into Changes")
}

func TestCollapsedGitHeaderCountsCanonicalRepositoriesOnly(t *testing.T) {
	m := gitSidebar(t)
	snap := gitSnapshot(review.StateReady)
	snap.Repositories = append(snap.Repositories, review.Repository{ID: "fallback", Name: "workspace", Kind: review.RepoNone, Worktrees: []review.Worktree{{ID: "directory", Path: ".", State: review.StateReady}}})
	m.SetGitChanges(GitChanges{Available: true, Snapshot: snap})
	header := ansi.Strip(m.git.lines(m.contentWidth(false))[0])
	assert.Contains(t, header, "Git · 2", "linked worktrees remain children of their single repository group")
	assert.NotContains(t, header, "Git · 3", "a non-Git Files workspace is not a repository")
}

func TestGitHeaderHoverKeepsInactiveModeDimmed(t *testing.T) {
	for _, mode := range []review.Mode{review.ModeFiles, review.ModeChanges} {
		m := gitSidebar(t)
		m.SetGitChanges(GitChanges{Available: true, Mode: mode})
		m.git.focused = false
		m.git.setHovered(-1)
		before := m.git.summaryFrame(40, "")
		m.git.setHovered(0)
		// Production hover reads the shared ramp from hoverValues; a bare setHovered has no progress.
		after := m.git.summaryFrame(40, "", map[string]hoverValue{"git:git:summary": {value: 1, target: 1}})
		assert.NotEqual(t, before, after)
		assert.Equal(t, ansi.Strip(before), ansi.Strip(after))
		assert.Contains(t, ansi.Strip(after), "Files - Git", "collapsed header hover preserves the whole label")
		assert.NotContains(t, after, styles.MutedStyle.Render("Files"), "collapsed header does not leave one mode dimmed")
		assert.NotContains(t, after, styles.MutedStyle.Render("Git"), "collapsed header does not leave one mode dimmed")
		m.git.expanded = map[string]bool{gitSectionKey: true}
		m.git.setHovered(-1)
		expanded := m.git.summaryFrame(40, "")
		selected, inactive := "Git", "Files"
		if mode == review.ModeFiles {
			selected, inactive = "Files", "Git"
		}
		assert.Contains(t, expanded, styles.TabPrimaryStyle.Render(selected))
		assert.Contains(t, expanded, styles.MutedStyle.Render(inactive), "expanded mode choice stays dimmed")
	}
}

func gitTreeLines(m *model) []string {
	var out []string
	for _, line := range m.git.lines(m.contentWidth(false)) {
		out = append(out, strings.TrimRight(ansi.Strip(line), " "))
	}
	return out
}

func TestGitSectionGuidesOnlyBelowTopLevel(t *testing.T) {
	m := gitSidebar(t)
	snap := gitSnapshot(review.StateReady)
	snap.Repositories[1].Worktrees = append([]review.Worktree{{ID: "w4", Path: ".", Branch: "main", State: review.StateReady, CountsState: review.StateReady}}, snap.Repositories[1].Worktrees...)
	snap.Repositories[1].Worktrees = append(snap.Repositories[1].Worktrees, review.Worktree{ID: "w3", Path: "nested/feature", Branch: "feature", Linked: true, State: review.StateReady, CountsState: review.StateReady, Files: []review.FileChange{{Path: "a.go", Group: review.GroupStaged, Status: "A", Added: 7}}})
	snap.Repositories[1].HiddenOutside = 1
	m.SetGitChanges(GitChanges{Available: true, Snapshot: snap})
	m.git.expanded = map[string]bool{gitSectionKey: true, "repo:r1": true, "wt:w1": true, "grp:w1:" + strconv.Itoa(int(review.GroupUnstaged)): true, "repo:r2": true, "wt:w3": true, "grp:w3:" + strconv.Itoa(int(review.GroupStaged)): true}
	m.git.rebuild()
	lines := gitTreeLines(m)
	require.GreaterOrEqual(t, len(lines), 13)
	prefix := func(i int) string { return lines[i][:min(len(lines[i]), 10)] }
	assert.True(t, strings.HasPrefix(lines[1], "  docker-agent"), "repository directory is top-level: %q", lines[1])
	assert.True(t, strings.HasPrefix(lines[2], "  └ ● docker-agent"), "the main checkout is nested beneath its directory: %q", lines[2])
	assert.True(t, strings.HasPrefix(lines[3], "    ├ Unstaged"), "groups branch from the worktree: %q", lines[3])
	assert.True(t, strings.HasPrefix(lines[4], "    │ └ M pkg/tui/panel.go"), "files branch from the group: %q", lines[4])
	assert.True(t, strings.HasPrefix(lines[5], "    └ Untracked"), prefix(5))
	assert.True(t, strings.HasPrefix(lines[6], "  nested"), "multi-worktree repository row has no guide: %q", lines[6])
	assert.True(t, strings.HasPrefix(lines[7], "  ├ nested"), prefix(7))
	assert.Contains(t, lines[7], "⎇ main", "the main checkout remains a child even when it is the only visible checkout")
	assert.True(t, strings.HasPrefix(lines[8], "  ├ worktrees/dev"), prefix(8))
	assert.True(t, strings.HasPrefix(lines[9], "  └ nested/feature"), prefix(9))
	assert.True(t, strings.HasPrefix(lines[10], "    └ Staged"), prefix(10))
	assert.True(t, strings.HasPrefix(lines[11], "      └ A a.go"), prefix(11))
	assert.True(t, strings.HasPrefix(lines[12], "    ⊘ +1 outside"), prefix(12))
}

func TestGitSectionAggregatesLineCountsWhenKnown(t *testing.T) {
	m := gitSidebar(t)
	snap := gitSnapshot(review.StateReady)
	snap.Repositories[0].Worktrees[0].Files = append(snap.Repositories[0].Worktrees[0].Files,
		review.FileChange{Path: "pkg/tui/view.go", Group: review.GroupUnstaged, Status: "M", Added: 10, Removed: 4},
		review.FileChange{Path: "logo.png", Group: review.GroupUnstaged, Status: "M", Binary: true})
	m.SetGitChanges(GitChanges{Available: true, Snapshot: snap})
	m.git.expanded = map[string]bool{gitSectionKey: true, "repo:r1": true, "wt:w1": true, "grp:w1:" + strconv.Itoa(int(review.GroupUnstaged)): true}
	m.git.rebuild()
	width := m.contentWidth(false)
	raw := m.git.lines(width)
	lines := gitTreeLines(m)
	assert.Regexp(t, `docker-agent  ⎇ main\s+4 \+13 −5 ⌄$`, lines[2], "worktree row sums every group")
	assert.Regexp(t, `Unstaged\s+3 \+13 −5 ⌄$`, lines[3], "group row sums its files; the binary adds nothing")
	assert.Regexp(t, `panel.go\s+\+3 −1$`, lines[4])
	assert.Regexp(t, `view.go\s+\+10 −4$`, lines[5])
	assert.Regexp(t, `logo.png\s+bin$`, lines[6], "binary files keep the review panel's marker")
	assert.Regexp(t, `Untracked\s+1 ›$`, lines[7], "no deltas when the sums are zero")
	assert.Contains(t, raw[2], styles.SuccessStyle.Render("+13"))
	assert.Contains(t, raw[2], styles.ErrorStyle.Render("−5"))
	for _, line := range raw {
		assert.LessOrEqual(t, ansi.StringWidth(line), width, "rows stay inside the sidebar: %q", ansi.Strip(line))
	}

	narrowRow := m.git.renderRow(m.git.rows[3], 18, false, 0)
	narrow := ansi.Strip(narrowRow)
	assert.Equal(t, 18, ansi.StringWidth(narrow), "a narrow row keeps its stats and elides the path: %q", narrow)
	assert.Contains(t, narrow, "+3 −1")
	assert.Contains(t, narrow, "…")
	assert.NotContains(t, strings.TrimRight(narrow, " "), "−…", "the tail never clips the semantic count")
}

func TestGitFileLineCountsStayInlineAndSemanticWhenNarrow(t *testing.T) {
	m := gitSidebar(t)
	snap := gitSnapshot(review.StateReady)
	file := snap.Repositories[0].Worktrees[0].Files[0]
	r := gitRow{kind: gitRowFile, guides: "    ", worktree: "w1", file: file, counts: true}

	wide := m.git.renderRow(r, 64, false, 0)
	assert.Contains(t, ansi.Strip(wide), "M pkg/tui/panel.go +3 −1", "line deltas sit beside the filename rather than at the row edge")
	assert.Contains(t, wide, styles.SuccessStyle.Render("+3"))
	assert.Contains(t, wide, styles.ErrorStyle.Render("−1"))

	narrow := m.git.renderRow(r, 18, false, 0)
	plain := ansi.Strip(narrow)
	assert.Equal(t, 18, ansi.StringWidth(narrow))
	assert.Contains(t, plain, "+3 −1", "both line counts remain visible when the filename is truncated")
	assert.Contains(t, narrow, styles.SuccessStyle.Render("+3"))
	assert.Contains(t, narrow, styles.ErrorStyle.Render("−1"))
	assert.NotContains(t, plain, "pkg/tui/panel.go", "the filename yields before semantic statistics")
	assert.NotContains(t, strings.TrimRight(plain, " "), "−…", "stats are not truncated as if they were a right tail")
	multi := m.git.renderRow(gitRow{kind: gitRowFile, guides: "    ", file: review.FileChange{Path: "a-very-long-name.go", Status: "M", Added: 123, Removed: 45}, counts: true}, 22, false, 0)
	assert.Contains(t, ansi.Strip(multi), "+123 −45", "multi-digit counts stay complete at the narrow boundary")
	assert.NotContains(t, strings.TrimRight(ansi.Strip(multi), " "), "−…", "multi-digit stats are not ellipsized")

	unknown := m.git.renderRow(gitRow{kind: gitRowFile, guides: "    ", file: file, counts: false}, 64, false, 0)
	assert.NotContains(t, ansi.Strip(unknown), "+3")
	assert.NotContains(t, ansi.Strip(unknown), "−1")
	binary := m.git.renderRow(gitRow{kind: gitRowFile, guides: "    ", file: review.FileChange{Path: "image.png", Status: "M", Binary: true}, counts: true}, 64, false, 0)
	assert.Contains(t, ansi.Strip(binary), "image.png bin")
	assert.NotContains(t, ansi.Strip(binary), "+0", "binary metadata is not represented as zero line counts")
}

func TestGitSectionHidesLineCountsUntilKnown(t *testing.T) {
	m := gitSidebar(t)
	snap := gitSnapshot(review.StateReady)
	snap.Repositories[0].Worktrees[0].CountsState = review.StateLoading
	snap.Repositories[0].Worktrees[0].Files[1].Binary = true
	m.SetGitChanges(GitChanges{Available: true, Snapshot: snap})
	m.git.expanded = map[string]bool{gitSectionKey: true, "repo:r1": true, "wt:w1": true, "grp:w1:" + strconv.Itoa(int(review.GroupUnstaged)): true, "grp:w1:" + strconv.Itoa(int(review.GroupUntracked)): true}
	m.git.rebuild()
	lines := gitTreeLines(m)
	assert.Regexp(t, `docker-agent  ⎇ main\s+2 ⌄$`, lines[2], "line totals stay hidden until complete")
	assert.Regexp(t, `Unstaged\s+1 ⌄$`, lines[3])
	assert.Regexp(t, `panel.go$`, lines[4])
	assert.Regexp(t, `Untracked\s+1`, lines[5])
	assert.Regexp(t, `notes.md$`, lines[6], "untracked binary flags await metadata")
	assert.Contains(t, ansi.Strip(lines[0]), "Files - Git · 2", "the compact header counts repositories only")
	assert.Contains(t, lines[0], m.spinner.RawFrame(), "the one header communicates pending count metadata")
	assert.NotContains(t, lines[4], "bin")
}

func TestGitSectionHeaderSpinnerTracksPendingChanges(t *testing.T) {
	m := gitSidebar(t)
	snap := gitSnapshot(review.StateReady)
	m.SetGitChanges(GitChanges{Available: true, Snapshot: snap, Scanning: map[string]bool{"w2": true}})
	m.git.expanded = map[string]bool{gitSectionKey: true}
	m.git.rebuild()
	m.invalidateCache()
	view := gitView(m)
	lines := strings.Split(view, "\n")
	var header string
	for _, line := range lines {
		if strings.Contains(line, "Files - Git") {
			header = line
			break
		}
	}
	assert.NotEmpty(t, header)
	assert.Contains(t, header, m.spinner.RawFrame(), "pending status shows a spinner in the one header")
	assert.NotContains(t, view, "Changes(", "the old count/tab row is gone")
	assert.NotContains(t, view, "Browse", "the old second mode row is gone")

	snap.Repositories[1].Worktrees[0].State = review.StateReady
	snap.Repositories[1].Worktrees[0].CountsState = review.StateReady
	m.SetGitChanges(GitChanges{Available: true, Snapshot: snap})
	view = gitView(m)
	lines = strings.Split(view, "\n")
	for _, line := range lines {
		if strings.Contains(line, "Files - Git") {
			assert.NotContains(t, line, "⠋", "spinner clears when status loading ends")
		}
	}
	assert.NotContains(t, view, "Partial", "partial status remains in expanded detail only")
}

func TestGitSectionLineMatchesLines(t *testing.T) {
	m := gitSidebar(t)
	m.SetGitChanges(GitChanges{Available: true, Snapshot: gitSnapshot(review.StateReady)})
	m.git.expanded = map[string]bool{gitSectionKey: true, "repo:r1": true, "wt:w1": true, "grp:w1:" + strconv.Itoa(int(review.GroupUnstaged)): true}
	m.git.rebuild()
	m.git.focused, m.git.cursor = true, 2
	m.git.setHovered(3)
	width := m.contentWidth(false)
	lines := m.git.lines(width)
	require.Len(t, lines, len(m.git.displayRows)+1)
	for i, want := range lines {
		assert.Equal(t, want, m.git.line(width, i), "line %d", i)
	}
	assert.Equal(t, "", m.git.line(width, len(lines)), "out-of-range lines render empty")
	assert.NotEqual(t, ansi.Strip(lines[3]), lines[3], "the hovered row is styled")
}

func TestGitSectionLazyWorktreeAndRetainedExpansion(t *testing.T) {
	m := gitSidebar(t)
	m.SetGitChanges(GitChanges{Available: true})
	m.ActivateGitLine("0")
	m.SetGitChanges(GitChanges{Available: true, Snapshot: gitSnapshot(review.StateUnscanned)})

	m.ActivateGitLine(gitLine(m, "docker-agent"))
	reqs := drainGit(m.ActivateGitLine(gitLine(m, "● docker-agent")))
	assert.Equal(t, []GitRequestMsg{{Action: GitLoadWorktree, WorktreeID: "w1"}}, reqs, "status loads only for the expanded worktree")

	m.SetGitChanges(GitChanges{Available: true, Snapshot: gitSnapshot(review.StateReady)})
	view := gitView(m)
	assert.Contains(t, view, "Unstaged", "the expansion survives the async snapshot")
	assert.Contains(t, view, "Files - Git", "the compact header remains singular while another worktree is unscanned")
	assert.NotContains(t, view, "Changes(")
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
	press("down", "right", "down", "right", "down", "right", "down")
	assert.Contains(t, gitView(m), "panel.go")
	open := press("enter")
	require.Len(t, open, 1)
	assert.Equal(t, GitOpen, open[0].Action)
	press("left", "left")
	assert.NotContains(t, gitView(m), "panel.go", "left collapses back up the tree")
	assert.Equal(t, []GitRequestMsg{{Action: GitRefresh}}, press("r"))
}

func TestGitHeaderModeLabelsCollapseAndSwitchSections(t *testing.T) {
	for _, mode := range []review.Mode{review.ModeFiles, review.ModeChanges} {
		t.Run(map[review.Mode]string{review.ModeFiles: "files", review.ModeChanges: "changes"}[mode], func(t *testing.T) {
			m := gitSidebar(t)
			m.git.expanded = map[string]bool{gitSectionKey: true}
			m.SetGitChanges(GitChanges{Available: true, Snapshot: gitSnapshot(review.StateReady), Mode: mode})
			m.renderSections(m.contentWidth(false))
			m.placement = nil
			lines := m.git.lines(m.contentWidth(false))
			assert.Contains(t, ansi.Strip(lines[0]), "Files - Git")
			filesMode, filesHit := m.git.modeAtX(1)
			gitMode, gitHit := m.git.modeAtX(ansi.StringWidth("Files - ") + 1)
			require.True(t, filesHit, "the actual Files label remains a header hit target")
			require.True(t, gitHit, "the actual Git label remains a header hit target")
			active, inactive := filesMode, gitMode
			activePayload, inactivePayload := "mode:browse", "mode:changes"
			if mode == review.ModeChanges {
				active, inactive = gitMode, filesMode
				activePayload, inactivePayload = "mode:changes", "mode:browse"
			}
			assert.Equal(t, mode, active)
			assert.NotEqual(t, mode, inactive)

			activeCol := 1
			if mode == review.ModeChanges {
				activeCol = ansi.StringWidth("Files - ") + 1
			}
			result, payload := m.HandleClickType(m.layoutCfg.PaddingLeft+activeCol, m.gitStart-m.scrollview.ScrollOffset())
			require.Equal(t, ClickGit, result)
			require.Equal(t, activePayload, payload, "the actual active word is the hit target")
			_ = drainGit(m.ActivateGitLine(payload))
			assert.False(t, m.git.open(), "clicking the active label collapses the section")
			collapsedMode, hit := m.git.modeAtX(activeCol)
			require.True(t, hit, "the actual label remains clickable while collapsed")
			assert.Equal(t, active, collapsedMode)
			result, payload = m.HandleClickType(m.layoutCfg.PaddingLeft+activeCol, m.gitStart-m.scrollview.ScrollOffset())
			require.Equal(t, ClickGit, result)
			require.Equal(t, activePayload, payload)
			_ = drainGit(m.ActivateGitLine(payload))
			assert.True(t, m.git.open(), "either mode label opens a collapsed section")
			assert.Equal(t, mode, m.git.data.Mode, "collapsed clicks select their label's mode")
			_ = drainGit(m.ActivateGitLine(activePayload))
			assert.False(t, m.git.open(), "the active label collapses again")
			_ = drainGit(m.ActivateGitLine(inactivePayload))
			assert.True(t, m.git.open(), "the inactive label switches mode and opens")
			assert.Equal(t, inactive, m.git.data.Mode)
		})
	}
}

func TestGitSectionClickTargetsInSidebar(t *testing.T) {
	m := gitSidebar(t)
	m.SetGitChanges(GitChanges{Available: true})
	m.renderSections(m.contentWidth(false))
	require.GreaterOrEqual(t, m.gitStart, 0)
	m.placement = nil
	result, payload := m.HandleClickType(m.layoutCfg.PaddingLeft+1, m.gitStart-m.scrollview.ScrollOffset())
	assert.Equal(t, ClickGit, result)
	assert.Equal(t, "mode:browse", payload, "the collapsed Files label selects Files mode")
	result, payload = m.HandleClickType(m.layoutCfg.PaddingLeft+m.contentWidth(false)-1, m.gitStart-m.scrollview.ScrollOffset())
	assert.Equal(t, ClickGit, result)
	assert.Equal(t, "0", payload, "the trailing chevron retains a dedicated expand hit target")
}

func TestGitRowsShareAnimatedHoverAndReviewDimming(t *testing.T) {
	m := gitSidebar(t)
	open := review.DocumentKey{WorktreeID: "w1", Path: "pkg/tui/panel.go", Group: review.GroupUnstaged}
	snap := gitSnapshot(review.StateReady)
	snap.Repositories[0].Worktrees[0].Files = append(snap.Repositories[0].Worktrees[0].Files,
		review.FileChange{Path: "pkg/tui/view.go", Group: review.GroupUnstaged, Status: "M", Added: 2})
	m.SetGitChanges(GitChanges{Available: true, Snapshot: snap, Open: open})
	m.git.expanded = map[string]bool{gitSectionKey: true, "repo:r1": true, "wt:w1": true, "grp:w1:" + strconv.Itoa(int(review.GroupUnstaged)): true}
	m.git.rebuild()
	firstLine, secondLine := gitLine(m, "panel.go"), gitLine(m, "view.go")
	firstIndex, _ := strconv.Atoi(firstLine)
	secondIndex, _ := strconv.Atoi(secondLine)
	first, second := m.git.displayRows[firstIndex-1], m.git.displayRows[secondIndex-1]
	width := m.contentWidth(false)

	openRow := m.git.line(width, firstIndex)
	assert.NotContains(t, ansi.Strip(openRow), "▌", "the open file has no bespoke selection bar")
	assert.NotContains(t, openRow, styles.SelectionStyle.Render("pkg/tui/panel.go"), "the open file does not use a unique selection highlight")
	assert.NotContains(t, openRow, styles.FadeLine(ansi.Strip(openRow), .55), "the open file remains at normal brightness")

	otherRow := m.git.line(width, secondIndex)
	baseOther := strings.Repeat(" ", min(2, max(0, width-1))) + m.git.renderRow(second, max(1, width-min(2, max(0, width-1))), false, 0)
	assert.Equal(t, styles.FadeLine(baseOther, .55), otherRow, "other Git entries dim while a file is reviewed")
	assert.NotContains(t, otherRow, styles.BaseStyle.Background(styles.TabHoverBg).Foreground(styles.TabHoverFg).Render("pkg/tui/view.go"), "hover does not introduce a bespoke filled background")

	key := "git:" + m.git.rowID(firstIndex)
	hoverValues := map[string]hoverValue{key: {value: .5, target: 1}}
	widthForFile := max(1, width-min(2, max(0, width-1)))
	stats := fileStats(first.file, first.counts)
	avail := max(1, widthForFile-ansi.StringWidth(first.guides)-2-ansi.StringWidth(stats)-1)
	name := truncateMiddleGit(sanitizeGit(first.file.Path), avail)
	wantHover := styles.HoverText(styles.BaseStyle.Render(name), .5, styles.TextPrimary)
	hoveredRow := m.git.line(width, firstIndex, hoverValues)
	assert.Contains(t, hoveredRow, wantHover, "Git rows use the same HoverText treatment and fractional progress as other sidebar fields")
	assert.Contains(t, hoveredRow, styles.SuccessStyle.Render("+3"), "animated hover keeps addition colors semantic")
	assert.Contains(t, hoveredRow, styles.ErrorStyle.Render("−1"), "animated hover keeps deletion colors semantic")
	assert.NotContains(t, hoveredRow, styles.SelectionStyle.Render(name))
	assert.NotContains(t, ansi.Strip(hoveredRow), "▌")

	m.git.focused, m.git.cursor = true, firstIndex
	focusedRow := m.git.line(width, firstIndex, hoverValues)
	assert.Contains(t, focusedRow, styles.HoverText(styles.HighlightWhiteStyle.Render(name), .5, styles.TextPrimary), "keyboard focus keeps its selection base and receives the shared hover ramp once")
	assert.NotContains(t, focusedRow, styles.SelectionStyle.Render(name))
	assert.NotContains(t, ansi.Strip(focusedRow), "▌")
	assert.Contains(t, focusedRow, styles.SuccessStyle.Render("+3"), "keyboard selection preserves semantic addition color")
	assert.Contains(t, focusedRow, styles.ErrorStyle.Render("−1"), "keyboard selection preserves semantic deletion color")
	summary := m.git.line(width, 0)
	assert.NotEqual(t, styles.FadeLine(summary, .55), summary, "reviewing a file does not dim the Git summary")
}

func TestGitWatchScopeIncludesOnlyExpandedWorktrees(t *testing.T) {
	m := gitSidebar(t)
	m.SetGitChanges(GitChanges{Owner: "s1", Available: true, Snapshot: gitSnapshot(review.StateReady)})
	m.git.expanded = map[string]bool{}
	m.git.expanded[gitSectionKey] = true
	m.git.expanded["repo:r1"] = true
	m.git.expanded["wt:w1"] = true
	m.git.rebuild()
	watch := m.GitWatchState()
	assert.True(t, watch.Open)
	assert.Equal(t, []string{"w1"}, watch.ExpandedWorktrees)
	m.git.expanded[gitSectionKey] = false
	assert.False(t, m.GitWatchState().Open)
}

func TestGitSectionExplainsBoundedDiscoveryAndOffersSupportedContinueAction(t *testing.T) {
	m := gitSidebar(t)
	snap := gitSnapshot(review.StateReady)
	snap.Scope.LimitReason = "depth"
	snap.Scope.LimitValue = 8
	snap.Scope.Note = "Partial scan: directory depth limit reached (8)."
	m.SetGitChanges(GitChanges{Owner: "s1", Available: true, Snapshot: snap, ContinueDiscovery: true})
	if m.git.expanded == nil {
		m.git.expanded = map[string]bool{}
	}
	m.git.expanded[gitSectionKey] = true
	m.git.rebuild()
	lines := gitTreeLines(m)
	joined := strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
	assert.Contains(t, joined, "Incomplete · depth 8")
	assert.Contains(t, joined, "Continue scan")
	assert.NotContains(t, joined, "Partial scan:")
	continueLine := -1
	for i, row := range m.git.displayRows {
		if row.kind == gitRowContinue {
			continueLine = i + 1
			break
		}
	}
	require.Positive(t, continueLine)
	requests := drainGit(m.ActivateGitLine(strconv.Itoa(continueLine)))
	require.Len(t, requests, 1)
	assert.Equal(t, GitContinueDiscovery, requests[0].Action)

	remoteSnapshot := *snap
	remoteSnapshot.Scope.Remote = true
	m.SetGitChanges(GitChanges{Available: true, Snapshot: &remoteSnapshot})
	remoteText := gitView(m)
	assert.Contains(t, strings.Join(strings.Fields(remoteText), " "), "Incomplete · depth 8 · remote")
	assert.NotContains(t, remoteText, "This remote view cannot continue discovery")
	assert.NotContains(t, remoteText, "repositories may be missing")
	assert.NotContains(t, remoteText, "Continue scan", "remote provider does not advertise unsupported continuation")

	m.SetGitChanges(GitChanges{Available: true, Snapshot: snap, DiscoveryContinuationSupported: true, ContinuingDiscovery: true})
	deeper := gitView(m)
	assert.Contains(t, deeper, "Scanning deeper…")
	assert.NotContains(t, deeper, "Continue scan", "a second continuation cannot start before the current pass ends")
}

func TestGitSectionShowsPartialDiscoveryEvenWhenNoRepositoriesWereFound(t *testing.T) {
	m := gitSidebar(t)
	snap := &review.Snapshot{Scope: review.Scope{LimitReason: "entries", LimitValue: 20_000, Note: "Partial scan: entry limit reached (20000 entries)."}}
	m.SetGitChanges(GitChanges{Available: true, Snapshot: snap, ContinueDiscovery: true})
	m.git.expanded = map[string]bool{gitSectionKey: true}
	m.git.rebuild()
	view := strings.Join(strings.Fields(strings.Join(gitTreeLines(m), " ")), " ")
	assert.Contains(t, view, "Incomplete · entries 20000")
	assert.Contains(t, view, "Continue scan")
	assert.NotContains(t, view, "no Git repositories", "a stopped partial scan must not imply complete discovery")
}

func TestGitModeTabsKeyboardAndScopedBrowseRequests(t *testing.T) {
	m := gitSidebar(t)
	m.SetGitChanges(GitChanges{Owner: "page", Available: true, Snapshot: gitSnapshot(review.StateReady)})
	m.git.expanded = map[string]bool{gitSectionKey: true}
	m.git.rebuild()
	m.invalidateCache()
	gitView(m)
	m.placement = nil
	modeY := m.gitStart
	changesX := m.layoutCfg.PaddingLeft + ansi.StringWidth("Files - ") + 1
	browseX := m.layoutCfg.PaddingLeft + 1
	result, payload := m.HandleClickType(changesX, modeY)
	require.Equal(t, ClickGit, result)
	require.Equal(t, "mode:changes", payload)
	result, payload = m.HandleClickType(browseX, modeY)
	require.Equal(t, ClickGit, result)
	require.Equal(t, "mode:browse", payload)

	requests := drainGit(m.ActivateGitLine("mode:browse"))
	require.Equal(t, review.ModeFiles, m.git.data.Mode)
	require.Contains(t, requests, GitRequestMsg{Owner: "page", Action: GitBrowse})
	require.Contains(t, requests, GitRequestMsg{Owner: "page", Action: GitLoadDirectory, WorktreeID: "w1"})
	require.NotContains(t, requests, GitRequestMsg{Owner: "page", Action: GitLoadDirectory, WorktreeID: "w2"}, "collapsed secondary worktrees stay lazy")
	require.Contains(t, ansi.Strip(gitView(m)), "Files - Git")
	require.NotContains(t, ansi.Strip(gitView(m)), "Changes(")
	require.NotContains(t, ansi.Strip(gitView(m)), "Browse")

	_, cmd := m.GitKey(tea.KeyPressMsg{Code: 'b', Text: "b"})
	requests = drainGit(cmd)
	require.Empty(t, requests, "the active Browse mode is idempotent")
	_, cmd = m.GitKey(tea.KeyPressMsg{Code: tea.KeyTab})
	requests = drainGit(cmd)
	require.Equal(t, review.ModeChanges, m.git.data.Mode)
	require.Contains(t, requests, GitRequestMsg{Owner: "page", Action: GitShowChanges})
	_, cmd = m.GitKey(tea.KeyPressMsg{Code: '2', Text: "2"})
	requests = drainGit(cmd)
	require.Equal(t, review.ModeFiles, m.git.data.Mode)
	require.Contains(t, requests, GitRequestMsg{Owner: "page", Action: GitBrowse})
}

func TestBrowseTreeAsyncDirectoryStateAndWholeFileOpen(t *testing.T) {
	m := gitSidebar(t)
	snapshot := &review.Snapshot{Repositories: []review.Repository{{ID: "r", Name: "repo", Worktrees: []review.Worktree{{ID: "wt", Path: "nested/current", Current: true, Branch: "main", State: review.StateReady, Files: []review.FileChange{{Path: "src/main.go", Status: "M"}}}}}}}
	m.SetGitChanges(GitChanges{Owner: "page", Available: true, Snapshot: snapshot})
	m.git.expanded = map[string]bool{gitSectionKey: true}
	m.git.rebuild()
	m.invalidateCache()
	request := drainGit(m.SetGitMode(review.ModeFiles))
	require.Contains(t, request, GitRequestMsg{Owner: "page", Action: GitLoadDirectory, WorktreeID: "wt"})
	require.Contains(t, request, GitRequestMsg{Owner: "page", Action: GitBrowse})
	require.False(t, m.git.expanded[browseRepoKey("r")], "Files mode does not pre-expand the repository")
	require.False(t, m.git.expanded[browseWorktreeKey("wt")], "Files mode does not pre-expand the worktree")
	require.Empty(t, drainGit(m.git.requestDirectory("wt", "")), "duplicate in-flight loads are suppressed")
	require.Contains(t, ansi.Strip(gitView(m)), "Loading…")

	m.SetGitChanges(GitChanges{Owner: "page", Available: true, Snapshot: snapshot, Mode: review.ModeFiles, Directories: []review.Directory{{WorktreeID: "wt", State: review.StateReady, Entries: []review.Entry{{Name: "src", Dir: true}}}}})
	require.False(t, m.git.directoryPending[directoryIdentity("wt", "")], "directory completion clears its pending state")
	require.Contains(t, ansi.Strip(gitView(m)), "src/")

	line := gitLine(m, "src/")
	require.NotEmpty(t, line)
	childRequests := drainGit(m.ActivateGitLine(line))
	require.Equal(t, []GitRequestMsg{{Owner: "page", Action: GitLoadDirectory, WorktreeID: "wt", Path: "src"}}, childRequests)
	m.SetGitChanges(GitChanges{Owner: "page", Available: true, Snapshot: snapshot, Mode: review.ModeFiles, Directories: []review.Directory{
		{WorktreeID: "wt", State: review.StateReady, Entries: []review.Entry{{Name: "src", Dir: true}}},
		{WorktreeID: "wt", Path: "src", State: review.StateReady, Entries: []review.Entry{{Name: "main.go"}}},
	}})
	require.Contains(t, ansi.Strip(gitView(m)), "M main.go")
	fileLine := gitLine(m, "main.go")
	opened := drainGit(m.ActivateGitLine(fileLine))
	require.Equal(t, []GitRequestMsg{{Owner: "page", Action: GitOpenFile, WorktreeID: "wt", Path: "src/main.go"}}, opened)
}

func TestActiveSubagentBranchesAutoExpandWithoutOverwritingManualChoices(t *testing.T) {
	m := gitSidebar(t)
	m.rootSessionID = "collapse"
	m.treeCollapsed = false
	snapshot := collapseFixture()
	branch := &snapshot.Nodes[0].Children[0]
	branch.Node.State = subagent.NodeIdle
	branch.Children[0].Node.State = subagent.NodeIdle
	branch.Children[0].Children[0].Node.State = subagent.NodeIdle
	m.SetSubagentTree(snapshot)
	require.True(t, m.collapsedBranches[branch.Node.ID], "inactive branch defaults collapsed")

	branch.Children[0].Node.State = subagent.NodeRunning
	m.SetSubagentTree(snapshot)
	require.False(t, m.collapsedBranches[branch.Node.ID], "an active descendant reveals its ancestors")
	branch.Children[0].Node.State = subagent.NodeIdle
	m.SetSubagentTree(snapshot)
	require.False(t, m.collapsedBranches[branch.Node.ID], "an active-to-idle update does not forcibly hide an open branch")
	branch.Children[0].Node.State = subagent.NodeRunning
	m.SetSubagentTree(snapshot)

	m.toggleTreeControl(treeControl{whole: true})
	require.True(t, m.treeCollapsed)
	m.toggleTreeControl(treeControl{whole: true})
	require.False(t, m.collapsedBranches[branch.Node.ID], "section reopen preserves active-only default for still-active descendants")
	branch.Children[0].Children[0].Node.State = subagent.NodeCompleted
	branch.Children[0].Node.State = subagent.NodeIdle
	m.SetSubagentTree(snapshot)
	m.toggleTreeControl(treeControl{whole: true})
	m.toggleTreeControl(treeControl{whole: true})
	require.True(t, m.collapsedBranches[branch.Node.ID], "once idle, section reopen restores the default collapsed state")

	// Explicit expansion is remembered after refresh/reopen.
	m.toggleTreeControl(treeControl{id: branch.Node.ID})
	require.False(t, m.collapsedBranches[branch.Node.ID])
	branch.Children[0].Node.State = subagent.NodeRunning
	m.SetSubagentTree(snapshot)
	branch.Children[0].Node.State = subagent.NodeIdle
	m.SetSubagentTree(snapshot)
	require.False(t, m.collapsedBranches[branch.Node.ID], "manual open survives an inactive refresh")
	m.toggleTreeControl(treeControl{whole: true})
	m.toggleTreeControl(treeControl{whole: true})
	require.False(t, m.collapsedBranches[branch.Node.ID], "manual open survives section reopen")

	// An explicit close wins over active descendants and later refreshes.
	m.toggleTreeControl(treeControl{id: branch.Node.ID})
	require.True(t, m.collapsedBranches[branch.Node.ID])
	branch.Children[0].Node.State = subagent.NodeRunning
	m.SetSubagentTree(snapshot)
	require.True(t, m.collapsedBranches[branch.Node.ID], "manual close is not overridden by auto expansion")
	m.toggleTreeControl(treeControl{whole: true})
	m.toggleTreeControl(treeControl{whole: true})
	require.True(t, m.collapsedBranches[branch.Node.ID], "manual close survives section reopen")
}

func TestBrowseSnapshotsPreserveManualCollapseAndDirectoryRetry(t *testing.T) {
	m := gitSidebar(t)
	snapshot := &review.Snapshot{Generation: 1, Repositories: []review.Repository{{ID: "r", Name: "repo", Worktrees: []review.Worktree{{ID: "wt", Path: "nested/current", Current: true, Branch: "main", State: review.StateReady}}}}}
	m.SetGitChanges(GitChanges{Owner: "page", Available: true, Snapshot: snapshot})
	m.git.expanded = map[string]bool{gitSectionKey: true}
	m.git.rebuild()
	requests := drainGit(m.SetGitMode(review.ModeFiles))
	require.Contains(t, requests, GitRequestMsg{Owner: "page", Action: GitLoadDirectory, WorktreeID: "wt"})

	directories := []review.Directory{
		{WorktreeID: "wt", State: review.StateReady, Entries: []review.Entry{{Name: "folder", Dir: true}, {Name: "README.md"}}},
		{WorktreeID: "wt", Path: "folder", State: review.StateReady, Entries: []review.Entry{{Name: "inside.md"}}},
	}
	m.SetGitChanges(GitChanges{Owner: "page", Available: true, Snapshot: snapshot, Mode: review.ModeFiles, Directories: directories})
	folder, ok := findBrowseDirectory(m.git.rows, "wt", "folder")
	require.True(t, ok)
	require.False(t, m.git.expanded[browseDirKey("wt", "folder")], "folders start collapsed")
	require.Contains(t, ansi.Strip(gitView(m)), "README.md", "current scope contents should render at the root without a worktree folder")
	require.NotContains(t, ansi.Strip(gitView(m)), "inside.md", "nested directories remain collapsed by default")
	_ = m.git.toggle(folder)
	m.invalidateCache()
	require.True(t, m.git.expanded[browseDirKey("wt", "folder")])
	require.Contains(t, ansi.Strip(gitView(m)), "inside.md", "explicit folder expansion reveals real contents")
	folder, ok = findBrowseDirectory(m.git.rows, "wt", "folder")
	require.True(t, ok)
	_ = m.git.toggle(folder)
	m.invalidateCache()
	require.False(t, m.git.expanded[browseDirKey("wt", "folder")])

	snapshot.Generation++
	m.SetGitChanges(GitChanges{Owner: "page", Available: true, Snapshot: snapshot, Mode: review.ModeFiles, Directories: directories})
	require.False(t, m.git.expanded[browseDirKey("wt", "folder")], "metadata and directory pushes preserve explicit collapse")
	require.Contains(t, ansi.Strip(gitView(m)), "README.md")
	require.NotContains(t, ansi.Strip(gitView(m)), "inside.md", "a collapsed directory does not repaint its cached contents")

	folder, ok = findBrowseDirectory(m.git.rows, "wt", "folder")
	require.True(t, ok)
	_ = drainGit(m.git.toggle(folder))
	m.invalidateCache()
	require.True(t, m.git.expanded[browseDirKey("wt", "folder")])
	require.Contains(t, ansi.Strip(gitView(m)), "inside.md")
	_ = drainGit(m.SetGitMode(review.ModeChanges))
	_ = drainGit(m.SetGitMode(review.ModeFiles))
	require.True(t, m.git.expanded[browseDirKey("wt", "folder")], "mode changes preserve explicit expansion")
	require.Contains(t, ansi.Strip(gitView(m)), "inside.md", "expanded contents remain visible after switching modes")
	m.SetGitChanges(GitChanges{Owner: "page", Available: true, Snapshot: snapshot, Mode: review.ModeFiles, Directories: []review.Directory{{WorktreeID: "wt", State: review.StateReady, Entries: []review.Entry{{Name: "folder", Dir: true}}}, {WorktreeID: "wt", Path: "folder", State: review.StateError, Err: "denied"}}})
	require.False(t, m.git.directoryPending[directoryIdentity("wt", "folder")], "terminal errors release the in-flight guard")
	require.Contains(t, ansi.Strip(gitView(m)), "denied")
	folder, ok = findBrowseDirectory(m.git.rows, "wt", "folder")
	require.True(t, ok)
	retry := drainGit(m.git.toggle(folder))
	require.Equal(t, []GitRequestMsg{{Owner: "page", Action: GitLoadDirectory, WorktreeID: "wt", Path: "folder"}}, retry)
	require.True(t, m.git.directoryPending[directoryIdentity("wt", "folder")])

	m.git.directoryPending[directoryIdentity("wt", "folder")] = true
	handled, cmd := m.git.key("r")
	require.True(t, handled)
	require.Equal(t, []GitRequestMsg{{Owner: "page", Action: GitRefresh}}, drainGit(cmd))
	require.Empty(t, m.git.directoryPending, "browse refresh releases directory request guards until refreshed host state arrives")
}

func findBrowseDirectory(rows []gitRow, worktreeID, path string) (gitRow, bool) {
	for _, row := range rows {
		if row.kind == gitRowBrowse && row.branch && row.worktree == worktreeID && row.path == path {
			return row, true
		}
	}
	return gitRow{}, false
}

func TestFilesRendersWorkingDirectoryContentsAsCollapsedRoot(t *testing.T) {
	m := gitSidebar(t)
	snapshot := &review.Snapshot{Repositories: []review.Repository{
		{ID: "owner", Name: "enclosing", Kind: review.RepoPrimary, Worktrees: []review.Worktree{{ID: "owner-wt", Path: "services/agent", Current: true, Branch: "main", State: review.StateReady, CountsState: review.StateReady, Files: []review.FileChange{{Path: "scope.txt", Status: "M", Added: 8, Removed: 3}}}}},
		{ID: "child", Name: "child", Kind: review.RepoNested, Worktrees: []review.Worktree{{ID: "child-wt", Path: "child", Branch: "feature", State: review.StateReady, CountsState: review.StateReady}}},
	}}
	changes := GitChanges{Owner: "session-7", Available: true, Snapshot: snapshot, Mode: review.ModeFiles, Directories: []review.Directory{
		{WorktreeID: "owner-wt", State: review.StateReady, Entries: []review.Entry{{Name: "child", Dir: true}, {Name: "docs", Dir: true}, {Name: "scope.txt"}}},
		{WorktreeID: "owner-wt", Path: "docs", State: review.StateReady, Entries: []review.Entry{{Name: "hidden.md"}}},
	}}
	m.git.expanded = map[string]bool{gitSectionKey: true}
	m.SetGitChanges(changes)

	var lines []string
	for _, line := range gitTreeLines(m) {
		lines = append(lines, strings.Join(strings.Fields(line), " "))
	}
	got := strings.Join(lines, "\n") + "\n"
	want, err := os.ReadFile("testdata/git_files_child_root.golden")
	require.NoError(t, err)
	assert.Equal(t, string(want), got)
	assert.NotContains(t, got, "enclosing", "the working directory itself is not a peer of its children")
	assert.NotContains(t, got, "services/agent", "display paths do not define the authoritative browsing root")
	assert.Equal(t, 1, strings.Count(got, "child"), "the discovered repo row replaces its duplicate physical root entry")
	assert.NotContains(t, got, "hidden.md", "directories remain collapsed until explicitly expanded")
	assert.False(t, m.git.expanded[browseRepoKey("child")], "discovered child repositories start collapsed")
	assert.Equal(t, []string{"owner-wt"}, m.GitWatchState().ExpandedWorktrees, "the implicit scope root remains covered by scoped refresh")
}

func TestBrowseDiscoveredRepositoryKeepsCollapsedWorktreeHierarchy(t *testing.T) {
	m := gitSidebar(t)
	snapshot := &review.Snapshot{Repositories: []review.Repository{{
		ID: "child", Name: "child", Kind: review.RepoNested,
		Worktrees: []review.Worktree{{ID: "child-wt", Path: "packages/child", Branch: "feature", State: review.StateReady}},
	}}}
	m.SetGitChanges(GitChanges{Owner: "session-9", Available: true, Snapshot: snapshot, Mode: review.ModeFiles})
	m.git.expanded = map[string]bool{gitSectionKey: true}
	m.git.rebuild()
	repo, ok := findBrowseRepository(m.git.rows, "child")
	require.True(t, ok)
	require.False(t, repo.open, "discovered repositories begin collapsed")
	_ = drainGit(m.git.toggle(repo))
	worktree, ok := findBrowseWorktree(m.git.rows, "child-wt")
	require.True(t, ok, "the checkout remains nested beneath its repository")
	require.False(t, worktree.open, "worktrees also begin collapsed")
	requests := drainGit(m.git.toggle(worktree))
	require.Equal(t, []GitRequestMsg{{Owner: "session-9", Action: GitLoadDirectory, WorktreeID: "child-wt"}}, requests)
	m.SetGitChanges(GitChanges{Owner: "session-9", Available: true, Snapshot: snapshot, Mode: review.ModeFiles, Directories: []review.Directory{{WorktreeID: "child-wt", State: review.StateReady, Entries: []review.Entry{{Name: "README.md"}}}}})
	assert.Contains(t, ansi.Strip(gitView(m)), "README.md", "explicitly expanding a worktree loads its real contents")
}

func TestBrowseKeepsSameNamedDirectoryInSecondaryWorktree(t *testing.T) {
	m := gitSidebar(t)
	snapshot := &review.Snapshot{Repositories: []review.Repository{
		{ID: "owner", Name: "owner", Worktrees: []review.Worktree{{ID: "current", Path: ".", Current: true, State: review.StateReady}}},
		{ID: "child", Name: "child", Kind: review.RepoNested, Worktrees: []review.Worktree{{ID: "child-wt", Path: "child", State: review.StateReady}}},
	}}
	directories := []review.Directory{
		{WorktreeID: "current", State: review.StateReady, Entries: []review.Entry{{Name: "child", Dir: true}}},
		{WorktreeID: "child-wt", State: review.StateReady, Entries: []review.Entry{{Name: "child", Dir: true}}},
	}
	m.SetGitChanges(GitChanges{Owner: "scope", Available: true, Snapshot: snapshot, Mode: review.ModeFiles, Directories: directories})
	m.git.expanded = map[string]bool{gitSectionKey: true, browseRepoKey("child"): true, browseWorktreeKey("child-wt"): true}
	m.git.rebuild()
	_, found := findBrowseDirectory(m.git.rows, "current", "child")
	require.False(t, found, "the owner's root listing defers a discovered repo directory to its repo row")
	_, found = findBrowseDirectory(m.git.rows, "child-wt", "child")
	require.True(t, found, "same-named ordinary directories inside child worktrees remain browsable")
}

func findBrowseRepository(rows []gitRow, id string) (gitRow, bool) {
	for _, row := range rows {
		if row.kind == gitRowRepo && row.key == browseRepoKey(id) {
			return row, true
		}
	}
	return gitRow{}, false
}

func TestFilesFlattensSyntheticWorkspaceRoot(t *testing.T) {
	m := gitSidebar(t)
	snapshot := &review.Snapshot{Repositories: []review.Repository{{ID: "scope", Name: "workspace", Kind: review.RepoNone, Worktrees: []review.Worktree{{ID: "directory", Path: ".", Current: true, State: review.StateReady}}}}}
	m.SetGitChanges(GitChanges{Owner: "local", Available: true, Snapshot: snapshot, Mode: review.ModeFiles, Directories: []review.Directory{{WorktreeID: "directory", State: review.StateReady, Entries: []review.Entry{{Name: "notes.md"}}}}})
	m.git.expanded = map[string]bool{gitSectionKey: true}
	m.git.rebuild()
	m.invalidateCache()
	view := ansi.Strip(gitView(m))
	assert.Contains(t, view, "notes.md")
	assert.NotContains(t, view, "workspace", "the synthetic workspace root is not rendered as a directory row")
	assert.NotContains(t, view, "directory", "the synthetic current worktree is not rendered as a directory row")
}

func findBrowseWorktree(rows []gitRow, id string) (gitRow, bool) {
	for _, row := range rows {
		if row.kind == gitRowWorktree && row.worktree == id {
			return row, true
		}
	}
	return gitRow{}, false
}

func TestDirectoryGenerationClearsOnlyPendingRequestGuards(t *testing.T) {
	m := gitSidebar(t)
	snapshot := &review.Snapshot{Repositories: []review.Repository{{ID: "r", Name: "repo", Worktrees: []review.Worktree{{ID: "wt", Path: "subdir", Current: true, State: review.StateReady}}}}}
	dirs := []review.Directory{{WorktreeID: "wt", State: review.StateReady, Entries: []review.Entry{{Name: "folder", Dir: true}, {Name: "ready.go"}}}}
	m.SetGitChanges(GitChanges{Owner: "page", Available: true, Snapshot: snapshot, Mode: review.ModeFiles, DirectoryGeneration: 10, Directories: dirs})
	m.git.expanded = map[string]bool{gitSectionKey: true, browseDirKey("wt", "folder"): false}
	m.git.rebuild()

	first := drainGit(m.git.requestDirectory("wt", "missing"))
	require.Equal(t, []GitRequestMsg{{Owner: "page", Action: GitLoadDirectory, WorktreeID: "wt", Path: "missing"}}, first)
	require.True(t, m.git.directoryPending[directoryIdentity("wt", "missing")])

	// A repeated host push at the same generation neither drops pending work
	// nor changes the user's branch choices or cached ready directory.
	m.SetGitChanges(GitChanges{Owner: "page", Available: true, Snapshot: snapshot, Mode: review.ModeFiles, DirectoryGeneration: 10, Directories: dirs})
	require.True(t, m.git.directoryPending[directoryIdentity("wt", "missing")])
	require.False(t, m.git.expanded[browseDirKey("wt", "folder")])
	require.Len(t, m.git.data.Directories, 1)
	require.Equal(t, dirs[0], m.git.data.Directories[0])

	m.SetGitChanges(GitChanges{Owner: "page", Available: true, Loading: true, Snapshot: snapshot, Mode: review.ModeFiles, DirectoryGeneration: 11})
	require.Empty(t, m.git.directoryPending, "cancel/invalidation generation releases stale guards")
	require.False(t, m.git.expanded[browseDirKey("wt", "folder")], "generation changes preserve manual collapse")

	retry := drainGit(m.git.requestDirectory("wt", "missing"))
	require.Equal(t, []GitRequestMsg{{Owner: "page", Action: GitLoadDirectory, WorktreeID: "wt", Path: "missing"}}, retry)
}
