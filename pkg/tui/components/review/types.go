// Package review renders the read-only Changes and Files review panels.
// It performs no filesystem or Git IO: the host supplies immutable snapshots
// and documents, and receives typed requests in return.
package review

import "time"

type Mode int

const (
	ModeChanges Mode = iota
	ModeFiles
)

type LoadState int

const (
	StateUnscanned LoadState = iota
	StateLoading
	StateReady
	StateError
)

// Group orders Changes sections; GroupNone is used by Files documents.
type Group int

const (
	GroupNone Group = iota
	GroupConflicts
	GroupStaged
	GroupUnstaged
	GroupUntracked
)

var changeGroups = []Group{GroupConflicts, GroupStaged, GroupUnstaged, GroupUntracked}

func (g Group) String() string {
	switch g {
	case GroupConflicts:
		return "Conflicts"
	case GroupStaged:
		return "Staged"
	case GroupUnstaged:
		return "Unstaged"
	case GroupUntracked:
		return "Untracked"
	}
	return ""
}

// Base names what a Changes document compares.
func (g Group) Base() string {
	switch g {
	case GroupConflicts:
		return "unmerged"
	case GroupStaged:
		return "HEAD → index"
	case GroupUnstaged:
		return "index → working tree"
	case GroupUntracked:
		return "new file"
	}
	return ""
}

type RepoKind int

const (
	RepoPrimary RepoKind = iota
	RepoNested
	RepoSubmodule
	// RepoNone is a synthetic workspace record for Files mode outside Git.
	RepoNone
)

type Snapshot struct {
	Generation    uint64
	CapturedAt    time.Time
	MaybeOutdated bool
	Scope         Scope
	Repositories  []Repository
}

type Scope struct {
	Label       string
	Remote      bool
	RemoteLabel string
	// Unavailable replaces all data with an explanation when non-empty.
	Unavailable string
	// Note explains a partial result (for example limited discovery); it is
	// not a freshness signal and never hides the data that was found.
	Note string
}

type Repository struct {
	ID        string
	Name      string
	Kind      RepoKind
	Worktrees []Worktree
	// HiddenOutside counts linked worktrees the host may not disclose.
	HiddenOutside int
	Err           string
}

type Worktree struct {
	ID       string
	Path     string
	Branch   string
	Detached string
	Current  bool
	Linked   bool
	State    LoadState
	Err      string
	Files    []FileChange
}

type FileChange struct {
	Path, OldPath  string
	Group          Group
	Status         string
	Added, Removed int
	Binary         bool
}

type Directory struct {
	WorktreeID string
	Path       string
	State      LoadState
	Entries    []Entry
	Truncated  bool
	Err        string
}

type Entry struct {
	Name string
	Dir  bool
}

type DocumentKey struct {
	Mode       Mode
	WorktreeID string
	Path       string
	Group      Group
}

type LineKind int

const (
	LineContext LineKind = iota
	LineAdd
	LineRemove
	LineHunk
	LineNote
)

type Line struct {
	Kind     LineKind
	Old, New int
	Text     string
}

type Document struct {
	Key       DocumentKey
	State     LoadState
	Lines     []Line
	Binary    bool
	Truncated bool
	Note, Err string
	// PatchHeader enables patch copies; empty means hunks are not reconstructable.
	PatchHeader string
}

// Session describes the session the composer stays attached to.
type Session struct {
	Title      string
	WorktreeID string
	Working    bool
}

type Selection struct {
	Mode         Mode
	RepositoryID string
	WorktreeID   string
	Path         string
	Group        Group
}

// Anchor locates a review comment: repository, checkout, file, comparison,
// old/new line ranges, the exact context lines and a content fingerprint.
type Anchor struct {
	RepositoryID, RepositoryName string
	WorktreeID, CheckoutPath     string
	Branch, Detached             string
	Path, OldPath                string
	Mode                         Mode
	Group                        Group
	OldStart, OldEnd             int
	NewStart, NewEnd             int
	Context                      []string
	Version                      string
}

type Comment struct {
	ID     string
	Anchor Anchor
	Body   string
}

type DraftStatus int

const (
	DraftIdle DraftStatus = iota
	DraftPreviewing
	DraftSending
	DraftFailed
)

// DraftView is the host-owned review draft as the UI displays it.
type DraftView struct {
	Comments []Comment
	Revision uint64
	Status   DraftStatus
	Err      string
	// Target, Preview and Token describe a captured send; Token must be echoed on confirm.
	Target  string
	Preview string
	Token   string
}

type CopyKind int

const (
	CopyText CopyKind = iota
	CopyPatch
	CopyPath
)

type RefreshRequestMsg struct{ Owner, WorktreeID string }

type LoadWorktreeRequestMsg struct {
	Owner, WorktreeID string
	Refresh           bool
}

type LoadDirectoryRequestMsg struct{ Owner, WorktreeID, Path string }

type OpenDocumentRequestMsg struct {
	Owner string
	Key   DocumentKey
}

type CopyRequestMsg struct {
	Owner         string
	Kind          CopyKind
	Text, Summary string
}

type CloseRequestMsg struct{ Owner string }

// FocusRequestMsg asks the host to give the review keyboard focus after a click.
type FocusRequestMsg struct{ Owner string }

type AddCommentMsg struct {
	Owner  string
	Anchor Anchor
	Body   string
}

type EditCommentMsg struct{ Owner, ID, Body string }

type DeleteCommentMsg struct{ Owner, ID string }

// PreviewDraftMsg asks the host to capture a target and serialize the draft.
type PreviewDraftMsg struct {
	Owner    string
	Revision uint64
}

// SendDraftMsg confirms the previewed send identified by Token.
type SendDraftMsg struct {
	Owner    string
	Revision uint64
	Token    string
}

// CancelPreviewMsg discards a captured preview without sending.
type CancelPreviewMsg struct{ Owner string }

// HighlightedMsg carries syntax tokens computed off the event loop; the host
// passes it to ApplyHighlight. Stale results are ignored.
type HighlightedMsg struct {
	Owner  string
	seq    uint64
	tokens [][]token
}

// DockToggleRequestMsg asks the host to show or hide the chat beside the review.
type DockToggleRequestMsg struct{ Owner string }

type SelectionChangedMsg struct {
	Owner     string
	Selection Selection
}
