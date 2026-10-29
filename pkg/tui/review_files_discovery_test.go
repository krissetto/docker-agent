package tui

import (
	"context"
	"fmt"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/gitreview"
	view "github.com/docker/docker-agent/pkg/tui/components/review"
)

// discoveryLimitedReviewProvider reports a bounded discovery that found many
// sibling repositories, none of them browsed yet.
type discoveryLimitedReviewProvider struct{ *fakeReviewProvider }

func (p discoveryLimitedReviewProvider) Snapshot(ctx context.Context) (gitreview.Snapshot, error) {
	snapshot, err := p.fakeReviewProvider.Snapshot(ctx)
	if err != nil {
		return snapshot, err
	}
	snapshot.Scope.LimitReason, snapshot.Scope.LimitValue = "repositories", 100
	snapshot.Scope.Note = "Partial scan: repository limit reached (100)."
	for i := range 120 {
		id := fmt.Sprintf("sibling-%03d", i)
		snapshot.Repositories = append(snapshot.Repositories, gitreview.Repository{ID: id, Name: id, Kind: gitreview.KindNested, Worktrees: []gitreview.Worktree{
			{ID: id + "-wt", Path: "repos/" + id, State: "unscanned"},
		}})
	}
	return snapshot, nil
}

// Opening Files asks only for the scope root; clicking a folder asks only for
// that folder; the global discovery note never appears in the Files surface.
func TestFilesOpensOneFolderAndIgnoresGlobalDiscoveryLimit(t *testing.T) {
	h := newReviewHarness(t, 140)
	provider := discoveryLimitedReviewProvider{h.provider}
	h.m.gitReviewFactory = func(*app.App, string, string) (gitreview.Provider, *view.Snapshot, error) { return provider, nil, nil }
	h.open(true)
	require.Equal(t, []string{""}, h.provider.dirs, "Files opens with a single scope-root listing")

	h.pump(h.m.submitGitReviewRequest(view.LoadDirectoryRequestMsg{Owner: "s1", WorktreeID: "w1", Path: "src"}))
	require.Equal(t, []string{"", "src"}, h.provider.dirs, "a clicked folder adds exactly one request")

	frame := ansi.Strip(h.m.composeView().Content)
	require.NotContains(t, frame, "Partial scan", "Files does not present Git discovery limits")
}
