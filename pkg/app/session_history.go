package app

import (
	"context"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

const selectedHistoryItems = 50

var selectedHistoryLimits = runtime.SessionHistoryLimits{MaxItems: selectedHistoryItems}

func (a *App) SelectedSessionHistory() bool {
	transport, ok := a.sessions.(*runtime.SessionTransport)
	return ok && transport.SelectedSessionHistory()
}

func (a *App) StartSessionTreeWatch() {
	a.bridgeMu.Lock()
	defer a.bridgeMu.Unlock()
	if a.startTreeWatch != nil {
		a.startTreeWatch()
	}
}

func (a *App) RefreshSessionHistory() { a.startSessionEventBridge(a.ctx()) }

func (a *App) ReadOlderSessionHistory(ctx context.Context, id, cursor string) (runtime.SessionSnapshot, error) {
	if transport, ok := a.sessions.(*runtime.SessionTransport); ok && transport.SelectedSessionHistory() {
		return transport.ReadOlderHistory(ctx, id, cursor, selectedHistoryLimits)
	}
	return runtime.SessionSnapshot{}, runtime.ErrUnsupported
}

func (a *App) ReadSessionHistoryItem(ctx context.Context, id, cursor string, anchor runtime.SessionSnapshot, position int) (session.Item, error) {
	if transport, ok := a.sessions.(*runtime.SessionTransport); ok && transport.SelectedSessionHistory() {
		return transport.ReadHistoryItem(ctx, id, cursor, anchor, position)
	}
	return session.Item{}, runtime.ErrUnsupported
}

func (a *App) ReadSessionHistoryFallback(ctx context.Context, id string) (runtime.SessionSnapshot, error) {
	if transport, ok := a.sessions.(*runtime.SessionTransport); ok && transport.SelectedSessionHistory() {
		return transport.ReadHistoryFallback(ctx, id)
	}
	return runtime.SessionSnapshot{}, runtime.ErrUnsupported
}

// HistoryItems translates absolute history positions into a bounded contiguous
// presentation slice, with explicit placeholders for oversized items. Older
// peers reported the anchor as end_position even for pages ending before it.
func HistoryItems(snapshot runtime.SessionSnapshot, before int) []session.Item {
	start := snapshot.History.StartPosition
	if start > before || before-start > selectedHistoryItems {
		return nil
	}
	items := make([]session.Item, before-start)
	for _, item := range snapshot.Items {
		if item.Position >= start && item.Position < before {
			items[item.Position-start] = item.Item
		}
	}
	for _, positions := range [][]int{snapshot.History.OmittedPositions, snapshot.History.OmittedResponsePositions} {
		for _, pos := range positions {
			if pos >= start && pos < before {
				items[pos-start] = session.Item{Error: &session.Error{Code: lifecycle.HistoryPlaceholderCode, Message: "Loading large transcript item…"}}
			}
		}
	}
	return items
}

// FullSessionSnapshot is for explicit operations which need canonical items,
// such as branch proof generation or transcript export, not display.
func (a *App) FullSessionSnapshot(ctx context.Context) (*session.Session, error) {
	return fullSessionSnapshot(ctx, a.state(), a.SelectedSessionHistory())
}

func fullSessionSnapshot(ctx context.Context, state sessionState, selectedHistory bool) (*session.Session, error) {
	if selectedHistory && state.handle != nil {
		return state.handle.Snapshot(ctx)
	}
	return state.session, nil
}
