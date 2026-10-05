package tui

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/service/tuistate"
	"github.com/docker/docker-agent/pkg/tui/tuitest"
	"github.com/docker/docker-agent/pkg/userconfig"
)

type startupRestoreHandle struct {
	*lifecycleHandle

	snapshot *session.Session
	baseline chan struct{}
	failed   bool
	observed atomic.Int32
}

func (h *startupRestoreHandle) Observe(ctx context.Context, options runtime.ObserveOptions) (runtime.Observation, error) {
	if options.Tree {
		return runtime.Observation{}, runtime.ErrUnsupported
	}
	h.observed.Add(1)
	if h.failed {
		return runtime.Observation{}, &runtime.SessionError{Kind: runtime.SessionErrorUnsupported, SessionID: h.id, Operation: "observe"}
	}
	if h.baseline != nil {
		select {
		case <-h.baseline:
		case <-ctx.Done():
			return runtime.Observation{}, ctx.Err()
		}
	}
	observation, err := h.lifecycleHandle.Observe(ctx, options)
	observation.Initial = []runtime.SessionSnapshot{{
		Session:            h.snapshot.Clone(),
		Status:             runtime.SessionStatus{SessionID: h.id, AgentName: "root", State: runtime.SessionStateSettled},
		TranscriptPosition: h.snapshot.ItemCount(),
	}}
	return observation, err
}

type startupRestoreSessions struct {
	*lifecycleSessions

	handles map[string]*startupRestoreHandle
}

func (s *startupRestoreSessions) SessionByID(id string) (runtime.SessionHandle, error) {
	if handle := s.handles[id]; handle != nil {
		return handle, nil
	}
	return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: id}
}

func TestStartupRestoreHydratesSnapshotWithoutObservationBaseline(t *testing.T) {
	for _, sameInitial := range []bool{true, false} {
		for _, observation := range []string{"immediate", "delayed", "failed"} {
			name := "different-initial/" + observation
			if sameInitial {
				name = "same-initial/" + observation
			}
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				paths.SetDataDir(dir)
				paths.SetConfigDir(dir)
				t.Cleanup(func() { paths.SetDataDir(""); paths.SetConfigDir("") })
				require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
					cfg.Settings = &userconfig.Settings{RestoreTabs: new(true), ShowBanner: new(false)}
					return nil
				}))

				saved := session.New(session.WithID("saved-root"), session.WithAgentName("root"), session.WithTitle("Saved startup title"), session.WithWorkingDir(dir))
				saved.AddMessage(session.UserMessage("SAVED-STARTUP-TRANSCRIPT"))
				saved.SetUsage(1234, 56)
				handle := &startupRestoreHandle{lifecycleHandle: &lifecycleHandle{id: saved.ID}, snapshot: saved, failed: observation == "failed"}
				if observation == "delayed" {
					handle.baseline = make(chan struct{})
				}
				initial := saved
				if !sameInitial {
					initial = session.New(session.WithID("initial-root"), session.WithAgentName("root"), session.WithWorkingDir(dir))
				}
				sessions := &startupRestoreSessions{lifecycleSessions: newLifecycleSessions(), handles: map[string]*startupRestoreHandle{saved.ID: handle}}
				if !sameInitial {
					sessions.handles[initial.ID] = &startupRestoreHandle{lifecycleHandle: &lifecycleHandle{id: initial.ID}, snapshot: initial}
				}
				services := stubRuntime{}
				application := app.New(t.Context(), sessions, initial, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(services))
				store, err := tuistate.New(t.Context())
				require.NoError(t, err)
				require.NoError(t, store.AddTab(t.Context(), saved.ID, dir))
				require.NoError(t, store.SetActiveTab(t.Context(), saved.ID))
				require.NoError(t, store.Close())

				restored := 0
				restorer := func(ctx context.Context, id, _ string) (SpawnedSession, error) {
					restored++
					require.Equal(t, saved.ID, id)
					view, err := app.NewResolved(ctx, sessions, runtime.CommittedSessionView{SessionHandle: handle, Info: runtime.PreparedSessionViewInfo{
						SessionID: id, RootSessionID: id, Session: saved, WorkingDir: dir, Binding: runtime.SessionBinding{AgentName: "root"},
					}}, app.WithRuntimeServices(services))
					return SpawnedSession{App: view, Session: saved, Ownership: RuntimeBorrowed}, err
				}
				root := New(t.Context(), nil, application, dir, func() {}, WithSessionRestorer(restorer)).(*appModel)
				driver := tuitest.New(t, &lifecycleProgramModel{root}, 120, 40)
				// A delayed observer remains blocked until the stored snapshot is visible.
				driver.WaitFor(tuitest.Contains("SAVED-STARTUP-TRANSCRIPT"))
				driver.WaitFor(tuitest.Contains("Saved startup title"))
				driver.WaitFor(tuitest.Contains("1.3K"))
				driver.Send(lifecycleProgramProbe{inspect: func(owner *appModel) {
					require.Equal(t, saved.ID, owner.supervisor.ActiveID())
					require.Equal(t, saved.ID, owner.application.Session().ID)
					require.Empty(t, owner.pendingActiveTab)
					require.Empty(t, owner.pendingRestores)
					if sameInitial {
						require.Zero(t, restored)
						require.Equal(t, 1, owner.supervisor.Count())
					} else {
						require.Equal(t, 1, restored)
						require.Equal(t, 2, owner.supervisor.Count())
					}
				}})
				require.Eventually(t, func() bool { return handle.observed.Load() == 1 }, time.Second, time.Millisecond)
				if handle.baseline != nil {
					close(handle.baseline)
					driver.WaitFor(tuitest.Contains("SAVED-STARTUP-TRANSCRIPT"))
				}
			})
		}
	}
}
