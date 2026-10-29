package messages

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// Gated mid-paging coverage: DA_FIXES_COVERAGE=<dir> DA_INVESTIGATION_MODE=compact|plain
// DA_INVESTIGATION_MESSAGES=N go test -run TestFixesCoverage ./pkg/tui/components/messages/
// Each scenario writes <dir>/<scenario>-<mode>.txt, one line per checkpoint.
// The oracle is a canonical rebuild (invalidateAllItems + ensureAllItemsRendered)
// compared on geometry, so a match means the incremental state equals a fresh rebuild.

func fixesCoverageDir(tb testing.TB) string {
	tb.Helper()
	dir := os.Getenv("DA_FIXES_COVERAGE")
	if dir == "" {
		tb.Skip("set DA_FIXES_COVERAGE=<dir> to write mid-paging coverage traces")
	}
	return dir
}

type fixesTrace struct {
	name string
	b    strings.Builder
}

func (tr *fixesTrace) step(label string, m *model) {
	state, view := fixesStateHash(m)
	fmt.Fprintf(&tr.b, "%s state=%s view=%s scroll=%d hist=%d sel=%v:%d-%d user=%v\n",
		label, state, view, m.scrollOffset, m.historyStart, m.selection.active, m.selection.startLine, m.selection.endLine, m.userHasScrolled)
}

func (tr *fixesTrace) oracle(label string, m *model) {
	before := fixesGeometryHash(m)
	m.InvalidateRenderCaches()
	m.ensureAllItemsRendered()
	after := fixesGeometryHash(m)
	match := "match"
	if before != after {
		match = "mismatch"
	}
	fmt.Fprintf(&tr.b, "%s oracle=%s incremental=%s canonical=%s\n", label, match, before, after)
}

func (tr *fixesTrace) write(tb testing.TB, dir, mode string) {
	tb.Helper()
	require.NoError(tb, os.MkdirAll(dir, 0o755))
	require.NoError(tb, os.WriteFile(filepath.Join(dir, tr.name+"-"+mode+".txt"), []byte(tr.b.String()), 0o600))
}

