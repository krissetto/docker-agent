package runtime

import (
	"context"
	"time"
)

// SessionSummaryPageOptions requests one bounded metadata-only page. Cursor is
// opaque to callers and bound to the source/child scope supplied by the host.
type SessionSummaryPageOptions struct {
	IncludeChildren bool
	Source          string
	WorkingDir      string
	Limit           int
	Cursor          string
}

// SessionSummaryPage contains at most the requested number of metadata rows.
// NextCursor is empty when no further page exists.
type SessionSummaryPage struct {
	Entries    []SessionSummaryEntry
	NextCursor string
}

// SessionSummaryPager is the bounded alternative to SessionSummaryCatalog.
// It never hydrates a session or returns transcript content.
type SessionSummaryPager interface {
	ListSessionSummaryPage(ctx context.Context, options SessionSummaryPageOptions) (SessionSummaryPage, error)
}

type localSummaryCursor struct {
	CreatedAt  time.Time `json:"at"`
	ID         string    `json:"id"`
	Source     string    `json:"source"`
	Children   bool      `json:"children"`
	WorkingDir string    `json:"working_dir,omitempty"`
}
