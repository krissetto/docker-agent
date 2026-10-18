package tui

import (
	"context"

	"github.com/docker/docker-agent/pkg/runtime"
)

// ReviewSessionAgent is bounded, non-secret metadata for an agent available
// from the host's currently authorized source. It never contains credentials.
type ReviewSessionAgent struct {
	Source      string
	Name        string
	Description string
}

// ReviewSessionRequest identifies one explicit new-session review send. The
// host must treat SessionID and RequestID as idempotency keys: retries carry
// the same IDs and exact body and must never create another target.
type ReviewSessionRequest struct {
	SessionID  string
	RequestID  string
	AgentName  string
	WorkingDir string
	Source     string
	Body       string
}

// ReviewSessionResult reports admission of the literal first input. Accepted
// indicates that the target accepted this exact request; Durable describes
// persistence strength only. A successful result does not imply model work has
// completed.
type ReviewSessionResult struct {
	SessionID string
	TurnID    string
	Title     string
	Accepted  bool
	Durable   bool
}

// ReviewSessionFactory is the host-owned boundary for explicitly creating and
// submitting a review to a new session. Implementations use the active
// authorized source and retain accepted runtime ownership for normal host
// shutdown; they must not add/focus a visible TUI tab. Listing is metadata-only
// and bounded. CreateAndSubmit must be idempotent for identical request IDs
// and reject reuse with a different request fingerprint.
type ReviewSessionFactory interface {
	ListAgents(ctx context.Context) ([]ReviewSessionAgent, error)
	CreateAndSubmit(ctx context.Context, request ReviewSessionRequest) (ReviewSessionResult, error)
}

// WithReviewSessionFactory exposes the host's safe new-target path to review
// UI. A nil factory leaves the New session choice unavailable.
func WithReviewSessionFactory(factory ReviewSessionFactory) Option {
	return func(m *appModel) {
		m.reviewSessionFactory = factory
		if pager, ok := factory.(interface {
			ListSessionSummaryPage(context.Context, string, int) ([]runtime.SessionSummaryEntry, string, error)
		}); ok {
			m.gitReviewCatalog = pager.ListSessionSummaryPage
		}
	}
}
