package runtime

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/session"
)

const (
	defaultHistoryItems     = 50
	maxHistoryItems         = 100
	defaultHistoryBytes     = 2 << 20
	maxHistoryBytes         = 4 << 20
	defaultHistoryItemBytes = 128 << 10
	maxHistoryItemBytes     = 256 << 10
)

var historyCursorMACKey = func() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic("cannot initialize history cursor key")
	}
	return key
}()

var ErrHistoryCursorStale = errors.New("history_cursor_stale")
var ErrHistoryCursorInvalid = errors.New("invalid history cursor")
var ErrHistoryMetadataTooLarge = errors.New("history metadata exceeds response limit")

type historyCursor struct {
	Version         int    `json:"v"`
	SessionID       string `json:"s"`
	Epoch           string `json:"e"`
	AnchorCursor    uint64 `json:"c"`
	AnchorPosition  int    `json:"a"`
	BeforePosition  int    `json:"b"`
	HistoryRevision uint64 `json:"r"`
	StorageBefore   int64  `json:"k,omitempty"`
	ItemPosition    int    `json:"p,omitempty"`
	ItemStorageKey  int64  `json:"i,omitempty"`
}

func NormalizeHistoryLimits(in SessionHistoryLimits) (SessionHistoryLimits, error) {
	if in.MaxItems == 0 {
		in.MaxItems = defaultHistoryItems
	}
	if in.MaxBytes == 0 {
		in.MaxBytes = defaultHistoryBytes
	}
	if in.MaxItemBytes == 0 {
		in.MaxItemBytes = defaultHistoryItemBytes
	}
	if in.MaxItems < 1 || in.MaxItems > maxHistoryItems || in.MaxBytes < 16<<10 || in.MaxBytes > maxHistoryBytes || in.MaxItemBytes < 1 || in.MaxItemBytes > maxHistoryItemBytes {
		return SessionHistoryLimits{}, errors.New("invalid history limits")
	}
	return in, nil
}

func encodeHistoryCursor(cursor historyCursor) string {
	data, _ := json.Marshal(cursor)
	mac := hmac.New(sha256.New, historyCursorMACKey)
	_, _ = mac.Write(data)
	data = append(data, mac.Sum(nil)...)
	return base64.RawURLEncoding.EncodeToString(data)
}

const (
	historyPageCursorVersion = 1
	historyItemCursorVersion = 2
)

func decodeHistoryCursor(raw string, version int) (historyCursor, error) {
	if len(raw) > 5462 {
		return historyCursor{}, ErrHistoryCursorInvalid
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || len(data) < sha256.Size || len(data) > 4096 {
		return historyCursor{}, ErrHistoryCursorInvalid
	}
	payload, signature := data[:len(data)-sha256.Size], data[len(data)-sha256.Size:]
	mac := hmac.New(sha256.New, historyCursorMACKey)
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return historyCursor{}, ErrHistoryCursorInvalid
	}
	var cursor historyCursor
	if err := json.Unmarshal(payload, &cursor); err != nil || cursor.Version != version || cursor.SessionID == "" || cursor.Epoch == "" || cursor.AnchorPosition < 0 || cursor.BeforePosition < 0 || cursor.BeforePosition > cursor.AnchorPosition {
		return historyCursor{}, ErrHistoryCursorInvalid
	}
	return cursor, nil
}

const historyOmittedResponseByteLimit = "item_exceeds_response_byte_limit"

func (snapshot SessionSnapshot) HistoryOmittedRanges() []api.SessionHistoryOmittedRange {
	if snapshot.History == nil {
		return nil
	}
	var ranges []api.SessionHistoryOmittedRange
	add := func(positions []int, reason string) {
		for _, position := range positions {
			omitted := api.SessionHistoryOmittedRange{StartPosition: position, EndPosition: position + 1, Reason: reason}
			if cursor := snapshot.HistoryItemCursors[position]; cursor != "" {
				omitted.Items = []api.SessionHistoryOmittedItem{{Position: position, Cursor: cursor}}
			}
			ranges = append(ranges, omitted)
		}
	}
	add(snapshot.History.OmittedPositions, "item_exceeds_item_byte_limit")
	add(snapshot.History.OmittedResponsePositions, historyOmittedResponseByteLimit)
	slices.SortFunc(ranges, func(a, b api.SessionHistoryOmittedRange) int { return a.StartPosition - b.StartPosition })
	return ranges
}

