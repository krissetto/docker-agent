package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
)

func TestSessionHandleFalseForkSkillsReturnsTypedUnsupported(t *testing.T) {
	localRuntime, sess := newSessionFixture(t)
	handle, err := localRuntime.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	require.False(t, handle.Metadata().Capabilities.ForkSkills)

	calls := []struct {
		name string
		call func() error
	}{
		{"start", func() error {
			return handle.StartSkillFork(t.Context(), "operation", skillstool.RunSkillArgs{})
		}},
		{"run", func() error {
			_, err := handle.RunSkillFork(t.Context(), skillstool.RunSkillArgs{}, nil)
			return err
		}},
	}
	for _, call := range calls {
		t.Run(call.name, func(t *testing.T) {
			err := call.call()
			require.ErrorIs(t, err, ErrUnsupported)
			var sessionErr *SessionError
			require.ErrorAs(t, err, &sessionErr)
			assert.Equal(t, SessionErrorUnsupported, sessionErr.Kind)
			assert.Equal(t, SessionOperationRunSkill, sessionErr.Operation)
			assert.Equal(t, handle.ID(), sessionErr.SessionID)
		})
	}
}
