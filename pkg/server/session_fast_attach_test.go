package server

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

type fastAttachStore struct {
	*session.SQLiteSessionStore

	fullReads atomic.Int64
}

func (s *fastAttachStore) GetSession(ctx context.Context, id string) (*session.Session, error) {
	s.fullReads.Add(1)
	return s.SQLiteSessionStore.GetSession(ctx, id)
}

func (s *fastAttachStore) GetSessionOwnHistory(ctx context.Context, id string) (*session.Session, error) {
	s.fullReads.Add(1)
	return s.SQLiteSessionStore.GetSessionOwnHistory(ctx, id)
}

func openFastAttachStore(tb testing.TB) *fastAttachStore {
	tb.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(tb, err)
	db.SetMaxOpenConns(1)
	store, err := session.NewSQLiteSessionStoreFromDB(tb.Context(), db)
	require.NoError(tb, err)
	tb.Cleanup(func() { require.NoError(tb, store.Close()) })
	return &fastAttachStore{SQLiteSessionStore: store}
}

func fastAttachManager(tb testing.TB, ctx context.Context, store session.Store) *SessionManager {
	tb.Helper()
	policy := runtime.DefaultSessionResourcePolicy()
	return NewSessionManager(ctx, config.Sources{"agent": &memorySource{data: "agents: {}"}}, store, 0,
		&config.RuntimeConfig{}, WithSessionRuntimeFactory(reopenFactory(store, policy)))
}

