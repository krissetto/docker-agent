package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/docker/docker-agent/pkg/api"
)

const maxSessionSummaryPageSize = 200

// ListSessionSummaryPage negotiates and reads exactly one metadata page. It
// never falls back to a transcript catalog or silently drains all pages.
func (r *SessionTransport) ListSessionSummaryPage(ctx context.Context, options SessionSummaryPageOptions) (SessionSummaryPage, error) {
	if options.Limit < 1 || options.Limit > maxSessionSummaryPageSize {
		return SessionSummaryPage{}, errors.New("session summary page limit must be between 1 and 200")
	}
	if options.Source != "" && options.Source != r.source {
		return SessionSummaryPage{}, errors.New("session summary source scope mismatch")
	}
	query := url.Values{
		"view":             {"summary"},
		"include_children": {strconv.FormatBool(options.IncludeChildren)},
		"limit":            {strconv.Itoa(options.Limit)},
	}
	if options.Cursor != "" {
		if len(options.Cursor) > 4096 || strings.IndexFunc(options.Cursor, func(r rune) bool {
			return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_'
		}) >= 0 {
			return SessionSummaryPage{}, errors.New("invalid session summary cursor")
		}
		wrapped, err := base64.RawURLEncoding.DecodeString(options.Cursor)
		if err != nil {
			return SessionSummaryPage{}, errors.New("invalid session summary cursor")
		}
		var cursor remoteSummaryCursor
		if err := json.Unmarshal(wrapped, &cursor); err != nil || cursor.Source != r.source || cursor.Children != options.IncludeChildren || cursor.Cursor == "" {
			return SessionSummaryPage{}, errors.New("session summary cursor scope mismatch")
		}
		query.Set("cursor", cursor.Cursor)
	}
	var catalog api.SessionSummaryCatalog[SessionSummaryEntry]
	if err := r.client.sessionJSON(ctx, http.MethodGet, api.SessionAPIPath+"?"+query.Encode(), nil, &catalog); err != nil {
		return SessionSummaryPage{}, err
	}
	if catalog.Version != sessionWireVersion || catalog.View != "summary" || len(catalog.Sessions) > options.Limit {
		return SessionSummaryPage{}, UnsupportedSessionOperation("", "session_summaries")
	}
	page := SessionSummaryPage{Entries: make([]SessionSummaryEntry, 0, len(catalog.Sessions))}
	seen := make(map[string]bool, len(catalog.Sessions))
	for _, row := range catalog.Sessions {
		if row.SessionID == "" || seen[row.SessionID] || (!options.IncludeChildren && row.ParentID != "") {
			return SessionSummaryPage{}, errors.New("invalid session summary page identity or scope")
		}
		seen[row.SessionID] = true
		if r.source != "" && row.Source != r.source {
			continue
		}
		if options.WorkingDir != "" && row.WorkingDir != options.WorkingDir {
			continue
		}
		page.Entries = append(page.Entries, row)
	}
	if catalog.NextCursor != "" {
		if err := validateRemoteCatalogCursor(catalog.NextCursor, len(catalog.Sessions)); err != nil {
			return SessionSummaryPage{}, err
		}
		data, _ := json.Marshal(remoteSummaryCursor{Cursor: catalog.NextCursor, Source: r.source, Children: options.IncludeChildren, WorkingDir: options.WorkingDir})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(data)
	}
	return page, nil
}

type remoteSummaryCursor struct {
	Cursor     string `json:"c"`
	Source     string `json:"s"`
	Children   bool   `json:"k"`
	WorkingDir string `json:"w,omitempty"`
}

func validateRemoteCatalogCursor(cursor string, rows int) error {
	if rows == 0 || len(cursor) > 2048 || strings.IndexFunc(cursor, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_'
	}) >= 0 {
		return errors.New("invalid session catalog cursor or empty continuation page")
	}
	return nil
}

var _ SessionSummaryPager = (*SessionTransport)(nil)
