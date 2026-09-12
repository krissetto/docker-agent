package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestSessionInteractionsRouteResumeBySession(t *testing.T) {
	i := newSessionInteractions()
	a := i.resumeChannel("a")
	b := i.resumeChannel("b")
	require.True(t, i.sendResume("b", ResumeReject("no")))
	select {
	case <-a:
		t.Fatal("session a consumed session b response")
	default:
	}
	assert.Equal(t, ResumeTypeReject, (<-b).Type)
	assert.False(t, i.sendResume("", ResumeApprove()), "unaddressed resume is rejected")
}

func TestSessionRespondNormalizesResumeForSession(t *testing.T) {
	rt, sess := newSessionFixture(t)
	h, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	ch := rt.interactions.resumeChannel(sess.ID)
	driver, ok := rt.sessionDrivers.Lookup(sess.ID)
	require.True(t, ok)
	driver.RegisterInteraction("request", InteractionConfirmation)
	require.NoError(t, h.Respond(t.Context(), InteractionResponse{InteractionID: "request", Kind: InteractionConfirmation, Resume: ResumeRequest{Type: ResumeTypeApproveSafe}}))
	assert.Equal(t, ResumeTypeApproveBalanced, (<-ch).Type)
}

func TestAsyncSubagentMarkerSurvivesClone(t *testing.T) {
	s := session.New(session.WithAsyncSubagent(true))
	assert.True(t, s.Clone().AsyncSubagent)
}
