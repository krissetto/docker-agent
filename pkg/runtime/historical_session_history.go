package runtime

import (
	"context"
	"errors"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/session"
)

func (h *historicalSessionHandle) readHistory(ctx context.Context, before, total int, storageBefore int64, limits SessionHistoryLimits, fits func(session.BoundedItemPage) bool) (session.BoundedItemPage, error) {
	if h.history != nil {
		page, err := h.history.ReadViewHistoryPage(ctx, h.id, total, before, limits.MaxItems, limits.MaxItemBytes, storageBefore, func(page session.BoundedItemPage) bool {
			h.snapshot.SetHistoryTurnMetadata(&page)
			return fits(page)
		})
		h.snapshot.SetHistoryTurnMetadata(&page)
		return page, err
	}
	return h.snapshot.ReadBoundedItemsWithBudget(before, limits.MaxItems, limits.MaxItemBytes, fits)
}

func (h *historicalSessionHandle) historyPosition(ctx context.Context) (int, error) {
	if h.history == nil {
		return h.snapshot.ItemCount(), nil
	}
	total, _, err := h.history.ReadViewHistoryState(ctx, h.id)
	return total, err
}

func (h *historicalSessionHandle) tailSnapshot(ctx context.Context, rawLimits SessionHistoryLimits) (SessionSnapshot, error) {
	limits, err := NormalizeHistoryLimits(rawLimits)
	if err != nil {
		return SessionSnapshot{}, err
	}
	total, err := h.historyPosition(ctx)
	if err != nil {
		return SessionSnapshot{}, err
	}
	header, err := h.snapshot.HistoryHeaderSnapshot(limits.MaxBytes)
	if err != nil {
		return SessionSnapshot{}, ErrHistoryMetadataTooLarge
	}
	project := func(page session.BoundedItemPage) SessionSnapshot {
		out := snapshotFromHistoryPage(h.id, "historical", 0, total, page, h.snapshot.HistoryRevisionSnapshot())
		out.Session = header
		out.Session.TurnTimings, out.Session.TurnOutcomes = page.TurnTimings, page.TurnOutcomes
		out.Status = SessionStatus{SessionID: h.id, AgentName: h.metadata.AgentName, State: SessionStateSettled}
		return out
	}
	page, err := h.readHistory(ctx, total, total, 0, limits, func(page session.BoundedItemPage) bool { return historyPageFits(project(page), limits.MaxBytes) })
	if err != nil {
		return SessionSnapshot{}, err
	}
	out := project(page)
	if !historyPageFits(out, limits.MaxBytes) {
		return SessionSnapshot{}, ErrHistoryMetadataTooLarge
	}
	return out, nil
}

func (h *historicalSessionHandle) ReadHistoryPage(ctx context.Context, sessionID, cursor string, rawLimits SessionHistoryLimits) (SessionSnapshot, error) {
	limits, err := NormalizeHistoryLimits(rawLimits)
	if err != nil {
		return SessionSnapshot{}, err
	}
	var out SessionSnapshot
	err = h.ReadHistoryPageBounded(ctx, sessionID, cursor, limits, func(page SessionSnapshot) bool {
		out = page
		return sessionHistoryPageFits(page, limits.MaxBytes)
	})
	return out, err
}

func (h *historicalSessionHandle) ReadHistoryPageBounded(ctx context.Context, sessionID, rawCursor string, rawLimits SessionHistoryLimits, accept func(SessionSnapshot) bool) error {
	limits, err := NormalizeHistoryLimits(rawLimits)
	if err != nil {
		return err
	}
	if accept == nil {
		return errors.New("history page acceptance callback is required")
	}
	cursor, err := decodeHistoryCursor(rawCursor, historyPageCursorVersion)
	if err != nil || sessionID != h.id || cursor.SessionID != h.id {
		return ErrHistoryCursorInvalid
	}
	total, err := h.historyPosition(ctx)
	if err != nil {
		return err
	}
	if cursor.Epoch != "historical" || cursor.AnchorPosition != total || cursor.HistoryRevision != h.snapshot.HistoryRevisionSnapshot() {
		return ErrHistoryCursorStale
	}
	project := func(page session.BoundedItemPage) SessionSnapshot {
		out := snapshotFromHistoryPage(h.id, cursor.Epoch, cursor.AnchorCursor, total, page, cursor.HistoryRevision)
		out.Session = h.snapshot.MetadataSnapshot()
		return out
	}
	page, err := h.readHistory(ctx, cursor.BeforePosition, total, cursor.StorageBefore, limits, func(page session.BoundedItemPage) bool { return accept(project(page)) })
	if err != nil {
		return err
	}
	if !accept(project(page)) {
		return ErrHistoryMetadataTooLarge
	}
	return nil
}

func (h *historicalSessionHandle) ReadHistoryItem(ctx context.Context, sessionID, rawCursor string) (api.SessionHistoryItemPage, error) {
	cursor, err := readHistoryItemCursor(rawCursor, sessionID, h.id)
	if err != nil {
		return api.SessionHistoryItemPage{}, err
	}
	if err := ctx.Err(); err != nil {
		return api.SessionHistoryItemPage{}, err
	}
	if cursor.Epoch != "historical" || cursor.HistoryRevision != h.snapshot.HistoryRevisionSnapshot() {
		return api.SessionHistoryItemPage{}, ErrHistoryCursorStale
	}
	var item session.Item
	if h.history != nil {
		item, err = h.history.ReadViewHistoryItem(ctx, h.id, cursor.AnchorPosition, cursor.ItemPosition, cursor.ItemStorageKey, api.SessionHistoryItemMaxBytes)
	} else {
		if cursor.AnchorPosition != h.snapshot.ItemCount() {
			return api.SessionHistoryItemPage{}, ErrHistoryCursorStale
		}
		item, err = h.snapshot.ReadHistoryItem(cursor.ItemPosition)
	}
	return historyItemPage(cursor, item, err)
}

var _ SessionHistoryReader = (*historicalSessionHandle)(nil)
var _ SessionHistoryItemReader = (*historicalSessionHandle)(nil)
