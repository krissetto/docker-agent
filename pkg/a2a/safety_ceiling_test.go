package a2a

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/servesafety"
	"github.com/docker/docker-agent/pkg/session"
)

type ceilingStore struct {
	session.Store

	fail bool
}

func (s *ceilingStore) UpdateSession(ctx context.Context, value *session.Session) error {
	if s.fail {
		return errors.New("ceiling persistence failed")
	}
	return s.Store.UpdateSession(ctx, value)
}

func TestColdA2AResumeCanonicalSafetyCeiling(t *testing.T) {
	for _, tc := range []struct {
		name              string
		interactive, fail bool
	}{{name: "ceiling"}, {name: "interactive", interactive: true}, {name: "persistence", fail: true}} {
		t.Run(tc.name, func(t *testing.T) {
			tm, ag := newMockTeam("reply")
			store := &ceilingStore{Store: session.NewInMemorySessionStore()}
			sess := session.New(session.WithID("cold"), session.WithOrigin("a2a"), session.WithAgentName("root"), session.WithSafetyPolicy(session.SafetyPolicyAutonomous), session.WithNonInteractive(!tc.interactive))
			sess.SetAttribute(runtime.SessionAgentAttribute, "root")
			require.NoError(t, store.AddSession(t.Context(), sess))
			rt, err := runtime.NewLocalRuntime(t.Context(), tm, runtime.WithSessionStore(store))
			require.NoError(t, err)
			owner := runtime.NewSessionRuntimeSupervisor(rt)
			t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
			store.fail = tc.fail
			var invocationErr error
			for _, err := range runDockerAgent(newFakeInvocationContext(t.Context(), sess.ID, "hi"), tm, ag.Name(), ag, store, servesafety.Resolved{Policy: session.SafetyPolicyRestricted}, t.TempDir(), owner.Runtime()) {
				invocationErr = errors.Join(invocationErr, err)
			}
			h, err := owner.Runtime().SessionByID(sess.ID)
			require.NoError(t, err)
			snapshot, err := h.Snapshot(t.Context())
			require.NoError(t, err)
			if tc.fail {
				require.Error(t, invocationErr)
				require.Empty(t, snapshot.GetAllMessages(), "no Submit after failed canonical admission")
				require.Equal(t, session.SafetyPolicyAutonomous, snapshot.GetSafetyPolicy())
			} else {
				require.NoError(t, invocationErr)
				require.Equal(t, session.SafetyPolicyRestricted, snapshot.GetSafetyPolicy())
				require.True(t, snapshot.NonInteractive)
			}
		})
	}
}