func historyPageWire(snapshot SessionSnapshot) api.SessionSnapshot[SessionState, InteractionKind, any] {
	out := api.SessionSnapshot[SessionState, InteractionKind, any]{BackgroundProcesses: snapshot.BackgroundProcesses, ParentSessionID: "", Projection: "tail", Session: snapshot.Session, Status: api.SessionStatus[SessionState]{SessionID: snapshot.Status.SessionID, AgentName: snapshot.Status.AgentName, State: snapshot.Status.State, Pending: snapshot.Status.Pending, TurnID: snapshot.Status.TurnID, LastError: snapshot.Status.LastError, InterruptedTurns: snapshot.Status.InterruptedTurns, Dormant: snapshot.Status.Dormant, Stopped: snapshot.Status.Stopped, PauseArmed: snapshot.Status.PauseArmed, Paused: snapshot.Status.Paused, PauseGeneration: snapshot.Status.PauseGeneration}, Epoch: snapshot.Epoch, Cursor: snapshot.Cursor, TranscriptPosition: snapshot.TranscriptPosition, TitleStatus: snapshot.TitleStatus}
	if snapshot.Session != nil {
		out.ParentSessionID = snapshot.Session.ParentID
	}
	out.Items = make([]api.SessionHistoryItem, len(snapshot.Items))
	for i, item := range snapshot.Items {
		out.Items[i] = api.SessionHistoryItem{Position: item.Position, Item: item.Item}
	}
	for _, item := range snapshot.Interactions {
		out.Interactions = append(out.Interactions, api.SessionInteraction[InteractionKind, any]{SessionID: item.SessionID, InteractionID: item.InteractionID, Kind: item.Kind, ElicitationID: item.ElicitationID, Event: item.Event})
	}
	for _, item := range snapshot.PendingInputs {
		out.PendingInputs = append(out.PendingInputs, api.SessionPendingInput{TurnID: item.TurnID, Content: item.Content, MultiContent: item.MultiContent, SessionPosition: item.SessionPosition, InputOrigin: item.InputOrigin, SenderID: item.SenderID, SenderName: item.SenderName, ReportOutcome: item.ReportOutcome, InputMode: item.InputMode})
	}
	for _, event := range snapshot.Presentation {
		out.Presentation = append(out.Presentation, event)
	}
	if snapshot.History != nil {
		out.History = &api.SessionHistory{StartPosition: snapshot.History.StartPosition, EndPosition: snapshot.History.BeforePosition, TotalItems: snapshot.History.TotalItems, OmittedRanges: snapshot.HistoryOmittedRanges(), NextCursor: snapshot.HistoryCursor, Truncated: snapshot.History.HasMore}
	}
	return out
}

func historyPageFits(snapshot SessionSnapshot, limit int) bool {
	wire := historyPageWire(snapshot)
	if session.BoundedJSONSize(wire, limit) > limit {
		return false
	}
	data, err := json.Marshal(wire)
	return err == nil && len(data) <= limit
}

func sessionHistoryPageFits(snapshot SessionSnapshot, limit int) bool {
	if snapshot.History == nil || snapshot.Session == nil {
		return false
	}
	history := api.SessionHistory{StartPosition: snapshot.History.StartPosition, EndPosition: snapshot.History.BeforePosition, TotalItems: snapshot.History.TotalItems, OmittedRanges: snapshot.HistoryOmittedRanges(), NextCursor: snapshot.HistoryCursor, Truncated: snapshot.History.HasMore}
	page := api.SessionHistoryPage{Projection: "tail", SessionID: snapshot.Session.ID, Epoch: snapshot.Epoch, Cursor: snapshot.Cursor, TranscriptPosition: snapshot.TranscriptPosition, History: history, TurnTimings: snapshot.History.TurnTimings, TurnOutcomes: snapshot.History.TurnOutcomes, Items: make([]api.SessionHistoryItem, len(snapshot.Items))}
	for i, item := range snapshot.Items {
		page.Items[i] = api.SessionHistoryItem{Position: item.Position, Item: item.Item}
	}
	return session.BoundedJSONSize(page, limit) <= limit
}

func snapshotFromHistoryPage(sessionID string, epoch string, anchorCursor uint64, anchorPosition int, page session.BoundedItemPage, revision uint64) SessionSnapshot {
	out := SessionSnapshot{Projection: "tail", Items: page.Items, Epoch: epoch, Cursor: anchorCursor, TranscriptPosition: anchorPosition}
	out.History = &page
	if page.HasMore {
		out.HistoryCursor = encodeHistoryCursor(historyCursor{Version: historyPageCursorVersion, SessionID: sessionID, Epoch: epoch, AnchorCursor: anchorCursor, AnchorPosition: anchorPosition, BeforePosition: page.NextBeforePosition, HistoryRevision: revision, StorageBefore: page.NextStoragePosition})
	}
	if len(page.OmittedStoragePositions) > 0 {
		out.HistoryItemCursors = make(map[int]string, len(page.OmittedStoragePositions))
		for pos, key := range page.OmittedStoragePositions {
			out.HistoryItemCursors[pos] = encodeHistoryCursor(historyCursor{Version: historyItemCursorVersion, SessionID: sessionID, Epoch: epoch, AnchorCursor: anchorCursor, AnchorPosition: anchorPosition, HistoryRevision: revision, ItemPosition: pos, ItemStorageKey: key})
		}
	}
	return out
}

