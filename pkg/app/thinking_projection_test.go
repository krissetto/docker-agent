package app

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

// metadataThinkingController deliberately implements mutation only. Projection
// remains metadata-backed, preserving compatibility with sessions created before
// the read-only thinking interfaces were introduced.
type metadataThinkingController struct {
	projectionSession

	level effort.Level
}

func (a *metadataThinkingController) Metadata() runtime.SessionMetadata {
	return runtime.SessionMetadata{
		SessionID:     a.id,
		AgentName:     "root",
		ThinkingLevel: a.level,
		ThinkingLevels: []effort.Level{
			effort.Low,
			effort.High,
		},
		Capabilities: runtime.SessionCapabilities{ThinkingLevels: true},
	}
}

func (a *metadataThinkingController) ThinkingLevels(context.Context) []effort.Level {
	return a.Metadata().ThinkingLevels
}

func (a *metadataThinkingController) CurrentThinkingLevel(context.Context) effort.Level {
	return a.level
}

func (a *metadataThinkingController) CycleThinkingLevel(context.Context) (effort.Level, error) {
	a.level = effort.High
	return a.level, nil
}

func (a *metadataThinkingController) SetThinkingLevel(_ context.Context, level effort.Level) (effort.Level, error) {
	a.level = level
	return level, nil
}

func TestCurrentThinkingProjectionFallsBackToSessionMetadata(t *testing.T) {
	controller := &metadataThinkingController{
		projectionSession: projectionSession{id: "thinking-metadata"},
		level:             effort.Low,
	}
	a := &App{currentState: sessionState{session: session.New(session.WithID(controller.id)), handle: controller}}

	assert.Equal(t, effort.Low, a.CurrentAgentThinkingLevel(t.Context()))
	assert.Equal(t, []effort.Level{effort.Low, effort.High}, a.CurrentAgentThinkingLevels(t.Context()))

	applied, err := a.SetAgentThinkingLevel(t.Context(), effort.High)
	require.NoError(t, err)
	assert.Equal(t, effort.High, applied)
	assert.Equal(t, effort.High, a.CurrentAgentThinkingLevel(t.Context()))
}
