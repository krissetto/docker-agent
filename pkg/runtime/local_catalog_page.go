package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/docker/docker-agent/pkg/session"
)

const maxLocalSessionSummaryPageSize = 200

func (v *localSessionRuntimeView) ListSessionSummaryPage(ctx context.Context, options SessionSummaryPageOptions) (SessionSummaryPage, error) {
	if options.Limit < 1 || options.Limit > maxLocalSessionSummaryPageSize {
		return SessionSummaryPage{}, errors.New("session summary page limit must be between 1 and 200")
	}
	if options.Source == "" {
		return SessionSummaryPage{}, errors.New("session summary page requires source scope")
	}
	store, ok := v.runtime.sessionStore.(session.PagedSummaryStore)
	if !ok {
		return SessionSummaryPage{}, UnsupportedSessionOperation("", "session_summary_page")
	}
	cursor := localSummaryCursor{}
	if options.Cursor != "" {
		if len(options.Cursor) > 4096 {
			return SessionSummaryPage{}, errors.New("invalid session summary cursor")
		}
		data, err := base64.RawURLEncoding.DecodeString(options.Cursor)
		if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.Source != options.Source || cursor.Children != options.IncludeChildren || cursor.WorkingDir != options.WorkingDir || cursor.ID == "" || cursor.CreatedAt.IsZero() {
			return SessionSummaryPage{}, errors.New("invalid session summary cursor")
		}
	}
	page, err := store.GetSessionSummaryPage(ctx, session.SummaryPageOptions{
		IncludeChildren: options.IncludeChildren,
		Limit:           options.Limit,
		AfterCreatedAt:  cursor.CreatedAt,
		AfterID:         cursor.ID,
	})
	if err != nil {
		return SessionSummaryPage{}, err
	}
	result := SessionSummaryPage{Entries: make([]SessionSummaryEntry, 0, len(page.Summaries))}
	for _, row := range page.Summaries {
		if err := ctx.Err(); err != nil {
			return SessionSummaryPage{}, err
		}
		if row.ID == "" {
			return SessionSummaryPage{}, errors.New("session summary is missing canonical identity")
		}
		source := row.Attributes["docker-agent.actor.source"]
		if source != options.Source {
			continue
		}
		if options.WorkingDir != "" && row.WorkingDir != options.WorkingDir {
			continue
		}
		bound := row.Attributes[SessionAgentAttribute]
		entry := SessionSummaryEntry{
			SessionID: row.ID, ParentID: row.ParentID, Title: row.Title, AgentName: bound,
			Model: row.AgentModelOverrides[bound], Source: source, CreatedAt: row.CreatedAt,
			UpdatedAt: row.CreatedAt.Format(time.RFC3339), Starred: row.Starred,
			NumMessages: row.NumMessages, Cost: row.Cost, WorkingDir: row.WorkingDir,
		}
		if driver, loaded := v.runtime.sessionDrivers.Lookup(row.ID); loaded {
			driver.mu.Lock()
			if !driver.stopped && !driver.reclaiming {
				entry.Loaded, entry.Loadable = true, true
				entry.AgentName, entry.Model = driver.AgentNameLocked(), driver.modelRef
			}
			driver.mu.Unlock()
		}
		if !entry.Loaded && entry.ParentID == "" && entry.AgentName != "" && entry.Source == options.Source {
			if _, agentErr := v.runtime.team.Agent(entry.AgentName); agentErr == nil {
				entry.Loadable = true
			}
		}
		if entry.ParentID != "" && !entry.Loaded {
			entry.RequiresConfirmation = true
		}
		if !entry.Loaded && !entry.Loadable && !entry.RequiresConfirmation {
			entry.RouteError = "session is not available in this source"
		}
		result.Entries = append(result.Entries, entry)
	}
	if page.HasMore && len(page.Summaries) > 0 {
		last := page.Summaries[len(page.Summaries)-1]
		data, _ := json.Marshal(localSummaryCursor{CreatedAt: last.CreatedAt, ID: last.ID, Source: options.Source, Children: options.IncludeChildren, WorkingDir: options.WorkingDir})
		result.NextCursor = base64.RawURLEncoding.EncodeToString(data)
	}
	return result, nil
}

var _ SessionSummaryPager = (*localSessionRuntimeView)(nil)