func TestSQLiteSelectedAttachPagesAndThenHydratesOnSubmit(t *testing.T) {
	store := openFastAttachStore(t)
	root := session.New(session.WithID("cold-selected"), session.WithWorkingDir(t.TempDir()), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "root", sessionSourceAttribute: "agent"}))
	root.SafetyPolicy = session.SafetyPolicyAutonomous
	root.ToolsApproved, root.Starred = true, true
	root.AddAttachedFile(root.WorkingDir + "/notes.txt")
	root.SetTokensAndCost(321, 123, 0)
	for i := range 120 {
		m := session.UserMessage(fmt.Sprintf("history-%03d", i))
		m.TurnID = m.Message.Content
		root.AddMessage(m)
		root.SetTurnTiming(m.TurnID, session.TurnTiming{Version: session.TurnTimingVersion, Basis: session.TurnTimingBasisActiveWall, TurnID: m.TurnID, DurationMs: int64(i), Outcome: session.TurnTimingOutcomeCompleted})
	}
	pending := session.UserMessage("durable-queued")
	pending.Pending, pending.Accepted, pending.TurnID = true, true, "queued"
	root.AddMessage(pending)
	require.NoError(t, store.AddSession(t.Context(), root))
	manager := fastAttachManager(t, t.Context(), store)
	t.Cleanup(func() { require.NoError(t, manager.Shutdown(context.WithoutCancel(t.Context()))) })
	srv := NewWithManager(manager, "")
	base := api.SessionAPIPath + "/" + root.ID
	prepared := sessionRequest(t, srv, http.MethodGet, base+"?view=prepare-info&projection=metadata", "", "")
	require.Equal(t, http.StatusOK, prepared.Code, prepared.Body.String())
	var info struct {
		Info runtime.PreparedSessionViewInfo `json:"info"`
	}
	require.NoError(t, json.Unmarshal(prepared.Body.Bytes(), &info))
	confirmation := info.Info.Confirmation()
	body, err := json.Marshal(runtime.SessionEdit{Kind: runtime.SessionEditOpenView, ViewConfirmation: &confirmation})
	require.NoError(t, err)
	opened := sessionRequest(t, srv, http.MethodPatch, base+"?view=open-view&projection=metadata", string(body), "")
	require.Equal(t, http.StatusOK, opened.Code, opened.Body.String())
	assert.NotContains(t, opened.Body.String(), "history-")

	httpServer := httptest.NewServer(srv.e)
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	// Keep the same stream subscribed across pagination and first submit.
	scanner, closeTail := fastAttachStream(t, ctx, httpServer, base+"/events?projection=tail&max_items=3&max_bytes=16384")
	defer closeTail()
	tail := readFastAttachFrame(t, scanner)
	require.Equal(t, "snapshot", tail.Type)
	require.Equal(t, "ready", readFastAttachFrame(t, scanner).Type)
	require.NotNil(t, tail.Snapshot)
	anchor := tail.Snapshot
	require.Equal(t, 121, anchor.TranscriptPosition)
	require.Equal(t, 118, anchor.History.StartPosition)
	require.Equal(t, 121, anchor.History.EndPosition)
	require.Len(t, anchor.Items, 3)
	require.Len(t, anchor.PendingInputs, 1)
	assert.Equal(t, 120, anchor.PendingInputs[0].SessionPosition)
	assert.Equal(t, "queued", anchor.PendingInputs[0].TurnID)
	assert.Empty(t, anchor.Session.Messages)
	assert.Equal(t, int64(119), anchor.Session.TurnTimings["history-119"].DurationMs)
	require.Len(t, anchor.Presentation, 2)

	treeScan, closeTree := fastAttachStream(t, ctx, httpServer, base+"/events?tree=true&projection=metadata")
	defer closeTree()
	tree := readFastAttachFrame(t, treeScan)
	require.Equal(t, "snapshot", tree.Type)
	require.NotNil(t, tree.Snapshot)
	assert.Equal(t, "metadata", tree.Snapshot.Projection)
	assert.NotNil(t, tree.Snapshot.Session.SubagentTree)
	assert.Empty(t, tree.Snapshot.Session.Messages)
	assertFastAttachPresentation(t, anchor.Presentation, tree.Snapshot.Presentation)
	require.Equal(t, "ready", readFastAttachFrame(t, treeScan).Type)

	cursor := anchor.History.NextCursor
	for end := 118; end >= 115; end -= 3 {
		response := sessionRequest(t, srv, http.MethodGet, base+"/history?projection=tail&max_items=3&max_bytes=16384&cursor="+url.QueryEscape(cursor), "", "")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.LessOrEqual(t, response.Body.Len(), 16384)
		var page api.SessionHistoryPage
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page))
		assert.Equal(t, end, page.History.EndPosition)
		assert.Equal(t, end-3, page.History.StartPosition)
		assert.Equal(t, anchor.TranscriptPosition, page.History.TotalItems)
		assert.Equal(t, anchor.Cursor, page.Cursor)
		assert.Equal(t, anchor.Epoch, page.Epoch)
		require.Len(t, page.Items, 3)
		for i, item := range page.Items {
			pos := end - 3 + i
			assert.Equal(t, pos, item.Position)
			assert.Equal(t, fmt.Sprintf("history-%03d", pos), item.Item.Message.Message.Content)
			assert.Equal(t, int64(pos), page.TurnTimings[item.Item.Message.TurnID].DurationMs)
		}
		cursor = page.History.NextCursor
	}
	require.Zero(t, store.fullReads.Load(), "prepare, open, tail, tree and older pages cannot hydrate history")

	handle, err := manager.Handle(ctx, root.ID)
	require.NoError(t, err)
	submitted, err := handle.Submit(ctx, runtime.TurnInput{Content: "new live input"})
	require.NoError(t, err)
	assert.EqualValues(t, 1, store.fullReads.Load(), "the first operation materializes once")
	last := anchor.Cursor
	var accepted int
	for accepted != 1 {
		frame := readFastAttachFrame(t, scanner)
		if frame.Envelope == nil || frame.Envelope.Sequence == 0 {
			continue
		}
		assert.Greater(t, frame.Envelope.Sequence, last)
		last = frame.Envelope.Sequence
		if frame.Envelope.TurnID == submitted.TurnID && frame.Envelope.TranscriptPosition >= 0 {
			assert.GreaterOrEqual(t, frame.Envelope.TranscriptPosition, anchor.TranscriptPosition)
			accepted++
		}
	}
	full, err := handle.Snapshot(ctx)
	require.NoError(t, err)
	legacy, err := handle.Observe(ctx, runtime.ObserveOptions{})
	require.NoError(t, err)
	defer legacy.Cancel()
	assertFastAttachPresentation(t, anchor.Presentation, sessionSnapshot(legacy.Primary()).Presentation)
	assert.Equal(t, full.CreatedAt, anchor.Session.CreatedAt)
	assert.Equal(t, full.SafetyPolicy, anchor.Session.SafetyPolicy)
	assert.Equal(t, full.ToolsApproved, anchor.Session.ToolsApproved)
	assert.Equal(t, full.AttachedFiles, anchor.Session.AttachedFiles)
	assert.Equal(t, full.Starred, anchor.Session.Starred)
	assert.Equal(t, root.InputTokens, anchor.Session.InputTokens)
	assert.Equal(t, root.OutputTokens, anchor.Session.OutputTokens)
	require.Equal(t, "history-000", full.Messages[0].Message.Message.Content)
}

