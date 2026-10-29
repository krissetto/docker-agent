package session

import (
	"strings"
	"testing"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/stretchr/testify/require"
)

func TestReadBoundedItemsCountsOversizedPositionsAndPreservesToolLinkage(t *testing.T) {
	s := New(WithID("s"))
	s.AddMessage(UserMessage("old"))
	s.AddMessage(UserMessage(strings.Repeat("x", 2<<20)))
	s.AddMessage(NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: "call"}}}))
	s.AddMessage(NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleTool, ToolCallID: "call", Content: "result"}))
	page, err := s.ReadBoundedItems(4, 4, 64<<10, 256<<10)
	require.NoError(t, err)
	require.Equal(t, []int{1}, page.OmittedPositions)
	require.Equal(t, []int{0, 2, 3}, []int{page.Items[0].Position, page.Items[1].Position, page.Items[2].Position})
	require.Equal(t, "call", page.Items[1].Item.Message.Message.ToolCalls[0].ID)
	require.Equal(t, "call", page.Items[2].Item.Message.Message.ToolCallID)
}

func TestReadBoundedItemsStopsAtPageBytesWithoutOmittingFitRecord(t *testing.T) {
	s := New(WithID("s"))
	s.AddMessage(UserMessage(strings.Repeat("x", 90)))
	s.AddMessage(UserMessage(strings.Repeat("y", 90)))
	page, err := s.ReadBoundedItems(2, 2, 4096, 256)
	require.NoError(t, err)
	require.Empty(t, page.OmittedPositions)
	require.Len(t, page.Items, 1)
	require.Equal(t, 1, page.Items[0].Position)
	require.True(t, page.HasMore)
	require.Equal(t, 1, page.NextBeforePosition)
}

func TestJSONWithinBoundDoesNotMarshalCustomValue(t *testing.T) {
	value := &historyUnsafeMarshaler{}
	require.False(t, JSONWithinBound(value, 128))
	require.Zero(t, value.calls)
}

type historyUnsafeMarshaler struct{ calls int }

func (*historyUnsafeMarshaler) MarshalJSON() ([]byte, error) { return []byte(`"ok"`), nil }

func TestReadBoundedItemsWithBudgetKeepsMixedOmissionRangesOrdered(t *testing.T) {
	s := New(WithID("mixed-omissions"))
	s.AddMessage(UserMessage(strings.Repeat("r", 15<<10)))
	s.AddMessage(UserMessage("fit one"))
	s.AddMessage(UserMessage(strings.Repeat("i", 17<<10)))
	s.AddMessage(UserMessage("fit two"))
	s.AddMessage(UserMessage(strings.Repeat("r", 15<<10)))
	page, err := s.ReadBoundedItemsWithBudget(5, 5, 16<<10, func(candidate BoundedItemPage) bool {
		for _, item := range candidate.Items {
			if item.Position == 4 {
				return false
			}
		}
		return len(candidate.Items)+len(candidate.OmittedPositions)+len(candidate.OmittedResponsePositions) <= 4
	})
	require.NoError(t, err)
	require.Equal(t, []int{1, 3}, []int{page.Items[0].Position, page.Items[1].Position})
	require.Equal(t, []int{2}, page.OmittedPositions)
	require.Equal(t, []int{4}, page.OmittedResponsePositions)
	require.Equal(t, 1, page.NextBeforePosition, "oldest unconsumed record is left for the next cursor")
}
