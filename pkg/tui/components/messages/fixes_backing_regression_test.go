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

// Plain history prepends keep front headroom in renderedBacking. Every path that
// replaces or clears renderedLines must drop it; otherwise the old transcript
// stays reachable and a later replay can alias stale rows.

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
	require.NotNil(t, m.renderedBacking, "precondition: plain prepend created live front headroom")
	marker := fixesFindingMarker(t, m.renderedLines)
	require.Contains(t, fixesLinesText(m.renderedLines), marker, "precondition: marker is in the canonical transcript")
	return m, marker
}

func TestRenderedBackingDroppedOnEmptyReplayReset(t *testing.T) {
	m, marker := newPartialPlainHistory(t)
	BeginReplay(m, PrepareReplay(session.New(session.WithID("empty-reset"))), nil)
	drainCoverageReplay(t, m)
	require.Nil(t, m.renderedBacking, "replay reset must drop the previous transcript's headroom")
	require.NotContains(t, fixesLinesText(m.renderedLines), marker, "canonical transcript holds no old rows after reset")
	require.NotContains(t, fixesLinesText(m.renderedLines), "Finding", "no heading of the previous transcript survives")
	require.NotContains(t, ansi.Strip(m.View()), "Finding", "viewport shows no old rows after reset")
}

func TestRenderedBackingDroppedOnInvalidateAndReload(t *testing.T) {
	m, marker := newPartialPlainHistory(t)
	m.invalidateAllItems()
	require.Nil(t, m.renderedBacking, "invalidation clears renderedLines and must drop headroom")
	m.ensureAllItemsRendered()
	require.Nil(t, m.renderedBacking, "a rebuild replaces the window without headroom")
	require.Contains(t, fixesLinesText(m.renderedLines), marker, "rebuild keeps the same transcript content")

	m, marker = newPartialPlainHistory(t)
	_ = m.LoadFromSession(session.New(session.WithID("reloaded")), nil)
	require.Nil(t, m.renderedBacking, "session reload must drop the previous transcript's headroom")
	require.NotContains(t, fixesLinesText(m.renderedLines), marker, "reload holds no old canonical rows")
	require.NotContains(t, ansi.Strip(m.View()), "Finding", "reload shows no old rows in the viewport")
}

func TestRenderedBackingDroppedOnReplayReconcile(t *testing.T) {
	m, marker := newPartialPlainHistory(t)
	m.renderDirty = true
	require.True(t, ReconcileReplay(m), "a render-only replay runs after geometry invalidation")
	drainCoverageReplay(t, m)
	require.Nil(t, m.renderedBacking, "render-only replay replaces renderedLines and must drop headroom")
	require.Contains(t, fixesLinesText(m.renderedLines), marker, "reconciled canonical transcript keeps its content")
}
