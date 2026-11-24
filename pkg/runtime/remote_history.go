package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/session"
)

// WithSelectedSessionHistory enables bounded presentation for the managed tabbed client.
func WithSelectedSessionHistory() SessionTransportOption {
	return func(r *SessionTransport) { r.selectedSessionHistory = true }
}

func (r *SessionTransport) SelectedSessionHistory() bool { return r.selectedSessionHistory }

func historyQuery(in SessionHistoryLimits) (url.Values, error) {
	limits, err := NormalizeHistoryLimits(in)
	if err != nil {
		return nil, err
	}
	return url.Values{
		"projection":     {api.SessionHistoryProjection},
		"max_items":      {strconv.Itoa(limits.MaxItems)},
		"max_bytes":      {strconv.Itoa(limits.MaxBytes)},
		"max_item_bytes": {strconv.Itoa(limits.MaxItemBytes)},
	}, nil
}

func unsupportedObservationProjection(err error) bool {
	var httpErr *sessionHTTPError
	return errors.Is(err, ErrUnsupported) || (errors.As(err, &httpErr) && (httpErr.status == http.StatusBadRequest || httpErr.status == http.StatusNotImplemented || httpErr.status == http.StatusRequestEntityTooLarge))
}

func (s *remoteSession) ObserveTreeMetadata(ctx context.Context) (Observation, error) {
	options := ObserveOptions{Tree: true, OrderedTree: true}
	observation, err := s.runtime.client.attachSessionProjection(ctx, s.ID(), options, api.SessionViewMetadataProjection)
	if unsupportedObservationProjection(err) {
		return s.Observe(ctx, options)
	}
	return observation, err
}

// ReadOlderHistory retrieves one page pinned to the original observation anchor.
func (r *SessionTransport) ReadOlderHistory(ctx context.Context, id, cursor string, limits SessionHistoryLimits) (SessionSnapshot, error) {
	query, err := historyQuery(limits)
	if err != nil {
		return SessionSnapshot{}, err
	}
	query.Set("cursor", cursor)
	var page api.SessionHistoryPage
	endpoint := api.SessionAPIPath + "/" + url.PathEscape(id) + "/history?" + query.Encode()
	if err := r.client.sessionJSON(ctx, http.MethodGet, endpoint, nil, &page); err != nil {
		return SessionSnapshot{}, err
	}
	if page.SessionID != id || page.Projection != api.SessionHistoryProjection || page.Epoch == "" {
		return SessionSnapshot{}, protocolError(errors.New("invalid session history page identity"))
	}
	out := SessionSnapshot{Projection: page.Projection, Epoch: page.Epoch, Cursor: page.Cursor, TranscriptPosition: page.TranscriptPosition}
	if err := decodeHistoryPage(&out, &page.History, page.Items); err != nil {
		return SessionSnapshot{}, protocolError(err)
	}
	out.History.TurnTimings, out.History.TurnOutcomes = page.TurnTimings, page.TurnOutcomes
	return out, nil
}

func decodeHistoryPage(out *SessionSnapshot, history *api.SessionHistory, items []api.SessionHistoryItem) error {
	if history == nil || out.Epoch == "" || history.StartPosition < 0 || history.EndPosition < history.StartPosition || history.EndPosition > out.TranscriptPosition || history.TotalItems != out.TranscriptPosition || history.Truncated != (history.StartPosition > 0) || history.Truncated != (history.NextCursor != "") || len(items) > maxHistoryItems {
		return errors.New("invalid session history bounds")
	}
	page := &session.BoundedItemPage{StartPosition: history.StartPosition, BeforePosition: history.EndPosition, NextBeforePosition: history.StartPosition, TotalItems: history.TotalItems, HasMore: history.Truncated}
	seen := make(map[int]bool, len(items))
	prev := history.StartPosition - 1
	for _, item := range items {
		if item.Position <= prev || item.Position >= history.EndPosition {
			return errors.New("invalid session history item position")
		}
		page.Items = append(page.Items, session.BoundedItem{Position: item.Position, Item: item.Item})
		prev = item.Position
		seen[prev] = true
	}
	for _, omitted := range history.OmittedRanges {
		if omitted.StartPosition < history.StartPosition || omitted.EndPosition <= omitted.StartPosition || omitted.EndPosition > history.EndPosition || omitted.EndPosition-omitted.StartPosition > maxHistoryItems {
			return errors.New("invalid session history omission")
		}
		for pos := omitted.StartPosition; pos < omitted.EndPosition; pos++ {
			if seen[pos] || len(seen) >= maxHistoryItems {
				return errors.New("overlapping or excessive session history omissions")
			}
			seen[pos] = true
			if omitted.Reason == historyOmittedResponseByteLimit {
				page.OmittedResponsePositions = append(page.OmittedResponsePositions, pos)
			} else {
				page.OmittedPositions = append(page.OmittedPositions, pos)
			}
		}
	}
	// Older history endpoints echoed the original anchor as their end. The
	// reported positions still identify the actual contiguous scanned range.
	actualEnd := page.StartPosition + len(seen)
	if actualEnd > page.BeforePosition || (actualEnd != page.BeforePosition && page.BeforePosition != out.TranscriptPosition) {
		return errors.New("incomplete session history page")
	}
	for pos := page.StartPosition; pos < actualEnd; pos++ {
		if !seen[pos] {
			return errors.New("gap in session history page")
		}
	}
	page.ScannedPositions = len(seen)
	out.Items, out.History, out.HistoryCursor = page.Items, page, history.NextCursor
	return nil
}
