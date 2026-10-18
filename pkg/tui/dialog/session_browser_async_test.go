package dialog

import (
	"context"
	"errors"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestSessionBrowserAsyncLoadPreservesFilteringAndOrder(t *testing.T) {
	t.Parallel()

	want := []session.Summary{
		{ID: "away", Title: "Elsewhere", Starred: true, WorkingDir: "/work/elsewhere", CreatedAt: time.Now()},
		{ID: "home", Title: "Workspace", Starred: true, WorkingDir: "/work/project", CreatedAt: time.Now()},
		{ID: "empty", Title: "", WorkingDir: "/work/project"},
	}
	d := NewLoadingSessionBrowserDialog(context.Background(), func(_ context.Context) ([]session.Summary, error) {
		return want, nil
	}, "/work/project").(*sessionBrowserDialog)

	cmd := d.startLoad()
	loaded := cmd().(SessionBrowserLoadedMsg)
	_, _ = d.Update(loaded)

	require.False(t, d.loading)
	require.NoError(t, d.loadErr)
	require.Equal(t, []string{"away", "home"}, sessionBrowserIDs(d.sessions), "loading keeps catalog recency order")
	require.Equal(t, []string{"home", "away"}, sessionBrowserIDs(d.filtered), "workspace matches are grouped first while retaining recency within groups")

	d.starFilter = 1
	d.textInput.SetValue("away")
	d.filterSessions()
	d.textInput.SetValue("home")
	d.filterSessions()
	require.Equal(t, []string{"home"}, sessionBrowserIDs(d.filtered), "loaded metadata continues through existing star and text filters")
}

func TestSessionBrowserAsyncLoadErrorCanRetry(t *testing.T) {
	t.Parallel()

	calls := 0
	d := NewLoadingSessionBrowserDialog(context.Background(), func(context.Context) ([]session.Summary, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("storage unavailable")
		}
		return []session.Summary{{ID: "s", Title: "Recovered"}}, nil
	}, "").(*sessionBrowserDialog)

	_, _ = d.Update(d.startLoad()().(SessionBrowserLoadedMsg))
	d.SetSize(100, 30)
	require.Contains(t, d.View(), "Failed to load sessions: storage unavailable")
	require.Contains(t, d.View(), "Press r to retry")

	_, retry := d.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
	require.NotNil(t, retry)
	_, _ = d.Update(retry().(SessionBrowserLoadedMsg))
	require.Equal(t, 2, calls)
	require.Empty(t, d.loadErr)
	require.Equal(t, []string{"s"}, sessionBrowserIDs(d.filtered))
}

func TestSessionBrowserAsyncLoadDropsCanceledAndStaleResults(t *testing.T) {
	t.Parallel()

	started := make(chan context.Context, 2)
	d := NewLoadingSessionBrowserDialog(context.Background(), func(ctx context.Context) ([]session.Summary, error) {
		started <- ctx
		return nil, nil
	}, "").(*sessionBrowserDialog)
	firstMsg := d.startLoad()().(SessionBrowserLoadedMsg)
	firstCtx := <-started
	secondMsg := d.startLoad()().(SessionBrowserLoadedMsg)
	secondCtx := <-started
	require.ErrorIs(t, firstCtx.Err(), context.Canceled)

	_, _ = d.Update(firstMsg)
	require.True(t, d.loading, "an earlier request cannot replace the active request")
	require.Empty(t, d.sessions)

	d.Cleanup()
	require.ErrorIs(t, secondCtx.Err(), context.Canceled)
	_, _ = d.Update(secondMsg)
	require.True(t, d.loading)
	require.Empty(t, d.sessions, "a canceled load cannot publish results")
}

func sessionBrowserIDs(sessions []session.Summary) []string {
	ids := make([]string, len(sessions))
	for i, sess := range sessions {
		ids[i] = sess.ID
	}
	return ids
}
