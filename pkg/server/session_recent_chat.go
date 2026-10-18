package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

var errRecentChatScopeInvalid = errors.New("recent-chat scope is not authorized")

const (
	recentChatAncestryLimit = 32
	recentChatScanLimit     = 128
)

type recentChatCursor struct {
	Version   int    `json:"v"`
	SessionID string `json:"s"`
	Position  int    `json:"p"`
	ItemID    int64  `json:"i"`
}

func (s *Server) recentCanonicalChat(c echo.Context) error {
	id := c.Param("id")
	limit := api.RecentChatDefaultLimit
	if values, ok := c.QueryParams()["limit"]; ok {
		if len(values) != 1 {
			return sessionRequestError("limit must be specified once")
		}
		parsed, err := strconv.Atoi(values[0])
		if err != nil || parsed < 1 || parsed > api.RecentChatMaxLimit {
			return sessionRequestError("limit must be between 1 and 24")
		}
		limit = parsed
	}
	if values, ok := c.QueryParams()["source"]; ok && len(values) != 1 {
		return sessionRequestError("source must be specified once")
	}
	requestedSource := c.QueryParam("source")
	if len(c.QueryParams()["cursor"]) > 1 {
		return sessionRequestError("cursor must be specified once")
	}

	store, ok := s.sm.sessionStore.(session.RecentChatStore)
	if !ok {
		return sessionHTTPError(runtime.UnsupportedSessionOperation(id, "recent_chat"))
	}
	selected, root, err := readRecentChatScope(c.Request().Context(), store, id)
	if err != nil {
		return recentChatScopeError(err, id)
	}
	if requestedSource != "" && requestedSource != root.Source {
		return sessionHTTPError(&runtime.SessionError{Kind: runtime.SessionErrorWrongSession, SessionID: id, Operation: "recent_chat", Detail: "session belongs to a different source"})
	}
	if root.Source != "" {
		if _, err := recentChatSourceRegistry(s.sm, root.Source); err != nil {
			return sessionHTTPError(&runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: id, Operation: "source"})
		}
	} else if len(s.sm.sourceNames()) > 1 {
		return sessionHTTPError(&runtime.SessionError{Kind: runtime.SessionErrorWrongSession, SessionID: id, Operation: "recent_chat", Detail: "session source is ambiguous"})
	}

	beforePosition := 0
	var beforeItemID int64
	cursor := c.QueryParam("cursor")
	if cursor != "" {
		parsed, err := decodeRecentChatCursor(cursor)
		if err != nil || parsed.SessionID != id || parsed.Position <= 0 || parsed.ItemID < 0 {
			return sessionRequestError("invalid recent-chat cursor")
		}
		if err := store.ValidateRecentChatAnchor(c.Request().Context(), id, parsed.Position, parsed.ItemID); err != nil {
			if errors.Is(err, session.ErrRecentChatCursorStale) {
				return echo.NewHTTPError(http.StatusConflict, "recent-chat cursor is stale")
			}
			return sessionHTTPError(err)
		}
		beforePosition, beforeItemID = parsed.Position, parsed.ItemID
	}
	items, err := store.ReadRecentChatItems(c.Request().Context(), id, beforePosition, beforeItemID, limit, recentChatScanLimit, api.RecentChatMaxItemTextBytes)
	if err != nil {
		return sessionHTTPError(err)
	}

	response := api.RecentChat{
		Version: api.SessionAPIVersion, SessionID: selected.ID, RootSessionID: root.ID,
		Source: root.Source, AgentID: selected.AgentID, AgentName: selected.AgentName,
		RuntimeStatus: "offline", Messages: make([]api.RecentChatMessage, 0, len(items.Messages)),
	}
	if selected.AgentName == "" {
		response.RuntimeStatus = "historical"
	} else if handle := s.sm.loadedSession(id); handle != nil {
		if attention, ok := handle.(runtime.ResidentAttentionReader); ok {
			projection, attentionErr := attention.ReadResidentAttention(c.Request().Context())
			if attentionErr == nil && projection.Known && projection.AgentID == selected.AgentID && projection.AgentName == selected.AgentName && projection.ParentID == selected.ParentID && projection.Source == selected.Source && recentChatSameWorkspace(projection.WorkingDir, selected.WorkingDir) {
				response.RuntimeStatus = "resident"
				if !projection.ExecutionAvailable {
					response.RuntimeStatus = "read_only"
				}
				response.AttentionKnown = true
				response.ApprovalCount = projection.ApprovalCount
				response.ElicitationCount = projection.ElicitationCount
				response.ExecutionAvailable = projection.ExecutionAvailable
				if status, statusErr := handle.Status(c.Request().Context()); statusErr == nil {
					response.State = string(status.State)
					response.Pending = status.Pending
				}
			} else {
				response.RuntimeStatus = "unknown"
			}
		}
	}
	for _, message := range items.Messages {
		response.Messages = append(response.Messages, api.RecentChatMessage{
			ID: message.ID, Position: message.Position, Role: message.Role,
			AgentName: message.AgentName, Text: message.Text, Truncated: message.Truncated,
		})
	}
	// Store readers return newest-first for efficient reverse-index scans. Keep
	// the API's display order chronological without touching older transcript rows.
	for left, right := 0, len(response.Messages)-1; left < right; left, right = left+1, right-1 {
		response.Messages[left], response.Messages[right] = response.Messages[right], response.Messages[left]
	}
	response.TruncatedBefore = items.HasMore || beforePosition > 0
	if items.HasMore {
		response.BeforeCursor = encodeRecentChatCursor(recentChatCursor{Version: 1, SessionID: id, Position: items.NextBeforePosition, ItemID: items.NextBeforeItemID})
	}

	response, err = fitRecentChatResponse(response, api.RecentChatMaxResponseBytes)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "unable to fit recent-chat response within byte limit")
	}
	encoded, err := json.Marshal(response)
	if err != nil || len(encoded) > api.RecentChatMaxResponseBytes {
		return echo.NewHTTPError(http.StatusInternalServerError, "unable to fit recent-chat response within byte limit")
	}
	return c.Blob(http.StatusOK, "application/json", encoded)
}

