package tui

import (
	"context"
	"errors"
	"strings"

	"github.com/docker/docker-agent/pkg/gitreview"
	view "github.com/docker/docker-agent/pkg/tui/components/review"
)

func reviewSnapshot(in gitreview.Snapshot) view.Snapshot {
	out := view.Snapshot{Generation: in.Generation, CapturedAt: in.CapturedAt, MaybeOutdated: in.MaybeOutdated, Browse: in.Browse, Scope: view.Scope{Label: in.Scope.Label, Note: in.Scope.Note, LimitReason: string(in.Scope.LimitReason), LimitValue: in.Scope.LimitValue, Remote: in.Scope.Remote, RemoteLabel: in.Scope.RemoteLabel, Unavailable: in.Scope.Unavailable}}
	for _, repo := range in.Repositories {
		r := view.Repository{ID: repo.ID, Name: repo.Name, Kind: reviewRepoKind(repo.Kind), HiddenOutside: repo.HiddenOutside, Err: repo.Err}
		for _, wt := range repo.Worktrees {
			r.Worktrees = append(r.Worktrees, reviewWorktree(wt))
		}
		out.Repositories = append(out.Repositories, r)
	}
	return out
}
func reviewWorktree(in gitreview.Worktree) view.Worktree {
	detached := ""
	if in.Detached {
		detached = in.DetachedHead
		if detached == "" {
			detached = "HEAD"
		}
	}
	return view.Worktree{ID: in.ID, Path: in.Path, Branch: in.Branch, Detached: detached, Current: in.Current, Linked: in.Linked, State: reviewState(in.State), CountsState: reviewLoadedState(string(in.CountsState)), Err: in.Err, Files: reviewFiles(in.Files)}
}
func reviewDirectory(in gitreview.Directory) view.Directory {
	out := view.Directory{WorktreeID: in.WorktreeID, Path: in.Path, State: reviewLoadedState(string(in.State)), Truncated: in.Truncated, Err: in.Err}
	for _, e := range in.Entries {
		out.Entries = append(out.Entries, view.Entry{Name: e.Name, Dir: e.Dir})
	}
	return out
}
func reviewDocument(in gitreview.Document) view.Document {
	out := view.Document{Key: reviewKey(in.Key), State: reviewLoadedState(string(in.State)), Binary: in.Binary, Truncated: in.Truncated, Note: in.Note, Err: in.Err, PatchHeader: reviewPatchHeader(in.PatchHeader)}
	files := out.Key.Mode == view.ModeFiles
	for _, indicator := range in.Indicators {
		out.Indicators = append(out.Indicators, view.DocumentIndicator{Line: indicator.Line, Kind: view.ChangeKind(indicator.Kind)})
	}
	for _, line := range in.Lines {
		kind := reviewLineKind(line.Kind)
		if files && kind != view.LineNote {
			// File contents are plain text, never diff additions.
			kind = view.LineContext
		}
		out.Lines = append(out.Lines, view.Line{Kind: kind, Old: line.Old, New: line.New, Text: line.Text})
	}
	return out
}
func gitReviewKey(in view.DocumentKey) gitreview.DocumentKey {
	mode := "changes"
	if in.Mode == view.ModeFiles {
		mode = "files"
	}
	return gitreview.DocumentKey{Mode: mode, WorktreeID: in.WorktreeID, Path: in.Path, Group: gitReviewGroup(in.Group)}
}
func reviewKey(in gitreview.DocumentKey) view.DocumentKey {
	mode := view.ModeChanges
	if in.Mode == "files" {
		mode = view.ModeFiles
	}
	return view.DocumentKey{Mode: mode, WorktreeID: in.WorktreeID, Path: in.Path, Group: reviewGroup(in.Group)}
}
func reviewRepoKind(in gitreview.Kind) view.RepoKind {
	switch in {
	case gitreview.KindNested:
		return view.RepoNested
	case gitreview.KindSubmodule:
		return view.RepoSubmodule
	case gitreview.KindNone:
		return view.RepoNone
	}
	return view.RepoPrimary
}
func reviewGroup(in gitreview.FileGroup) view.Group {
	switch in {
	case gitreview.GroupConflicts:
		return view.GroupConflicts
	case gitreview.GroupStaged:
		return view.GroupStaged
	case gitreview.GroupUnstaged:
		return view.GroupUnstaged
	case gitreview.GroupUntracked:
		return view.GroupUntracked
	}
	return view.GroupNone
}
func gitReviewGroup(in view.Group) gitreview.FileGroup {
	switch in {
	case view.GroupConflicts:
		return gitreview.GroupConflicts
	case view.GroupStaged:
		return gitreview.GroupStaged
	case view.GroupUnstaged:
		return gitreview.GroupUnstaged
	case view.GroupUntracked:
		return gitreview.GroupUntracked
	}
	return gitreview.GroupNone
}
func reviewFiles(in []gitreview.FileChange) []view.FileChange {
	out := make([]view.FileChange, 0, len(in))
	for _, f := range in {
		out = append(out, view.FileChange{Path: f.Path, OldPath: f.OldPath, Group: reviewGroup(f.Group), Status: f.Status, Added: f.Added, Removed: f.Removed, Binary: f.Binary})
	}
	return out
}
func reviewState(in string) view.LoadState {
	switch in {
	case "clean", "changed", "ready":
		return view.StateReady
	case "loading":
		return view.StateLoading
	case "error", "unavailable":
		return view.StateError
	}
	return view.StateUnscanned
}

// reviewScanSnapshot picks the explicit snapshot source. Files never falls back
// to repository discovery when a provider cannot browse.
func reviewScanSnapshot(provider gitreview.Provider, browse bool) func(context.Context) (gitreview.Snapshot, error) {
	if !browse {
		return provider.Snapshot
	}
	if browser, ok := provider.(gitreview.BrowseProvider); ok {
		return browser.BrowseRoot
	}
	return func(context.Context) (gitreview.Snapshot, error) {
		return gitreview.Snapshot{}, errors.New("folder browsing is not available for this workspace")
	}
}

// reviewPatchHeader keeps only complete file headers, which patch copies need.
func reviewPatchHeader(header string) string {
	if !strings.Contains(header, "--- ") || !strings.Contains(header, "+++ ") {
		return ""
	}
	return header
}

// reviewLoadedState treats any successfully returned folder or document as
// ready unless it explicitly reports loading or an error.
func reviewLoadedState(in string) view.LoadState {
	switch state := reviewState(in); state {
	case view.StateLoading, view.StateError:
		return state
	}
	return view.StateReady
}

func reviewLineKind(in gitreview.DiffLineKind) view.LineKind {
	switch in {
	case gitreview.DiffAdd:
		return view.LineAdd
	case gitreview.DiffRemove:
		return view.LineRemove
	case gitreview.DiffHunk:
		return view.LineHunk
	case gitreview.DiffNote:
		return view.LineNote
	}
	return view.LineContext
}
