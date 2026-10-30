package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
)

// reopenPolicy keeps one resident driver: the next admission reclaims the
// settled idle session through the production capacity path, with no test hook.
func reopenPolicy() runtime.SessionResourcePolicy {
	policy := runtime.DefaultSessionResourcePolicy()
	policy.MaxSessions = 1
	policy.IdleRetention = 0
	return policy
}

func reopenFactory(store session.Store, policy runtime.SessionResourcePolicy) SessionRuntimeFactory {
	return func(ctx context.Context, _ config.Source, workingDir string) (runtime.SessionRuntimeSupervisor, error) {
		root := agent.New("root", "prompt", agent.WithModel(sessionHTTPProvider{}))
		other := agent.New("other", "prompt", agent.WithModel(sessionHTTPProvider{}))
		rt, err := runtime.NewLocalRuntime(ctx, team.New(team.WithAgents(root, other)),
			runtime.WithSessionStore(store), runtime.WithWorkingDir(workingDir), runtime.WithSessionResourcePolicy(policy))
		if err != nil {
			return nil, err
		}
		return runtime.NewSessionRuntimeSupervisor(rt), nil
	}
}

type reopenFixture struct {
	srv    *Server
	sm     *SessionManager
	store  session.Store
	router *workspaceSessionRuntimes
}

func newReopenFixture(t *testing.T) reopenFixture {
	t.Helper()
	return newReopenFixtureWithPolicy(t, reopenPolicy())
}

func newReopenFixtureWithPolicy(t *testing.T, policy runtime.SessionResourcePolicy) reopenFixture {
	t.Helper()
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	sm := NewSessionManager(t.Context(), config.Sources{"agent": &memorySource{data: "agents: {}"}}, store, 0,
		&config.RuntimeConfig{}, WithSessionRuntimeFactory(reopenFactory(store, policy)))
	t.Cleanup(func() { require.NoError(t, sm.Shutdown(context.WithoutCancel(t.Context()))) })
	router, ok := sm.sessionRegistry.(*workspaceSessionRuntimes)
	require.True(t, ok)
	return reopenFixture{srv: NewWithManager(sm, ""), sm: sm, store: store, router: router}
}

// reopenSettledTranscript creates the cached root, runs one turn, and waits
// until the driver is settled with both messages durable.
func reopenSettledTranscript(t *testing.T, f reopenFixture, id string) []string {
	t.Helper()
	rec := sessionRequest(t, f.srv, http.MethodPost, "/api/v2/sessions", `{"agent_name":"root","session_id":"`+id+`"}`, "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = sessionRequest(t, f.srv, http.MethodPost, "/api/v2/sessions/"+id+"/messages", `{"content":"hello"}`, "")
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		h, err := f.router.SessionByID(id)
		if !assert.NoError(c, err) {
			return
		}
		status, err := h.Status(t.Context())
		assert.NoError(c, err)
		assert.Equal(c, runtime.SessionStateSettled, status.State)
	}, 5*time.Second, time.Millisecond)
	return reopenMessages(t, f.srv, id, 2)
}

// reopenMessages returns the GET snapshot transcript once it holds want messages.
func reopenMessages(t *testing.T, srv *Server, id string, want int) []string {
	t.Helper()
	var out []string
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		rec := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/"+id+"/snapshot", "", "")
		if !assert.Equal(c, http.StatusOK, rec.Code, rec.Body.String()) {
			return
		}
		var snapshot sessionSnapshotDTO
		if !assert.NoError(c, json.Unmarshal(rec.Body.Bytes(), &snapshot)) || !assert.NotNil(c, snapshot.Session) {
			return
		}
		out = transcriptOf(snapshot.Session)
		assert.Len(c, out, want)
	}, 5*time.Second, time.Millisecond)
	return out
}

func transcriptOf(sess *session.Session) []string {
	var out []string
	for _, m := range sess.GetAllMessages() {
		out = append(out, string(m.Message.Role)+":"+m.Message.Content)
	}
	return out
}

// reopenEvictByPressure admits a second root into the one-slot runtime. The
// production capacity policy reclaims the settled cached driver to make room.
func reopenEvictByPressure(t *testing.T, f reopenFixture, id string) {
	t.Helper()
	var attempt atomic.Int32
	require.Eventually(t, func() bool {
		n := attempt.Add(1)
		_, err := f.router.CreateSession(t.Context(),
			session.New(session.WithID(fmt.Sprintf("%s-pressure-%d", id, n)), session.WithAgentName("root")),
			runtime.SessionBinding{AgentName: "root"})
		return err == nil
	}, 5*time.Second, time.Millisecond)
	_, err := f.router.SessionByID(id)
	var sessionErr *runtime.SessionError
	require.True(t, errors.As(err, &sessionErr) && sessionErr.Kind == runtime.SessionErrorNotFound,
		"precondition: pressure must reclaim the cached driver from the runtime, got %v", err)
}

