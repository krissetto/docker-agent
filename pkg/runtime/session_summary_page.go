package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/session"
)

type summaryPageCursor struct {
	Version         int       `json:"v"`
	CreatedAt       time.Time `json:"at"`
	ID              string    `json:"id"`
	IncludeChildren bool      `json:"children"`
	QueryHash       string    `json:"query"`
}

func summaryQueryHash(query string) string {
	sum := sha256.Sum256([]byte(query))
	return hex.EncodeToString(sum[:])
}

func normalizeSummaryPageOptions(options SessionSummaryPageOptions) (SessionSummaryPageOptions, error) {
	if options.Limit == 0 {
		options.Limit = api.SessionCatalogDefaultLimit
	}
	if options.Limit < 1 || options.Limit > api.SessionCatalogMaxLimit {
		return options, &SessionError{Kind: SessionErrorInvalid, Operation: "session_summary_page", Detail: "limit must be between 1 and 200"}
	}
	options.Query = strings.TrimSpace(options.Query)
	if options.Cursor != "" {
		if err := advanceSessionCatalog(url.Values{}, map[string]bool{}, options.Cursor, 1); err != nil {
			return options, &SessionError{Kind: SessionErrorInvalid, Operation: "session_summary_page", Detail: "invalid summary cursor"}
		}
	}
	return options, nil
}

func validateSummaryPage(page SessionSummaryPage, options SessionSummaryPageOptions) error {
	if len(page.Entries) > options.Limit {
		return errors.New("session summary page exceeds requested limit")
	}
	seen := make(map[string]bool, len(page.Entries))
	for _, entry := range page.Entries {
		if entry.SessionID == "" || seen[entry.SessionID] {
			return errors.New("invalid or duplicate session summary identity")
		}
		seen[entry.SessionID] = true
		if !options.IncludeChildren && entry.ParentID != "" {
			return errors.New("session summary exceeds requested scope")
		}
	}
	return advanceSessionCatalog(url.Values{}, map[string]bool{options.Cursor: true}, page.NextCursor, len(page.Entries))
}

func (v *localSessionRuntimeView) ListSessionSummaryPage(ctx context.Context, options SessionSummaryPageOptions) (SessionSummaryPage, error) {
	if err := ctx.Err(); err != nil {
		return SessionSummaryPage{}, err
	}
	options, err := normalizeSummaryPageOptions(options)
	if err != nil {
		return SessionSummaryPage{}, err
	}
	store, ok := v.runtime.sessionStore.(session.PagedSummaryStore)
	if !ok {
		return SessionSummaryPage{}, UnsupportedSessionOperation("", "session_summary_page")
	}
	cursor := summaryPageCursor{}
	if options.Cursor != "" {
		data, err := base64.RawURLEncoding.DecodeString(options.Cursor)
		if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.Version != 1 || cursor.ID == "" || cursor.CreatedAt.IsZero() || cursor.IncludeChildren != options.IncludeChildren || cursor.QueryHash != summaryQueryHash(options.Query) {
			return SessionSummaryPage{}, &SessionError{Kind: SessionErrorInvalid, Operation: "session_summary_page", Detail: "invalid summary cursor or scope"}
		}
	}
	stored, err := store.GetSessionSummaryPage(ctx, session.SummaryPageOptions{IncludeChildren: options.IncludeChildren, Limit: options.Limit, Query: options.Query, AfterCreatedAt: cursor.CreatedAt, AfterID: cursor.ID})
	if err != nil {
		return SessionSummaryPage{}, err
	}
	page := SessionSummaryPage{Entries: make([]SessionSummaryEntry, 0, len(stored.Summaries))}
	for _, row := range stored.Summaries {
		if err := ctx.Err(); err != nil {
			return SessionSummaryPage{}, err
		}
		bound := row.Attributes[SessionAgentAttribute]
		entry := SessionSummaryEntry{SessionID: row.ID, ParentID: row.ParentID, Title: row.Title, AgentName: bound, Model: row.AgentModelOverrides[bound], Source: row.Attributes["docker-agent.actor.source"], CreatedAt: row.CreatedAt, UpdatedAt: row.CreatedAt.Format(time.RFC3339Nano), Starred: row.Starred, NumMessages: row.NumMessages, Cost: row.Cost, WorkingDir: row.WorkingDir}
		if driver, loaded := v.runtime.sessionDrivers.Lookup(row.ID); loaded {
			driver.mu.Lock()
			if !driver.stopped && !driver.reclaiming {
				entry.Loaded, entry.Loadable = true, true
				entry.AgentName, entry.Model = driver.AgentNameLocked(), driver.modelRef
			}
			driver.mu.Unlock()
		}
		if !entry.Loaded {
			if bound == "" {
				entry.RouteError = "session is not attachable"
			} else if _, err := v.runtime.team.Agent(bound); err != nil {
				entry.RouteError = "persisted session agent is unavailable"
			} else {
				// An off-page parent is not a missing parent; confirmed loading validates ancestry.
				entry.Loadable = row.ParentID == ""
				entry.RequiresConfirmation = row.ParentID != ""
			}
		}
		page.Entries = append(page.Entries, entry)
	}
	if stored.HasMore {
		if len(page.Entries) == 0 {
			return SessionSummaryPage{}, errors.New("invalid empty continuation page")
		}
		last := page.Entries[len(page.Entries)-1]
		if last.CreatedAt.IsZero() {
			return SessionSummaryPage{}, errors.New("session summary cursor is missing creation time")
		}
		data, err := json.Marshal(summaryPageCursor{Version: 1, CreatedAt: last.CreatedAt, ID: last.SessionID, IncludeChildren: options.IncludeChildren, QueryHash: summaryQueryHash(options.Query)})
		if err != nil {
			return SessionSummaryPage{}, err
		}
		page.NextCursor = base64.RawURLEncoding.EncodeToString(data)
	}
	if err := validateSummaryPage(page, options); err != nil {
		return SessionSummaryPage{}, err
	}
	return page, nil
}

var _ SessionSummaryPager = (*localSessionRuntimeView)(nil)
var _ SessionSummaryPager = (*SessionTransport)(nil)
