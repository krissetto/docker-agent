package chat

import (
	"slices"

	tea "charm.land/bubbletea/v2"

	chatmsg "github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func (p *chatPage) setBackgroundProcesses(sessionID string, processes []tools.BackgroundProcess) tea.Cmd {
	if sessionID == "" || p.app == nil {
		return nil
	}
	sess := p.app.Session()
	if sess == nil || sess.ID != sessionID {
		return nil
	}
	if len(processes) == 0 {
		return p.clearBackgroundProcesses(sess)
	}
	normalized := make([]tools.BackgroundProcess, 0, len(processes))
	for _, process := range processes {
		if process.SessionID == sess.ID {
			normalized = append(normalized, process)
		}
	}
	if len(normalized) == 0 {
		return p.clearBackgroundProcesses(sess)
	}
	if p.backgroundLinkCacheValid && p.backgroundLinkSession == sess && p.backgroundLinkItemCount == sess.ItemCount() && p.backgroundLinkRevision == sess.HistoryRevisionSnapshot() && slices.Equal(p.backgroundLinkProcesses, normalized) {
		if p.backgroundSidebarSeeded {
			return p.syncShellBackgroundProcesses(p.authoritativeBackgroundProcesses())
		}
		p.backgroundSidebarSeeded = true
		return p.sidebar.SetBackgroundProcesses(sess.ID, p.authoritativeBackgroundProcesses())
	}
	p.backgroundProcesses = normalized
	return p.refreshBackgroundProcessLinks()
}

func (p *chatPage) clearBackgroundProcesses(sess *session.Session) tea.Cmd {
	p.backgroundProcesses = nil
	p.backgroundProcessToolCalls = nil
	p.backgroundLinkToolCallIDs = nil
	p.backgroundLinkToolCallCounts = nil
	p.backgroundLinkSeenPositions = nil
	p.backgroundLinkSession = sess
	p.backgroundLinkItemCount = sess.ItemCount()
	p.backgroundLinkRevision = sess.HistoryRevisionSnapshot()
	p.backgroundLinkProcesses = nil
	p.backgroundLinkCacheValid = true
	p.backgroundSidebarSeeded = true
	return tea.Batch(p.sidebar.SetBackgroundProcesses(sess.ID, nil), p.syncShellBackgroundProcesses(nil))
}

func (p *chatPage) refreshBackgroundProcessLinks() tea.Cmd {
	if p.app == nil {
		return nil
	}
	sess := p.app.Session()
	if sess == nil {
		p.backgroundProcessToolCalls = nil
		p.backgroundLinkToolCallIDs = nil
		p.backgroundLinkCacheValid = false
		return nil
	}
	if len(p.backgroundProcesses) == 0 {
		return nil
	}
	if p.backgroundLinkCacheValid && p.backgroundLinkSession == sess && p.backgroundLinkItemCount == sess.ItemCount() && p.backgroundLinkRevision == sess.HistoryRevisionSnapshot() {
		return p.reprojectBackgroundProcessLinks()
	}
	items := sess.ItemsSnapshot()
	transcriptCounts := transcriptToolCallCounts(items)
	for _, ids := range p.backgroundRemoteToolCalls {
		for _, id := range ids {
			transcriptCounts[id]++
		}
	}
	transcriptIDs := uniqueTranscriptToolCallIDs(transcriptCounts)
	p.backgroundLinkToolCallCounts = transcriptCounts
	p.backgroundLinkToolCallIDs = transcriptIDs
	p.backgroundLinkSeenPositions = make(map[int]struct{})
	p.backgroundLinkSession = sess
	p.backgroundLinkItemCount = len(items)
	p.backgroundLinkRevision = sess.HistoryRevisionSnapshot()
	return p.reprojectBackgroundProcessLinks()
}

func transcriptToolCallCounts(items []session.Item) map[string]int {
	counts := make(map[string]int)
	for _, item := range items {
		if !item.IsMessage() || item.Message.Message.Role != chatmsg.MessageRoleAssistant {
			continue
		}
		for _, call := range item.Message.Message.ToolCalls {
			if call.ID != "" {
				counts[call.ID]++
			}
		}
	}
	return counts
}

func uniqueTranscriptToolCallIDs(counts map[string]int) map[string]struct{} {
	unique := make(map[string]struct{}, len(counts))
	for id, count := range counts {
		if count == 1 {
			unique[id] = struct{}{}
		}
	}
	return unique
}

