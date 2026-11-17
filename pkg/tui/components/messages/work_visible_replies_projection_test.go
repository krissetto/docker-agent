package messages

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestProjectWorkVisibleRepliesLimitsLogicalGroupsPerExchange(t *testing.T) {
	for _, test := range []struct {
		name    string
		policy  messages.VisibleReplies
		visible int
	}{
		{name: "latest", policy: messages.VisibleRepliesLatest, visible: 1},
		{name: "three", policy: messages.VisibleRepliesThree, visible: 3},
		{name: "five", policy: messages.VisibleRepliesFive, visible: 5},
		{name: "all", policy: messages.VisibleRepliesAll, visible: 7},
		{name: "zero defaults to three", visible: 3},
		{name: "unknown defaults to three", policy: messages.VisibleReplies("future"), visible: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			items := visibleReplyProjectionMessages(append([]types.MessageType{types.MessageTypeUser}, repeated(types.MessageTypeAssistant, 7)...)...)
			groups := make(map[*types.Message]uint64)
			for i := 1; i < len(items); i++ {
				items[i].Content = "reply"
				groups[items[i]] = uint64(i)
			}
			projection := projectWorkWithVisibleReplies(items, groups, test.policy, resolveSessionOpener(items, nil))
			exchange := projection.exchanges[0]
			wantVisible := test.visible
			require.Equal(t, []int{len(items) - wantVisible}, []int{exchange.response[0]})
			require.Len(t, exchange.response, wantVisible)
			require.Len(t, exchange.work, 7-wantVisible)
			for i, source := range exchange.work {
				require.Equal(t, i+1, source, "older answers remain in chronological order")
				require.Equal(t, 0, projection.bySource[source])
			}
			for i, source := range exchange.response {
				require.Equal(t, len(items)-wantVisible+i, source)
				require.NotContains(t, projection.bySource, source)
			}
		})
	}
}

func TestProjectWorkVisibleRepliesCountsGroupsNotPartsOrCoordination(t *testing.T) {
	items := visibleReplyProjectionMessages(
		types.MessageTypeUser,
		types.MessageTypeAssistant,
		types.MessageTypeAssistant,
		types.MessageTypeRuntimeNotice,
		types.MessageTypeAssistant,
		types.MessageTypeAssistantReasoningBlock,
		types.MessageTypeToolCall,
		types.MessageTypeAssistant,
		types.MessageTypeAssistant,
		types.MessageTypeAssistant,
	)
	items[4].InputOrigin = session.InputOriginAgent
	for _, source := range []int{1, 2, 7, 8, 9} {
		items[source].Content = "answer"
	}
	groups := map[*types.Message]uint64{
		items[1]: 10, items[2]: 10, // multipart answer
		items[7]: 11, items[8]: 11, // another multipart answer
		items[9]: 12,
	}
	projection := projectWorkWithVisibleReplies(items, groups, messages.VisibleRepliesLatest, resolveSessionOpener(items, nil))
	require.Equal(t, []int{1, 2, 3, 4, 5, 6, 7, 8}, projection.exchanges[0].work)
	require.Equal(t, []int{9}, projection.exchanges[0].response)
	for _, source := range []int{1, 2, 4, 7, 8, 9} {
		_, inWork := projection.bySource[source]
		inResponse := containsSource(projection.exchanges[0].response, source)
		require.NotEqual(t, inWork, inResponse, "eligible source %d belongs to exactly one partition", source)
	}
}

func TestProjectWorkVisibleRepliesSeparatesFallbackGroupsAtRealWork(t *testing.T) {
	items := visibleReplyProjectionMessages(
		types.MessageTypeUser,
		types.MessageTypeAssistant,
		types.MessageTypeAssistant,
		types.MessageTypeRuntimeNotice,
		types.MessageTypeAssistant,
		types.MessageTypeToolCall,
		types.MessageTypeAssistant,
		types.MessageTypeAssistant,
	)
	for _, source := range []int{1, 2, 4, 6, 7} {
		items[source].Content = "answer"
	}
	projection := projectWorkWithVisibleReplies(items, nil, messages.VisibleRepliesLatest, resolveSessionOpener(items, nil))
	require.Equal(t, []int{1, 2, 3, 4, 5}, projection.exchanges[0].work)
	require.Equal(t, []int{6, 7}, projection.exchanges[0].response, "adjacent legacy parts are one reply")
}

