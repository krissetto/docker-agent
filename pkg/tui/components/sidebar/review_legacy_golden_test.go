package sidebar

import (
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/review"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const reviewLegacyGolden = "testdata/review_legacy_rows.golden"

// Non-docker-next themes must render exactly as before: resting review rows keep the
// legacy whole-row FadeLine(.55), and agent identity hover keeps the legacy +0.25 lift.
// The golden is generated from the parent commit (UPDATE_GOLDEN=1), not the current tree.
func TestNonDockerNextReviewAndAgentRowsMatchParentGolden(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates globals.
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original); styles.SetAgentOrder(nil) })
	got := renderLegacyGoldenRows(t)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		require.NoError(t, os.WriteFile(reviewLegacyGolden, []byte(got), 0o644))
	}
	want, err := os.ReadFile(reviewLegacyGolden)
	require.NoError(t, err, "run UPDATE_GOLDEN=1 on the parent commit to capture the golden")
	require.Equal(t, string(want), got)
}

func renderLegacyGoldenRows(t *testing.T) string {
	t.Helper()
	refs, err := styles.ListThemeRefs()
	require.NoError(t, err)
	sort.Strings(refs)
	var b strings.Builder
	for _, ref := range refs {
		if !styles.IsBuiltinTheme(ref) || strings.HasPrefix(ref, "docker-next") {
			continue
		}
		theme, err := styles.LoadTheme(ref)
		require.NoError(t, err)
		styles.ApplyTheme(theme)

		m := gitSidebar(t)
		snap := gitSnapshot(review.StateReady)
		snap.Repositories[0].Worktrees[0].Files = append(snap.Repositories[0].Worktrees[0].Files,
			review.FileChange{Path: "pkg/tui/view.go", Group: review.GroupUnstaged, Status: "M", Added: 2})
		m.SetGitChanges(GitChanges{Available: true, Snapshot: snap, Open: review.DocumentKey{WorktreeID: "w1", Path: "pkg/tui/panel.go", Group: review.GroupUnstaged}})
		m.git.expanded = map[string]bool{gitSectionKey: true, "repo:r1": true, "wt:w1": true, "grp:w1:" + strconv.Itoa(int(review.GroupUnstaged)): true}
		m.git.rebuild()
		width := m.contentWidth(false)
		resting, _ := strconv.Atoi(gitLine(m, "view.go"))
		open, _ := strconv.Atoi(gitLine(m, "panel.go"))
		key := "git:" + m.git.rowID(resting)
		emit := func(id, s string) { fmt.Fprintf(&b, "%s\t%s\t%s\n", ref, id, hex.EncodeToString([]byte(s))) }
		emit("review.resting", m.git.line(width, resting))
		emit("review.resting.hover", m.git.line(width, resting, map[string]hoverValue{key: {value: 1, target: 1}}))
		emit("review.open", m.git.line(width, open))

		names := make([]string, len(theme.Colors.AgentHues))
		for i := range names {
			names[i] = fmt.Sprintf("agent%d", i)
		}
		styles.SetAgentOrder(names)
		for i, name := range names {
			emit(fmt.Sprintf("agent.slot%02d.identity", i), styles.AgentIdentityStyle(name, false).Render(name))
			emit(fmt.Sprintf("agent.slot%02d.identity-hover", i), styles.AgentIdentityStyle(name, true).Render(name))
		}
		styles.SetAgentOrder(nil)
	}
	return b.String()
}
