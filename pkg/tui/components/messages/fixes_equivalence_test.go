package messages

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// Gated equivalence trace: DA_FIXES_EQUIV=<outfile> DA_INVESTIGATION_MODE=compact|plain
// DA_INVESTIGATION_MESSAGES=N go test -run TestFixesEquivalenceTrace ./pkg/tui/components/messages/
// Compact history uses fixed 8-item steps, so every step is traced. Plain history
// batches are wall-clock bounded (2 ms), so only deterministic checkpoints are traced.

func fixesStateHash(m *model) (string, string) {
	state := sha256.New()
	fmt.Fprintf(state, "total=%d hist=%d scroll=%d views=%d lines=%d sel=%d focused=%v\n",
		m.totalHeight, m.historyStart, m.scrollOffset, len(m.views), len(m.renderedLines), m.selectedMessageIndex, m.focused)
	for _, line := range m.renderedLines {
		state.Write([]byte(line))
		state.Write([]byte{0})
	}
	for _, off := range m.lineOffsets {
		fmt.Fprintf(state, "o%d,", off)
	}
	keys := make([]int, 0, len(m.renderedItems))
	for k := range m.renderedItems {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		r := m.renderedItems[k]
		fmt.Fprintf(state, "r%d:%d+%d;", k, r.start, r.height)
	}
	view := sha256.Sum256([]byte(m.View()))
	stateSum := state.Sum(nil)
	return hex.EncodeToString(stateSum[:12]), hex.EncodeToString(view[:12])
}