func TestProjectWorkVisibleRepliesResetsAtCommittedHumanExchange(t *testing.T) {
	items := visibleReplyProjectionMessages(
		types.MessageTypeAssistant,
		types.MessageTypeAssistant,
		types.MessageTypeUser,
		types.MessageTypeAssistant,
		types.MessageTypeUser,
		types.MessageTypeAssistant,
		types.MessageTypeAssistant,
	)
	items[4].Pending = true
	for _, source := range []int{0, 1, 3, 5, 6} {
		items[source].Content = "answer"
	}
	groups := map[*types.Message]uint64{items[0]: 1, items[1]: 2, items[3]: 3, items[5]: 4, items[6]: 5}
	projection := projectWorkWithVisibleReplies(items, groups, messages.VisibleRepliesLatest, resolveSessionOpener(items, nil))
	require.Len(t, projection.exchanges, 2)
	// The first public assistant row opens the session, so it stays visible outside Work.
	require.Empty(t, projection.exchanges[0].work)
	require.Equal(t, []int{0, 1}, projection.exchanges[0].response)
	require.Equal(t, []int{3, 5}, projection.exchanges[1].work, "pending users do not reset the exchange or retention count")
	require.Equal(t, []int{6}, projection.exchanges[1].response)
}

func TestProjectWorkVisibleRepliesPreservesExceptionsAndSources(t *testing.T) {
	items := visibleReplyProjectionMessages(
		types.MessageTypeUser,
		types.MessageTypeAssistant,
		types.MessageTypeToolCall,
		types.MessageTypeError,
		types.MessageTypeCancelled,
	)
	items[2].ToolStatus = types.ToolStatusConfirmation
	items[1].Content = "answer"
	original := make([]types.Message, len(items))
	for i, item := range items {
		original[i] = *item
	}
	projection := projectWorkWithVisibleReplies(items, nil, messages.VisibleRepliesThree, resolveSessionOpener(items, nil))
	require.Equal(t, []int{1}, projection.exchanges[0].response)
	require.Empty(t, projection.exchanges[0].work)
	require.NotContains(t, projection.bySource, 2)
	require.NotContains(t, projection.bySource, 3)
	require.NotContains(t, projection.bySource, 4)
	for i, item := range items {
		require.Equal(t, original[i], *item, "projection leaves source content and metadata untouched")
	}
}

func repeated(kind types.MessageType, count int) []types.MessageType {
	items := make([]types.MessageType, count)
	for i := range items {
		items[i] = kind
	}
	return items
}

func containsSource(sources []int, target int) bool {
	return slices.Contains(sources, target)
}

func TestProjectWorkVisibleRepliesWorkArrivalsDoNotEvictLatestAnswer(t *testing.T) {
	items := visibleReplyProjectionMessages(
		types.MessageTypeUser,
		types.MessageTypeAssistant,
		types.MessageTypeAssistantReasoningBlock,
		types.MessageTypeToolCall,
		types.MessageTypeRuntimeNotice,
		types.MessageTypeAssistant,
	)
	items[1].Content = "answer"
	items[5].Content = "next answer"
	groups := map[*types.Message]uint64{items[1]: 1, items[5]: 2}
	projection := projectWorkWithVisibleReplies(items, groups, messages.VisibleRepliesLatest, resolveSessionOpener(items, nil))
	require.Equal(t, []int{1, 2, 3, 4}, projection.exchanges[0].work)
	require.Equal(t, []int{5}, projection.exchanges[0].response, "work does not consume a reply slot or demote the active answer")
}

