package runtime

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools/builtin/skills"
)

type sessionContractFixture struct {
	handle       SessionHandle
	injectGap    func()
	settle       func()
	afterRelease func() SessionHandle
	unavailable  []string
	close        func()
}

type sessionContractFactory struct {
	name string
	new  func(*testing.T) sessionContractFixture
}

func TestSessionHandleConformance(t *testing.T) {
	factories := []sessionContractFactory{
		{name: "local", new: newLocalSessionContractFixture},
		{name: "remote", new: newRemoteSessionContractFixture},
	}
	for _, factory := range factories {
		t.Run(factory.name, func(t *testing.T) {
			t.Run("identity_metadata_and_commands", func(t *testing.T) {
				fixture := factory.new(t)
				defer fixture.close()
				handle := fixture.handle
				id, agentName, metadata := handle.ID(), handle.AgentName(), handle.Metadata()
				metadata.Capabilities.AvailableModels = append(metadata.Capabilities.AvailableModels, "mutated")
				assert.Equal(t, id, handle.ID())
				assert.Equal(t, agentName, handle.AgentName())
				assert.Equal(t, id, handle.Metadata().SessionID)
				assert.Equal(t, agentName, handle.Metadata().AgentName)
				assert.NotContains(t, handle.Metadata().Capabilities.AvailableModels, "mutated")

				calls := []struct {
					name string
					call func() (Submission, error)
				}{
					{"submit", func() (Submission, error) { return handle.Submit(t.Context(), TurnInput{Content: "submit"}) }},
					{"retry", func() (Submission, error) { return handle.Retry(t.Context()) }},
					{"steer", func() (Submission, error) { return handle.Steer(t.Context(), TurnInput{Content: "steer"}) }},
				}
				var submissions []Submission
				seenTurns := map[string]bool{}
				for _, call := range calls {
					submission, callErr := call.call()
					require.NoError(t, callErr, call.name)
					assert.Equal(t, id, submission.SessionID)
					assert.NotEmpty(t, submission.TurnID)
					assert.False(t, seenTurns[submission.TurnID], "turn correlation must be unique")
					seenTurns[submission.TurnID] = true
					submissions = append(submissions, submission)
				}
				fixture.settle()
				zero := uint64(0)
				observation, observeErr := handle.Observe(t.Context(), ObserveOptions{Since: &zero, Buffer: 64})
				require.NoError(t, observeErr)
				defer observation.Cancel()
				var previous uint64
				wanted := make(map[string]bool)
				for _, submission := range submissions {
					wanted[submission.TurnID] = true
				}
				seen := make(map[string]bool)
				for _, envelope := range observation.Replay {
					if !wanted[envelope.TurnID] {
						continue
					}
					assert.Greater(t, envelope.Sequence, previous, "correlated events retain journal order")
					previous = envelope.Sequence
					seen[envelope.TurnID] = true
				}
				for _, submission := range submissions {
					assert.True(t, seen[submission.TurnID], "command %s must remain correlated in replay", submission.TurnID)
				}
			})

			t.Run("cancel_outcomes", func(t *testing.T) {
				fixture := factory.new(t)
				defer fixture.close()
				handle := fixture.handle
				submission, err := handle.Submit(t.Context(), TurnInput{Content: "cancel"})
				require.NoError(t, err)
				first, err := handle.Cancel(t.Context(), submission.TurnID)
				require.NoError(t, err)
				assert.Equal(t, handle.ID(), first.SessionID)
				assert.Equal(t, submission.TurnID, first.TurnID)
				assert.Equal(t, CancelAccepted, first.Outcome)
				second, err := handle.Cancel(t.Context(), submission.TurnID)
				require.NoError(t, err)
				assert.Contains(t, []CancelOutcome{CancelAlreadyCancelling, CancelNotActive}, second.Outcome)
				unknown, err := handle.Cancel(t.Context(), "unknown-turn")
				require.NoError(t, err)
				assert.Equal(t, CancelNotActive, unknown.Outcome)
			})

			t.Run("observe_replay_and_gap", func(t *testing.T) {
				fixture := factory.new(t)
				defer fixture.close()
				handle := fixture.handle
				first, err := handle.Submit(t.Context(), TurnInput{Content: "replay"})
				require.NoError(t, err)
				zero := uint64(0)
				replayed, err := handle.Observe(t.Context(), ObserveOptions{Since: &zero})
				require.NoError(t, err)
				require.NotEmpty(t, replayed.Replay)
				assert.Equal(t, first.TurnID, replayed.Replay[len(replayed.Replay)-1].TurnID)
				replayed.Cancel()

				fixture.injectGap()
				gapped, err := handle.Observe(t.Context(), ObserveOptions{Since: &zero})
				require.NoError(t, err)
				defer gapped.Cancel()
				require.NotEmpty(t, gapped.Replay)
				assert.True(t, gapped.Replay[0].Gap)
				assert.Greater(t, gapped.Replay[0].FirstAvailable, zero)
			})

			t.Run("stale_interaction_and_release", func(t *testing.T) {
				fixture := factory.new(t)
				defer fixture.close()
				handle := fixture.handle
				err := handle.Respond(t.Context(), InteractionResponse{InteractionID: "stale", Kind: InteractionConfirmation, Resume: ResumeApprove()})
				assertSessionContractError(t, err, SessionErrorStale, SessionOperationRespond)
				require.NoError(t, handle.Release(t.Context()))
				assert.Equal(t, handle.ID(), fixture.afterRelease().ID(), "release relinquishes this handle generation, not durable identity")
			})

			t.Run("advertised_false_capabilities_are_unsupported", func(t *testing.T) {
				fixture := factory.new(t)
				defer fixture.close()
				assertUnavailableCapabilities(t, fixture.handle, fixture.unavailable)
			})
		})
	}
}

