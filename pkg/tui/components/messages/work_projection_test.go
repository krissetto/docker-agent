package messages

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/types"
)

func workProjectionMessages(typesList ...types.MessageType) []*types.Message {
	items := make([]*types.Message, len(typesList))
	for i, typ := range typesList {
		items[i] = &types.Message{Type: typ}
		if typ == types.MessageTypeAssistant {
			items[i].Content = "reply"
		}
	}
	return items
}

func TestProjectWorkKeepsNewestResponseUntilActualContinuation(t *testing.T) {
	items := workProjectionMessages(
		types.MessageTypeUser,
		types.MessageTypeAssistant,
		types.MessageTypeRuntimeNotice,
		types.MessageTypeAssistantReasoningBlock,
		types.MessageTypeToolCall,
		types.MessageTypeAssistant,
	)
	groups := map[*types.Message]uint64{items[1]: 1, items[5]: 2}
	got := projectWork(items, groups)
	require.Len(t, got.exchanges, 1)
	require.Equal(t, []int{1, 2, 3, 4}, got.exchanges[0].work)
	require.Equal(t, []int{5}, got.exchanges[0].response)
}

func TestProjectWorkNoticeDoesNotDemoteCandidateAndHumanInputStartsExchange(t *testing.T) {
	items := workProjectionMessages(
		types.MessageTypeUser,
		types.MessageTypeAssistant,
		types.MessageTypeRuntimeNotice,
		types.MessageTypeAssistant,
		types.MessageTypeUser,
		types.MessageTypeAssistant,
	)
	groups := map[*types.Message]uint64{items[1]: 1, items[3]: 1, items[5]: 2}
	got := projectWork(items, groups)
	require.Len(t, got.exchanges, 2)
	require.Equal(t, []int{1, 3}, got.exchanges[0].response)
	require.Equal(t, []int{2}, got.exchanges[0].work)
	require.Equal(t, []int{5}, got.exchanges[1].response)
}

func TestProjectWorkLeavesInteractionsAndTerminalMessagesOutside(t *testing.T) {
	items := workProjectionMessages(
		types.MessageTypeUser,
		types.MessageTypeAssistant,
		types.MessageTypeToolCall,
		types.MessageTypeError,
		types.MessageTypeCancelled,
	)
	items[2].ToolStatus = types.ToolStatusConfirmation
	got := projectWork(items, nil)
	require.Equal(t, []int{1}, got.exchanges[0].response)
	require.Empty(t, got.exchanges[0].work)
}

func TestProjectWorkGroupsToolContinuationAndKeepsMediaAnswerSpan(t *testing.T) {
	items := workProjectionMessages(
		types.MessageTypeUser,
		types.MessageTypeAssistant,
		types.MessageTypeAssistant,
		types.MessageTypeToolCall,
		types.MessageTypeAssistant,
	)
	got := projectWork(items, nil)
	require.Equal(t, []int{1, 2, 3}, got.exchanges[0].work)
	require.Equal(t, []int{4}, got.exchanges[0].response)
}

func TestProjectWorkKeepsOrdinarySpanVisibleUntilToolArrives(t *testing.T) {
	items := workProjectionMessages(
		types.MessageTypeUser,
		types.MessageTypeAssistant,
		types.MessageTypeAssistant,
		types.MessageTypeToolCall,
		types.MessageTypeAssistant,
	)
	groups := map[*types.Message]uint64{items[1]: 1, items[2]: 1, items[4]: 2}
	projection := projectWork(items, groups)
	require.Equal(t, []int{1, 2, 3}, projection.exchanges[0].work)
	require.Equal(t, []int{4}, projection.exchanges[0].response)
}

func TestProjectWorkSourceMembershipIsLosslessAndOrdered(t *testing.T) {
	items := workProjectionMessages(
		types.MessageTypeUser,
		types.MessageTypeAssistant,
		types.MessageTypeAssistantReasoningBlock,
		types.MessageTypeToolCall,
		types.MessageTypeRuntimeNotice,
		types.MessageTypeAssistant,
		types.MessageTypeError,
	)
	projection := projectWork(items, nil)
	work := projection.exchanges[0].work
	require.Equal(t, []int{1, 2, 3, 4}, work)
	require.Equal(t, []int{5}, projection.exchanges[0].response)
	for i, source := range work {
		require.Equal(t, 0, projection.bySource[source])
		if i > 0 {
			require.Less(t, work[i-1], source)
		}
	}
	require.NotContains(t, projection.bySource, 6, "terminal errors remain outside compact Work")
}

func TestProjectWorkTreatsCoordinationAndShellOutputAsWorkWithoutHumanBoundary(t *testing.T) {
	items := workProjectionMessages(
		types.MessageTypeUser,
		types.MessageTypeAssistant,
		types.MessageTypeRuntimeNotice,
		types.MessageTypeAgentInput,
		types.MessageTypeAgentReturn,
		types.MessageTypeShellOutput,
		types.MessageTypeToolResult,
		types.MessageTypeAssistant,
	)
	groups := map[*types.Message]uint64{items[1]: 1, items[7]: 2}
	projection := projectWork(items, groups)
	require.Len(t, projection.exchanges, 1, "only a real human message splits exchanges")
	require.Equal(t, []int{1, 2, 3, 4, 5, 6}, projection.exchanges[0].work)
	require.Equal(t, []int{7}, projection.exchanges[0].response)
}

func TestPendingHumanReceiptDoesNotCreateExchangeBoundary(t *testing.T) {
	items := workProjectionMessages(types.MessageTypeUser, types.MessageTypeAssistant, types.MessageTypeUser, types.MessageTypeAssistant)
	items[2].Pending = true
	groups := map[*types.Message]uint64{items[1]: 1, items[3]: 2}
	projection := projectWork(items, groups)
	require.Len(t, projection.exchanges, 1)
	require.Equal(t, []int{1}, projection.exchanges[0].work)
	require.Equal(t, []int{3}, projection.exchanges[0].response)
}
