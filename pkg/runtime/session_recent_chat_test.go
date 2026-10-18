package runtime

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func TestResidentAttentionCountsOutstandingInteractionsWithoutTranscript(t *testing.T) {
	root := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/mock-model"}))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	handle, err := rt.CreateSession(t.Context(), session.New(session.WithID("recent-chat-attention")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	resident := handle.(*sessionHandle)
	err = resident.driver.ownerCall(t.Context(), func() error {
		resident.driver.interactions["confirm"] = sessionInteraction{kind: InteractionConfirmation}
		resident.driver.interactions["elicit"] = sessionInteraction{kind: InteractionElicitation}
		return nil
	})
	require.NoError(t, err)
	attention, err := handle.(ResidentAttentionReader).ReadResidentAttention(t.Context())
	require.NoError(t, err)
	require.True(t, attention.Known)
	require.Equal(t, 1, attention.ApprovalCount)
	require.Equal(t, 1, attention.ElicitationCount)
	err = resident.driver.ownerCall(t.Context(), func() error {
		delete(resident.driver.interactions, "confirm")
		delete(resident.driver.interactions, "elicit")
		return nil
	})
	require.NoError(t, err)
	attention, err = handle.(ResidentAttentionReader).ReadResidentAttention(t.Context())
	require.NoError(t, err)
	require.True(t, attention.Known)
	require.Zero(t, attention.ApprovalCount)
	require.Zero(t, attention.ElicitationCount)
}
