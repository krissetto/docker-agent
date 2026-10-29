package tui

import (
	"fmt"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	view "github.com/docker/docker-agent/pkg/tui/components/review"
	"github.com/docker/docker-agent/pkg/tui/dialog"
)

type scrollbarScheduler struct{ now time.Time }

func (s *scrollbarScheduler) Now() time.Time { return s.now }
func (s *scrollbarScheduler) Tick(delay time.Duration, create func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		s.now = s.now.Add(delay)
		return create(s.now)
	}
}

func scrollbarReview(t *testing.T) (*appModel, *view.Model) {
	t.Helper()
	scheduler := &scrollbarScheduler{now: time.Unix(1, 0)}
	ar := animation.NewRuntimeWithScheduler(scheduler)
	t.Cleanup(ar.Stop)
	model := view.New("review")
	model.SetInspector(true)
	model.SetSize(60, 14)
	key := view.DocumentKey{Mode: view.ModeChanges, WorktreeID: "worktree", Path: "example.go", Group: view.GroupUnstaged}
	model.OpenDocument(key)
	document := view.Document{Key: key, State: view.StateReady}
	for i := range 100 {
		document.Lines = append(document.Lines, view.Line{Kind: view.LineContext, New: i + 1, Old: i + 1, Text: fmt.Sprintf("line %d", i)})
	}
	model.SetDocument(document)
	root := &appModel{
		ar: ar, dialogMgr: dialog.New(ar), gitReviewOwner: "review",
		gitReviews: map[string]*gitReviewWorkspace{"review": {
			anchor: "review", open: true, mode: view.ModeFiles,
			models: map[view.Mode]*view.Model{view.ModeFiles: model},
		}},
	}
	t.Cleanup(root.dialogMgr.Cleanup)
	t.Cleanup(root.stopReviewAnimations)
	root.syncReviewAnimations()
	return root, model
}

func tickScrollbarReview(t *testing.T, root *appModel) {
	t.Helper()
	command := root.ar.Continue()
	require.NotNil(t, command)
	tick, ok := command().(animation.TickMsg)
	require.True(t, ok)
	tick, accepted := root.ar.Accept(tick)
	require.True(t, accepted)
	root.tickReviewAnimation(tick)
}

func TestReviewScrollbarRootTicksAndReleasesIdleLease(t *testing.T) {
	root, model := scrollbarReview(t)
	before := model.View()
	model.HandleWheelDelta(20, 5, 4)
	require.True(t, root.ar.HasActive())
	root.viewCacheValid = true
	tickScrollbarReview(t, root)
	require.False(t, root.viewCacheValid, "standalone review fade invalidates the root cache")
	require.NotEqual(t, before, model.View())
	for frames := 0; root.ar.HasActive() && frames < 180; frames++ {
		tickScrollbarReview(t, root)
	}
	require.False(t, root.ar.HasActive(), "settled scrollbar leaves no idle timer chain")
}

func TestReviewScrollbarStopsWhenHiddenAndResumesWithoutStaleHover(t *testing.T) {
	root, model := scrollbarReview(t)
	model.HandleWheelDelta(20, 5, 4)
	require.True(t, root.ar.HasActive())
	root.gitReviewOwner = ""
	root.syncReviewAnimations()
	require.False(t, root.ar.HasActive())
	model.HandleWheelDelta(20, 5, 4)
	require.False(t, root.ar.HasActive(), "offscreen offset changes must not restart animation")
	root.gitReviewOwner = "review"
	root.syncReviewAnimations()
	require.False(t, root.ar.HasActive(), "showing a surface is not scrolling it")
	model.HandleWheelDelta(20, 5, 4)
	require.True(t, root.ar.HasActive())
}

func TestReviewScrollbarReplacedOwnerDoesNotLeakLease(t *testing.T) {
	root, model := scrollbarReview(t)
	model.HandleWheelDelta(20, 5, 4)
	require.True(t, root.ar.HasActive())
	root.gitReviews = nil
	root.syncReviewAnimations()
	require.False(t, root.ar.HasActive(), "a discarded workspace still releases its old view lease")
}