func TestFixesEquivalenceTrace(t *testing.T) {
	out := os.Getenv("DA_FIXES_EQUIV")
	if out == "" {
		t.Skip("set DA_FIXES_EQUIV=<outfile> to write the equivalence trace")
	}
	compact := investigationCompact(t)
	sess := retainedMemoryWorkload(investigationRecords())
	ar := animation.NewRuntime()
	m := investigationNewView(t, ar, compact)
	t.Cleanup(m.StopAnimations)
	var trace strings.Builder
	checkpoint := func(label string) {
		state, view := fixesStateHash(m)
		fmt.Fprintf(&trace, "%s state=%s view=%s\n", label, state, view)
	}

	_ = m.SetSize(120, 40)
	BeginReplay(m, PrepareReplay(sess), nil)
	for i := 0; ; i++ {
		require.Less(t, i, 1_000_000, "replay did not finish")
		done, cmd := ContinueReplay(m)
		runReplayCommand(m, cmd)
		if done {
			break
		}
	}
	checkpoint("replay-complete")

	for i := 0; HistoryPending(m); i++ {
		require.Less(t, i, 10_000, "history did not finish")
		runReplayCommand(m, ContinueHistory(m))
		if compact {
			checkpoint(fmt.Sprintf("history-step-%d", i))
		}
	}
	checkpoint("history-complete")

	for _, size := range [][2]int{{60, 24}, {160, 40}, {100, 24}} {
		_ = m.SetSize(size[0], size[1])
		checkpoint(fmt.Sprintf("resize-%dx%d", size[0], size[1]))
	}

	_ = m.AddAssistantMessage("worker", "")
	for i := range 40 {
		_ = m.AppendToLastMessage("worker", "Streamed **chunk** "+strconv.Itoa(i)+" with `code` and more words to wrap.\n")
		checkpoint(fmt.Sprintf("stream-%d", i))
	}
	call := tools.ToolCall{ID: "trace-call", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"true"}`}}
	_ = m.AddOrUpdateToolCall("worker", call, tools.Tool{Name: "shell"}, types.ToolStatusRunning)
	checkpoint("tool-running")
	_ = m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: call.ID, Response: "ok"}, types.ToolStatusCompleted)
	checkpoint("tool-completed")

	_ = m.Focus()
	m.selectedMessageIndex = len(m.views) / 2
	checkpoint("selected")
	m.scrollToTop()
	checkpoint("scrolled-top")

	require.NoError(t, os.MkdirAll(filepath.Dir(out), 0o755))
	require.NoError(t, os.WriteFile(out, []byte(trace.String()), 0o600))
	t.Logf("trace lines=%d file=%s", strings.Count(trace.String(), "\n"), out)
}

// Gated artifacts: DA_FIXES_ARTIFACTS=<dir>. Per width/height/mode writes ANSI,
// stripped text and anchor metadata at partial, settled, scrolled, active, and resized stages.
func TestFixesArtifacts(t *testing.T) {
	dir := os.Getenv("DA_FIXES_ARTIFACTS")
	if dir == "" {
		t.Skip("set DA_FIXES_ARTIFACTS=<dir> to write rendered artifacts")
	}
	records := investigationRecords()
	for _, compact := range []bool{false, true} {
		mode := "plain"
		if compact {
			mode = "compact"
		}
		for _, width := range []int{60, 100, 160} {
			for _, height := range []int{24, 40} {
				ar := animation.NewRuntime()
				m := NewScrollableView(ar, width, height, &service.SessionState{}).(*model)
				if compact {
					_ = m.SetCompactWorkLog(true)
				}
				t.Cleanup(m.StopAnimations)
				_ = m.SetSize(width, height)
				sess := retainedMemoryWorkload(records)
				BeginReplay(m, PrepareReplay(sess), nil)
				done, cmd := ContinueReplay(m)
				runReplayCommand(m, cmd)
				writeFixesArtifact(t, dir, mode, width, height, "partial", m, done)
				for range 1_000_000 {
					done, cmd = ContinueReplay(m)
					runReplayCommand(m, cmd)
					if done {
						break
					}
				}
				for range 10_000 {
					if !HistoryPending(m) {
						break
					}
					runReplayCommand(m, ContinueHistory(m))
				}
				writeFixesArtifact(t, dir, mode, width, height, "settled", m, true)
				m.scrollToTop()
				writeFixesArtifact(t, dir, mode, width, height, "scrolled", m, true)
				call := tools.ToolCall{ID: "artifact-call", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"true"}`}}
				_ = m.AddOrUpdateToolCall("worker", call, tools.Tool{Name: "shell"}, types.ToolStatusRunning)
				writeFixesArtifact(t, dir, mode, width, height, "active-tool", m, true)
				_ = m.SetSize(width+20, height+4)
				writeFixesArtifact(t, dir, mode, width, height, "resized", m, true)
				m.StopAnimations()
			}
		}
	}
}

func writeFixesArtifact(t *testing.T, dir, mode string, width, height int, stage string, m *model, replayDone bool) {
	t.Helper()
	view := m.View()
	base := filepath.Join(dir, fmt.Sprintf("%s-w%d-h%d-%s", mode, width, height, stage))
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(base+".ansi", []byte(view), 0o600))
	require.NoError(t, os.WriteFile(base+".txt", []byte(ansi.Strip(view)), 0o600))
	var meta strings.Builder
	fmt.Fprintf(&meta, "mode=%s width=%d height=%d stage=%s replay_done=%v history_pending=%v\n", mode, width, height, stage, replayDone, HistoryPending(m))
	fmt.Fprintf(&meta, "total_height=%d history_start=%d scroll_offset=%d views=%d rendered_lines=%d\n", m.totalHeight, m.historyStart, m.scrollOffset, len(m.views), len(m.renderedLines))
	fmt.Fprintf(&meta, "line_offsets=%v\n", m.lineOffsets)
	keys := make([]int, 0, len(m.renderedItems))
	for k := range m.renderedItems {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		r := m.renderedItems[k]
		fmt.Fprintf(&meta, "anchor index=%d start=%d height=%d\n", k, r.start, r.height)
	}
	require.NoError(t, os.WriteFile(base+".meta", []byte(meta.String()), 0o600))
}
