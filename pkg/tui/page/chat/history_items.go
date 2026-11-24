package chat

import (
	"errors"
	"reflect"
	"slices"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	list "github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type historyItemRequest struct {
	position int
	cursor   string
	witness  []session.BoundedItem
}

type historyItemMsg struct {
	history     *olderHistory
	position    int
	item        *session.Session
	media       map[int][]types.AssistantMedia
	requests    []generatedMediaRequest
	fallback    *runtime.SessionSnapshot
	fallbackErr error
	err         error
}

type historyItems struct {
	pending     []historyItemRequest
	removed     []int
	loading     bool
	fallback    *runtime.SessionSnapshot
	fallbackErr error
}

func (h *olderHistory) addOmissions(snapshot runtime.SessionSnapshot) {
	var positions []int
	positions = append(positions, snapshot.History.OmittedPositions...)
	positions = append(positions, snapshot.History.OmittedResponsePositions...)
	slices.Sort(positions)
	// Serve the most recent placeholders first.
	for _, pos := range slices.Backward(positions) {
		h.items.pending = append(h.items.pending, historyItemRequest{position: pos, cursor: snapshot.HistoryItemCursors[pos], witness: snapshot.Items})
	}
}

func (p *chatPage) loadHistoryItem() tea.Cmd {
	h := p.olderHistory
	if h == nil || h.items.loading || len(h.items.pending) == 0 || p.presentationHidden || p.replay != nil || p.app == nil {
		return nil
	}
	h.items.loading = true
	request := h.items.pending[0]
	h.items.pending[0] = historyItemRequest{}
	h.items.pending = h.items.pending[1:]
	id := h.sessionID
	anchor := runtime.SessionSnapshot{Epoch: h.epoch, Cursor: h.cursor, TranscriptPosition: h.anchor}
	fallback, fallbackErr := h.items.fallback, h.items.fallbackErr
	var parentID string
	if sess := p.app.Session(); sess != nil {
		parentID = sess.ParentID
	}
	canResolve := p.app.CanResolveGeneratedFiles()
	run := p.historyItemCommand(h, request, anchor, fallback, fallbackErr, id, parentID, canResolve)
	// Let the placeholder's frame reach the terminal before starting I/O.
	return p.replayCommand(tea.Tick(16*time.Millisecond, func(time.Time) tea.Msg {
		return historyItemStartMsg{history: h, run: run}
	}))
}

type historyItemStartMsg struct {
	history *olderHistory
	run     tea.Cmd
}

func (p *chatPage) historyItemCommand(h *olderHistory, request historyItemRequest, anchor runtime.SessionSnapshot, fallback *runtime.SessionSnapshot, fallbackErr error, id, parentID string, canResolve bool) tea.Cmd {
	app, ctx := p.app, p.ctx()
	return func() tea.Msg {
		msg := historyItemMsg{history: h, position: request.position, fallback: fallback, fallbackErr: fallbackErr}
		item, err := app.ReadSessionHistoryItem(ctx, id, request.cursor, anchor, request.position)
		if errors.Is(err, runtime.ErrUnsupported) || errors.Is(err, session.ErrHistoryItemTooLarge) {
			if msg.fallback == nil && msg.fallbackErr == nil {
				snapshot, err := app.ReadSessionHistoryFallback(ctx, id)
				if err != nil {
					msg.fallbackErr = err
				} else {
					msg.fallback = &snapshot
				}
			}
			item, msg.err = legacyHistoryItem(anchor, request, msg.fallback, msg.fallbackErr)
			if msg.err != nil && errors.Is(err, session.ErrHistoryItemTooLarge) {
				msg.err = errors.Join(err, msg.err)
			}
		} else {
			msg.err = err
		}
		if msg.err == nil {
			msg.item = session.New(session.WithID(id))
			msg.item.ParentID = parentID
			msg.item.Messages = []session.Item{item}
			if canResolve {
				msg.media, msg.requests = collectGeneratedMedia(msg.item, nil)
			}
		}
		return msg
	}
}

func legacyHistoryItem(anchor runtime.SessionSnapshot, request historyItemRequest, fallback *runtime.SessionSnapshot, fallbackErr error) (session.Item, error) {
	if fallbackErr != nil {
		return session.Item{}, fallbackErr
	}
	if fallback == nil || fallback.Epoch != anchor.Epoch || fallback.Cursor < anchor.Cursor || fallback.TranscriptPosition < anchor.TranscriptPosition ||
		fallback.Session.ItemCount() < anchor.TranscriptPosition || request.position < 0 || request.position >= anchor.TranscriptPosition ||
		(len(request.witness) == 0 && fallback.Cursor != anchor.Cursor) {
		return session.Item{}, runtime.ErrHistoryCursorStale
	}
	for _, witness := range request.witness {
		if !reflect.DeepEqual(witness.Item, fallback.Session.Messages[witness.Position]) {
			return session.Item{}, runtime.ErrHistoryCursorStale
		}
	}
	return fallback.Session.Messages[request.position], nil
}

// A pending-input withdrawal shifts presentation positions, but not the
// position signed into outstanding requests.
func (h *historyItems) visiblePosition(position int) int {
	for _, removed := range h.removed {
		if position == removed {
			return -1
		}
		if position > removed {
			position--
		}
	}
	return position
}

func (p *chatPage) applyHistoryItem(msg historyItemMsg) tea.Cmd {
	h := msg.history
	if p.olderHistory != h {
		return nil
	}
	h.items.loading = false
	h.items.fallback, h.items.fallbackErr = msg.fallback, msg.fallbackErr
	position := h.items.visiblePosition(msg.position)
	if position < 0 {
		return p.loadHistoryItem()
	}
	if msg.err != nil {
		reason := msg.err.Error()
		if errors.Is(msg.err, runtime.ErrHistoryCursorStale) {
			reason = "history changed; reopen the session to reload the item"
		}
		return tea.Batch(list.ReplaceHistoryItem(p.messages, position, nil, nil, nil, "Large transcript item unavailable: "+reason), p.loadHistoryItem())
	}
	item := msg.item.Messages[0]
	if item.Message != nil && !item.Message.Pending {
		p.inputReplay.Consume(item.Message.TurnID, position)
	}
	cmd := list.ReplaceHistoryItem(p.messages, position, msg.item, h.toolResults, msg.media, "")
	list.AddHistoryToolResults(h.toolResults, msg.item.Messages)
	return tea.Batch(cmd, p.resolveGeneratedMediaCmd(msg.requests), p.loadHistoryItem())
}