func (p *chatPage) commitBackgroundToolCalls(event *runtime.MessageAddedEvent) tea.Cmd {
	if event == nil || p.app == nil {
		return nil
	}
	sess := p.app.Session()
	if sess == nil || event.SessionID != sess.ID {
		return nil
	}
	// Remote commits carry call identities, not the committed message body.
	if event.Message == nil && event.SessionPosition >= 0 {
		ids := event.CommittedToolCallIDs()
		if len(ids) == 0 {
			return nil
		}
		if p.backgroundRemoteToolCalls == nil {
			p.backgroundRemoteToolCalls = make(map[int][]string)
		}
		if _, seen := p.backgroundRemoteToolCalls[event.SessionPosition]; seen {
			return nil
		}
		p.backgroundRemoteToolCalls[event.SessionPosition] = slices.Clone(ids)
		p.backgroundLinkCacheValid = false
		return p.refreshBackgroundProcessLinks()
	}
	if len(p.backgroundProcesses) == 0 {
		return nil
	}
	if !p.backgroundLinkCacheValid || p.backgroundLinkSession != sess || p.backgroundLinkRevision != sess.HistoryRevisionSnapshot() {
		p.backgroundLinkCacheValid = false
		return p.refreshBackgroundProcessLinks()
	}
	if event.SessionPosition >= 0 {
		if _, seen := p.backgroundLinkSeenPositions[event.SessionPosition]; seen {
			return nil
		}
		if event.SessionPosition != p.backgroundLinkItemCount || sess.ItemCount() != p.backgroundLinkItemCount+1 {
			p.backgroundLinkCacheValid = false
			return p.refreshBackgroundProcessLinks()
		}
		p.backgroundLinkSeenPositions[event.SessionPosition] = struct{}{}
	} else {
		p.backgroundLinkCacheValid = false
		return p.refreshBackgroundProcessLinks()
	}
	for _, id := range event.CommittedToolCallIDs() {
		if id != "" {
			p.backgroundLinkToolCallCounts[id]++
		}
	}
	p.backgroundLinkToolCallIDs = uniqueTranscriptToolCallIDs(p.backgroundLinkToolCallCounts)
	p.backgroundLinkItemCount = sess.ItemCount()
	p.backgroundLinkRevision = sess.HistoryRevisionSnapshot()
	return p.reprojectBackgroundProcessLinks()
}

func (p *chatPage) reprojectBackgroundProcessLinks() tea.Cmd {
	sess := p.app.Session()
	if sess == nil {
		p.backgroundLinkCacheValid = false
		return nil
	}
	visible := slices.Clone(p.backgroundProcesses)
	safe := make(map[string]struct{}, len(visible))
	for i := range visible {
		process := &visible[i]
		if _, unique := p.backgroundLinkToolCallIDs[process.ToolCallID]; process.ToolCallID == "" || process.SessionID != sess.ID || p.messages.HasAmbiguousShellBackgroundProcess(process.ID) || !unique || !p.messages.HasUniqueToolCall(process.ToolCallID) {
			process.ToolCallID = ""
			continue
		}
		safe[process.ToolCallID] = struct{}{}
	}
	p.backgroundProcessToolCalls = safe
	p.backgroundLinkProcesses = slices.Clone(p.backgroundProcesses)
	p.backgroundLinkCacheValid = true
	p.backgroundSidebarSeeded = true
	return tea.Batch(p.sidebar.SetBackgroundProcesses(sess.ID, visible), p.syncShellBackgroundProcesses(visible))
}

func (p *chatPage) authoritativeBackgroundProcesses() []tools.BackgroundProcess {
	sess := p.app.Session()
	if sess == nil || len(p.backgroundProcesses) == 0 {
		return nil
	}
	out := slices.Clone(p.backgroundProcesses)
	for i := range out {
		if _, safe := p.backgroundProcessToolCalls[out[i].ToolCallID]; !safe {
			out[i].ToolCallID = ""
		}
	}
	return out
}

func (p *chatPage) navigateToBackgroundProcess(toolCallID string) bool {
	if toolCallID == "" {
		return false
	}
	if _, ok := p.backgroundProcessToolCalls[toolCallID]; !ok {
		return false
	}
	sess := p.app.Session()
	if sess == nil || sess != p.backgroundLinkSession || sess.ItemCount() != p.backgroundLinkItemCount || sess.HistoryRevisionSnapshot() != p.backgroundLinkRevision {
		return false
	}
	if _, ok := p.backgroundLinkToolCallIDs[toolCallID]; !ok || !p.messages.HasUniqueToolCall(toolCallID) {
		return false
	}
	return p.messages.NavigateToToolCall(toolCallID)
}

func (p *chatPage) syncShellBackgroundProcesses(processes []tools.BackgroundProcess) tea.Cmd {
	sess := p.app.Session()
	if sess == nil {
		return nil
	}
	// Capture the immutable handle: an async fetch must not follow a tab switch.
	source, _ := p.app.SessionHandle().(tools.BackgroundProcessOutputSource)
	var fetch func(string) (tools.BackgroundProcessOutput, error)
	if source != nil {
		ctx := p.ctx()
		fetch = func(id string) (tools.BackgroundProcessOutput, error) {
			return source.BackgroundProcessOutput(ctx, id, 100_000)
		}
	}
	return tea.Batch(p.messages.SetShellBackgroundOutputInterest(sess.ID, p.app.BackgroundProcessOutputInterestSetter()), p.messages.SetShellBackgroundProcesses(sess.ID, processes, fetch))
}
