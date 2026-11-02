package messages

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// Plain history prepends used to keep front headroom in renderedBacking. The
// slot layout keeps no flattened prepend buffer: rows are measured lazily and
// the mirror is rewritten per slot. What must hold is the same retention rule:
// every path that replaces or clears the transcript drops its materialized
// entries and their rows, so the old transcript stays unreachable. Retained
// state is checked through layoutWorkStats instead of the removed buffer.

func fixesLinesText(lines []string) string {
	stripped := make([]string, len(lines))
	for i, line := range lines {
		stripped[i] = ansi.Strip(line)
	}
	return strings.Join(stripped, "\n")
}

// fixesFindingMarker returns a heading from the canonical transcript (renderedLines).
// The viewport is not used: at small heights the oldest headings are off screen.
func fixesFindingMarker(t *testing.T, lines []string) string {
	t.Helper()
	for _, line := range lines {
		stripped := ansi.Strip(line)
		if idx := strings.Index(stripped, "Finding "); idx >= 0 {
			fields := strings.Fields(stripped[idx:])
			require.GreaterOrEqual(t, len(fields), 2, "heading must carry a number")
			return fields[0] + " " + fields[1]
		}
	}
	require.FailNow(t, "canonical transcript must contain a Finding heading")
	return ""
}

func newPartialPlainHistory(t *testing.T) (*model, string) {
	t.Helper()
	m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	BeginReplay(m, PrepareReplay(retainedMemoryWorkload(60)), nil)
	drainCoverageReplay(t, m)
	require.True(t, HistoryPending(m), "fixture must leave older history to page in")
	runReplayCommand(m, ContinueHistory(m))
	// The full transcript is materialized only here, as an explicit test step.
	m.ensureAllItemsRendered()
	marker := fixesFindingMarker(t, m.renderedLines)
	require.Contains(t, fixesLinesText(m.renderedLines), marker, "precondition: marker is in the canonical transcript")
	require.Positive(t, m.layoutWorkStats().entries, "precondition: history holds materialized entries")
	return m, marker
}

func TestRenderedBackingDroppedOnEmptyReplayReset(t *testing.T) {
	m, marker := newPartialPlainHistory(t)
	BeginReplay(m, PrepareReplay(session.New(session.WithID("empty-reset"))), nil)
	drainCoverageReplay(t, m)
	require.Zero(t, m.layoutWorkStats().entries, "replay reset must drop the previous transcript's entries")
	require.NotContains(t, fixesLinesText(m.renderedLines), marker, "canonical transcript holds no old rows after reset")
	require.NotContains(t, fixesLinesText(m.renderedLines), "Finding", "no heading of the previous transcript survives")
	require.NotContains(t, ansi.Strip(m.View()), "Finding", "viewport shows no old rows after reset")
}

func TestRenderedBackingDroppedOnInvalidateAndReload(t *testing.T) {
	m, marker := newPartialPlainHistory(t)
	m.invalidateAllItems()
	require.Zero(t, m.layoutWorkStats().entries, "invalidation drops every materialized entry")
	require.Empty(t, strings.Join(m.renderedLines, ""), "invalidation clears the mirror rows; its length is kept until the next layout")
	m.ensureAllItemsRendered()
	require.Contains(t, fixesLinesText(m.renderedLines), marker, "rebuild keeps the same transcript content")

	m, marker = newPartialPlainHistory(t)
	_ = m.LoadFromSession(session.New(session.WithID("reloaded")), nil)
	require.Zero(t, m.layoutWorkStats().entries, "session reload must drop the previous transcript's entries")
	require.NotContains(t, fixesLinesText(m.renderedLines), marker, "reload holds no old canonical rows")
	require.NotContains(t, ansi.Strip(m.View()), "Finding", "reload shows no old rows in the viewport")
}

func TestRenderedBackingDroppedOnReplayReconcile(t *testing.T) {
	m, marker := newPartialPlainHistory(t)
	m.renderDirty = true
	require.True(t, ReconcileReplay(m), "a render-only replay runs after geometry invalidation")
	drainCoverageReplay(t, m)
	m.ensureAllItemsRendered()
	require.Contains(t, fixesLinesText(m.renderedLines), marker, "reconciled canonical transcript keeps its content")
}
