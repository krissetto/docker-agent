package runtime

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
)

func TestRemoteCatalogFollowsAllPages(t *testing.T) {
	for _, summary := range []bool{false, true} {
		t.Run(fmt.Sprintf("summary=%t", summary), func(t *testing.T) {
			const total = 123
			calls := 0
			created := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, api.SessionAPIPath, r.URL.Path)
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, strconv.Itoa(api.SessionCatalogDefaultLimit), r.URL.Query().Get("limit"))
				if summary {
					assert.Equal(t, "summary", r.URL.Query().Get("view"))
					assert.Equal(t, "true", r.URL.Query().Get("include_children"))
				}
				start := 0
				if cursor := r.URL.Query().Get("cursor"); cursor != "" {
					raw, err := base64.RawURLEncoding.DecodeString(cursor)
					if !assert.NoError(t, err) {
						http.Error(w, "cursor", http.StatusBadRequest)
						return
					}
					start, err = strconv.Atoi(string(raw))
					if !assert.NoError(t, err) {
						http.Error(w, "cursor", http.StatusBadRequest)
						return
					}
				}
				end := min(start+api.SessionCatalogDefaultLimit, total)
				next := ""
				if end < total {
					next = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(end)))
				}
				w.Header().Set("Content-Type", "application/json")
				if summary {
					page := api.SessionSummaryCatalog[SessionSummaryEntry]{Version: api.SessionAPIVersion, View: "summary", NextCursor: next}
					for i := start; i < end; i++ {
						page.Sessions = append(page.Sessions, SessionSummaryEntry{SessionID: strconv.Itoa(i), ParentID: "parent", CreatedAt: created, NumMessages: 17, Cost: 2.5, RequiresConfirmation: true})
					}
					assert.NoError(t, json.NewEncoder(w).Encode(page))
				} else {
					page := api.SessionCatalog[SessionState]{Version: api.SessionAPIVersion, NextCursor: next}
					for i := start; i < end; i++ {
						page.Sessions = append(page.Sessions, api.SessionResource[SessionState]{SessionID: strconv.Itoa(i), CreatedAt: created.Format(time.RFC3339Nano), NumMessages: 17, Cost: 2.5})
					}
					assert.NoError(t, json.NewEncoder(w).Encode(page))
				}
			}))
			defer server.Close()
			client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
			require.NoError(t, err)
			transport, err := NewSessionTransport(client)
			require.NoError(t, err)
			if summary {
				rows, err := transport.ListSessionSummaries(t.Context(), SessionSummaryOptions{IncludeChildren: true})
				require.NoError(t, err)
				require.Len(t, rows, total)
				assert.Equal(t, "122", rows[122].SessionID)
				assert.True(t, rows[122].RequiresConfirmation)
				assert.Equal(t, 17, rows[122].NumMessages)
				assert.InDelta(t, 2.5, rows[122].Cost, 0.001)
			} else {
				rows, err := transport.ListSessions(t.Context())
				require.NoError(t, err)
				require.Len(t, rows, total)
				assert.Equal(t, "122", rows[122].SessionID)
				assert.Equal(t, created, rows[122].CreatedAt)
				assert.Equal(t, 17, rows[122].NumMessages, "metadata count does not depend on transcript")
				assert.InDelta(t, 2.5, rows[122].Cost, 0.001)
			}
			assert.Equal(t, 3, calls)
		})
	}
}

func TestRemoteCatalogRejectsIncompleteListings(t *testing.T) {
	for _, summary := range []bool{false, true} {
		for _, failure := range []string{"repeat", "cycle", "invalid-cursor", "long-cursor", "empty-page", "later-http", "later-json", "later-version", "duplicate", "missing-identity"} {
			t.Run(fmt.Sprintf("summary=%t/%s", summary, failure), func(t *testing.T) {
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if calls > 4 {
						http.Error(w, "unbounded pagination", http.StatusInternalServerError)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					version, next, id := api.SessionAPIVersion, "cGFnZQ", strconv.Itoa(calls)
					empty := false
					if calls > 1 {
						next = ""
						switch failure {
						case "repeat":
							next = "cGFnZQ"
						case "cycle":
							if calls == 2 {
								next = "cGFnZTI"
							} else {
								next = "cGFnZQ"
							}
						case "invalid-cursor":
							next = "?cursor=other"
						case "long-cursor":
							next = strings.Repeat("a", 2049)
						case "empty-page":
							empty = true
							next = "cGFnZTI"
						case "later-http":
							http.Error(w, `{"error":"persistence"}`, http.StatusServiceUnavailable)
							return
						case "later-json":
							fmt.Fprint(w, `{"sessions":`)
							return
						case "later-version":
							version++
						case "duplicate":
							id = "1"
						case "missing-identity":
							id = ""
						}
					}
					if summary {
						page := api.SessionSummaryCatalog[SessionSummaryEntry]{Version: version, View: "summary", NextCursor: next}
						if !empty {
							page.Sessions = []SessionSummaryEntry{{SessionID: id}}
						}
						assert.NoError(t, json.NewEncoder(w).Encode(page))
					} else {
						page := api.SessionCatalog[SessionState]{Version: version, NextCursor: next}
						if !empty {
							page.Sessions = []api.SessionResource[SessionState]{{SessionID: id}}
						}
						assert.NoError(t, json.NewEncoder(w).Encode(page))
					}
				}))
				defer server.Close()
				client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
				require.NoError(t, err)
				transport, err := NewSessionTransport(client)
				require.NoError(t, err)
				if summary {
					rows, err := transport.ListSessionSummaries(t.Context(), SessionSummaryOptions{})
					require.Error(t, err)
					assert.Nil(t, rows, "never return an incomplete listing")
				} else {
					rows, err := transport.ListSessions(t.Context())
					require.Error(t, err)
					assert.Nil(t, rows, "never return an incomplete listing")
				}
				if failure == "cycle" {
					assert.Equal(t, 3, calls)
				} else {
					assert.Equal(t, 2, calls)
				}
			})
		}
	}
}
