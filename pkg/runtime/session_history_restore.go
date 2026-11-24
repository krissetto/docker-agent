package runtime

import (
	"context"

	"github.com/docker/docker-agent/pkg/session"
)

type deferredSessionHistory struct {
	store   session.ViewHistoryStore
	total   int
	pending []QueuedMessage // used only when constructing the dormant owner
	load    chan struct{}   // one reader loads the full history when required
}

func (d *sessionDriver) historyPositionLocked() int {
	if d.history != nil {
		return d.history.total
	}
	return d.sess.ItemCount()
}

func (d *sessionDriver) readHistoryLocked(ctx context.Context, before int, storageBefore int64, limits SessionHistoryLimits, fits func(session.BoundedItemPage) bool) (session.BoundedItemPage, error) {
	if d.history != nil {
		page, err := d.history.store.ReadViewHistoryPage(ctx, d.identityID, d.history.total, before, limits.MaxItems, limits.MaxItemBytes, storageBefore, func(page session.BoundedItemPage) bool {
			d.sess.SetHistoryTurnMetadata(&page)
			return fits(page)
		})
		d.sess.SetHistoryTurnMetadata(&page)
		return page, err
	}
	return d.sess.ReadBoundedItemsWithBudget(before, limits.MaxItems, limits.MaxItemBytes, fits)
}

func (d *sessionDriver) ensureHistory(ctx context.Context) error {
	for {
		d.mu.Lock()
		history := d.history
		if history == nil {
			d.mu.Unlock()
			return nil
		}
		if load := history.load; load != nil {
			d.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-load:
				continue
			}
		}
		loading := make(chan struct{})
		history.load = loading
		d.mu.Unlock()

		stored, err := d.r.sessionStore.GetSession(ctx, d.identityID)
		if err == nil && stored.ItemCount() != history.total {
			err = &SessionError{Kind: SessionErrorConflict, SessionID: d.identityID, Operation: "load_history"}
		}
		if err == nil {
			err = d.ownerCall(ctx, func() error {
				if d.sess == nil || d.reclaiming {
					return ErrSessionClosed
				}
				if d.history != history {
					return nil // A complete replacement won while the read was in flight.
				}
				// Preserve owner metadata and event/cursor identity. The durable
				// prefix has not changed, so outstanding page cursors remain valid.
				d.sess.RestoreHistoryItems(stored.Messages)
				d.history = nil
				return nil
			})
		}
		d.mu.Lock()
		history.load = nil
		close(loading)
		d.mu.Unlock()
		return err
	}
}

func (d *sessionDriver) viewSnapshot(ctx context.Context, metadataOnly bool) (*session.Session, error) {
	if !metadataOnly {
		return d.ownerSnapshot(ctx)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sess.MetadataSnapshot(), nil
}