func assertSessionContractError(t *testing.T, err error, kind SessionErrorKind, operation SessionOperation) {
	t.Helper()
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	assert.Equal(t, kind, sessionErr.Kind)
	assert.Equal(t, operation, sessionErr.Operation)
}

func assertUnavailableCapabilities(t *testing.T, handle SessionHandle, _ []string) {
	t.Helper()
	if !handle.Metadata().Capabilities.ForkSkills {
		available, err := handle.Skills(t.Context())
		require.NoError(t, err)
		assert.Empty(t, available)
		resolved, err := handle.ResolveSkillCommand(t.Context(), "/missing")
		require.NoError(t, err)
		assert.Empty(t, resolved)
	}
	type capabilityCall struct {
		operation SessionOperation
		call      func() error
	}
	calls := map[string][]capabilityCall{
		"Compaction":        {{SessionOperationCompact, func() error { return handle.Compact(t.Context(), "", nil) }}},
		"TargetCompaction":  {{SessionOperationCompactTarget, func() error { return handle.CompactTarget(t.Context(), handle.ID(), "", nil) }}},
		"ModelSwitching":    {{SessionOperationSetModel, func() error { return handle.SetModel(t.Context(), "unavailable") }}},
		"ContextInspection": {{SessionOperationContext, func() error { _, err := handle.ContextBreakdown(t.Context()); return err }}},
		"LiveSessions":      {{SessionOperationLiveSessions, func() error { _, err := handle.LiveSessions(t.Context()); return err }}},
		// UpdateTitle is a core SessionHandle method, not part of SessionEditing
		// (SetStarred/RemoveAttachment), so it is never gated here.
		"SessionEditing": {
			{SessionOperationSetStarred, func() error { return handle.SetStarred(t.Context(), true) }},
			{SessionOperationRemoveAttachment, func() error { return handle.RemoveAttachment(t.Context(), "missing") }},
		},
		"ForkSkills": {
			{SessionOperationRunSkill, func() error {
				return handle.StartSkillFork(t.Context(), "operation", skills.RunSkillArgs{})
			}},
			{SessionOperationRunSkill, func() error {
				_, err := handle.RunSkillFork(t.Context(), skills.RunSkillArgs{}, nil)
				return err
			}},
		},
		"Pause":               {{SessionOperationPause, func() error { _, err := handle.TogglePause(t.Context()); return err }}},
		"ModelCatalogRefresh": {{SessionOperationRefreshModels, func() error { return handle.RefreshModelsCatalog(t.Context()) }}},
		"ThinkingLevels": {
			{SessionOperationThinkingLevel, func() error { _, err := handle.CycleThinkingLevel(t.Context()); return err }},
			{SessionOperationThinkingLevel, func() error {
				_, err := handle.SetThinkingLevel(t.Context(), "high")
				return err
			}},
		},
		"Todos": {
			{SessionOperationTodos, func() error { _, err := handle.Todos(t.Context()); return err }},
			{SessionOperationSetTodoStatus, func() error { _, err := handle.SetTodoStatus(t.Context(), "missing", "completed"); return err }},
			{SessionOperationRemoveTodo, func() error { _, err := handle.RemoveTodo(t.Context(), "missing"); return err }},
		},
	}
	capabilities := reflect.ValueOf(handle.Metadata().Capabilities)
	capabilityType := capabilities.Type()
	for index := range capabilities.NumField() {
		field := capabilities.Field(index)
		if field.Kind() != reflect.Bool || field.Bool() {
			continue
		}
		name := capabilityType.Field(index).Name
		applicable, ok := calls[name]
		require.True(t, ok, "false capability %s needs a conformance caller", name)
		for _, item := range applicable {
			err := item.call()
			assertSessionContractError(t, err, SessionErrorUnsupported, item.operation)
			require.ErrorIs(t, err, ErrUnsupported, name)
		}
	}
}

