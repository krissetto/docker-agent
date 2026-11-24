package chat

import (
	"errors"
	"log/slog"
	"slices"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	list "github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type olderHistory struct {
	sessionID, epoch, next string
	cursor                 uint64
	anchor, before         int
	loading                bool
	toolResults            map[string]string
	items                  historyItems
}

type olderHistoryPageMsg struct {
	history  *olderHistory
	page     runtime.SessionSnapshot
	prefix   *session.Session
	media    map[int][]types.AssistantMedia
	requests []generatedMediaRequest
	err      error
}

func (p *chatPage) setOlderHistory(snapshot runtime.SessionSnapshot) {
	p.olderHistory = nil
	list.SetOlderHistoryAvailable(p.messages, false)
	if snapshot.History == nil || snapshot.Session == nil {
		return
	}
	p.olderHistory = &olderHistory{
		sessionID: snapshot.Session.ID, epoch: snapshot.Epoch, next: snapshot.HistoryCursor,
		cursor: snapshot.Cursor, anchor: snapshot.TranscriptPosition, before: snapshot.History.StartPosition, toolResults: make(map[string]string),
	}
	list.AddHistoryToolResults(p.olderHistory.toolResults, snapshot.Session.Messages)
	p.olderHistory.addOmissions(snapshot)
	list.SetOlderHistoryAvailable(p.messages, snapshot.HistoryCursor != "")
}

func (p *chatPage) loadOlderHistory() tea.Cmd {
	h := p.olderHistory
	if h == nil || h.loading || h.next == "" || p.presentationHidden || p.replay != nil || p.editing {
		return nil
	}
	h.loading = true
	ctx, application := p.ctx(), p.app
	canResolve := application.CanResolveGeneratedFiles()
	id, cursor, before := h.sessionID, h.next, h.before
	prefix := session.New(session.WithID(id))
	if current := application.Session(); current != nil {
		prefix.ParentID = current.ParentID
	}
	return p.replayCommand(func() tea.Msg {
		started := time.Now()
		page, err := application.ReadOlderSessionHistory(ctx, id, cursor)
		slog.DebugContext(ctx, "Session presentation timing", "phase", "load_older_page", "elapsed", time.Since(started), "items", len(page.Items), "ok", err == nil)
		result := olderHistoryPageMsg{history: h, page: page, prefix: prefix, err: err}
		if err == nil && page.History != nil {
			prefix.Messages = app.HistoryItems(page, before)
			prefix.TurnTimings = page.History.TurnTimings
			prefix.TurnOutcomes = page.History.TurnOutcomes
			if canResolve {
				result.media, result.requests = collectGeneratedMedia(prefix, nil)
			}
		}
		return result
	})
}

func (p *chatPage) applyOlderHistory(msg olderHistoryPageMsg) tea.Cmd {
	h := msg.history
	if p.olderHistory != h {
		return nil
	}
	h.loading = false
	if len(h.items.removed) != 0 {
		msg.err = runtime.ErrHistoryCursorStale
	}
	if msg.err != nil {
		if errors.Is(msg.err, runtime.ErrHistoryCursorStale) {
			p.olderHistory = nil
			list.SetOlderHistoryAvailable(p.messages, false)
			return tea.Batch(notification.WarningCmd("History changed; reloading the recent conversation"), p.refreshHistory())
		}
		return notification.ErrorCmd("Could not load older history: " + msg.err.Error())
	}
	page := msg.page
	// The first tail is authoritative for live events; paging never moves that
	// barrier. Older servers echoed the original anchor rather than page end.
	if page.History == nil || page.Epoch != h.epoch || page.Cursor != h.cursor || page.TranscriptPosition != h.anchor ||
		(page.History.BeforePosition != h.before && page.History.BeforePosition != h.anchor) || page.History.StartPosition >= h.before ||
		len(msg.prefix.Messages) != h.before-page.History.StartPosition || page.History.ScannedPositions != len(msg.prefix.Messages) || page.HistoryCursor == h.next ||
		slices.ContainsFunc(page.Items, func(item session.BoundedItem) bool { return item.Position >= h.before }) {
		h.next = ""
		list.SetOlderHistoryAvailable(p.messages, false)
		return notification.ErrorCmd("Could not load older history: invalid history boundary")
	}
	h.before, h.next = page.History.StartPosition, page.HistoryCursor
	for i, item := range msg.prefix.Messages {
		if item.Message != nil && !item.Message.Pending {
			p.inputReplay.Consume(item.Message.TurnID, h.before+i)
		}
	}
	cmd := list.PrependHistory(p.messages, msg.prefix, h.before, h.toolResults, msg.media)
	list.AddHistoryToolResults(h.toolResults, msg.prefix.Messages)
	list.SetOlderHistoryAvailable(p.messages, h.next != "")
	h.addOmissions(page)
	return tea.Batch(cmd, p.resolveGeneratedMediaCmd(msg.requests), p.loadHistoryItem())
}

func (p *chatPage) refreshHistory() tea.Cmd {
	application := p.app
	return func() tea.Msg {
		application.RefreshSessionHistory()
		return nil
	}
}
