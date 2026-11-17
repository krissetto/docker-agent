package messages

import (
	"slices"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/workblock"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// replyHandoffExchangeID is stable across source-index changes. Normal
// exchanges use their committed human-boundary message; Orphan distinguishes
// deterministic pre-boundary history generations supplied by the owner.
type replyHandoffExchangeID struct {
	Boundary   *types.Message
	Orphan     uint64
	Generation uint64
}

// replyHandoffComponentID identifies one logical answer within an exchange.
// Group is the responseGroups identity; Source is the stable pointer fallback
// for old/replayed messages that do not carry one.
type replyHandoffComponentID struct {
	Exchange replyHandoffExchangeID
	Group    uint64
	Source   *types.Message
}

func responseHandoffComponent(exchange replyHandoffExchangeID, group uint64, firstSource *types.Message) replyHandoffComponentID {
	id := replyHandoffComponentID{Exchange: exchange, Group: group}
	if group == 0 {
		id.Source = firstSource
	}
	return id
}

// replyHandoffLayoutRow is one source-owned rendered line from the affected
// exchange's currently visible row map. Y and Height are fractional terminal-
// row coordinates so the owner can quantize the shared paint/hit/selection map
// only once per frame; never pass a full-history snapshot for a stream token.
type replyHandoffLayoutRow struct {
	Source          *types.Message
	SourceIndex     int
	LocalRow        int
	Component       replyHandoffComponentID
	Text            string
	Composite       *workblock.Model
	CompositeLine   int
	CompositeAnchor int
	Y               float64
	Height          float64
}

type replyHandoffRowKey struct {
	Source   *types.Message
	LocalRow int
}

type replyHandoffPhase uint8

const (
	replyHandoffIdle replyHandoffPhase = iota
	replyHandoffFadeOut
	replyHandoffReflow
	replyHandoffFadeIn
)

// replyHandoffFrameRow is the single source map consumers should use for paint,
// hit testing, selection, hover, and anchors. Spacer rows have no source and
// are deliberately noninteractive.
type replyHandoffFrameRow struct {
	Source          *types.Message
	SourceIndex     int
	LocalRow        int
	Component       replyHandoffComponentID
	Text            string
	Composite       *workblock.Model
	CompositeLine   int
	CompositeAnchor int
	Y               float64
	Height          float64
	Opacity         float64
	Spacer          bool
	Interactive     bool
}

type replyHandoffBatch struct {
	retiring           []replyHandoffComponentID
	incoming           replyHandoffComponentID
	hasIncoming        bool
	additionalIncoming []replyHandoffComponentID
}

// replyHandoff keeps transient display rows while source messages remain authoritative.
type replyHandoff struct {
	transition animation.Transition
	phase      replyHandoffPhase

	oldLayout    []replyHandoffLayoutRow
	targetLayout []replyHandoffLayoutRow
	latestLayout []replyHandoffLayoutRow
	batch        replyHandoffBatch
	queued       *replyHandoffBatch
	queuedLayout []replyHandoffLayoutRow
}

func newReplyHandoff(ar *animation.Runtime) *replyHandoff {
	return &replyHandoff{transition: animation.NewTransition(ar)}
}

func (h *replyHandoff) Active() bool { return h != nil && h.phase != replyHandoffIdle }

func (h *replyHandoff) Phase() replyHandoffPhase {
	if h == nil {
		return replyHandoffIdle
	}
	return h.phase
}

// Begin freezes the retiring set and the current source-map endpoints for the
// affected exchange only. If a handoff is running, the new target is coalesced
// as a later bounded batch; stream/layout refreshes should use UpdateTarget
// with that exchange's visible rows instead of rebuilding full history.
func (h *replyHandoff) Begin(oldLayout, target []replyHandoffLayoutRow, retiring []replyHandoffComponentID, incoming *replyHandoffComponentID) tea.Cmd {
	if h == nil {
		return nil
	}
	batch := replyHandoffBatch{retiring: uniqueComponents(retiring)}
	if incoming != nil {
		batch.incoming, batch.hasIncoming = *incoming, true
	}
	if h.Active() {
		if h.queued == nil {
			h.queued = &batch
		} else {
			h.queued.retiring = uniqueComponents(append(h.queued.retiring, batch.retiring...))
			if batch.hasIncoming {
				if h.queued.hasIncoming && h.queued.incoming != batch.incoming {
					h.queued.additionalIncoming = uniqueComponents(append(h.queued.additionalIncoming, h.queued.incoming))
				}
				h.queued.incoming, h.queued.hasIncoming = batch.incoming, true
			}
		}
		h.queuedLayout = cloneHandoffLayout(target)
		return nil
	}
	h.oldLayout = cloneHandoffLayout(oldLayout)
	h.targetLayout = cloneHandoffLayout(target)
	h.latestLayout = cloneHandoffLayout(target)
	h.batch = batch
	if len(batch.retiring) > 0 {
		return h.startPhase(replyHandoffFadeOut)
	}
	if batch.hasIncoming {
		return h.startPhase(replyHandoffFadeIn)
	}
	h.phase = replyHandoffIdle
	h.oldLayout = nil
	h.batch = replyHandoffBatch{}
	return nil
}

// UpdateTarget refreshes source rows in the affected exchange as content,
// width, or indexes change. It never restarts a phase clock; during fade-in all
// rows of the incoming logical component inherit the same current opacity.
// Callers must preserve the transcript's segmented streaming tail and avoid
// rebuilding or cloning unrelated retained-history rows on each chunk/tick.
func (h *replyHandoff) UpdateTarget(layout []replyHandoffLayoutRow) {
	if h == nil {
		return
	}
	if h.phase == replyHandoffIdle {
		h.latestLayout = cloneHandoffLayout(layout)
		h.targetLayout = cloneHandoffLayout(layout)
		return
	}
	h.latestLayout = cloneHandoffLayout(layout)
	indices := make(map[*types.Message]int, len(layout))
	for _, row := range layout {
		if row.Source != nil {
			indices[row.Source] = row.SourceIndex
		}
	}
	for i := range h.oldLayout {
		if index, ok := indices[h.oldLayout[i].Source]; ok {
			h.oldLayout[i].SourceIndex = index
		}
	}
	if h.queued != nil {
		h.queuedLayout = cloneHandoffLayout(layout)
	}
	allowed := make(map[replyHandoffComponentID]struct{}, len(h.targetLayout)+1)
	for _, row := range h.targetLayout {
		allowed[row.Component] = struct{}{}
	}
	for _, incoming := range h.batch.incomingComponents() {
		allowed[incoming] = struct{}{}
	}
	filtered := make([]replyHandoffLayoutRow, 0, len(layout))
	for _, row := range layout {
		if _, ok := allowed[row.Component]; ok {
			filtered = append(filtered, row)
		}
	}
	h.targetLayout = filtered
}

// Rebase replaces a layout endpoint after a resize while preserving the active
// phase clock. During reflow the current mapped source rows are used to solve a
// matching start endpoint at the existing progress value.
func (h *replyHandoff) Rebase(oldLayout, target []replyHandoffLayoutRow) {
	if h == nil || !h.Active() {
		return
	}
	current := h.Rows()
	h.latestLayout = cloneHandoffLayout(target)
	switch h.phase {
	case replyHandoffFadeOut:
		oldByKey := make(map[replyHandoffRowKey]replyHandoffLayoutRow, len(oldLayout))
		lastBySource := make(map[*types.Message]replyHandoffLayoutRow)
		for _, row := range oldLayout {
			if row.Source == nil {
				continue
			}
			key := replyHandoffRowKey{Source: row.Source, LocalRow: row.LocalRow}
			oldByKey[key] = row
			prior, ok := lastBySource[row.Source]
			if !ok || row.LocalRow > prior.LocalRow {
				lastBySource[row.Source] = row
			}
		}
		rebasedOld := mergeResizedOldLayout(oldLayout, target)
		for i := range rebasedOld {
			row := &rebasedOld[i]
			if row.Source == nil {
				continue
			}
			key := replyHandoffRowKey{Source: row.Source, LocalRow: row.LocalRow}
			if prior, ok := oldByKey[key]; ok {
				row.Y, row.Height = prior.Y, prior.Height
				continue
			}
			if prior, ok := lastBySource[row.Source]; ok {
				row.Y = prior.Y + prior.Height + float64(row.LocalRow-prior.LocalRow-1)
			}
		}
		h.oldLayout = rebasedOld
		h.targetLayout = cloneHandoffLayout(target)
	case replyHandoffFadeIn:
		h.targetLayout = cloneHandoffLayout(target)
	case replyHandoffReflow:
		progress := h.transition.Value()
		if progress >= 1 {
			progress = 1 - 1e-9
		}
		currentByKey := make(map[replyHandoffRowKey]replyHandoffFrameRow, len(current))
		for _, row := range current {
			if row.Source != nil {
				currentByKey[replyHandoffRowKey{Source: row.Source, LocalRow: row.LocalRow}] = row
			}
		}
		start := cloneHandoffLayout(target)
		for i := range start {
			key := replyHandoffRowKey{Source: start[i].Source, LocalRow: start[i].LocalRow}
			row, hasRow := currentByKey[key]
			if !hasRow || start[i].Source == nil {
				continue
			}
			start[i].Y = (row.Y - progress*start[i].Y) / (1 - progress)
			start[i].Height = maxFloat(0.01, (row.Height-progress*start[i].Height)/(1-progress))
		}
		h.oldLayout = start
		h.targetLayout = cloneHandoffLayout(target)
	}
	if h.queued != nil {
		h.queuedLayout = cloneHandoffLayout(target)
	}
}

// UpdateComponent refreshes one logical component's rows without rebuilding
// its exchange or disturbing unrelated retained history. Streaming appends use
// this path so work and historical rows are not cloned per token.
func (h *replyHandoff) UpdateQueuedComponent(component replyHandoffComponentID, rows []replyHandoffLayoutRow) {
	if h == nil || h.queued == nil {
		return
	}
	h.queuedLayout = replaceHandoffComponent(h.queuedLayout, component, rows)
}

func (h *replyHandoff) UpdateComponent(component replyHandoffComponentID, rows []replyHandoffLayoutRow) {
	if h == nil || h.phase == replyHandoffIdle {
		return
	}
	h.targetLayout = replaceHandoffComponent(h.targetLayout, component, rows)
	h.latestLayout = replaceHandoffComponent(h.latestLayout, component, rows)
	h.UpdateQueuedComponent(component, rows)
}

// Tick advances only finite presentation phases. The model owner calls this
// after accepting the shared animation TickMsg and invalidates output only when
// it returns true.
func (h *replyHandoff) Tick() (changed bool, cmd tea.Cmd) {
	if h == nil || !h.Active() {
		return false, nil
	}
	h.transition.Tick()
	if h.transition.Running() {
		return true, nil
	}
	switch h.phase {
	case replyHandoffFadeOut:
		cmd = h.startPhase(replyHandoffReflow)
	case replyHandoffReflow:
		if h.batch.hasIncoming {
			cmd = h.startPhase(replyHandoffFadeIn)
		} else {
			cmd = h.finishBatch()
		}
	case replyHandoffFadeIn:
		cmd = h.finishBatch()
	}
	return true, cmd
}

// finishBatch consumes the newest coalesced target at the endpoint of either
// reflow-only settings changes or the incoming fade. A busy stream cannot
// strand a queued target behind a completed phase.
func (h *replyHandoff) finishBatch() tea.Cmd {
	if h.queued != nil {
		oldLayout := cloneHandoffLayout(h.targetLayout)
		next := *h.queued
		target := cloneHandoffLayout(h.queuedLayout)
		h.queued = nil
		h.queuedLayout = nil
		h.oldLayout = oldLayout
		h.targetLayout = target
		h.latestLayout = cloneHandoffLayout(target)
		h.batch = next
		if len(next.retiring) > 0 {
			return h.startPhase(replyHandoffFadeOut)
		}
		if next.hasIncoming {
			return h.startPhase(replyHandoffFadeIn)
		}
		h.phase = replyHandoffIdle
		h.oldLayout = nil
		h.batch = replyHandoffBatch{}
		return nil
	}
	h.phase = replyHandoffIdle
	h.oldLayout = nil
	h.latestLayout = cloneHandoffLayout(h.targetLayout)
	h.batch = replyHandoffBatch{}
	return nil
}

func (h *replyHandoff) startPhase(phase replyHandoffPhase) tea.Cmd {
	h.phase = phase
	// Fade phases 150-250ms; reflow 200-300ms (existing animation constants).
	duration := animation.ShortDuration / 2
	if phase == replyHandoffReflow {
		duration = animation.MediumDuration / 2
	}
	return h.transition.Start(duration, animation.EaseInOutCubic)
}

// Rows returns source-aware display geometry for one frame. Retirees remain at
// their old position during fade-out; reflow uses real source rows plus inert
// contracting spacers; fade-in shows the latest target source rows.
func (h *replyHandoff) Rows() []replyHandoffFrameRow {
	if h == nil || h.phase == replyHandoffIdle {
		if h == nil {
			return nil
		}
		return mappedHandoffRows(h.latestLayout, 1, false)
	}
	progress := h.transition.Value()
	switch h.phase {
	case replyHandoffFadeOut:
		rows := make([]replyHandoffFrameRow, 0, len(h.oldLayout))
		for _, row := range h.oldLayout {
			if h.batch.isIncoming(row.Component) {
				continue
			}
			alpha := 1.0
			if h.isRetiring(row.Component) {
				alpha = 1 - progress
			}
			rows = append(rows, handoffFrameRow(row, alpha, false))
		}
		return rows
	case replyHandoffReflow:
		return h.reflowRows(progress)
	case replyHandoffFadeIn:
		rows := make([]replyHandoffFrameRow, 0, len(h.targetLayout))
		alpha := progress
		for _, row := range h.targetLayout {
			rowAlpha := 1.0
			if h.batch.isIncoming(row.Component) {
				rowAlpha = alpha
			}
			rows = append(rows, handoffFrameRow(row, rowAlpha, false))
		}
		return rows
	default:
		return mappedHandoffRows(h.latestLayout, 1, false)
	}
}

func (h *replyHandoff) reflowRows(progress float64) []replyHandoffFrameRow {
	oldByKey := make(map[replyHandoffRowKey]replyHandoffLayoutRow, len(h.oldLayout))
	for _, row := range h.oldLayout {
		oldByKey[replyHandoffRowKey{Source: row.Source, LocalRow: row.LocalRow}] = row
	}
	targetKeys := make(map[replyHandoffRowKey]struct{}, len(h.targetLayout))
	rows := make([]replyHandoffFrameRow, 0, len(h.targetLayout)+len(h.oldLayout))
	for _, target := range h.targetLayout {
		key := replyHandoffRowKey{Source: target.Source, LocalRow: target.LocalRow}
		targetKeys[key] = struct{}{}
		if h.batch.isIncoming(target.Component) {
			rows = append(rows, replyHandoffFrameRow{
				SourceIndex: -1, LocalRow: -1, Component: target.Component,
				Y: target.Y, Height: target.Height, Opacity: 0, Spacer: true, Interactive: false,
			})
			continue
		}
		// A Work anchor can move across retained replies when a response joins
		// the block. Its header is not prose: keep it at its destination rather
		// than sweeping across (and stealing hits from) those retained rows.
		if target.Composite != nil && target.Source == nil {
			rows = append(rows, handoffFrameRow(target, 1, false))
			continue
		}
		old, existed := oldByKey[key]
		if !existed {
			row := target
			row.Y = target.Y + target.Height*(1-progress)
			row.Height = target.Height * progress
			opacity := 1.0
			if h.isRetiring(target.Component) {
				opacity = progress
			}
			rows = append(rows, handoffFrameRow(row, opacity, false))
			continue
		}
		row := target
		row.Y = interpolate(old.Y, target.Y, progress)
		row.Height = interpolate(old.Height, target.Height, progress)
		opacity := 1.0
		if h.isRetiring(target.Component) {
			opacity = progress
		}
		rows = append(rows, handoffFrameRow(row, opacity, false))
	}
	for _, old := range h.oldLayout {
		if !h.isRetiring(old.Component) {
			continue
		}
		if _, mappedAtDestination := targetKeys[replyHandoffRowKey{Source: old.Source, LocalRow: old.LocalRow}]; mappedAtDestination {
			continue
		}
		rows = append(rows, replyHandoffFrameRow{
			Component: old.Component, SourceIndex: -1, LocalRow: -1,
			Y: old.Y, Height: old.Height * (1 - progress),
			Opacity: 0, Spacer: true, Interactive: false,
		})
	}
	slices.SortFunc(rows, func(a, b replyHandoffFrameRow) int {
		if a.Spacer != b.Spacer {
			if a.Spacer {
				return -1
			}
			return 1
		}
		if a.Y < b.Y {
			return -1
		}
		if a.Y > b.Y {
			return 1
		}
		return 0
	})
	return rows
}

func (h *replyHandoff) isRetiring(component replyHandoffComponentID) bool {
	return slices.Contains(h.batch.retiring, component)
}

// Stop cancels finite motion and drops captured maps while preserving no
// historical source payload. Reset/replay/disposal owners call it on teardown.
func (h *replyHandoff) Stop() {
	if h == nil {
		return
	}
	h.transition.Cancel()
	h.phase = replyHandoffIdle
	h.oldLayout = nil
	h.targetLayout = nil
	h.latestLayout = nil
	h.batch = replyHandoffBatch{}
	h.queued = nil
	h.queuedLayout = nil
}

func mappedHandoffRows(layout []replyHandoffLayoutRow, opacity float64, spacer bool) []replyHandoffFrameRow {
	rows := make([]replyHandoffFrameRow, 0, len(layout))
	for _, row := range layout {
		rows = append(rows, handoffFrameRow(row, opacity, spacer))
	}
	return rows
}

func handoffFrameRow(row replyHandoffLayoutRow, opacity float64, spacer bool) replyHandoffFrameRow {
	return replyHandoffFrameRow{
		Source: row.Source, SourceIndex: row.SourceIndex, LocalRow: row.LocalRow,
		Component: row.Component, Text: row.Text, Composite: row.Composite, CompositeLine: row.CompositeLine,
		CompositeAnchor: row.CompositeAnchor,
		Y:               row.Y, Height: row.Height, Opacity: opacity,
		Spacer: spacer, Interactive: !spacer && opacity > 0,
	}
}

func mergeResizedOldLayout(old, target []replyHandoffLayoutRow) []replyHandoffLayoutRow {
	targetBySource := make(map[*types.Message][]replyHandoffLayoutRow)
	for _, row := range target {
		if row.Source != nil {
			targetBySource[row.Source] = append(targetBySource[row.Source], row)
		}
	}
	var out []replyHandoffLayoutRow
	seen := make(map[*types.Message]map[int]struct{})
	lastY := make(map[*types.Message]float64)
	lastLocal := make(map[*types.Message]int)
	for _, row := range old {
		if row.Source == nil {
			out = append(out, row)
			continue
		}
		if seen[row.Source] == nil {
			seen[row.Source] = make(map[int]struct{})
		}
		if rows := targetBySource[row.Source]; row.LocalRow >= 0 && row.LocalRow < len(rows) {
			row.Text = rows[row.LocalRow].Text
			row.Composite, row.CompositeLine, row.CompositeAnchor = rows[row.LocalRow].Composite, rows[row.LocalRow].CompositeLine, rows[row.LocalRow].CompositeAnchor
			row.Height = rows[row.LocalRow].Height
			seen[row.Source][row.LocalRow] = struct{}{}
		}
		lastY[row.Source], lastLocal[row.Source] = row.Y, row.LocalRow
		out = append(out, row)
	}
	for source, rows := range targetBySource {
		seenRows := seen[source]
		for _, row := range rows {
			if _, exists := seenRows[row.LocalRow]; exists {
				continue
			}
			row.Y = lastY[source] + float64(row.LocalRow-lastLocal[source])
			out = append(out, row)
		}
	}
	slices.SortFunc(out, func(a, b replyHandoffLayoutRow) int {
		if a.Y < b.Y {
			return -1
		}
		if a.Y > b.Y {
			return 1
		}
		return 0
	})
	return out
}

func replaceHandoffComponent(layout []replyHandoffLayoutRow, component replyHandoffComponentID, rows []replyHandoffLayoutRow) []replyHandoffLayoutRow {
	filtered := make([]replyHandoffLayoutRow, 0, len(layout)+len(rows))
	insertAt := -1
	for _, row := range layout {
		if row.Component == component {
			if insertAt < 0 {
				insertAt = len(filtered)
			}
			continue
		}
		filtered = append(filtered, row)
	}
	if insertAt < 0 {
		insertAt = len(filtered)
	}
	return slices.Insert(filtered, insertAt, rows...)
}

func (b replyHandoffBatch) isIncoming(component replyHandoffComponentID) bool {
	return b.hasIncoming && (b.incoming == component || slices.Contains(b.additionalIncoming, component))
}

func (b replyHandoffBatch) incomingComponents() []replyHandoffComponentID {
	if !b.hasIncoming {
		return nil
	}
	return uniqueComponents(append(slices.Clone(b.additionalIncoming), b.incoming))
}

func cloneHandoffLayout(layout []replyHandoffLayoutRow) []replyHandoffLayoutRow {
	return slices.Clone(layout)
}

func uniqueComponents(components []replyHandoffComponentID) []replyHandoffComponentID {
	if len(components) < 2 {
		return slices.Clone(components)
	}
	seen := make(map[replyHandoffComponentID]struct{}, len(components))
	unique := make([]replyHandoffComponentID, 0, len(components))
	for _, component := range components {
		if _, ok := seen[component]; ok {
			continue
		}
		seen[component] = struct{}{}
		unique = append(unique, component)
	}
	return unique
}

func interpolate(from, to, progress float64) float64 {
	return from + (to-from)*progress
}
