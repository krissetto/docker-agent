package messages

import (
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/session"
	tuimessages "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// workExchange is an ephemeral display grouping. Source messages remain in
// model.messages; these indices only describe their compact presentation.
type workExchange struct {
	boundary int
	work     []int
	response []int
}

type workProjection struct {
	exchanges []workExchange
	bySource  map[int]int
}

// projectWork maps source messages into human-input-bounded display exchanges.
// Committed answer-group IDs disambiguate adjacent ordinary outputs while
// preserving their part boundaries and source-owned views.
func projectWork(messages []*types.Message, responseGroups map[*types.Message]uint64) workProjection {
	projection := workProjection{bySource: make(map[int]int)}
	current := -1
	var candidate []int
	var candidateGroup uint64
	ensureExchange := func(boundary int) int {
		projection.exchanges = append(projection.exchanges, workExchange{boundary: boundary})
		return len(projection.exchanges) - 1
	}
	demoteCandidate := func() {
		if current < 0 || len(candidate) == 0 {
			return
		}
		projection.exchanges[current].work = append(projection.exchanges[current].work, candidate...)
		candidate = nil
		candidateGroup = 0
	}
	finishCandidate := func() {
		if current >= 0 && len(candidate) > 0 {
			projection.exchanges[current].response = append(projection.exchanges[current].response, candidate...)
		}
		candidate = nil
		candidateGroup = 0
	}

	for i, msg := range messages {
		if msg == nil {
			continue
		}
		if msg.Type == types.MessageTypeUser {
			if msg.Pending {
				continue
			}
			finishCandidate()
			current = ensureExchange(i)
			continue
		}
		if current < 0 {
			current = ensureExchange(-1)
		}
		switch msg.Type {
		case types.MessageTypeAssistant:
			group := responseGroups[msg]
			if len(candidate) > 0 && group != 0 && candidateGroup != 0 && group != candidateGroup {
				demoteCandidate()
			}
			if len(candidate) == 0 {
				candidateGroup = group
			}
			candidate = append(candidate, i)
		case types.MessageTypeAssistantReasoningBlock:
			demoteCandidate()
			projection.exchanges[current].work = append(projection.exchanges[current].work, i)
		case types.MessageTypeToolCall:
			if msg.ToolStatus != types.ToolStatusConfirmation {
				demoteCandidate()
				projection.exchanges[current].work = append(projection.exchanges[current].work, i)
			}
		case types.MessageTypeRuntimeNotice, types.MessageTypeAgentInput, types.MessageTypeAgentReturn, types.MessageTypeShellOutput, types.MessageTypeToolResult:
			projection.exchanges[current].work = append(projection.exchanges[current].work, i)
		}
	}
	finishCandidate()

	for exchangeIndex, exchange := range projection.exchanges {
		slices.Sort(exchange.work)
		for _, source := range exchange.work {
			projection.bySource[source] = exchangeIndex
		}
	}
	return projection
}

// sessionOpenerEligible reports whether a source row can open the session:
// a non-pending human, incoming-agent input, or assistant reply with prose.
// Pending receipts, runtime notices, and empty assistant prefixes never qualify.
func sessionOpenerEligible(msg *types.Message) bool {
	if msg == nil || msg.Pending {
		return false
	}
	switch msg.Type {
	case types.MessageTypeUser, types.MessageTypeAgentInput:
		return true
	case types.MessageTypeAssistant:
		return strings.TrimSpace(msg.Content) != ""
	}
	return false
}

// resolveSessionOpener keeps an established opener while it remains a source
// row in messages, so a later prose flip before it cannot replace it. Otherwise
// the first eligible row in content order becomes the opener.
func resolveSessionOpener(messages []*types.Message, current *types.Message) *types.Message {
	if current != nil && slices.Contains(messages, current) && sessionOpenerEligible(current) {
		return current
	}
	for _, msg := range messages {
		if sessionOpenerEligible(msg) {
			return msg
		}
	}
	return nil
}

// projectWorkWithVisibleReplies applies the normalized logical prose-retention policy.
// The opener source row is placed in response directly, so it stays above Work
// and outside the retention limit; only that row is pinned, not its later siblings.
func projectWorkWithVisibleReplies(messages []*types.Message, responseGroups map[*types.Message]uint64, visible tuimessages.VisibleReplies, opener *types.Message) workProjection {
	projection := workProjection{bySource: make(map[int]int)}
	limit := visibleReplyLimit(visible)
	openerIndex := slices.Index(messages, opener)
	if opener == nil {
		openerIndex = -1
	}
	current := -1
	var fallback *replyGroup
	var groups []*replyGroup
	explicitGroups := make(map[uint64]*replyGroup)
	ensureExchange := func(boundary int) int {
		projection.exchanges = append(projection.exchanges, workExchange{boundary: boundary})
		groups = nil
		explicitGroups = make(map[uint64]*replyGroup)
		fallback = nil
		return len(projection.exchanges) - 1
	}
	finishExchange := func() {
		if current < 0 {
			return
		}
		exchange := &projection.exchanges[current]
		var proseGroups []*replyGroup
		for _, group := range groups {
			if group.hasProse {
				proseGroups = append(proseGroups, group)
			}
		}
		firstVisible := 0
		if limit > 0 && len(proseGroups) > limit {
			firstVisible = len(proseGroups) - limit
		}
		for _, group := range groups {
			if !group.hasProse {
				exchange.response = append(exchange.response, group.sources...)
			}
		}
		for _, group := range proseGroups[firstVisible:] {
			exchange.response = append(exchange.response, group.sources...)
		}
		for _, group := range proseGroups[:firstVisible] {
			exchange.work = append(exchange.work, group.sources...)
		}
		slices.Sort(exchange.work)
		slices.Sort(exchange.response)
		for _, source := range exchange.work {
			projection.bySource[source] = current
		}
	}
	appendWork := func(source int) {
		projection.exchanges[current].work = append(projection.exchanges[current].work, source)
	}
	appendResponse := func(source int) {
		projection.exchanges[current].response = append(projection.exchanges[current].response, source)
	}

	for i, msg := range messages {
		if msg == nil {
			continue
		}
		if msg.Type == types.MessageTypeUser {
			if msg.Pending {
				continue
			}
			finishExchange()
			current = ensureExchange(i)
			continue
		}
		if current < 0 {
			current = ensureExchange(-1)
		}
		switch msg.Type {
		case types.MessageTypeAssistant:
			if i == openerIndex {
				// Only this source row is pinned; the logical group that follows
				// starts fresh and keeps the normal retention limit.
				fallback = nil
				appendResponse(i)
				continue
			}
			if msg.InputOrigin == session.InputOriginAgent {
				fallback = nil
				appendWork(i)
				continue
			}
			groupID := responseGroups[msg]
			if groupID != 0 {
				fallback = nil
				group := explicitGroups[groupID]
				if group == nil {
					group = &replyGroup{}
					explicitGroups[groupID] = group
					groups = append(groups, group)
				}
				group.sources = append(group.sources, i)
				group.hasProse = group.hasProse || strings.TrimSpace(msg.Content) != ""
				continue
			}
			if fallback == nil {
				fallback = &replyGroup{}
				groups = append(groups, fallback)
			}
			fallback.sources = append(fallback.sources, i)
			fallback.hasProse = fallback.hasProse || strings.TrimSpace(msg.Content) != ""
		case types.MessageTypeAssistantReasoningBlock:
			fallback = nil
			appendWork(i)
		case types.MessageTypeToolCall:
			if msg.ToolStatus != types.ToolStatusConfirmation {
				fallback = nil
				appendWork(i)
			}
		case types.MessageTypeAgentInput:
			if i == openerIndex {
				appendResponse(i)
				continue
			}
			appendWork(i)
		case types.MessageTypeRuntimeNotice, types.MessageTypeAgentReturn, types.MessageTypeShellOutput, types.MessageTypeToolResult:
			appendWork(i)
		}
	}
	finishExchange()
	return projection
}

// replyGroup represents one logical ordinary assistant answer, potentially
// spanning multiple source messages or media parts.
type replyGroup struct {
	sources  []int
	hasProse bool
}

func visibleReplyLimit(visible tuimessages.VisibleReplies) int {
	switch visible {
	case tuimessages.VisibleRepliesLatest:
		return 1
	case tuimessages.VisibleRepliesFive:
		return 5
	case tuimessages.VisibleRepliesAll:
		return -1
	case tuimessages.VisibleRepliesThree:
		return 3
	default:
		return 3
	}
}
