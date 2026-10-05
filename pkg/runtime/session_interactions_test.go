package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestSessionRespondNormalizesResumeForSession(t *testing.T) {
	rt, sess := newSessionFixture(t)
	h, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	driver, ok := rt.sessionDrivers.Lookup(sess.ID)
	require.True(t, ok)
	ch, err := driver.registerResume(t.Context(), "request", InteractionConfirmation)
	require.NoError(t, err)
	require.NoError(t, h.Respond(t.Context(), InteractionResponse{InteractionID: "request", Kind: InteractionConfirmation, Resume: ResumeRequest{Type: ResumeTypeApproveSafe}}))
	assert.Equal(t, ResumeTypeApproveBalanced, (<-ch).Type)
}

func TestAsyncSubagentMarkerSurvivesClone(t *testing.T) {
	s := session.New(session.WithAsyncSubagent(true))
	assert.True(t, s.Clone().AsyncSubagent)
}