func TestProjectWorkVisibleRepliesKeepsExplicitGroupAcrossWorkParts(t *testing.T) {
	items := visibleReplyProjectionMessages(
		types.MessageTypeUser,
		types.MessageTypeAssistant,
		types.MessageTypeToolCall,
		types.MessageTypeAssistant,
		types.MessageTypeAssistant,
	)
	items[1].Content, items[3].Content, items[4].Content = "first", "first part", "second"
	groups := map[*types.Message]uint64{items[1]: 10, items[3]: 10, items[4]: 11}
	projection := projectWorkWithVisibleReplies(items, groups, messages.VisibleRepliesLatest, resolveSessionOpener(items, nil))
	require.Equal(t, []int{1, 2, 3}, projection.exchanges[0].work)
	require.Equal(t, []int{4}, projection.exchanges[0].response)

	groups[items[4]] = 10
	latest := projectWorkWithVisibleReplies(items, groups, messages.VisibleRepliesLatest, resolveSessionOpener(items, nil))
	require.Equal(t, []int{2}, latest.exchanges[0].work)
	require.Equal(t, []int{1, 3, 4}, latest.exchanges[0].response, "same explicit group remains one answer even across work")
}

func TestProjectWorkVisibleRepliesRetainsNewestAnswerUntilAnotherAnswer(t *testing.T) {
	items := visibleReplyProjectionMessages(
		types.MessageTypeUser,
		types.MessageTypeAssistant,
		types.MessageTypeAssistantReasoningBlock,
		types.MessageTypeToolCall,
		types.MessageTypeRuntimeNotice,
	)
	groups := map[*types.Message]uint64{items[1]: 1}
	projection := projectWorkWithVisibleReplies(items, groups, messages.VisibleRepliesLatest, resolveSessionOpener(items, nil))
	require.Equal(t, []int{2, 3, 4}, projection.exchanges[0].work)
	require.Equal(t, []int{1}, projection.exchanges[0].response, "work does not evict a reply slot")
}

func TestProjectWorkVisibleRepliesIgnoresEmptyAndMediaOnlyReplyGroups(t *testing.T) {
	items := visibleReplyProjectionMessages(
		types.MessageTypeUser,
		types.MessageTypeAssistant,
		types.MessageTypeAssistant,
		types.MessageTypeAssistant,
	)
	items[1].Content = ""
	items[2].Content = ""
	items[2].AssistantMedia = []types.AssistantMedia{{Fallback: "generated image"}}
	items[3].Content = "actual prose"
	groups := map[*types.Message]uint64{items[1]: 1, items[2]: 2, items[3]: 3}
	projection := projectWorkWithVisibleReplies(items, groups, messages.VisibleRepliesLatest, resolveSessionOpener(items, nil))
	require.Empty(t, projection.exchanges[0].work)
	require.Equal(t, []int{2, 3}, projection.exchanges[0].response, "empty groups have no presentation; media-only groups do not consume a prose slot")
}

func TestProjectWorkVisibleRepliesKeepsEmptyPartWithItsProseGroup(t *testing.T) {
	items := visibleReplyProjectionMessages(
		types.MessageTypeUser,
		types.MessageTypeAssistant,
		types.MessageTypeAssistant,
	)
	items[2].Content = "answer"
	groups := map[*types.Message]uint64{items[1]: 1, items[2]: 1}
	projection := projectWorkWithVisibleReplies(items, groups, messages.VisibleRepliesLatest, resolveSessionOpener(items, nil))
	require.Equal(t, []int{1, 2}, projection.exchanges[0].response, "non-prose parts remain attached to the same answer group")
}

func visibleReplyProjectionMessages(typesList ...types.MessageType) []*types.Message {
	items := workProjectionMessages(typesList...)
	for _, item := range items {
		if item != nil && item.Type == types.MessageTypeAssistant {
			item.Content = "reply"
		}
	}
	return items
}
