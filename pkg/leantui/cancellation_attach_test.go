package leantui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/runtime"
)

func TestLeanNewViewerCancelsCanonicalTurnWithoutSubmitting(t *testing.T) {
	f := newViewerLifecycleFixture(t)
	accepted, err := f.child.Submit(t.Context(), runtime.TurnInput{Content: "owned outside this viewer"})
	require.NoError(t, err)
	viewerLifecycleNextCall(t, f.provider)
	attached := f.attach(t, string(f.node))
	require.Equal(t, accepted.TurnID, attached.Presentation().Status.TurnID)
	require.Empty(t, f.m.pendingUsers)
	f.m.interruptMode = "always"
	f.m.handleKey(t.Context(), ui.Key{Typ: ui.KeyEsc})
	status, err := f.child.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, accepted.TurnID, status.TurnID)
	assert.Equal(t, runtime.SessionStateRunning, status.State)
	f.m.handleKey(t.Context(), ui.Key{Typ: ui.KeyRune, Runes: []rune{'y'}})
	require.NoError(t, f.child.AwaitTurn(t.Context(), accepted.TurnID))
	assert.Equal(t, int32(1), f.provider.canceled.Load())
	status, err = f.child.Status(t.Context())
	require.NoError(t, err)
	assert.Empty(t, status.TurnID)
}
