// Package gitreview provides bounded, read-only Git review data for a local workspace.
package gitreview

import (
	"context"
	"errors"
	"time"
)

var ErrUnavailable = errors.New("git review unavailable")
var ErrOutsideScope = errors.New("path is outside git review scope")
var ErrNotFound = errors.New("git review item not found")
var ErrTooLarge = errors.New("git review result exceeds the configured limit")

// Kind describes how a repository was discovered beneath the review scope.
type Kind string

const (
	KindPrimary   Kind = "primary"
	KindNested    Kind = "nested"
	KindSubmodule Kind = "submodule"
	KindNone      Kind = "none"
)

type Scope struct {
	Label       string
	Note        string
	Remote      bool
	RemoteLabel string
	Unavailable string
}

type Snapshot struct {
	Generation    uint64
	CapturedAt    time.Time
	MaybeOutdated bool
	Scope         Scope
	Repositories  []Repository
}

type Repository struct {
	ID            string
	Name          string
	Kind          Kind
	Worktrees     []Worktree
	HiddenOutside int
	Err           string
}

type Worktree struct {
	ID           string
	Path         string
	Branch       string
	Detached     bool
	DetachedHead string
	Current      bool
	Linked       bool
	State        string
	Err          string
	Files        []FileChange
}

type FileGroup string

const (
	GroupConflicts FileGroup = "conflicts"
	GroupStaged    FileGroup = "staged"
	GroupUnstaged  FileGroup = "unstaged"
	GroupUntracked FileGroup = "untracked"
	GroupNone      FileGroup = "none"
)

type FileChange struct {
	Path    string
	OldPath string
	Group   FileGroup
	Status  string
	Added   int
	Removed int
	Binary  bool
}

type LoadState string

const (
	StateUnscanned LoadState = "unscanned"
	StateLoading   LoadState = "loading"
	StateReady     LoadState = "ready"
	StateError     LoadState = "error"
)

type Directory struct {
	WorktreeID string
	Path       string
	State      LoadState
	Entries    []DirectoryEntry
	Truncated  bool
	Err        string
}

type DirectoryEntry struct {
	Name string
	Dir  bool
}

type DocumentKey struct {
	Mode       string // "changes" or "files"
	WorktreeID string
	Path       string
	Group      FileGroup
}

type DiffLineKind string

const (
	DiffContext DiffLineKind = "context"
	DiffAdd     DiffLineKind = "add"
	DiffRemove  DiffLineKind = "remove"
	DiffHunk    DiffLineKind = "hunk"
	DiffNote    DiffLineKind = "note"
)

type DocumentLine struct {
	Kind DiffLineKind
	Old  int
	New  int
	Text string
}

type Document struct {
	Key         DocumentKey
	State       LoadState
	Lines       []DocumentLine
	Binary      bool
	Truncated   bool
	Note        string
	Err         string
	PatchHeader string
}

// Provider performs only bounded read operations. A Snapshot call is an
// explicit refresh boundary; implementations may cache by Generation.
type Provider interface {
	Snapshot(ctx context.Context) (Snapshot, error)
	LoadWorktree(ctx context.Context, worktreeID string) (Worktree, error)
	Directory(ctx context.Context, worktreeID, path string) (Directory, error)
	Document(ctx context.Context, key DocumentKey) (Document, error)
}
