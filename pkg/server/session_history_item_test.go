package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestHistoryItemResponseChecksSizeBeforeWriting(t *testing.T) {
	recorder := httptest.NewRecorder()
	c := echo.New().NewContext(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody), recorder)
	writer := historyItemResponseWriter{context: c, maxBytes: 4}
	err := json.NewEncoder(writer).Encode("ab")
	require.ErrorIs(t, err, session.ErrHistoryItemTooLarge)
	var httpErr *echo.HTTPError
	require.ErrorAs(t, sessionHTTPError(err), &httpErr)
	assert.Equal(t, http.StatusRequestEntityTooLarge, httpErr.Code)
	assert.Equal(t, map[string]any{"error": "history_item_too_large"}, httpErr.Message)
	assert.False(t, c.Response().Committed)
	assert.Empty(t, recorder.Body)
	require.NoError(t, json.NewEncoder(writer).Encode("a"))
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "\"a\"\n", recorder.Body.String())
}
