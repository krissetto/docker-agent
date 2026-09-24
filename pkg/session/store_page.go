package session

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"time"
)

// SummaryPageOptions orders metadata by creation descending, then ID ascending.
// AfterCreatedAt and AfterID form an exclusive cursor. Limit must be positive.
type SummaryPageOptions struct {
	IncludeChildren bool
	// Query is a literal title/working-directory substring, with ASCII case folding.
	Query          string
	Limit          int
	AfterCreatedAt time.Time
	AfterID        string
}
type SummaryPage struct {
	Summaries []Summary
	HasMore   bool
}
type PagedSummaryStore interface {
	GetSessionSummaryPage(ctx context.Context, options SummaryPageOptions) (SummaryPage, error)
}

func compareSummaries(a, b Summary) int {
	if n := b.CreatedAt.Compare(a.CreatedAt); n != 0 {
		return n
	}
	return cmp.Compare(a.ID, b.ID)
}

func (s *InMemorySessionStore) GetSessionSummaryPage(ctx context.Context, options SummaryPageOptions) (SummaryPage, error) {
	if options.Limit <= 0 || options.Limit > 1000 {
		return SummaryPage{}, errors.New("summary page limit must be between 1 and 1000")
	}
	if err := ctx.Err(); err != nil {
		return SummaryPage{}, err
	}
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	// Retain only limit+1 candidates, rather than materializing the catalog.
	selected := make([]*Session, 0, options.Limit+1)
	query := foldSummaryQuery(strings.TrimSpace(options.Query))
	cursor := Summary{CreatedAt: options.AfterCreatedAt, ID: options.AfterID}
	s.sessions.Range(func(_ string, sess *Session) bool {
		if ctx.Err() != nil {
			return false
		}
		if !options.IncludeChildren && sess.ParentID != "" {
			return true
		}
		if query != "" && !strings.Contains(foldSummaryQuery(sess.TitleSnapshot()), query) && !strings.Contains(foldSummaryQuery(sess.WorkingDir), query) {
			return true
		}
		key := Summary{CreatedAt: sess.CreatedAt, ID: sess.ID}
		if options.AfterID != "" && compareSummaries(key, cursor) <= 0 {
			return true
		}
		i, _ := slices.BinarySearchFunc(selected, key, func(a *Session, b Summary) int { return compareSummaries(Summary{CreatedAt: a.CreatedAt, ID: a.ID}, b) })
		if i > options.Limit {
			return true
		}
		selected = slices.Insert(selected, i, sess)
		if len(selected) > options.Limit+1 {
			selected = selected[:options.Limit+1]
		}
		return true
	})
	if err := ctx.Err(); err != nil {
		return SummaryPage{}, err
	}
	page := SummaryPage{HasMore: len(selected) > options.Limit}
	if page.HasMore {
		selected = selected[:options.Limit]
	}
	for _, value := range selected {
		_, _, cost := value.TokensAndCost()
		overrides, _ := value.ModelStateSnapshot()
		page.Summaries = append(page.Summaries, Summary{ID: value.ID, Title: value.TitleSnapshot(), CreatedAt: value.CreatedAt, Starred: value.Starred, NumMessages: value.MessageCount(), Cost: cost, WorkingDir: value.WorkingDir, Attributes: value.AttributesSnapshot(), ParentID: value.ParentID, AgentModelOverrides: overrides})
	}
	return page, nil
}

func (s *SQLiteSessionStore) GetSessionSummaryPage(ctx context.Context, options SummaryPageOptions) (SummaryPage, error) {
	if options.Limit <= 0 || options.Limit > 1000 {
		return SummaryPage{}, errors.New("summary page limit must be between 1 and 1000")
	}
	rows, err := s.sessionSummaries(ctx, SummaryScope{IncludeChildren: options.IncludeChildren}, &options)
	if err != nil {
		return SummaryPage{}, err
	}
	page := SummaryPage{Summaries: rows, HasMore: len(rows) > options.Limit}
	if page.HasMore {
		page.Summaries = page.Summaries[:options.Limit]
	}
	return page, nil
}

// Match SQLite lower(), which folds ASCII rather than depending on locale.
func foldSummaryQuery(value string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, value)
}