func newLocalSessionContractFixture(t *testing.T) sessionContractFixture {
	t.Helper()
	releaseRun := make(chan struct{})
	var releaseOnce sync.Once
	provider := &multiRunProvider{
		mockProvider: &mockProvider{id: "test/contract"},
		build: func() chat.MessageStream {
			return &blockingMockStream{responses: []chat.MessageStreamResponse{{Choices: []chat.MessageStreamChoice{{Index: 0, Delta: chat.MessageDelta{Content: "ok"}}}}}, release: releaseRun}
		},
	}
	tm := team.New(team.WithAgents(agent.New("contract", "prompt", agent.WithModel(provider))))
	runtime, err := NewLocalRuntime(t.Context(), tm)
	require.NoError(t, err)
	sess := session.New(session.WithID("contract-session"))
	handle, err := runtime.CreateSession(t.Context(), sess, SessionBinding{AgentName: "contract"})
	require.NoError(t, err)
	local := handle.(*sessionHandle)
	local.driver.events = newSessionEventHubWithLimits(64, 8<<20)
	return sessionContractFixture{
		handle: handle,
		injectGap: func() {
			local.driver.events.mu.Lock()
			local.driver.events.capacity = 0
			local.driver.events.mu.Unlock()
			local.driver.events.Publish(handle.ID(), UserMessage("evicted", handle.ID(), nil))
		},
		settle: func() {
			releaseOnce.Do(func() { close(releaseRun) })
			local.driver.Wait()
		},
		afterRelease: func() SessionHandle {
			releaseOnce.Do(func() { close(releaseRun) })
			local.driver.Wait()
			require.NoError(t, handle.Release(t.Context()))
			reopened, reopenErr := runtime.CreateSession(t.Context(), sess.Clone(), SessionBinding{AgentName: "contract"})
			require.NoError(t, reopenErr)
			return reopened
		},
		unavailable: []string{"model_switching", "thinking_levels", "model_catalog_refresh"},
		close: func() {
			releaseOnce.Do(func() { close(releaseRun) })
			_ = runtime.Close()
		},
	}
}

type remoteContractServer struct {
	t          *testing.T
	server     *httptest.Server
	mu         sync.Mutex
	sequence   uint64
	journal    []remoteSessionEnvelope
	activeTurn string
	cancelling bool
}

func newRemoteSessionContractFixture(t *testing.T) sessionContractFixture {
	t.Helper()
	fixture := &remoteContractServer{t: t}
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	client, err := NewClient(fixture.server.URL, WithHTTPClient(fixture.server.Client()))
	require.NoError(t, err)
	transport, err := NewSessionTransport(client)
	require.NoError(t, err)
	handle, err := transport.SessionByID("contract-session")
	require.NoError(t, err)
	require.NoError(t, handle.(Hydrator).Hydrate(t.Context()))
	return sessionContractFixture{
		handle: handle,
		injectGap: func() {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			fixture.sequence++
			fixture.journal = []remoteSessionEnvelope{{Version: sessionWireVersion, SessionID: handle.ID(), Sequence: fixture.sequence, Gap: true, FirstAvailable: fixture.sequence}}
		},
		settle:       func() {},
		afterRelease: func() SessionHandle { return handle },
		unavailable:  []string{"model_switching", "thinking_levels", "fork_skills", "model_catalog_refresh"},
		close:        fixture.server.Close,
	}
}

