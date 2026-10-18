package api

import "github.com/docker/docker-agent/pkg/gitreview"

// GitReviewSnapshot is an explicit bounded read snapshot for a session's
// workspace. Clients cannot supply a workspace root.
type GitReviewSnapshot struct {
	Snapshot gitreview.Snapshot `json:"snapshot"`
}

type GitReviewWorktreeRequest struct {
	Generation uint64 `json:"generation"`
	WorktreeID string `json:"worktree_id"`
	Refresh    bool   `json:"refresh,omitempty"`
}

type GitReviewWorktree struct {
	Generation uint64             `json:"generation"`
	Worktree   gitreview.Worktree `json:"worktree"`
}

type GitReviewDirectory struct {
	Generation uint64 `json:"generation"`
	WorktreeID string `json:"worktree_id"`
	Path       string `json:"path"`
}

type GitReviewDirectoryResult struct {
	Generation uint64              `json:"generation"`
	Directory  gitreview.Directory `json:"directory"`
}

type GitReviewDocument struct {
	Generation uint64                `json:"generation"`
	Key        gitreview.DocumentKey `json:"key"`
}

type GitReviewDocumentResult struct {
	Generation uint64             `json:"generation"`
	Document   gitreview.Document `json:"document"`
}