func assertFastAttachPresentation(t *testing.T, want, got []any) {
	t.Helper()
	require.Len(t, got, len(want))
	for i := range want {
		normalize := func(event any) map[string]any {
			data, err := json.Marshal(event)
			require.NoError(t, err)
			var fields map[string]any
			require.NoError(t, json.Unmarshal(data, &fields))
			delete(fields, "timestamp")
			return fields
		}
		assert.Equal(t, normalize(want[i]), normalize(got[i]))
	}
}

func fastAttachStream(t *testing.T, ctx context.Context, server *httptest.Server, path string) (*bufio.Scanner, func()) {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+path, http.NoBody)
	require.NoError(t, err)
	response, err := server.Client().Do(request) //nolint:bodyclose // ownership transfers to the returned cleanup
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	return bufio.NewScanner(response.Body), func() { require.NoError(t, response.Body.Close()) }
}

func readFastAttachFrame(t *testing.T, scanner *bufio.Scanner) sessionStreamMessage {
	t.Helper()
	for scanner.Scan() {
		if line, ok := strings.CutPrefix(scanner.Text(), "data: "); ok {
			var frame sessionStreamMessage
			require.NoError(t, json.Unmarshal([]byte(line), &frame))
			return frame
		}
	}
	t.Fatalf("session stream ended: %v", scanner.Err())
	return sessionStreamMessage{}
}

func BenchmarkSQLiteSelectedAttach(b *testing.B) {
	store := openFastAttachStore(b)
	root := session.New(session.WithID("benchmark-selected"), session.WithWorkingDir(b.TempDir()), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "root", sessionSourceAttribute: "agent"}))
	content := strings.Repeat("synthetic transcript ", 20)
	for range 5000 {
		root.AddMessage(session.UserMessage(content))
	}
	require.NoError(b, store.AddSession(b.Context(), root))
	for _, projection := range []string{"legacy", "tail"} {
		b.Run(projection, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				manager := fastAttachManager(b, b.Context(), store)
				b.StartTimer()
				ready, err := manager.confirmedSessionViewInfo(b.Context(), root.ID, projection == "tail")
				require.NoError(b, err)
				p, err := manager.prepareSessionView(b.Context(), ready.SessionID, projection == "tail")
				require.NoError(b, err)
				require.NoError(b, ready.Confirmation().Validate(p.Info()))
				committed, err := p.Commit(b.Context())
				require.NoError(b, err)
				options := runtime.ObserveOptions{}
				if projection == "tail" {
					options.HistoryTail = &runtime.SessionHistoryLimits{}
				}
				obs, err := committed.SessionHandle.Observe(b.Context(), options)
				require.NoError(b, err)
				_, err = json.Marshal(sessionSnapshot(obs.Primary()))
				require.NoError(b, err)
				b.StopTimer()
				obs.Cancel()
				p.Abort()
				require.NoError(b, manager.Shutdown(b.Context()))
				b.StartTimer()
			}
		})
	}
}

