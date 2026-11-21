package chat

import (
	"cmp"
	"slices"

	"github.com/docker/docker-agent/pkg/runtime"
)

// ComposerQueueItem identifies one pending message for the active page only.
type ComposerQueueItem struct {
	Order    uint64
	Content  string
	Editable bool
}

// ComposerQueue snapshots the visible queue in submission order. Runtime
// entries can be withdrawn, but upstream has no atomic replace operation.
func (p *chatPage) ComposerQueue() []ComposerQueueItem {
	items := make([]ComposerQueueItem, 0, len(p.messageQueue)+len(p.pendingMessages))
	for _, item := range p.messageQueue {
		items = append(items, ComposerQueueItem{Order: item.order, Content: item.content, Editable: true})
	}
	for _, item := range p.pendingMessages {
		items = append(items, ComposerQueueItem{Order: item.order, Content: item.content})
	}
	slices.SortFunc(items, func(a, b ComposerQueueItem) int { return cmp.Compare(a.Order, b.Order) })
	return items
}

func (p *chatPage) EditComposerQueueItem(order uint64, content string) bool {
	index := slices.IndexFunc(p.messageQueue, func(item queuedMessage) bool { return item.order == order })
	if index < 0 {
		return false
	}
	p.messageQueue[index].content = content
	p.syncQueueToSidebar()
	return true
}

func (p *chatPage) RemoveComposerQueueItem(order uint64) bool {
	index := slices.IndexFunc(p.messageQueue, func(item queuedMessage) bool { return item.order == order })
	if index >= 0 {
		p.messageQueue = slices.Delete(p.messageQueue, index, index+1)
	} else {
		index = slices.IndexFunc(p.pendingMessages, func(item queuedMessage) bool { return item.order == order })
		if index < 0 || p.app == nil {
			return false
		}
		item := p.pendingMessages[index]
		if !p.app.CancelPendingMessage(p.ctx(), runtime.QueuedMessage{ID: item.id}, item.followUp) {
			return false
		}
		p.pendingMessages = slices.Delete(p.pendingMessages, index, index+1)
	}
	p.syncQueueToSidebar()
	return true
}