func recentChatSourceRegistry(sm *SessionManager, sourceID string) (runtime.SessionRuntime, error) {
	if _, exactSource := sm.Source(sourceID); exactSource {
		if registry, ok := sm.sourceRegistry(sourceID); ok && registry != nil {
			return registry, nil
		}
		return nil, errors.New("session source is unavailable")
	}
	if _, err := sm.resolveSource(sourceID); err != nil {
		return nil, err
	}
	want := config.StableSourceKey(sourceID)
	var registry runtime.SessionRuntime
	var matches int
	for _, name := range sm.sourceNames() {
		if config.StableSourceKey(name) != want {
			continue
		}
		if _, ok := sm.Source(name); !ok {
			continue
		}
		candidate, ok := sm.sourceRegistry(name)
		if !ok || candidate == nil {
			continue
		}
		registry, matches = candidate, matches+1
	}
	if matches != 1 {
		return nil, errors.New("session source is unavailable or ambiguous")
	}
	return registry, nil
}

func readRecentChatScope(ctx context.Context, store session.RecentChatStore, id string) (*session.RecentChatMetadata, *session.RecentChatMetadata, error) {
	selected, err := store.ReadRecentChatMetadata(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if selected.ID != id {
		return nil, nil, fmt.Errorf("%w: session metadata identity mismatch", errRecentChatScopeInvalid)
	}
	chain := []*session.RecentChatMetadata{selected}
	seen := map[string]bool{id: true}
	for chain[len(chain)-1].ParentID != "" {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if len(chain) >= recentChatAncestryLimit {
			return nil, nil, fmt.Errorf("%w: session ancestry exceeds recent-chat bound", errRecentChatScopeInvalid)
		}
		parentID := chain[len(chain)-1].ParentID
		if seen[parentID] {
			return nil, nil, fmt.Errorf("%w: session ancestry contains a cycle", errRecentChatScopeInvalid)
		}
		parent, err := store.ReadRecentChatMetadata(ctx, parentID)
		if err != nil {
			return nil, nil, err
		}
		if parent.ID != parentID {
			return nil, nil, fmt.Errorf("%w: session ancestry identity mismatch", errRecentChatScopeInvalid)
		}
		seen[parentID] = true
		chain = append(chain, parent)
	}
	root := chain[len(chain)-1]
	rootNode := subagent.SessionRootID(root.ID)
	nodeBySession := map[string]subagent.NodeID{root.ID: rootNode}
	nodeIDs := map[subagent.NodeID]bool{rootNode: true}
	for i := len(chain) - 2; i >= 0; i-- {
		child, parent := chain[i], chain[i+1]
		if child.Source != root.Source || parent.Source != root.Source {
			return nil, nil, fmt.Errorf("%w: session ancestry source does not match its root", errRecentChatScopeInvalid)
		}
		if !recentChatSameWorkspace(child.WorkingDir, root.WorkingDir) || !recentChatSameWorkspace(parent.WorkingDir, root.WorkingDir) {
			return nil, nil, fmt.Errorf("%w: session ancestry workspace does not match its root", errRecentChatScopeInvalid)
		}
		if !validRecentChatAgentProvenance(child) || !validRecentChatAgentProvenance(parent) {
			return nil, nil, fmt.Errorf("%w: session agent identity provenance is incomplete", errRecentChatScopeInvalid)
		}
		record, err := store.ReadRecentChatChild(ctx, child.ID)
		if err != nil {
			return nil, nil, err
		}
		if record.RootSessionID != root.ID || record.ParentSessionID != parent.ID || record.NodeSessionID != child.ID ||
			record.NodeID == "" || record.NodeParent != string(nodeBySession[parent.ID]) || record.NodeAgent == "" || record.NodeAgent != child.AgentName || nodeIDs[subagent.NodeID(record.NodeID)] {
			return nil, nil, fmt.Errorf("%w: session child membership does not match persisted ancestry", errRecentChatScopeInvalid)
		}
		if child.ParentAgentID != "" && child.ParentAgentName == "" || child.ParentAgentName != "" && child.ParentAgentName != parent.AgentName || child.ParentAgentID != "" && child.ParentAgentID != parent.AgentID {
			return nil, nil, fmt.Errorf("%w: session parent-agent provenance does not match persisted ancestry", errRecentChatScopeInvalid)
		}
		nodeBySession[child.ID] = subagent.NodeID(record.NodeID)
		nodeIDs[subagent.NodeID(record.NodeID)] = true
	}
	if root.ParentAgentName != "" || root.ParentAgentID != "" {
		return nil, nil, fmt.Errorf("%w: root has parent-agent provenance", errRecentChatScopeInvalid)
	}
	for _, metadata := range chain {
		if metadata.Source != root.Source {
			return nil, nil, fmt.Errorf("%w: session ancestry source does not match its root", errRecentChatScopeInvalid)
		}
		if !recentChatSameWorkspace(metadata.WorkingDir, root.WorkingDir) {
			return nil, nil, fmt.Errorf("%w: session ancestry workspace does not match its root", errRecentChatScopeInvalid)
		}
		if !validRecentChatAgentProvenance(metadata) {
			return nil, nil, fmt.Errorf("%w: session agent identity provenance is incomplete", errRecentChatScopeInvalid)
		}
	}
	return selected, root, nil
}

func validRecentChatAgentProvenance(metadata *session.RecentChatMetadata) bool {
	return metadata.AgentID == "" || metadata.AgentName != ""
}

func recentChatSameWorkspace(a, b string) bool {
	normalize := func(path string) string {
		if path == "" {
			return ""
		}
		if absolute, err := filepath.Abs(path); err == nil {
			path = absolute
		}
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		}
		return filepath.Clean(path)
	}
	return normalize(a) == normalize(b)
}

