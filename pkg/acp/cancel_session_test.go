package acp

import (
	"context"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestCancelStopsOnlyCurrentRunAndSessionRemainsUsable(t *testing.T) {
	t.Parallel()

	rt := &fakeRuntime{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	a := &Agent{sessions: map[string]*Session{
		testSessionID: {id: testSessionID, sess: session.New(session.WithID(testSessionID)), rt: rt, session: rt, cancel: cancel},
	}}
	require.NoError(t, a.Cancel(t.Context(), acpsdk.CancelNotification{SessionId: acpsdk.SessionId(testSessionID)}))
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	assert.Zero(t, rt.stopWakes(), "the owned turn, not notification handler, cancels execution")

	_, err := rt.Submit(t.Context(), runtime.TurnInput{Content: "resume"})
	require.NoError(t, err)
}