// TestSessionCacheReopensEvictedResidentHandle proves a server-cached handle whose
// runtime driver was reclaimed by capacity pressure is reopened from the durable
// store by the same API surface, with the same ID and transcript.
func TestSessionCacheReopensEvictedResidentHandle(t *testing.T) {
	f := newReopenFixture(t)
	const id = "a-cached"
	before := reopenSettledTranscript(t, f, id)
	require.Len(t, before, 2)
	reopenEvictByPressure(t, f, id)

	rec := sessionRequest(t, f.srv, http.MethodGet, "/api/v2/sessions/"+id, "", "")
	require.Equal(t, http.StatusOK, rec.Code, "GET after eviction: %s", rec.Body.String())
	var metadata sessionMetadataDTO
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &metadata))
	assert.Equal(t, id, metadata.SessionID)

	rec = sessionRequest(t, f.srv, http.MethodGet, "/api/v2/sessions/"+id+"/status", "", "")
	require.Equal(t, http.StatusOK, rec.Code, "status after eviction: %s", rec.Body.String())

	assert.Equal(t, before, reopenMessages(t, f.srv, id, 2), "snapshot after eviction must keep the transcript")

	rec = sessionRequest(t, f.srv, http.MethodPost, "/api/v2/sessions/"+id+"/messages", `{"content":"again"}`, "")
	require.Equal(t, http.StatusAccepted, rec.Code, "submit after eviction: %s", rec.Body.String())
	after := reopenMessages(t, f.srv, id, 4)
	assert.Equal(t, before, after[:2], "reopened turn must append to, not replace, the transcript")
}

// assertActiveListings checks both active listing surfaces against the wanted
// loaded IDs: the legacy route and the canonical catalog.
func assertActiveListings(t *testing.T, srv *Server, want []string) {
	t.Helper()
	rec := sessionRequest(t, srv, http.MethodGet, "/api/sessions?active=true", "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var legacy []api.SessionsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &legacy))
	gotLegacy := make([]string, 0, len(legacy))
	for _, row := range legacy {
		gotLegacy = append(gotLegacy, row.ID)
	}
	assert.ElementsMatch(t, want, gotLegacy)

	rec = sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions?active=true", "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var catalog sessionCatalogDTO
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &catalog))
	gotCatalog := make([]string, 0, len(catalog.Sessions))
	for _, row := range catalog.Sessions {
		assert.True(t, row.Loaded, row.SessionID)
		gotCatalog = append(gotCatalog, row.SessionID)
	}
	assert.ElementsMatch(t, want, gotCatalog)
}

// TestSessionCacheActiveListingsSkipEvictedEntry: an evicted cached session must
// not fail the legacy active listing or appear as loaded in the catalog.
func TestSessionCacheActiveListingsSkipEvictedEntry(t *testing.T) {
	f := newReopenFixture(t)
	const id = "a-cached"
	reopenSettledTranscript(t, f, id)
	reopenEvictByPressure(t, f, id)
	assertActiveListings(t, f.srv, nil)
}

// TestSessionCacheActiveListingsMixLiveAndEvictedEntries: one evicted entry beside
// a live resident must leave exactly the live session listed.
func TestSessionCacheActiveListingsMixLiveAndEvictedEntries(t *testing.T) {
	policy := reopenPolicy()
	policy.MaxSessions = 2
	f := newReopenFixtureWithPolicy(t, policy)
	reopenSettledTranscript(t, f, "a-stale")
	reopenSettledTranscript(t, f, "b-live")
	reopenEvictByPressure(t, f, "a-stale")
	_, err := f.router.SessionByID("b-live")
	require.NoError(t, err, "the live resident must survive pressure that reclaimed a-stale")
	assertActiveListings(t, f.srv, []string{"b-live"})
}

// TestSessionCacheActiveListingsKeepRegisteredStoppedEntry: an intentionally
// stopped session stays registered, so both active listings must keep it.
func TestSessionCacheActiveListingsKeepRegisteredStoppedEntry(t *testing.T) {
	f := newReopenFixture(t)
	const id = "stopped-listed"
	reopenSettledTranscript(t, f, id)
	rec := sessionRequest(t, f.srv, http.MethodPost, "/api/v2/sessions/"+id+"/stop-subtree", "", "")
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		h, err := f.router.SessionByID(id)
		if !assert.NoError(c, err) {
			return
		}
		status, err := h.Status(t.Context())
		assert.NoError(c, err)
		assert.True(c, status.Stopped)
	}, 5*time.Second, time.Millisecond)
	assertActiveListings(t, f.srv, []string{id})
}