func recentChatScopeError(err error, id string) error {
	if errors.Is(err, session.ErrNotFound) {
		return sessionHTTPError(&runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: id, Operation: "recent_chat"})
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, errRecentChatScopeInvalid) {
		return sessionHTTPError(&runtime.SessionError{Kind: runtime.SessionErrorWrongSession, SessionID: id, Operation: "recent_chat", Detail: err.Error()})
	}
	return sessionHTTPError(err)
}

func encodeRecentChatCursor(cursor recentChatCursor) string {
	data, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeRecentChatCursor(value string) (recentChatCursor, error) {
	var cursor recentChatCursor
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(data) > 1024 {
		return cursor, errors.New("invalid cursor")
	}
	if err := json.Unmarshal(data, &cursor); err != nil || cursor.Version != 1 || cursor.SessionID == "" {
		return recentChatCursor{}, errors.New("invalid cursor")
	}
	return cursor, nil
}

func fitRecentChatResponse(response api.RecentChat, maxBytes int) (api.RecentChat, error) {
	for len(response.Messages) > 0 {
		encoded, err := json.Marshal(response)
		if err != nil {
			return response, err
		}
		if len(encoded) <= maxBytes {
			return response, nil
		}
		// Preserve the newest messages first; drop oldest records until the
		// compact projection fits its hard wire budget.
		response.Messages = response.Messages[1:]
		response.TruncatedBefore = true
		if len(response.Messages) > 0 {
			oldest := response.Messages[0]
			response.BeforeCursor = encodeRecentChatCursor(recentChatCursor{Version: 1, SessionID: response.SessionID, Position: oldest.Position, ItemID: oldest.ID})
		}
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return response, err
	}
	if len(encoded) > maxBytes {
		return response, fmt.Errorf("metadata response exceeds %d bytes", maxBytes)
	}
	return response, nil
}