func (s *remoteContractServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	const prefix = api.SessionAPIPath + "/contract-session/"
	if len(r.URL.Path) < len(prefix) || r.URL.Path[:len(prefix)] != prefix {
		http.NotFound(w, r)
		return
	}
	operation := r.URL.Path[len(prefix):]
	w.Header().Set("Content-Type", "application/json")
	s.mu.Lock()
	defer s.mu.Unlock()
	switch operation {
	case "status":
		fmt.Fprint(w, `{"metadata":{"session_id":"contract-session","agent_name":"contract","capabilities":{"available_models":[]}},"status":{"session_id":"contract-session","agent_name":"contract","state":"settled","pending":0}}`)
	case "messages", "retry":
		mode := "retry"
		if operation == "messages" {
			var request struct {
				Mode string `json:"mode"`
			}
			assert.NoError(s.t, json.NewDecoder(r.Body).Decode(&request))
			mode = request.Mode
		}
		s.sequence++
		turnID := fmt.Sprintf("%s-%d", mode, s.sequence)
		s.activeTurn, s.cancelling = turnID, false
		event, _ := json.Marshal(StreamStarted("contract-session", "contract"))
		s.journal = append(s.journal, remoteSessionEnvelope{Version: sessionWireVersion, SessionID: "contract-session", TurnID: turnID, Sequence: s.sequence, TranscriptPosition: -1, Event: event})
		fmt.Fprintf(w, `{"session_id":"contract-session","turn_id":%q,"disposition":"queued"}`, turnID)
	case "cancel":
		var request struct {
			TurnID string `json:"turn_id"`
		}
		assert.NoError(s.t, json.NewDecoder(r.Body).Decode(&request))
		outcome := CancelNotActive
		if request.TurnID == s.activeTurn {
			if s.cancelling {
				outcome = CancelAlreadyCancelling
			} else {
				outcome, s.cancelling = CancelAccepted, true
			}
		}
		fmt.Fprintf(w, `{"session_id":"contract-session","turn_id":%q,"outcome":%q}`, request.TurnID, outcome)
	case "responses":
		w.WriteHeader(http.StatusPreconditionFailed)
		fmt.Fprint(w, `{"error":"stale","operation":"respond","session_id":"contract-session"}`)
	case "events":
		s.writeEvents(w)
	default:
		http.NotFound(w, r)
	}
}

func (s *remoteContractServer) writeEvents(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	writer := bufio.NewWriter(w)
	snapshot := remoteSessionSnapshot{Session: session.New(session.WithID("contract-session")), Status: api.SessionStatus[SessionState]{SessionID: "contract-session", AgentName: "contract", State: SessionStateSettled}, Cursor: s.sequence}
	writeContractSSE(writer, remoteSessionStreamMessage{Version: sessionWireVersion, Type: "snapshot", Snapshot: &snapshot})
	for i := range s.journal {
		writeContractSSE(writer, remoteSessionStreamMessage{Version: sessionWireVersion, Type: "event", Envelope: &s.journal[i]})
	}
	writeContractSSE(writer, remoteSessionStreamMessage{Version: sessionWireVersion, Type: "ready", Cursor: s.sequence})
	assert.NoError(s.t, writer.Flush())
}

func writeContractSSE(writer *bufio.Writer, message remoteSessionStreamMessage) {
	data, _ := json.Marshal(message)
	fmt.Fprintf(writer, "data: %s\n\n", data)
}

func TestRemoteSessionObserveReportsSeveredSSE(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"version\":2,\"type\":\"snapshot\",\"snapshot\":{\"session\":{\"id\":\"s\"},\"status\":{\"session_id\":\"s\",\"state\":\"settled\",\"pending\":0},\"cursor\":0}}\n\n")
		fmt.Fprint(w, "data: {\"version\":2,\"type\":\"ready\",\"cursor\":0}\n\n")
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := NewSessionTransport(client)
	require.NoError(t, err)
	handle, err := transport.SessionByID("s")
	require.NoError(t, err)
	observation, err := handle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer observation.Cancel()
	select {
	case streamErr := <-observation.Errors:
		require.Error(t, streamErr)
		require.ErrorContains(t, streamErr, "terminated at cursor 0")
	case <-time.After(5 * time.Second):
		t.Fatal("severed SSE did not report a terminal observation error")
	}
}
