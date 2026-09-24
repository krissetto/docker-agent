package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

type catalogCursor struct {
	Version   int       `json:"v"`
	CreatedAt time.Time `json:"at"`
	ID        string    `json:"id"`
	Scope     string    `json:"scope"`
}

func (s *Server) sessionCatalog(c echo.Context) error {
	c.Response().Header().Set("Cache-Control", "no-store")
	view := c.QueryParam("view")
	if view != "" && view != "summary" {
		return sessionRequestError("unknown catalog view")
	}
	children := false
	if raw := c.QueryParam("include_children"); raw != "" {
		var err error
		children, err = strconv.ParseBool(raw)
		if err != nil {
			return sessionRequestError("invalid include_children scope")
		}
	}
	active := c.QueryParam("active")
	if active != "" && active != "true" && active != "false" {
		return sessionRequestError("invalid active scope")
	}
	if view == "summary" && active != "" {
		return sessionRequestError("active filter is unsupported for summary view")
	}
	limit := api.SessionCatalogDefaultLimit
	if raw := c.QueryParam("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > api.SessionCatalogMaxLimit {
			return sessionRequestError("limit must be between 1 and 200")
		}
		limit = n
	}
	scope := view + ":" + strconv.FormatBool(children) + ":" + active
	cursor := catalogCursor{}
	if raw := c.QueryParam("cursor"); raw != "" {
		if len(raw) > 2048 {
			return sessionRequestError("invalid catalog cursor")
		}
		data, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.Version != api.SessionAPIVersion || cursor.ID == "" || cursor.CreatedAt.IsZero() || cursor.Scope != scope {
			return sessionRequestError("invalid catalog cursor or scope")
		}
	}
	rows := make([]runtime.SessionSummaryEntry, 0, limit)
	hasMore := false
	if active == "true" {
		// Live browsing is a derived metadata projection; never Snapshot transcripts.
		s.sm.runtimeSessions.Range(func(id string, live *activeRuntimes) bool {
			if live.handle == nil || id <= cursor.ID {
				return true
			}
			m := live.handle.Metadata()
			entry := runtime.SessionSummaryEntry{SessionID: id, AgentName: m.AgentName, Model: m.Model, Loaded: true, Loadable: true}
			pos := sort.Search(len(rows), func(i int) bool { return rows[i].SessionID >= id })
			if pos <= limit {
				rows = append(rows, runtime.SessionSummaryEntry{})
				copy(rows[pos+1:], rows[pos:])
				rows[pos] = entry
				if len(rows) > limit+1 {
					rows = rows[:limit+1]
				}
			}
			return true
		})
		if len(rows) > limit {
			rows = rows[:limit]
			hasMore = true
		}
		// A stable sentinel timestamp scopes live cursors without hydrating sessions.
		for i := range rows {
			rows[i].CreatedAt = time.Unix(1, 0).UTC()
		}
	} else {
		store, ok := s.sm.sessionStore.(session.PagedSummaryStore)
		if !ok {
			return sessionHTTPError(runtime.UnsupportedSessionOperation("", "session_summaries"))
		}
		page, err := store.GetSessionSummaryPage(c.Request().Context(), session.SummaryPageOptions{IncludeChildren: children, Limit: limit, AfterCreatedAt: cursor.CreatedAt, AfterID: cursor.ID})
		if err != nil {
			return sessionHTTPError(err)
		}
		hasMore = page.HasMore
		for _, row := range page.Summaries {
			if row.ID == "" {
				return sessionHTTPError(errors.New("invalid session summary identity"))
			}
			rows = append(rows, s.sm.summaryEntry(row))
		}
	}
	next := ""
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		data, _ := json.Marshal(catalogCursor{Version: api.SessionAPIVersion, CreatedAt: last.CreatedAt, ID: last.SessionID, Scope: scope})
		next = base64.RawURLEncoding.EncodeToString(data)
	}
	if view == "summary" {
		return c.JSON(http.StatusOK, api.SessionSummaryCatalog[runtime.SessionSummaryEntry]{Version: api.SessionAPIVersion, View: view, Sessions: rows, NextCursor: next})
	}
	catalog := sessionCatalogDTO{Version: api.SessionAPIVersion, NextCursor: next, Sessions: make([]sessionResourceDTO, 0, len(rows))}
	if len(s.sm.sessionRegistries) == 0 {
		if s.sm.sessionRegistry != nil {
			catalog.Sources = append(catalog.Sources, sessionCatalogSourceDTO{CanCreate: true})
		}
	} else {
		for source := range s.sm.sessionRegistries {
			catalog.Sources = append(catalog.Sources, sessionCatalogSourceDTO{Name: source, CanCreate: true})
		}
		sort.Slice(catalog.Sources, func(i, j int) bool { return catalog.Sources[i].Name < catalog.Sources[j].Name })
	}
	for _, row := range rows {
		entry := sessionResourceDTO{SessionID: row.SessionID, ParentID: row.ParentID, Title: row.Title, AgentName: row.AgentName, Source: row.Source, CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: row.UpdatedAt, Starred: row.Starred, NumMessages: row.NumMessages, Cost: row.Cost, WorkingDir: row.WorkingDir, Loaded: row.Loaded, Loadable: row.Loadable, Attachable: row.Loadable, RequiresConfirmation: row.RequiresConfirmation, RouteError: row.RouteError}
		if live, ok := s.sm.runtimeSessions.Load(row.SessionID); ok && live.handle != nil {
			if status, err := live.handle.Status(c.Request().Context()); err == nil {
				entry.StateKnown = true
				entry.State = status.State
				entry.LastError = status.LastError
				entry.Pending = &status.Pending
			} else {
				entry.RouteError = "session status unavailable"
			}
		}
		catalog.Sessions = append(catalog.Sessions, entry)
	}
	return c.JSON(http.StatusOK, catalog)
}

func (sm *SessionManager) summaryEntry(row session.Summary) runtime.SessionSummaryEntry {
	entry := runtime.SessionSummaryEntry{SessionID: row.ID, ParentID: row.ParentID, Title: row.Title, AgentName: row.Attributes[sessionAgentAttribute], Source: row.Attributes[sessionSourceAttribute], CreatedAt: row.CreatedAt, UpdatedAt: row.CreatedAt.UTC().Format(time.RFC3339Nano), Starred: row.Starred, NumMessages: row.NumMessages, Cost: row.Cost, WorkingDir: row.WorkingDir}
	entry.Model = row.AgentModelOverrides[entry.AgentName]
	if live, ok := sm.runtimeSessions.Load(row.ID); ok && live.handle != nil {
		metadata := live.handle.Metadata()
		entry.Loaded, entry.Loadable = true, true
		entry.AgentName, entry.Model = metadata.AgentName, metadata.Model
	} else if entry.AgentName == "" {
		entry.RouteError = "session is not attachable"
	} else if registry, _, err := sm.sessionRegistryForCreate(entry.Source); err != nil {
		entry.RouteError = "persisted session source is unavailable or ambiguous"
	} else if row.ParentID == "" {
		entry.Loadable = true
	} else if _, ok := registry.(runtime.TreeRestorer); !ok {
		entry.RouteError = "runtime cannot load durable child session tree"
	} else {
		entry.RequiresConfirmation = true
	}
	return entry
}