func TestSQLiteHistoryOmissionsCanBeFetchedWithoutHydration(t *testing.T) {
	store := openFastAttachStore(t)
	root := session.New(session.WithID("cold-oversize"), session.WithWorkingDir(t.TempDir()), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "root", sessionSourceAttribute: "agent"}))
	older := strings.Repeat("o", 32<<10)
	recent := strings.Repeat("r", (16<<20)+1)
	root.AddMessage(session.UserMessage("start"))
	root.AddMessage(session.UserMessage(older))
	root.AddMessage(session.UserMessage(recent))
	require.NoError(t, store.AddSession(t.Context(), root))
	manager := fastAttachManager(t, t.Context(), store)
	t.Cleanup(func() { require.NoError(t, manager.Shutdown(context.WithoutCancel(t.Context()))) })
	srv := NewWithManager(manager, "")
	base := api.SessionAPIPath + "/" + root.ID
	httpServer := httptest.NewServer(srv.e)
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	scanner, closeTail := fastAttachStream(t, ctx, httpServer, base+"/events?projection=tail&max_items=1&max_bytes=16384")
	defer closeTail()
	frame := readFastAttachFrame(t, scanner)
	require.Equal(t, "snapshot", frame.Type)
	require.Equal(t, "ready", readFastAttachFrame(t, scanner).Type)
	require.NotNil(t, frame.Snapshot)
	anchor := frame.Snapshot
	require.NotNil(t, anchor.History)
	require.Empty(t, anchor.Items)

	checkOmission := func(history api.SessionHistory, position int, reason, content string) {
		t.Helper()
		require.Len(t, history.OmittedRanges, 1)
		r := history.OmittedRanges[0]
		assert.Equal(t, position, r.StartPosition)
		assert.Equal(t, position+1, r.EndPosition)
		assert.Equal(t, reason, r.Reason)
		require.Len(t, r.Items, 1)
		assert.Equal(t, position, r.Items[0].Position)
		require.NotEmpty(t, r.Items[0].Cursor)
		response := sessionRequest(t, srv, http.MethodGet, base+"/history/item?cursor="+url.QueryEscape(r.Items[0].Cursor), "", "")
		require.Equal(t, http.StatusOK, response.Code)
		var page api.SessionHistoryItemPage
		require.NoError(t, json.NewDecoder(response.Body).Decode(&page))
		assert.Equal(t, root.ID, page.SessionID)
		assert.Equal(t, anchor.Epoch, page.Epoch)
		assert.Equal(t, anchor.Cursor, page.Cursor)
		assert.Equal(t, anchor.TranscriptPosition, page.TranscriptPosition)
		assert.Equal(t, position, page.Position)
		require.NotNil(t, page.Item.Message)
		require.Equal(t, sha256.Sum256([]byte(content)), sha256.Sum256([]byte(page.Item.Message.Message.Content)), "returned item content must match exactly")
	}
	invalid := sessionRequest(t, srv, http.MethodGet, base+"/history/item?cursor="+url.QueryEscape(anchor.History.NextCursor), "", "")
	require.Equal(t, http.StatusBadRequest, invalid.Code)
	assert.JSONEq(t, `{"error":"invalid_history_cursor"}`, invalid.Body.String())
	checkOmission(*anchor.History, 2, "item_exceeds_item_byte_limit", recent)

	response := sessionRequest(t, srv, http.MethodGet, base+"/history?projection=tail&max_items=1&max_bytes=16384&cursor="+url.QueryEscape(anchor.History.NextCursor), "", "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.LessOrEqual(t, response.Body.Len(), 16384)
	var olderPage api.SessionHistoryPage
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &olderPage))
	require.Empty(t, olderPage.Items)
	checkOmission(olderPage.History, 1, "item_exceeds_response_byte_limit", older)
	require.Zero(t, store.fullReads.Load(), "tail, older pages and item fetches cannot hydrate history")
}
