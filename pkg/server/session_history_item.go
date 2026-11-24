package server

import (
	"encoding/json"

	"github.com/labstack/echo/v4"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func (s *Server) canonicalSessionHistoryItem(c echo.Context) error {
	params := c.QueryParams()
	if len(params) != 1 || len(params["cursor"]) != 1 || params["cursor"][0] == "" {
		return sessionHTTPError(runtime.ErrHistoryCursorInvalid)
	}
	handle, err := s.sm.handle(c.Request().Context(), c.Param("id"), true)
	if err != nil {
		return sessionHTTPError(err)
	}
	reader, ok := handle.(runtime.SessionHistoryItemReader)
	if !ok {
		return sessionHTTPError(runtime.UnsupportedSessionOperation(handle.ID(), runtime.SessionOperationObserve))
	}
	page, err := reader.ReadHistoryItem(c.Request().Context(), handle.ID(), params["cursor"][0])
	if err != nil {
		return sessionHTTPError(err)
	}
	header := page
	header.Item = session.Item{}
	metadata, err := json.Marshal(header)
	if err != nil {
		return sessionHTTPError(err)
	}
	// Encoder writes one complete value. Check its size before committing 200,
	// without copying the encoder's buffer with Marshal or RawMessage.
	writer := historyItemResponseWriter{context: c, maxBytes: api.SessionHistoryItemMaxBytes + len(metadata) - len("{}") + 1}
	if err := json.NewEncoder(writer).Encode(page); err != nil {
		return sessionHTTPError(err)
	}
	return nil
}

type historyItemResponseWriter struct {
	context  echo.Context
	maxBytes int
}

func (w historyItemResponseWriter) Write(data []byte) (int, error) {
	if len(data) > w.maxBytes {
		return 0, session.ErrHistoryItemTooLarge
	}
	response := w.context.Response()
	response.Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	return response.Write(data)
}