func fixesGeometryHash(m *model) string {
	h := sha256.New()
	fmt.Fprintf(h, "total=%d hist=%d views=%d\n", m.totalHeight, m.historyStart, len(m.views))
	for _, line := range m.renderedLines {
		h.Write([]byte(line))
		h.Write([]byte{0})
	}
	for _, off := range m.lineOffsets {
		fmt.Fprintf(h, "o%d,", off)
	}
	keys := make([]int, 0, len(m.renderedItems))
	for k := range m.renderedItems {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		r := m.renderedItems[k]
		fmt.Fprintf(h, "r%d:%d+%d;", k, r.start, r.height)
	}
	for _, r := range m.workRanges {
		fmt.Fprintf(h, "w%d:%d+%d;", r.anchor, r.start, r.height)
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

func drainCoverageReplay(tb testing.TB, m *model) {
	tb.Helper()
	for i := 0; ; i++ {
		require.Less(tb, i, 1_000_000, "replay did not finish")
		done, cmd := ContinueReplay(m)
		runReplayCommand(m, cmd)
		if done {
			return
		}
	}
}

// Reader scrolled mid-paging with a live selection, plus width change, user
// message and assistant chunk between history steps, and oracle checks.
func TestFixesCoverageMidPagingReaderSelection(t *testing.T) {
	dir := fixesCoverageDir(t)
	compact := investigationCompact(t)
	m := investigationNewView(t, animation.NewRuntime(), compact)
	t.Cleanup(m.StopAnimations)
	_ = m.SetSize(100, 24)
	BeginReplay(m, PrepareReplay(retainedMemoryWorkload(investigationRecords())), nil)
	drainCoverageReplay(t, m)
	tr := &fixesTrace{name: "mid-paging-reader"}
	frames := filepath.Join(dir, "frames-midpaging")
	tr.step("settled", m)
	writeFixesArtifact(t, frames, investigationModeName(compact), 100, 24, "settled", m, true)
	for range 3 {
		m.scrollUp()
	}
	m.selection.active = true
	m.selection.startLine, m.selection.endLine = m.scrollOffset+1, m.scrollOffset+4
	m.selection.anchorStartLine, m.selection.anchorEndLine = m.selection.startLine, m.selection.endLine
	tr.step("reader-selected", m)
	writeFixesArtifact(t, frames, investigationModeName(compact), 100, 24, "reader-selected", m, true)
	for i := 0; HistoryPending(m); i++ {
		require.Less(t, i, 10_000, "history did not finish")
		runReplayCommand(m, ContinueHistory(m))
		tr.step(fmt.Sprintf("history-%d", i), m)
		switch i {
		case 1:
			_ = m.AddUserMessage("coverage user request during paging")
			tr.step("user-added", m)
		case 2:
			_ = m.AddAssistantMessage("worker", "")
			_ = m.AppendToLastMessage("worker", "Coverage assistant chunk during paging.")
			tr.step("assistant-chunk", m)
		case 3:
			_ = m.SetSize(60, 20)
			tr.step("resized-60x20", m)
		case 5:
			_ = m.SetSize(100, 24)
			tr.step("resized-100x24", m)
		}
		if i == 3 {
			tr.oracle(fmt.Sprintf("oracle-history-%d", i), m)
			writeFixesArtifact(t, frames, investigationModeName(compact), 100, 24, fmt.Sprintf("history-%d", i), m, false)
		}
	}
	tr.oracle("oracle-complete", m)
	tr.step("complete", m)
	writeFixesArtifact(t, frames, investigationModeName(compact), 100, 24, "complete", m, true)
	tr.write(t, dir, investigationModeName(compact))
}

// Expanded Work straddling the history boundary: long exchange at height 8,
// every Work block expanded, reader inside the expanded block.
func TestFixesCoverageExpandedWorkStraddle(t *testing.T) {
	dir := fixesCoverageDir(t)
	m := investigationNewView(t, animation.NewRuntime(), true)
	t.Cleanup(m.StopAnimations)
	_ = m.SetSize(82, 8)
	_ = m.SetVisibleReplies(msgtypes.VisibleRepliesLatest)
	BeginReplay(m, PrepareReplay(workHistoryLongExchange()), nil)
	drainCoverageReplay(t, m)
	tr := &fixesTrace{name: "expanded-work-straddle"}
	tr.step("settled", m)
	for _, block := range m.workHeaderBySource {
		block.SetExpandedImmediately(true)
	}
	m.invalidateAllItems()
	m.ensureAllItemsRendered()
	tr.step("work-expanded", m)
	if len(m.workRanges) > 0 {
		block := m.workRanges[0]
		m.setScrollOffset(block.start + block.height/2)
		m.userHasScrolled = true
	}
	tr.step("reader-in-work", m)
	frames := filepath.Join(dir, "frames-straddle")
	writeFixesArtifact(t, frames, "compact", 82, 8, "reader-in-work", m, true)
	for i := 0; HistoryPending(m); i++ {
		require.Less(t, i, 10_000, "history did not finish")
		runReplayCommand(m, ContinueHistory(m))
		tr.step(fmt.Sprintf("history-%d", i), m)
		if i == 4 {
			tr.oracle(fmt.Sprintf("oracle-history-%d", i), m)
		}
	}
	tr.oracle("oracle-complete", m)
	tr.step("complete", m)
	tr.write(t, dir, "compact")
}

// Fixed-clock reply handoff triggered immediately after history completes.
func TestFixesCoverageHandoffAfterHistory(t *testing.T) {
	dir := fixesCoverageDir(t)
	compact := investigationCompact(t)
	scheduler := &replyHandoffScheduler{now: testHandoffTime()}
	runtime := animation.NewRuntimeWithScheduler(scheduler)
	m := NewScrollableView(runtime, 84, 24, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations(); runtime.Stop() })
	_ = m.SetCompactWorkLog(compact)
	_ = m.SetVisibleReplies(msgtypes.VisibleRepliesLatest)
	BeginReplay(m, PrepareReplay(retainedMemoryWorkload(investigationRecords())), nil)
	drainCoverageReplay(t, m)
	for i := 0; HistoryPending(m); i++ {
		require.Less(t, i, 10_000, "history did not finish")
		runReplayCommand(m, ContinueHistory(m))
	}
	tr := &fixesTrace{name: "handoff-after-history"}
	tr.step("history-complete", m)
	_ = m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "incoming handoff answer after history"))
	tr.step("incoming-added", m)
	for i := range 80 {
		cmd := runtime.Continue()
		if cmd == nil {
			fmt.Fprintf(&tr.b, "tick-%d idle\n", i)
			break
		}
		raw, ok := cmd().(animation.TickMsg)
		require.True(t, ok)
		accepted, ok := runtime.Accept(raw)
		require.True(t, ok)
		_, _ = m.Update(accepted)
		phase := "none"
		if m.replyHandoff != nil {
			phase = fmt.Sprint(m.replyHandoff.Phase())
		}
		tr.step(fmt.Sprintf("tick-%d phase=%s stripped=%d", i, phase, len(ansi.Strip(m.View()))), m)
	}
	tr.write(t, dir, investigationModeName(compact))
}

func investigationModeName(compact bool) string {
	if compact {
		return "compact"
	}
	return "plain"
}
