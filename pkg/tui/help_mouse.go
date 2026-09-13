package tui

import "github.com/docker/docker-agent/pkg/tui/help"

// rootMouseHelp describes actual hit targets, not focus-only keyboard controls.
func rootMouseHelp() []help.Section {
	return []help.Section{
		{ID: "mouse.tabs", Title: "Session tabs \u2014 mouse", Entries: []help.Entry{
			{ID: "nd.tabs.mouse.tab.switch.tab", Keys: []string{"left click"}, Description: "Switch tab", Condition: "tab hit target visible"},
			{ID: "nd.tabs.mouse.tab.close.close.tab", Keys: []string{"left click"}, Description: "Close tab", Condition: "tab close \u00d7 hit target visible"},
			{ID: "nd.tabs.mouse.tab.close.tab.middle.click", Keys: []string{"middle click"}, Description: "Close tab middle click", Condition: "tab hit target visible"},
			{ID: "nd.tabs.mouse.plus.new.tab", Keys: []string{"left click"}, Description: "New tab", Condition: "plus hit target visible"},
			{ID: "nd.tabs.mouse.overflow.arrows.scroll.tab.strip", Keys: []string{"left click"}, Description: "Scroll tab strip", Condition: "overflow arrows hit target visible"},
			{ID: "nd.tabs.mouse.tab.strip.scroll.tab.strip.wheel", Keys: []string{"wheel"}, Description: "Scroll tab strip wheel", Condition: "tab strip hit target visible"},
			{ID: "nd.tabs.mouse.tab.reorder.tabs", Keys: []string{"hold drag release"}, Description: "Reorder tabs", Condition: "tab hit target visible"},
		}},
		{ID: "mouse.sidebar", Title: "Sidebar \u2014 mouse", Entries: []help.Entry{
			{ID: "nd.sidebar.mouse.toggle.glyph.toggle.sidebar", Keys: []string{"left click"}, Description: "Toggle sidebar", Condition: "toggle glyph hit target visible"},
			{ID: "nd.sidebar.mouse.divider.resize.sidebar", Keys: []string{"drag"}, Description: "Resize sidebar", Condition: "divider hit target visible"},
			{ID: "nd.sidebar.mouse.star.star.session", Keys: []string{"left click"}, Description: "Star session", Condition: "star hit target visible"},
			{ID: "nd.sidebar.mouse.title.rename.guidance", Keys: []string{"left click"}, Description: "Rename guidance", Condition: "title hit target visible"},
			{ID: "nd.sidebar.mouse.title.begin.title.edit", Keys: []string{"double click"}, Description: "Begin title edit", Condition: "title hit target visible"},
			{ID: "nd.sidebar.mouse.directory.label.copy.copy.directory", Keys: []string{"left click"}, Description: "Copy directory", Condition: "directory label/copy hit target visible"},
			{ID: "nd.sidebar.mouse.directory.arrow.open.directory", Keys: []string{"left click"}, Description: "Open directory", Condition: "directory arrow hit target visible"},
			{ID: "nd.sidebar.mouse.model.provider.open.model.picker", Keys: []string{"left click"}, Description: "Open model picker", Condition: "model/provider hit target visible"},
			{ID: "nd.sidebar.mouse.queue.body.edit.guidance", Keys: []string{"left click"}, Description: "Edit guidance", Condition: "queue body hit target visible"},
			{ID: "nd.sidebar.mouse.queue.body.canonical.pending.edit", Keys: []string{"double click"}, Description: "Canonical pending edit", Condition: "queue body hit target visible"},
			{ID: "nd.sidebar.mouse.queue.withdraw.exact.pending", Keys: []string{"left click"}, Description: "Withdraw exact pending", Condition: "queue \u00d7 hit target visible"},
			{ID: "nd.sidebar.mouse.agent.switch.agent", Keys: []string{"left click"}, Description: "Switch agent", Condition: "agent hit target visible"},
			{ID: "nd.sidebar.mouse.agent.agent.details", Keys: []string{"right click", "ctrl+left click"}, Description: "Agent details", Condition: "agent hit target visible"},
			{ID: "nd.sidebar.mouse.context.usage.context.details", Keys: []string{"left click"}, Description: "Context details", Condition: "context usage hit target visible"},
			{ID: "nd.sidebar.mouse.cost.usage.cost.details", Keys: []string{"left click"}, Description: "Cost details", Condition: "cost usage hit target visible"},
			{ID: "nd.sidebar.mouse.subagent.open.subagent.tab", Keys: []string{"left click"}, Description: "Open subagent tab", Condition: "subagent hit target visible"},
			{ID: "nd.sidebar.mouse.parent.switch.parent.tab", Keys: []string{"left click"}, Description: "Switch parent tab", Condition: "parent hit target visible"},
			{ID: "nd.sidebar.mouse.recap.branch.toggle.tree.branch", Keys: []string{"left click"}, Description: "Toggle tree branch", Condition: "recap/branch hit target visible"},
			{ID: "nd.sidebar.mouse.scroll.content.scroll.sidebar", Keys: []string{"wheel", "scrollbar drag", "scrollbar track click"}, Description: "Scroll sidebar", Condition: "scroll content hit target visible"},
		}},
		{ID: "mouse.transcript", Title: "Transcript \u2014 mouse", Entries: []help.Entry{
			{ID: "nd.transcript.mouse.toggle.block.expand.collapse.block", Keys: []string{"left click"}, Description: "Expand collapse block", Condition: "toggle block hit target visible"},
			{ID: "nd.transcript.mouse.edit.label.edit.persisted.user.message", Keys: []string{"left click"}, Description: "Edit persisted user message", Condition: "edit label hit target visible"},
			{ID: "nd.transcript.mouse.copy.label.copy.message", Keys: []string{"left click"}, Description: "Copy message", Condition: "copy label hit target visible"},
			{ID: "nd.transcript.mouse.code.copy.copy.code", Keys: []string{"left click"}, Description: "Copy code", Condition: "code copy hit target visible"},
			{ID: "nd.transcript.mouse.retry.label.retry.response", Keys: []string{"left click"}, Description: "Retry response", Condition: "retry label hit target visible"},
			{ID: "nd.transcript.mouse.url.open.url", Keys: []string{"left click"}, Description: "Open URL", Condition: "URL hit target visible"},
			{ID: "nd.transcript.mouse.body.select.word.and.auto.copy", Keys: []string{"double click"}, Description: "Select word and auto-copy", Condition: "body hit target visible"},
			{ID: "nd.transcript.mouse.body.select.line.and.auto.copy", Keys: []string{"triple click"}, Description: "Select line and auto-copy", Condition: "body hit target visible"},
			{ID: "nd.transcript.mouse.body.select.range.and.auto.copy", Keys: []string{"drag release"}, Description: "Select range and auto-copy", Condition: "body hit target visible"},
			{ID: "nd.transcript.mouse.input.parent.reference.switch.parent.tab", Keys: []string{"left click"}, Description: "Switch parent tab", Condition: "input parent reference hit target visible"},
			{ID: "nd.transcript.mouse.input.subagent.reference.open.subagent.tab", Keys: []string{"left click"}, Description: "Open subagent tab", Condition: "input subagent reference hit target visible"},
			{ID: "nd.transcript.mouse.scroll.area.scroll.transcript", Keys: []string{"wheel", "scrollbar drag", "scrollbar track click"}, Description: "Scroll transcript", Condition: "scroll area hit target visible"},
		}},
		{ID: "mouse.chrome", Title: "Application chrome \u2014 mouse", Entries: []help.Entry{
			{ID: "nd.chrome.mouse.contextbar.toggle.contextbar", Keys: []string{"left click"}, Description: "Toggle contextbar", Condition: "contextbar hit target visible"},
			{ID: "nd.chrome.mouse.attachment.contextbar.preview.attachment", Keys: []string{"left click"}, Description: "Preview attachment", Condition: "attachment contextbar hit target visible"},
			{ID: "nd.chrome.mouse.composer.focus.and.place.cursor", Keys: []string{"left click"}, Description: "Focus and place cursor", Condition: "composer hit target visible"},
			{ID: "nd.chrome.mouse.composer.divider.resize.editor", Keys: []string{"drag"}, Description: "Resize editor", Condition: "composer divider hit target visible"},
			{ID: "nd.chrome.mouse.messagebar.action.execute.action", Keys: []string{"left click"}, Description: "Execute action", Condition: "messagebar action hit target visible"},
			{ID: "nd.chrome.mouse.notification.dismiss.notification", Keys: []string{"click"}, Description: "Dismiss notification", Condition: "notification \u00d7 hit target visible"},
			{ID: "nd.chrome.mouse.notification.body.copy.notification", Keys: []string{"click"}, Description: "Copy notification", Condition: "notification body hit target visible"},
		}},
	}
}
