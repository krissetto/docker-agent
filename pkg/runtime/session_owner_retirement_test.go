package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestSessionOwnerRegressionReplacementRetiresOwner(t *testing.T) {
	rt := newPressureRuntime(t, 2)
	sess := session.New(session.WithID(t.Name()))
	h, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	old := h.(*sessionHandle).driver
	old.StopAll()
	old.Wait()
	_, err = rt.CreateSession(t.Context(), sess.Clone(), SessionBinding{})
	require.NoError(t, err)
	require.NoError(t, rt.Close())
	defer old.closeOwner()
	select {
	case <-old.ownerDone:
	default:
		t.Fatal("replaced owner remains live after runtime shutdown")
	}
}

type retirementToolset struct {
	cleanup func(context.Context) error
}

func (*retirementToolset) Tools(context.Context) ([]tools.Tool, error)   { return nil, nil }
func (s *retirementToolset) StopResourceOwner(ctx context.Context) error { return s.cleanup(ctx) }

func TestRestoreRetirementAllowsSynchronousRecallAndRetainsFailedAuthority(t *testing.T) {
	resource := &retirementToolset{}
	root := agent.New("root", "", agent.WithToolSets(resource), agent.WithModel(&mockProvider{id: "test/retirement"}))
	r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)))
	require.NoError(t, err)
	resource.cleanup = func(context.Context) error { return nil }
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	sess := session.New(session.WithID(t.Name()))
	h, err := r.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	old := h.(*sessionHandle).driver
	r.sessionDrivers.mu.Lock()
	old.maintenanceRetired = true
	r.sessionDrivers.mu.Unlock()
	old.StopAll()
	old.Wait()
	reservation, err := r.sessionDrivers.prepareRestore(t.Context(), old.session().Clone(), old)
	require.NoError(t, err)
	defer reservation.Discard()
	fail := true
	resource.cleanup = func(ctx context.Context) error {
		require.Same(t, old.resourceOwner, tools.ResourceOwnerFromContext(ctx))
		_, err := old.postSteer(ctx, QueuedMessage{Content: "late recall", RequestID: "recall"})
		require.ErrorIs(t, err, ErrSessionStopped)
		if fail {
			return assert.AnError
		}
		return nil
	}
	r.subagents.mu.Lock()
	err = r.sessionDrivers.ActivateRestoreBatch([]*restoreDriverReservation{reservation}, nil)
	r.subagents.mu.Unlock()
	require.ErrorIs(t, err, assert.AnError)
	resident, found := r.sessionDrivers.Lookup(sess.ID)
	require.True(t, found)
	require.Same(t, old, resident)
	select {
	case <-old.ownerDone:
		t.Fatal("failed resource retirement lost owner authority")
	default:
	}
	fail = false
	r.subagents.mu.Lock()
	err = r.sessionDrivers.ActivateRestoreBatch([]*restoreDriverReservation{reservation}, func() error { return assert.AnError })
	r.subagents.mu.Unlock()
	require.ErrorIs(t, err, assert.AnError)
	select {
	case <-old.ownerDone:
		t.Fatal("failed topology commit retired published owner")
	default:
	}
	r.subagents.mu.Lock()
	err = r.sessionDrivers.ActivateRestoreBatch([]*restoreDriverReservation{reservation}, nil)
	r.subagents.mu.Unlock()
	require.NoError(t, err)
	select {
	case <-old.ownerDone:
	default:
		t.Fatal("successful replacement did not retire owner")
	}
	resource.cleanup = func(context.Context) error { return nil }
}

func TestReplacementRetirementJoinsOutstandingDurableIO(t *testing.T) {
	r := newPressureRuntime(t, 2)
	sess := session.New(session.WithID(t.Name()))
	h, err := r.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	old := h.(*sessionHandle).driver
	r.sessionDrivers.mu.Lock()
	old.maintenanceRetired = true
	r.sessionDrivers.mu.Unlock()
	entered, release, acknowledged := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- old.durableIO(t.Context(), func() (sessionIOReservation, error) {
			return sessionIOReservation{write: func(context.Context) error { close(entered); <-release; return nil }, commit: func(err error) error { close(acknowledged); return err }}, nil
		})
	}()
	<-entered
	old.StopAll()
	replaced := make(chan error, 1)
	go func() { _, err := r.CreateSession(t.Context(), sess.Clone(), SessionBinding{}); replaced <- err }()
	select {
	case err := <-replaced:
		t.Fatalf("replacement overtook outstanding write: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	select {
	case <-old.ownerDone:
		t.Fatal("owner closed before durable acknowledgement")
	default:
	}
	unblock()
	require.NoError(t, <-writeDone)
	require.NoError(t, <-replaced)
	select {
	case <-acknowledged:
	default:
		t.Fatal("write was not acknowledged")
	}
	select {
	case <-old.ownerDone:
	default:
		t.Fatal("replacement left owner alive")
	}
}

func TestDelayedRecallCannotSteerReplacementGeneration(t *testing.T) {
	r, sess, _, agentTools := makeJudgedRuntime(t, "allow", "allowed", nil)
	h, err := r.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	var captured tools.Runtime
	agentTools[0].Handler = func(_ context.Context, _ tools.ToolCall, rt tools.Runtime) (*tools.ToolCallResult, error) {
		captured = rt
		return tools.ResultSuccess("done"), nil
	}
	r.processToolCalls(t.Context(), sess, []tools.ToolCall{{ID: "capture", Function: tools.FunctionCall{Name: "the_tool", Arguments: `{}`}}}, agentTools, EventSinkFunc(func(Event) {}))
	require.NotNil(t, captured)
	old := h.(*sessionHandle).driver
	old.StopAll()
	old.Wait()
	fresh, err := r.CreateSession(t.Context(), sess.Clone(), SessionBinding{})
	require.NoError(t, err)
	before, err := fresh.Snapshot(t.Context())
	require.NoError(t, err)
	require.ErrorIs(t, captured.Recall(t.Context(), "stale background recall"), ErrSessionClosed)
	snapshot, err := fresh.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, before.Messages, snapshot.Messages)
	require.False(t, fresh.(*sessionHandle).driver.HasPending())
}
