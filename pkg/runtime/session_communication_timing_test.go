package runtime

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestCommunicationGuidanceWakesIdleParentBeforeChildCompletion(t *testing.T) {
	childEntered, childRelease, parentEntered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	var childCalls, parentCalls atomic.Int32
	worker := coordinationReply("unused")
	worker.call = func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
		childCalls.Add(1)
		close(childEntered)
		select {
		case <-childRelease:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return newStreamBuilder().AddContent("finished").AddStopWithUsage(1, 1).Build(), nil
	}
	root := coordinationReply("received")
	root.call = func(context.Context, []chat.Message) (chat.MessageStream, error) {
		if parentCalls.Add(1) == 1 {
			close(parentEntered)
		}
		return newStreamBuilder().AddContent("received").AddStopWithUsage(1, 1).Build(), nil
	}
	rt, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), root, worker)
	t.Cleanup(func() { releaseOnce.Do(func() { close(childRelease) }) })
	parent := coordinationCreate(t, owner.Runtime(), "communication-parent", "")
	child := coordinationCreate(t, owner.Runtime(), "communication-child", parent.ID())
	turn, err := child.Submit(t.Context(), TurnInput{Content: "work"})
	require.NoError(t, err)
	coordinationWait(t, childEntered)
	childSnapshot, err := child.Snapshot(t.Context())
	require.NoError(t, err)
	call := tools.ToolCall{ID: "guidance", Function: tools.FunctionCall{Arguments: `{"to":"parent","message":"needed decision","request_id":"guidance"}`}}
	result, err := rt.handleSendMessage(t.Context(), childSnapshot, call, nil, tools.NopRuntime{})
	require.NoError(t, err)
	require.False(t, result.IsError, result.Output)
	var receipt subagent.DeliveryReceipt
	require.NoError(t, json.Unmarshal([]byte(result.Output), &receipt))
	require.True(t, receipt.Accepted)
	assert.Equal(t, subagent.DeliveryGuidance, receipt.Disposition)
	coordinationWait(t, parentEntered)
	coordinationAwait(t, parent, receipt.RequestID)
	coordinationSettled(t, parent)
	releaseOnce.Do(func() { close(childRelease) })
	coordinationAwait(t, child, turn.TurnID)
	reportID := "report:" + childReportID(child.ID(), turn.TurnID)
	coordinationAwait(t, parent, reportID)
	coordinationSettled(t, parent)
	snapshot, err := parent.Snapshot(t.Context())
	require.NoError(t, err)
	var order []string
	for _, item := range snapshot.MessagesSnapshot() {
		if item.Message != nil && item.Message.Accepted {
			assert.False(t, item.Message.Pending)
			order = append(order, item.Message.TurnID)
		}
	}
	assert.Equal(t, []string{receipt.RequestID, reportID}, order)
	assert.Equal(t, int32(1), childCalls.Load())
	assert.Equal(t, int32(2), parentCalls.Load())
}