// TestSessionCacheIntentionalStopIsNotReopened guards that an explicitly stopped,
// still registered session stays stopped and resident under capacity pressure.
func TestSessionCacheIntentionalStopIsNotReopened(t *testing.T) {
	f := newReopenFixture(t)
	const id = "stopped-cached"
	before := reopenSettledTranscript(t, f, id)
	rec := sessionRequest(t, f.srv, http.MethodPost, "/api/v2/sessions/"+id+"/stop-subtree", "", "")
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		h, err := f.router.SessionByID(id)
		if !assert.NoError(c, err) {
			return
		}
		status, err := h.Status(t.Context())
		assert.NoError(c, err)
		assert.True(c, status.Stopped)
	}, 5*time.Second, time.Millisecond)

	_, err := f.router.CreateSession(t.Context(), session.New(session.WithID(id+"-pressure"), session.WithAgentName("root")),
		runtime.SessionBinding{AgentName: "root"})
	var sessionErr *runtime.SessionError
	require.True(t, errors.As(err, &sessionErr) && sessionErr.Kind == runtime.SessionErrorCapacity,
		"a stopped registered driver is not reclaimable; got %v", err)

	rec = sessionRequest(t, f.srv, http.MethodGet, "/api/v2/sessions/"+id+"/status", "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	live, err := f.router.SessionByID(id)
	require.NoError(t, err)
	liveStatus, err := live.Status(t.Context())
	require.NoError(t, err)
	assert.True(t, liveStatus.Stopped)
	// Forward expectation: the HTTP status contract carries the stop flag. This
	// assertion needs the sessionStatus() mapping fix owned by the stop owner
	// (pkg/server/session_http.go); it is not satisfied by this change alone.
	var body struct {
		Status sessionStatusDTO `json:"status"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.True(t, body.Status.Stopped, "HTTP status must report the stop flag")
	// Observe refuses a stopped driver, so the durable transcript is the oracle here.
	stored, err := f.store.GetSession(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, before, transcriptOf(stored))
}

// TestSessionCacheDeletedSessionIsNotReopened guards that deletion after an
// eviction leaves no durable row, no registry driver, and no reopen path.
func TestSessionCacheDeletedSessionIsNotReopened(t *testing.T) {
	f := newReopenFixture(t)
	const id = "deleted-cached"
	reopenSettledTranscript(t, f, id)
	reopenEvictByPressure(t, f, id)

	rec := sessionRequest(t, f.srv, http.MethodDelete, "/api/v2/sessions/"+id, "", "")
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, err := f.sm.Handle(t.Context(), id)
	require.Error(t, err)
	rec = sessionRequest(t, f.srv, http.MethodGet, "/api/v2/sessions/"+id, "", "")
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	_, err = f.store.GetSession(t.Context(), id)
	require.ErrorIs(t, err, session.ErrNotFound)
	_, err = f.router.SessionByID(id)
	require.Error(t, err)
	assert.Zero(t, f.sm.runtimeSessions.Length())
}

// TestSessionCacheConcurrentReopenAfterEviction runs many Handle callers at once
// against an evicted cached session; they must all get the same usable session.
func TestSessionCacheConcurrentReopenAfterEviction(t *testing.T) {
	f := newReopenFixture(t)
	const id = "race-cached"
	before := reopenSettledTranscript(t, f, id)
	reopenEvictByPressure(t, f, id)

	const callers = 8
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	transcripts := make(chan []string, callers)
	for range callers {
		wg.Go(func() {
			h, err := f.sm.Handle(t.Context(), id)
			if err != nil {
				errs <- err
				return
			}
			if h.ID() != id {
				errs <- errors.New("handle ID mismatch: " + h.ID())
				return
			}
			snapshot, err := h.Snapshot(t.Context())
			if err != nil {
				errs <- err
				return
			}
			transcripts <- transcriptOf(snapshot)
		})
	}
	wg.Wait()
	close(errs)
	close(transcripts)
	for err := range errs {
		require.NoError(t, err)
	}
	for got := range transcripts {
		assert.Equal(t, before, got)
	}
	assert.Equal(t, 1, f.sm.runtimeSessions.Length())
}
