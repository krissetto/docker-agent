package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

type composerQueue interface {
	ComposerQueue() []chat.ComposerQueueItem
	EditComposerQueueItem(order uint64, content string) bool
	RemoveComposerQueueItem(order uint64) bool
}

type composerQueueAction struct {
	tab     *tabModel
	queue   composerQueue
	order   uint64
	content string
	remove  bool
}

func (m *appModel) composerQueueCommands() []commands.Item {
	queue, ok := m.activeTab.chatPage.(composerQueue)
	if !ok {
		return nil
	}
	tab := m.activeTab
	var items []commands.Item
	for _, item := range queue.ComposerQueue() {
		first, _, _ := strings.Cut(item.Content, "\n")
		label := ansi.Truncate(ansi.Strip(first), 60, "…")
		if item.Editable {
			items = append(items, commands.Item{ID: fmt.Sprintf("edit-%d", item.Order), Category: "Pending messages", Label: "Edit queued message: " + label, Execute: func(string) tea.Cmd {
				return core.CmdHandler(composerQueueAction{tab: tab, queue: queue, order: item.Order})
			}})
		}
		items = append(items, commands.Item{ID: fmt.Sprintf("remove-%d", item.Order), Category: "Pending messages", Label: "Remove queued message: " + label, Execute: func(string) tea.Cmd {
			return core.CmdHandler(composerQueueAction{tab: tab, queue: queue, order: item.Order, remove: true})
		}})
	}
	return items
}

func (m *appModel) handleComposerQueueAction(msg composerQueueAction) (tea.Model, tea.Cmd) {
	if msg.tab != m.activeTab || !m.queueActionIsCurrent(msg.queue) {
		return m, nil
	}
	if msg.remove {
		if msg.queue.RemoveComposerQueueItem(msg.order) {
			return m, notification.SuccessCmd("Removed pending message")
		}
		return m, notification.WarningCmd("Message is no longer pending or could not be withdrawn")
	}
	if msg.content != "" {
		if msg.queue.EditComposerQueueItem(msg.order, msg.content) {
			return m, notification.SuccessCmd("Updated queued message")
		}
		return m, notification.WarningCmd("Message is no longer pending")
	}
	for _, item := range msg.queue.ComposerQueue() {
		if item.Order == msg.order && item.Editable {
			return m.forwardDialog(dialog.OpenDialogMsg{Model: dialog.NewComposerQueueEdit(item.Content, func(text string) tea.Cmd {
				msg.content = text
				return core.CmdHandler(msg)
			})})
		}
	}
	return m, notification.WarningCmd("Message is no longer pending")
}

func (m *appModel) queueActionIsCurrent(queue composerQueue) bool {
	current, _ := m.activeTab.chatPage.(composerQueue)
	return current == queue
}
