package session

import (
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

var ErrHistoryItemTooLarge = errors.New("history item exceeds byte limit")
var ErrHistoryPageEnvelopeTooLarge = errors.New("history envelope cannot fit bounded response")

// BoundedItem is one complete canonical transcript item and its original
// absolute position. It never represents a reconstructed or display-only row.
type BoundedItem struct {
	Position int  `json:"position"`
	Item     Item `json:"item"`
}

// BoundedItemPage reads no more than maxScan transcript positions ending just
// before beforePosition. The byte limit applies to serialized complete items;
// callers must also account for their response envelope. Oversize records are
// returned as positions, never partially copied or silently discarded.
type BoundedItemPage struct {
	Items                    []BoundedItem
	OmittedPositions         []int
	OmittedResponsePositions []int
	StartPosition            int
	BeforePosition           int
	TotalItems               int
	HasMore                  bool
	NextBeforePosition       int
	SerializedItemBytes      int
	ScannedPositions         int
	TurnTimings              map[string]TurnTiming
	TurnOutcomes             map[string]string
	NextStoragePosition      int64 // optional store-specific keyset boundary
}

func (s *Session) ReadBoundedItems(beforePosition, maxScan, maxItemBytes, maxPageItemBytes int) (BoundedItemPage, error) {
	return s.ReadBoundedItemsWithBudget(beforePosition, maxScan, maxItemBytes, func(page BoundedItemPage) bool { return page.SerializedItemBytes <= maxPageItemBytes })
}

// ReadBoundedItemsWithBudget walks only a bounded window and asks fits to
// measure each complete candidate page, including caller-owned envelope,
// metadata, cursor and omission ranges. A position that cannot fit after a
// non-empty page remains unscanned for the next cursor; only intrinsically
// oversized records advance as omissions.
func (s *Session) ReadBoundedItemsWithBudget(beforePosition, maxScan, maxItemBytes int, fits func(BoundedItemPage) bool) (BoundedItemPage, error) {
	if s == nil || beforePosition < 0 || maxScan < 1 || maxItemBytes < 1 || fits == nil {
		return BoundedItemPage{}, errors.New("invalid bounded history request")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if beforePosition > len(s.Messages) {
		return BoundedItemPage{}, errors.New("history position exceeds transcript")
	}
	return readBoundedItemWindow(beforePosition, len(s.Messages), maxScan, maxItemBytes, fits, func(pos int) (Item, int64, bool, error) {
		return s.Messages[pos], 0, false, nil
	}, s.TurnTimings, s.TurnOutcomes)
}

func readBoundedItemWindow(beforePosition, total, maxScan, maxItemBytes int, fits func(BoundedItemPage) bool, read func(int) (Item, int64, bool, error), timings map[string]TurnTiming, outcomes map[string]string) (BoundedItemPage, error) {
	page := BoundedItemPage{BeforePosition: beforePosition, TotalItems: total, StartPosition: beforePosition, NextBeforePosition: beforePosition, Items: make([]BoundedItem, 0, maxScan)}
	lower := max(0, beforePosition-maxScan)
	scanned := 0
	for pos := beforePosition - 1; pos >= lower; pos-- {
		item, storagePosition, oversized, err := read(pos)
		if err != nil {
			return BoundedItemPage{}, err
		}
		if oversized || conservativeJSONUpperBound(reflect.ValueOf(item), maxItemBytes, 0) > maxItemBytes {
			trial := page
			trial.OmittedPositions = append(slices.Clone(page.OmittedPositions), pos)
			trial.StartPosition, trial.NextBeforePosition, trial.ScannedPositions, trial.NextStoragePosition = pos, pos, scanned+1, storagePosition
			trial.HasMore = pos > 0
			if !fits(trial) {
				if scanned == 0 {
					return BoundedItemPage{}, ErrHistoryPageEnvelopeTooLarge
				}
				page.HasMore = true
				page.ScannedPositions = scanned
				break
			}
			page = trial
			scanned++
			continue
		}
		data, err := json.Marshal(item)
		if err != nil {
			return BoundedItemPage{}, err
		}
		if len(data) > maxItemBytes {
			return BoundedItemPage{}, errors.New("history item exceeded verified byte bound")
		}
		var detached Item
		if err := json.Unmarshal(data, &detached); err != nil {
			return BoundedItemPage{}, err
		}
		trial := page
		trial.Items = append(slices.Clone(page.Items), BoundedItem{Position: pos, Item: detached})
		trial.addTurnMetadata(detached, timings, outcomes)
		trial.SerializedItemBytes += len(data)
		trial.StartPosition, trial.NextBeforePosition, trial.ScannedPositions, trial.NextStoragePosition = pos, pos, scanned+1, storagePosition
		trial.HasMore = pos > 0
		if fits(trial) {
			page = trial
			scanned++
			continue
		}
		if scanned == 0 {
			trial = page
			trial.OmittedResponsePositions = append(slices.Clone(page.OmittedResponsePositions), pos)
			trial.StartPosition, trial.NextBeforePosition, trial.ScannedPositions, trial.NextStoragePosition = pos, pos, 1, storagePosition
			trial.HasMore = pos > 0
			if !fits(trial) {
				return BoundedItemPage{}, ErrHistoryPageEnvelopeTooLarge
			}
			page = trial
			scanned++
			continue
		}
		page.HasMore = true
		page.ScannedPositions = scanned
		break
	}
	if !page.HasMore && lower > 0 {
		page.HasMore = true
		page.NextBeforePosition = min(page.NextBeforePosition, lower)
	}
	page.ScannedPositions = scanned
	for left, right := 0, len(page.Items)-1; left < right; left, right = left+1, right-1 {
		page.Items[left], page.Items[right] = page.Items[right], page.Items[left]
	}
	return page, nil
}

func conservativeJSONUpperBound(v reflect.Value, limit, depth int) int {
	if !v.IsValid() {
		return 4
	}
	if depth > 24 {
		return limit + 1
	}
	if v.Kind() == reflect.Pointer && v.Type().Elem().PkgPath() == "time" && v.Type().Elem().Name() == "Time" {
		return 64
	}
	if v.Type().PkgPath() == "time" && v.Type().Name() == "Time" {
		return 64
	}
	if v.Kind() == reflect.Pointer && v.Type().Elem().PkgPath() == "github.com/docker/docker-agent/pkg/session" && v.Type().Elem().Name() == "Session" {
		if v.IsNil() {
			return 4
		}
		return conservativeStruct(v.Elem(), limit, depth+1)
	}
	marshalerType := reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	if v.IsValid() && v.CanInterface() && v.Type().PkgPath() == "github.com/docker/docker-agent/pkg/api" && strings.HasPrefix(v.Type().Name(), "SessionSnapshot[") {
		return conservativeStruct(v, limit, depth)
	}
	if v.IsValid() && v.CanInterface() && v.Type().Implements(marshalerType) {
		if v.Type() == reflect.TypeOf(json.RawMessage{}) {
			return v.Len()
		}
		// Session.MarshalJSON serializes its exported session shape. Walk its
		// fields directly without invoking the potentially unbounded marshaler.
		if v.Type().PkgPath() == "github.com/docker/docker-agent/pkg/session" && v.Type().Name() == "Session" {
			if v.Kind() == reflect.Pointer {
				if v.IsNil() {
					return 4
				}
				v = v.Elem()
			}
			return conservativeStruct(v, limit, depth)
		}
		return limit + 1
	}
	if v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return 4
		}
		return conservativeJSONUpperBound(v.Elem(), limit, depth+1)
	}
	add := func(a, b int) int {
		if a > limit-b {
			return limit + 1
		}
		return a + b
	}
	switch v.Kind() {
	case reflect.String:
		return escapedJSONStringBytes(v.String(), limit)
	case reflect.Bool:
		if v.Bool() {
			return 4
		}
		return 5
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return len(strconv.FormatInt(v.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return len(strconv.FormatUint(v.Uint(), 10))
	case reflect.Float32, reflect.Float64:
		return len(strconv.FormatFloat(v.Float(), 'g', -1, v.Type().Bits()))
	case reflect.Slice, reflect.Array:
		if v.Len() > limit/2 {
			return limit + 1
		}
		n := 2
		for i := 0; i < v.Len(); i++ {
			n = add(n, conservativeJSONUpperBound(v.Index(i), limit, depth+1)+1)
			if n > limit {
				return n
			}
		}
		return n
	case reflect.Map:
		if v.Len() > limit/5 {
			return limit + 1
		}
		if v.IsNil() {
			return 4
		}
		n := 2
		iter := v.MapRange()
		for iter.Next() {
			n = add(n, conservativeJSONUpperBound(iter.Key(), limit, depth+1)+conservativeJSONUpperBound(iter.Value(), limit, depth+1)+3)
			if n > limit {
				return n
			}
		}
		return n
	case reflect.Struct:
		return conservativeStruct(v, limit, depth)
	default:
		// Do not invoke custom marshalers during preflight: they may allocate an
		// unbounded buffer. Only known JSON custom types are admitted by length.
		if v.Type().PkgPath() == "encoding/json" && v.Type().Name() == "RawMessage" {
			return v.Len()
		}
		return limit + 1
	}
}

// HistoryHeaderSnapshot copies bounded presentation metadata, never transcript or per-turn records.
func (s *Session) HistoryHeaderSnapshot(maxBytes int) (*Session, error) {
	if s == nil || maxBytes < 1 {
		return nil, errors.New("invalid history header limit")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	header := &Session{
		ID: s.ID, ParentID: s.ParentID, AgentName: s.AgentName, AgentID: s.AgentID,
		Origin: s.Origin, InputID: s.InputID, Title: s.Title, CreatedAt: s.CreatedAt,
		Evals: s.Evals, EvalResult: s.EvalResult, WorkingDir: s.WorkingDir,
		SafetyPolicy: s.SafetyPolicy, PriorSafetyPolicy: s.PriorSafetyPolicy, ToolsApproved: s.ToolsApproved,
		NonInteractive: s.NonInteractive, HideToolResults: s.HideToolResults, SendUserMessage: s.SendUserMessage,
		MaxIterations: s.MaxIterations, MaxConsecutiveToolCalls: s.MaxConsecutiveToolCalls,
		MaxOldToolCallTokens: s.MaxOldToolCallTokens, MaxToolResultTokens: s.MaxToolResultTokens,
		Starred: s.Starred, InputTokens: s.InputTokens, OutputTokens: s.OutputTokens, Cost: s.Cost,
		Permissions: s.Permissions, Attributes: s.Attributes, AgentModelOverrides: s.AgentModelOverrides,
		AgentThinkingLevels: s.AgentThinkingLevels, CustomModelsUsed: s.CustomModelsUsed,
		AttachedFiles: s.AttachedFiles, AsyncSubagent: s.AsyncSubagent,
	}
	if !JSONWithinBound(header, maxBytes) {
		return nil, errors.New("history metadata exceeds response limit")
	}
	// Instruction context, topology and per-turn records have independent sizes.
	return header.Clone(), nil
}

// RestoreHistoryItems materializes a previously unhydrated transcript without changing its revision.
// The caller transfers ownership of the freshly read items.
func (s *Session) RestoreHistoryItems(items []Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Messages = items
}

// SetHistoryTurnMetadata adds durable work metadata for a storage-backed page.
func (s *Session) SetHistoryTurnMetadata(page *BoundedItemPage) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, item := range page.Items {
		page.addTurnMetadata(item.Item, s.TurnTimings, s.TurnOutcomes)
	}
}

func (p *BoundedItemPage) addTurnMetadata(item Item, timings map[string]TurnTiming, outcomes map[string]string) {
	if item.Message == nil || item.Message.TurnID == "" {
		return
	}
	id := item.Message.TurnID
	if timing, ok := timings[id]; ok {
		p.TurnTimings = maps.Clone(p.TurnTimings)
		if p.TurnTimings == nil {
			p.TurnTimings = make(map[string]TurnTiming)
		}
		p.TurnTimings[id] = timing
	}
	if outcome, ok := outcomes[id]; ok {
		p.TurnOutcomes = maps.Clone(p.TurnOutcomes)
		if p.TurnOutcomes == nil {
			p.TurnOutcomes = make(map[string]string)
		}
		p.TurnOutcomes[id] = outcome
	}
}

func conservativeStruct(v reflect.Value, limit, depth int) int {
	add := func(a, b int) int {
		if a > limit-b {
			return limit + 1
		}
		return a + b
	}
	n := 2
	emitted := false
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" {
			continue
		}
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		parts := strings.Split(tag, ",")
		name := parts[0]
		if name == "" {
			name = field.Name
		}
		omitEmpty := false
		for _, option := range parts[1:] {
			if option == "omitempty" {
				omitEmpty = true
			}
		}
		if omitEmpty && jsonEmptyValue(v.Field(i)) {
			continue
		}
		value := conservativeJSONUpperBound(v.Field(i), limit, depth+1)
		key := len(name) + 2 // JSON keys in our wire structs are plain ASCII.
		member := key + 1 + value
		if emitted {
			member++
		}
		n = add(n, member)
		if n > limit {
			return n
		}
		emitted = true
	}
	return n
}

func jsonEmptyValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array:
		return v.IsZero()
	case reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Interface, reflect.Pointer:
		return v.IsNil()
	}
	return false
}

// JSONWithinBound returns false without invoking arbitrary MarshalJSON methods or
// allocating a serialized copy. The estimate is intentionally conservative.
func JSONWithinBound(value any, limit int) bool { return BoundedJSONSize(value, limit) <= limit }

// BoundedJSONSize estimates serialized bytes without invoking custom marshalers
// or traversing beyond limit. A result of limit+1 means it exceeds the bound.
func BoundedJSONSize(value any, limit int) int {
	if limit < 1 {
		return limit + 1
	}
	return conservativeJSONUpperBound(reflect.ValueOf(value), limit, 0)
}

// HistoryRevisionSnapshot identifies in-place transcript changes within a
// runtime epoch. Appends do not alter the revision.
func (s *Session) HistoryRevisionSnapshot() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.historyRevision
}

// MarkHistoryChanged fences history cursors after a non-append transcript
// replacement performed by an owner outside the ordinary session mutators.
func (s *Session) MarkHistoryChanged() {
	s.mu.Lock()
	s.historyRevision++
	s.mu.Unlock()
}

func escapedJSONStringBytes(text string, limit int) int {
	if len(text) > limit {
		return limit + 1
	}
	n := 2
	for len(text) > 0 {
		r, size := utf8.DecodeRuneInString(text)
		text = text[size:]
		encoded := size
		switch r {
		case '"', '\\', '\b', '\f', '\n', '\r', '\t':
			encoded = 2
		case '<', '>', '&', '\u2028', '\u2029':
			encoded = 6
		default:
			if r < 0x20 {
				encoded = 6
			}
		}
		if r == utf8.RuneError && size == 1 {
			encoded = 3
		}
		n += encoded
		if n > limit {
			return limit + 1
		}
	}
	return n
}