func (h *sessionHandle) ReadHistoryPage(ctx context.Context, sessionID, rawCursor string, limits SessionHistoryLimits) (SessionSnapshot, error) {
	limits, err := NormalizeHistoryLimits(limits)
	if err != nil {
		return SessionSnapshot{}, err
	}
	var out SessionSnapshot
	err = h.ReadHistoryPageBounded(ctx, sessionID, rawCursor, limits, func(page SessionSnapshot) bool { out = page; return sessionHistoryPageFits(page, limits.MaxBytes) })
	return out, err
}

func (h *sessionHandle) ReadHistoryPageBounded(ctx context.Context, sessionID, rawCursor string, limits SessionHistoryLimits, accept func(SessionSnapshot) bool) error {
	limits, err := NormalizeHistoryLimits(limits)
	if err != nil {
		return err
	}
	if accept == nil {
		return errors.New("history page acceptance callback is required")
	}
	cursor, err := decodeHistoryCursor(rawCursor, historyPageCursorVersion)
	if err != nil {
		return err
	}
	if sessionID != h.sessionID || cursor.SessionID != h.sessionID {
		return ErrHistoryCursorInvalid
	}
	d := h.driver
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sess == nil {
		return ErrHistoryCursorStale
	}
	epoch, _ := d.events.CurrentCursor(d.sessionIDLocked())
	if epoch != cursor.Epoch || d.sess.HistoryRevisionSnapshot() != cursor.HistoryRevision || cursor.AnchorPosition > d.historyPositionLocked() || cursor.BeforePosition > cursor.AnchorPosition {
		return ErrHistoryCursorStale
	}
	header := &session.Session{ID: h.sessionID}
	page, e := d.readHistoryLocked(ctx, cursor.BeforePosition, cursor.StorageBefore, limits, func(candidate session.BoundedItemPage) bool {
		candidate.TotalItems = cursor.AnchorPosition
		view := snapshotFromHistoryPage(cursor.SessionID, cursor.Epoch, cursor.AnchorCursor, cursor.AnchorPosition, candidate, cursor.HistoryRevision)
		view.Session = header
		return accept(view)
	})
	if e != nil {
		if errors.Is(e, session.ErrHistoryPageEnvelopeTooLarge) {
			return ErrHistoryMetadataTooLarge
		}
		return e
	}
	page.TotalItems = cursor.AnchorPosition
	out := snapshotFromHistoryPage(cursor.SessionID, cursor.Epoch, cursor.AnchorCursor, cursor.AnchorPosition, page, cursor.HistoryRevision)
	out.Session = header
	if !accept(out) {
		return ErrHistoryMetadataTooLarge
	}
	return nil
}

func readHistoryItemCursor(rawCursor, sessionID, handleID string) (historyCursor, error) {
	cursor, err := decodeHistoryCursor(rawCursor, historyItemCursorVersion)
	if err != nil || sessionID != handleID || cursor.SessionID != handleID || cursor.ItemPosition < 0 || cursor.ItemPosition >= cursor.AnchorPosition || cursor.ItemStorageKey < 0 {
		return historyCursor{}, ErrHistoryCursorInvalid
	}
	return cursor, nil
}

func historyItemPage(cursor historyCursor, item session.Item, err error) (api.SessionHistoryItemPage, error) {
	if errors.Is(err, session.ErrNotFound) {
		return api.SessionHistoryItemPage{}, ErrHistoryCursorStale
	}
	if err != nil {
		return api.SessionHistoryItemPage{}, err
	}
	return api.SessionHistoryItemPage{SessionID: cursor.SessionID, Epoch: cursor.Epoch, Cursor: cursor.AnchorCursor, TranscriptPosition: cursor.AnchorPosition, Position: cursor.ItemPosition, Item: item}, nil
}

func (h *sessionHandle) ReadHistoryItem(ctx context.Context, sessionID, rawCursor string) (api.SessionHistoryItemPage, error) {
	cursor, err := readHistoryItemCursor(rawCursor, sessionID, h.sessionID)
	if err != nil {
		return api.SessionHistoryItemPage{}, err
	}
	if err := ctx.Err(); err != nil {
		return api.SessionHistoryItemPage{}, err
	}
	d := h.driver
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sess == nil {
		return api.SessionHistoryItemPage{}, ErrHistoryCursorStale
	}
	epoch, _ := d.events.CurrentCursor(h.sessionID)
	if epoch != cursor.Epoch || d.sess.HistoryRevisionSnapshot() != cursor.HistoryRevision || cursor.AnchorPosition > d.historyPositionLocked() {
		return api.SessionHistoryItemPage{}, ErrHistoryCursorStale
	}
	var item session.Item
	if d.history != nil {
		item, err = d.history.store.ReadViewHistoryItem(ctx, h.sessionID, d.history.total, cursor.ItemPosition, cursor.ItemStorageKey, api.SessionHistoryItemMaxBytes)
	} else {
		item, err = d.sess.ReadHistoryItem(cursor.ItemPosition)
	}
	return historyItemPage(cursor, item, err)
}

var _ SessionHistoryReader = (*sessionHandle)(nil)
var _ SessionHistoryItemReader = (*sessionHandle)(nil)
