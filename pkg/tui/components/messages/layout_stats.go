package messages

import (
	"sync/atomic"

	"github.com/docker/docker-agent/pkg/tui/components/workblock"
)

// layoutStats counts the layout work a transcript model performs. The counters
// only observe work; they never change what is rendered or how it is placed.
type layoutStats struct {
	epochs                    uint64 // styling or width epochs (invalidateAllItems)
	itemVisits                uint64 // per-item iterations inside layout builds
	rowsAllocated             uint64 // flattened row slots allocated for renderedLines
	rowsCleared               uint64 // flattened row slots cleared or dropped
	indexNodes                uint64 // layout index nodes touched
	entries                   uint64 // materialized slots currently holding lines (cache plus uncached)
	slotRebuilds              uint64 // full O(N) slot index rebuilds (initial load or reorder only)
	slotLoops                 uint64 // per-slot iterations inside slot index work
	facadeEntries             uint64 // materialized entries placed or cleared in the renderedLines mirror
	sizeCalls                 uint64 // view SetSize calls issued by layout (resize and work grouping)
	projectionVisits          uint64 // work sources visited by a projection sync or compact toggle
	slotMetaVisits            uint64 // slot separator and metadata updates outside the index build
	blockViews                uint64 // workblock View, Height and Rows calls from the layout path (a cold block renders)
	viewCacheInvalidations    uint64 // InvalidateRenderCache calls on message views (restyling or explicit invalidation)
	projectionRebuilds        uint64 // projectWorkWithVisibleReplies calls (full work projection passes)
	anchorLookups             uint64 // workItemsForAnchor calls (one per work block measured)
	exchangeVisits            uint64 // exchanges touched by anchor lookups: at most one per lookup; a fallback scan would add more
	workRangeScans            uint64 // work range entries iterated by any remaining range scan (structural sync, explicit workRangeList)
	workIndexVisits           uint64 // entries visited by indexed work lookups (slot walk over a captured range, exchange and identity lookups)
	fullProjectionLookupScans uint64 // entries into a projection lookup fallback scan (no index); must stay 0 on hot paths
	themeUpdates              uint64 // per-slot theme generations delivered on materialization (syncViewTheme); a theme change itself visits no slot
	identityCatchupEntries    uint64 // unhooked tail entries indexed by the append-only catch-up (messagePointerIndex miss); production writes keep this 0
	identityEntries           uint64 // message identity index entries written by indexMessages (an append writes one)
	identityLookups           uint64 // message pointer identity lookups (messagePointerIndex, one index read each)
	blockReverseLookups       uint64 // block-to-anchor reverse lookups (workSourceForBlock)
	targetBodyRenders         uint64 // block reads for one requested or hit target (sourceToGlobalLine owner block, hit-path block reads)

	// Retention per owner, read from the current state (not cumulative). A body
	// is a materialized slot; rows are the lines it holds.
	bodiesCached   uint64 // bodies in renderedItems (cacheable, outside any handoff)
	bodiesUncached uint64 // bodies in uncached (live, hovered or selected)
	bodiesHandoff  uint64 // bodies kept only because a reply handoff reads them
	rowsCached     uint64 // rows held by renderedItems bodies
	rowsUncached   uint64 // rows held by uncached bodies
	rowsHandoff    uint64 // rows held by handoff-retained bodies
	facadeRows     uint64 // non-empty rows in the renderedLines mirror

	// Replay body work, cumulative from model construction and never reset by a
	// load. Observational only: no branch reads these fields.
	syncRenders        uint64 // renderItem bodies rendered on the model goroutine
	compositeViews     uint64 // composite.View calls that build a work block's replay body
	prepareScheduled   uint64 // message PrepareRender closures handed to a worker
	prepareExecuted    uint64 // message PrepareRender closures that ran on a worker (atomic source)
	prepareApplied     uint64 // prepared message renders applied to a view
	reasoningScheduled uint64 // reasoning PrepareReasoning closures handed to a worker
	reasoningExecuted  uint64 // reasoning PrepareReasoning closures that ran on a worker (atomic source)
	reasoningApplied   uint64 // prepared reasoning results applied to a view
}

// replayPrepareCounts holds the counters that worker goroutines increment. It
// lives behind a pointer because layoutStats is copied by value.
type replayPrepareCounts struct {
	prepareExecuted   atomic.Uint64
	reasoningExecuted atomic.Uint64
}

// layoutWorkStats returns a snapshot of the layout work counters. Retention is
// read from the materialized entries only, so the snapshot costs O(materialized).
func (m *model) layoutWorkStats() layoutStats {
	s := m.stats
	if m.prepareCounts != nil {
		s.prepareExecuted = m.prepareCounts.prepareExecuted.Load()
		s.reasoningExecuted = m.prepareCounts.reasoningExecuted.Load()
	}
	s.indexNodes = m.geom.NodesTouched()
	s.entries = uint64(len(m.renderedItems) + len(m.uncached))
	keep := m.handoffRetained()
	for i, e := range m.renderedItems {
		rows := uint64(len(e.lines))
		if _, ok := keep[i]; ok {
			s.bodiesHandoff++
			s.rowsHandoff += rows
			continue
		}
		s.bodiesCached++
		s.rowsCached += rows
	}
	for i, e := range m.uncached {
		rows := uint64(len(e.lines))
		if _, ok := keep[i]; ok {
			s.bodiesHandoff++
			s.rowsHandoff += rows
			continue
		}
		s.bodiesUncached++
		s.rowsUncached += rows
	}
	for _, e := range m.renderedItems {
		s.facadeRows += rowCount(e.span)
	}
	for _, e := range m.uncached {
		s.facadeRows += rowCount(e.span)
	}
	return s
}

// blockView, blockHeight and blockRows are the only workblock reads on the
// layout path. blockViews counts actual workblock View() invocations, not calls:
// View runs one, Height runs one (lipgloss.Height over m.View()) and Rows runs one
// (m.View() before copying rows). So blockHeight and blockRows each add two: their
// own entry plus the nested View they invoke. Interaction reads (SourceAt,
// SourceColumnAt, WheelAt) run on click and hover paths, not the layout path, and
// are not counted here.
func (m *model) blockView(block *workblock.Model) string {
	m.stats.blockViews++
	return block.View()
}

func (m *model) blockHeight(block *workblock.Model) int {
	m.stats.blockViews += 2
	return block.Height()
}

func (m *model) blockRows(block *workblock.Model) []workblock.Row {
	m.stats.blockViews += 2
	return block.Rows()
}

// rowCount converts a row span to a counter value; spans are never negative.
func rowCount(n int) uint64 {
	if n <= 0 {
		return 0
	}
	return uint64(n)
}
