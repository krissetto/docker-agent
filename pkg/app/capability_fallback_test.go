package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestNilSessionHandleOperationsReturnTypedUnsupported(t *testing.T) {
	a := &App{currentState: sessionState{session: session.New(session.WithID("missing-handle"))}}

	tests := []struct {
		name      string
		operation runtime.SessionOperation
		call      func() error
	}{
		{name: "refresh models", operation: runtime.SessionOperationRefreshModels, call: func() error { return a.RefreshModelsCatalog(t.Context()) }},
		{name: "pause", operation: runtime.SessionOperationPause, call: func() error { _, err := a.TogglePause(t.Context()); return err }},
		{name: "star", operation: runtime.SessionOperationSetStarred, call: func() error { return a.SetCurrentSessionStarred(t.Context(), true) }},
		{name: "cycle thinking", operation: runtime.SessionOperationThinkingLevel, call: func() error { _, err := a.CycleAgentThinkingLevel(t.Context()); return err }},
		{name: "set thinking", operation: runtime.SessionOperationThinkingLevel, call: func() error { _, err := a.SetAgentThinkingLevel(t.Context(), effort.High); return err }},
		{name: "fork skill", operation: runtime.SessionOperationRunSkill, call: func() error { return a.StartSkillForkOperation(t.Context(), "operation", "skill", "task") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			require.ErrorIs(t, err, runtime.ErrUnsupported)
			var sessionErr *runtime.SessionError
			require.ErrorAs(t, err, &sessionErr)
			assert.Equal(t, runtime.SessionErrorUnsupported, sessionErr.Kind)
			assert.Equal(t, "missing-handle", sessionErr.SessionID)
			assert.Equal(t, tc.operation, sessionErr.Operation)
		})
	}
}

func TestFalseModelCapabilityPreservesHandleTypedError(t *testing.T) {
	a := &App{currentState: sessionState{session: session.New(session.WithID("no-models")), handle: &projectionSession{id: "no-models"}}}

	err := a.SetCurrentAgentModel(t.Context(), "other/model")
	require.ErrorIs(t, err, runtime.ErrUnsupported)
	var sessionErr *runtime.SessionError
	require.ErrorAs(t, err, &sessionErr)
	assert.Equal(t, runtime.SessionOperationSetModel, sessionErr.Operation)
}
