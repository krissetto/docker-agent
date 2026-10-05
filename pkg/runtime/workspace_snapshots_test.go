package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/session"
)

type runtimeSnapshotStub struct {
	calls int
	cwd   string
}

func (*runtimeSnapshotStub) Enabled() bool            { return true }
func (*runtimeSnapshotStub) AutoInject(*hooks.Config) {}
func (*runtimeSnapshotStub) List(string) []builtins.SnapshotInfo {
	return []builtins.SnapshotInfo{{ID: 1, Files: 1}}
}

func (s *runtimeSnapshotStub) UndoLast(ctx context.Context, _, cwd string) (int, bool, error) {
	s.calls++
	s.cwd = cwd
	return 1, true, ctx.Err()
}

func (s *runtimeSnapshotStub) Reset(ctx context.Context, id, cwd string, _ int) (int, bool, error) {
	return s.UndoLast(ctx, id, cwd)
}

func TestWorkspaceSnapshotsCanonicalAdmission(t *testing.T) {
	rt, sess := newSessionFixture(t)
	sess.WorkingDir = t.TempDir()
	controller := &runtimeSnapshotStub{}
	WithSnapshotController(controller)(rt)
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	h := handle.(*sessionHandle)
	history, err := h.WorkspaceSnapshots(t.Context())
	require.NoError(t, err)
	h.driver.mu.Lock()
	h.driver.phase = sessionRunning
	h.driver.mu.Unlock()
	_, err = h.UndoWorkspaceSnapshot(t.Context(), history.Proof)
	require.Error(t, err)
	require.Zero(t, controller.calls)
	h.driver.mu.Lock()
	h.driver.phase = sessionIdle
	h.driver.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = h.ResetWorkspaceSnapshot(ctx, history.Proof, 0)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, controller.calls)
	_, err = h.UndoWorkspaceSnapshot(t.Context(), "foreign")
	require.Error(t, err)
	require.Zero(t, controller.calls)
	_, err = h.UndoWorkspaceSnapshot(t.Context(), history.Proof)
	require.NoError(t, err)
	require.Equal(t, sess.WorkingDir, controller.cwd)
	rt.sessionDrivers.mu.Lock()
	delete(rt.sessionDrivers.drivers, h.ID())
	rt.sessionDrivers.mu.Unlock()
	_, err = h.UndoWorkspaceSnapshot(t.Context(), history.Proof)
	require.Error(t, err)
	require.Equal(t, 1, controller.calls)
	rt.sessionDrivers.mu.Lock()
	defer rt.sessionDrivers.mu.Unlock()
	rt.sessionDrivers.drivers[h.ID()] = h.driver
}

func TestWorkspaceSnapshotProofIdentifiesCheckpoint(t *testing.T) {
	require.NotEqual(t, workspaceSnapshotProof("a", []builtins.SnapshotInfo{{ID: 1, Files: 1}}), workspaceSnapshotProof("a", []builtins.SnapshotInfo{{ID: 2, Files: 1}}))
	require.NotEqual(t, workspaceSnapshotProof("a", nil), workspaceSnapshotProof("b", nil))
}

type blockedSnapshotRestore struct {
	runtimeSnapshotStub

	blockedID string
	entered   chan struct{}
	release   chan struct{}
}

func (s *blockedSnapshotRestore) UndoLast(ctx context.Context, id, _ string) (int, bool, error) {
	if id == s.blockedID {
		close(s.entered)
		<-s.release
	}
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	return 1, true, nil
}

func (s *blockedSnapshotRestore) Reset(ctx context.Context, id, cwd string, _ int) (int, bool, error) {
	return s.UndoLast(ctx, id, cwd)
}

func TestWorkspaceSnapshotRestoreReservationStaysResponsive(t *testing.T) {
	for _, operation := range []string{"undo", "reset"} {
		t.Run(operation, func(t *testing.T) {
			rt, sess := newSessionFixture(t)
			sess.WorkingDir = t.TempDir()
			controller := &blockedSnapshotRestore{blockedID: sess.ID, entered: make(chan struct{}), release: make(chan struct{})}
			WithSnapshotController(controller)(rt)
			handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "root"})
			require.NoError(t, err)
			h := handle.(*sessionHandle)
			history, err := h.WorkspaceSnapshots(t.Context())
			require.NoError(t, err)
			other := session.New(session.WithWorkingDir(t.TempDir()))
			unrelated, err := rt.CreateSession(t.Context(), other, SessionBinding{AgentName: "root"})
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			release := sync.OnceFunc(func() { close(controller.release) })
			defer release()
			finished := make(chan error, 1)
			go func() {
				if operation == "undo" {
					_, err := h.UndoWorkspaceSnapshot(ctx, history.Proof)
					finished <- err
				} else {
					_, err := h.ResetWorkspaceSnapshot(ctx, history.Proof, 0)
					finished <- err
				}
			}()
			waitClosed(t, controller.entered, "restore worker entered")
			responsive := make(chan error, 1)
			go func() {
				_, err := h.Status(t.Context())
				if err != nil {
					responsive <- err
					return
				}
				observation, err := h.Observe(t.Context(), ObserveOptions{})
				if err != nil {
					responsive <- err
					return
				}
				observation.Cancel()
				result, err := h.Cancel(t.Context(), "not-an-active-turn")
				if err != nil {
					responsive <- err
					return
				}
				if result.Outcome != CancelNotActive {
					responsive <- errors.New("unexpected cancellation outcome")
					return
				}
				_, err = h.WorkspaceSnapshots(t.Context())
				if err != nil {
					responsive <- err
					return
				}
				foreign := unrelated.(SessionWorkspaceSnapshots)
				history, err := foreign.WorkspaceSnapshots(t.Context())
				if err != nil {
					responsive <- err
					return
				}
				_, err = foreign.UndoWorkspaceSnapshot(t.Context(), history.Proof)
				responsive <- err
			}()
			select {
			case err := <-responsive:
				require.NoError(t, err)
			case <-time.After(time.Second):
				t.Fatal("restore blocked owner or unrelated session")
			}
			_, err = h.Submit(t.Context(), TurnInput{Content: "must not start"})
			require.Error(t, err)
			cancel()
			select {
			case err := <-finished:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(time.Second):
				t.Fatal("canceled waiter did not return")
			}
			_, err = h.Submit(t.Context(), TurnInput{Content: "still reserved"})
			require.Error(t, err)
			latest, err := h.WorkspaceSnapshots(t.Context())
			require.NoError(t, err)
			require.Equal(t, history, latest)
			closed := make(chan error, 1)
			go func() { closed <- rt.Close() }()
			select {
			case err := <-closed:
				t.Fatalf("close abandoned reserved restore: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			release()
			select {
			case err := <-closed:
				require.NoError(t, err)
			case <-time.After(time.Second):
				t.Fatal("close failed to join restore reconciliation")
			}
			require.Eventually(t, func() bool {
				h.driver.mu.Lock()
				defer h.driver.mu.Unlock()
				return !h.driver.switchReserved && h.driver.ioReservations == 0
			}, time.Second, time.Millisecond)
			_, err = h.UndoWorkspaceSnapshot(t.Context(), "stale-proof")
			require.Error(t, err)
		})
	}
}
