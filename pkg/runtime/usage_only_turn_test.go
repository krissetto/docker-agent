package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// A provider-billed empty served turn is persisted as a usage-only assistant
// item; a cancelled turn and a turn without usage persist nothing.
func TestRecordAssistantMessageEmptyTurns(t *testing.T) {
	t.Parallel()

	root := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/m"}))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionCompaction(false))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, rt.Close()) })

	usage := &chat.Usage{InputTokens: 30, OutputTokens: 2}
	cost := 0.00066
	for _, tc := range []struct {
		name      string
		res       streamResult
		cancelled bool
		wantItems int
	}{
		{name: "billed empty stop persists a usage-only item", res: streamResult{Usage: usage, FinishReason: chat.FinishReasonStop}, wantItems: 1},
		{name: "billed empty cancelled turn persists nothing", res: streamResult{Usage: usage}, cancelled: true, wantItems: 0},
		{name: "empty turn without usage is skipped", res: streamResult{}, wantItems: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sess := session.New()
			ctx := t.Context()
			if tc.cancelled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			msg := rt.recordAssistantMessage(ctx, sess, root, tc.res, nil, "test/m", &cost, &collectSink{})
			require.Len(t, sess.Messages, tc.wantItems)
			if tc.wantItems == 0 {
				assert.Nil(t, msg)
				return
			}
			require.NotNil(t, msg)
			assert.Equal(t, "test/m", msg.Model)
			assert.InDelta(t, cost, msg.Cost, 1e-12)
			require.NotNil(t, sess.Messages[0].Message)
			assert.Empty(t, sess.Messages[0].Message.Message.Content)
			assert.Empty(t, sess.Messages[0].Message.Message.ToolCalls)
		})
	}
}
