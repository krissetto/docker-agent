package messages

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/workblock"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// expandedWorkCardFixture groups an expanded incoming card under an expanded Work header.
func expandedWorkCardFixture(t *testing.T) (*model, *workblock.Model) {
	t.Helper()
	ar := animation.NewRuntime()
	m := NewScrollableView(ar, 100, 80, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	m.SetPosition(0, 0)
	m.SetCompactWorkLog(true)
	m.addMessage(types.User("request"))
	m.addMessage(types.ToolCallMessage("root", tools.ToolCall{ID: "anchor", Function: tools.FunctionCall{Name: "probe"}}, tools.Tool{Name: "probe"}, types.ToolStatusCompleted))
	incoming := session.UserMessage(strings.Repeat("A long child reply stays inside the Work body. ", 12))
	incoming.InputOrigin, incoming.InputMode, incoming.SenderID, incoming.SenderName = session.InputOriginAgent, "steer", "child-session", "worker"
	m.addMessage(types.Input(incoming))
	m.workProjectionDirty = true
	m.syncWorkProjection()
	work := firstWorkBlock(m)
	require.NotNil(t, work)
	work.SetExpandedImmediately(true)
	m.views[2].(interface{ SetExpanded(bool) }).SetExpanded(true)
	m.invalidateItem(2)
	m.ensureAllItemsRendered()
	return m, work
}

// assertWorkCardInBody checks that the expanded card sits at the Work inset and inside its right edge.
func assertWorkCardInBody(t *testing.T, m *model, work *workblock.Model, stage string) {
	t.Helper()
	m.ensureAllItemsRendered()
	plain := ansi.Strip(work.View())
	require.Contains(t, plain, "From worker", "%s: the incoming card is visible inside Work", stage)
	for line := range strings.SplitSeq(plain, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		require.LessOrEqual(t, ansi.StringWidth(strings.TrimRight(line, " ")), m.contentWidth()-2, "%s: no Work row passes the right inset: %q", stage, line)
		if strings.Contains(line, "worker") || strings.Contains(line, "long child") {
			require.Equal(t, 2, len(line)-len(strings.TrimLeft(line, " ")), "%s: card keeps the Work left inset: %q", stage, line)
		}
	}
}

func TestExpandedWorkCardStaysInBodyAfterTranscriptResetPass(t *testing.T) {
	m, work := expandedWorkCardFixture(t)
	assertWorkCardInBody(t, m, work, "settled")

	// The transcript-wide resize pass sizes every view to full width, Work children included.
	m.views[2].SetSize(m.contentWidth(), 0)
	m.invalidateItem(2)
	assertWorkCardInBody(t, m, work, "after reset pass")
}

func TestExpandedWorkCardStaysInBodyAcrossTerminalWidths(t *testing.T) {
	m, work := expandedWorkCardFixture(t)
	for _, width := range []int{24, 60, 84, 120} {
		m.SetSize(width, 80)
		assertWorkCardInBody(t, m, work, fmt.Sprintf("width %d", width))
	}
}

// The deferred-tail path sizes the Work block to the new width before the transcript
// resize pass runs; that pass must not reset grouped children to full width.
func TestGroupedWorkCardKeepsBodyWidthWhenBlockResizesBeforeResizePass(t *testing.T) {
	m, work := expandedWorkCardFixture(t)
	const width = 84
	m.scrollview.SetSize(width, 80)
	work.SetSize(m.contentWidth(), 0)
	m.SetSize(width, 80)
	card, ok := m.views[2].(interface{ GetSize() (int, int) })
	require.True(t, ok)
	got, _ := card.GetSize()
	require.Equal(t, workblock.SourceContentWidth(m.contentWidth()), got, "grouped card keeps the Work body width after resize")
	assertWorkCardInBody(t, m, work, "after block-first resize")
}

// Repeated renders at unchanged widths must reuse the transcript and child output.
func TestRepeatedViewAtUnchangedWidthDoesNotRebuildWorkBody(t *testing.T) {
	m, work := expandedWorkCardFixture(t)
	_ = m.View()
	rebuilds := m.transcriptRebuilds
	bodyView := work.View()
	for range 3 {
		_ = m.View()
		m.SetSize(m.width, m.height)
	}
	require.Equal(t, rebuilds, m.transcriptRebuilds, "repeated View at the same width does not rebuild the transcript")
	require.Equal(t, bodyView, work.View(), "the Work body keeps its cached output")
}
