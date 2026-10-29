package messages

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	builtinshell "github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	tuimessages "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func fixesChildStateSession() *session.Session {
	sess := session.New(session.WithID("fixes-child-state"))
	sess.AddMessage(session.UserMessage("long shell and thinking exchange"))
	for i := range 30 {
		call := tools.ToolCall{ID: fmt.Sprintf("child-shell-%02d", i), Type: "function", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: fmt.Sprintf(`{"cmd":"seq %d"}`, i)}}
		sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{
			Role:            chat.MessageRoleAssistant,
			ToolCalls:       []tools.ToolCall{call},
			ToolDefinitions: []tools.Tool{{Name: builtinshell.ToolNameShell}},
			Presentation: []chat.AssistantPart{
				{Type: chat.AssistantPartReasoning, Text: fmt.Sprintf("CHILD_THOUGHT_%02d", i)},
				{Type: chat.AssistantPartContent, Text: fmt.Sprintf("CHILD_REPLY_%02d", i)},
				{Type: chat.AssistantPartToolCall, ToolCallID: call.ID},
			},
		}))
		var out strings.Builder
		for line := range 120 {
			fmt.Fprintf(&out, "shell-out-%02d-line-%03d\n", i, line)
		}
		sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleTool, ToolCallID: call.ID, Content: out.String()}))
	}
	sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{
		Role:         chat.MessageRoleAssistant,
		Content:      "CHILD_FINAL",
		Presentation: []chat.AssistantPart{{Type: chat.AssistantPartContent, Text: "CHILD_FINAL"}},
	}))
	return sess
}

type fixesShellView interface {
	SetExpanded(expanded bool)
	IsExpanded() bool
	BodyScrollState() (int, bool, bool)
}

// fixesSettleTicks advances the fixed clock until no animation is registered.
func fixesSettleTicks(t *testing.T, runtime *animation.Runtime, m *model) {
	t.Helper()
	for range 600 {
		cmd := runtime.Continue()
		if cmd == nil {
			return
		}
		raw, ok := cmd().(animation.TickMsg)
		require.True(t, ok)
		accepted, ok := runtime.Accept(raw)
		require.True(t, ok)
		_, _ = m.Update(accepted)
	}
	require.FailNow(t, "disclosure animation did not settle within 600 fixed-clock ticks")
}

// fixesBodyRow returns the viewport row showing the expanded shell body, or -1.
func fixesBodyRow(m *model, prefix string) int {
	for row, line := range strings.Split(m.View(), "\n") {
		if strings.Contains(ansi.Strip(line), prefix) {
			return row
		}
	}
	return -1
}

func fixesMarkerRow(m *model, marker string) int {
	for row, line := range strings.Split(m.View(), "\n") {
		if strings.Contains(ansi.Strip(line), marker) {
			return row
		}
	}
	return -1
}

func TestFixesCoverageThemeAndChildState(t *testing.T) {
	dir := fixesCoverageDir(t)
	scheduler := &replyHandoffScheduler{now: testHandoffTime()}
	runtime := animation.NewRuntimeWithScheduler(scheduler)
	m := NewScrollableView(runtime, 100, 16, &service.SessionState{}).(*model)
	previousTheme := styles.CurrentTheme()
	t.Cleanup(func() { m.StopAnimations(); runtime.Stop(); styles.ApplyTheme(previousTheme) })
	_ = m.SetCompactWorkLog(true)
	_ = m.SetSize(100, 16)
	BeginReplay(m, PrepareReplay(fixesChildStateSession()), nil)
	drainCoverageReplay(t, m)

	var shells []fixesShellView
	var reasoning *reasoningblock.Model
	var shellIndices []int
	for index := m.historyStart; index < len(m.views); index++ {
		view := m.views[index]
		if shell, ok := view.(fixesShellView); ok {
			shells = append(shells, shell)
			shellIndices = append(shellIndices, index)
		}
		if block, ok := view.(*reasoningblock.Model); ok && reasoning == nil {
			reasoning = block
		}
	}
	require.GreaterOrEqual(t, len(shells), 2, "fixture must replay at least two shell cards")
	require.NotNil(t, reasoning, "fixture must replay a reasoning block")
	prefix := strings.Replace(m.messages[shellIndices[0]].ToolCall.ID, "child-shell-", "shell-out-", 1) + "-line-"
	marker := prefix + "050"
	tr := &fixesTrace{name: "theme-child-state"}
	frames := filepath.Join(dir, "frames-child-state")

	shells[0].SetExpanded(true)
	reasoning.SetExpandedImmediately(true)
	nested := types.ToolCallMessage("worker", shellCall("fixture-nested", `{"cmd":"printf nested"}`), tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusCompleted)
	nested.Content = "NESTED_CHILD_OUTPUT"
	_ = reasoning.AddToolCall(nested)
	require.True(t, reasoning.SetToolCallExpandedImmediately(nested.ToolCall.ID, true))
	for _, block := range m.workHeaderBySource {
		block.SetExpandedImmediately(true)
	}
	fixesSettleTicks(t, runtime, m)
	m.invalidateAllItems()
	m.ensureAllItemsRendered()
	_ = m.View()
	require.True(t, shells[0].IsExpanded(), "long shell is expanded")
	require.False(t, shells[1].IsExpanded(), "sibling shell stays collapsed")
	require.True(t, reasoning.IsExpanded(), "nested reasoning is expanded")

	target := -1
	for i, line := range m.renderedLines {
		if strings.Contains(ansi.Strip(line), prefix) {
			target = i
			break
		}
	}
	require.GreaterOrEqual(t, target, 0, "the initially visible output window must be present in the loaded history")
	m.setScrollOffset(max(0, target-2))
	m.userHasScrolled = true
	_ = m.View()
	wheel := func(delta int) {
		row := fixesBodyRow(m, prefix)
		require.GreaterOrEqual(t, row, 0, "shell body must be visible for a wheel event")
		_, _ = m.Update(tuimessages.WheelCoalescedMsg{X: 5, Y: row, Delta: delta})
		_ = m.View()
	}
	for range 300 {
		if offset, _, _ := shells[0].BodyScrollState(); offset >= 45 && fixesMarkerRow(m, marker) >= 0 {
			break
		}
		if offset, _, _ := shells[0].BodyScrollState(); offset > 55 {
			wheel(-1)
		} else {
			wheel(1)
		}
	}
	offset, atBottom, ok := shells[0].BodyScrollState()
	require.True(t, ok)
	require.GreaterOrEqual(t, offset, 45, "wheel scrolls the long shell body mid-body")
	require.False(t, atBottom, "mid-body state is not at the bottom bound")
	require.GreaterOrEqual(t, fixesMarkerRow(m, marker), 0, "line 50 is visible in the viewport after the body scroll")
	require.True(t, HistoryPending(m), "child state must be exercised before history finishes")
	require.Contains(t, ansi.Strip(reasoning.View()), "NESTED_CHILD_OUTPUT")
	writeFixesArtifact(t, frames, "compact", 100, 16, "body-mid-scroll", m, true)
	fixesChildLine(tr, "body-mid-scroll", m, shells, reasoning)

	var transcriptBefore int
	for range 300 {
		if offset, _, _ := shells[0].BodyScrollState(); offset == 0 {
			break
		}
		wheel(-1)
	}
	transcriptBefore = m.scrollOffset
	wheel(-1)
	require.Equal(t, transcriptBefore, m.scrollOffset, "wheel at the body top bound is consumed by the child")
	offset, _, _ = shells[0].BodyScrollState()
	require.Zero(t, offset, "body is at its top bound")
	fmt.Fprintf(&tr.b, "wheel-top-bound transcript_before=%d transcript_after=%d\n", transcriptBefore, m.scrollOffset)

	for range 300 {
		if _, atBottom, _ := shells[0].BodyScrollState(); atBottom {
			break
		}
		wheel(1)
	}
	transcriptBefore = m.scrollOffset
	wheel(1)
	require.Equal(t, transcriptBefore, m.scrollOffset, "wheel at the body bottom bound is consumed by the child")
	_, atBottom, _ = shells[0].BodyScrollState()
	require.True(t, atBottom, "body is at its bottom bound")
	fmt.Fprintf(&tr.b, "wheel-bottom-bound transcript_before=%d transcript_after=%d\n", transcriptBefore, m.scrollOffset)
	fixesChildLine(tr, "body-bottom-bound", m, shells, reasoning)
	for range 300 {
		if offset, _, _ := shells[0].BodyScrollState(); offset <= 46 {
			break
		}
		wheel(-1)
	}
	bodyOffset, bottomBeforeTheme, _ := shells[0].BodyScrollState()
	require.Positive(t, bodyOffset)
	require.False(t, bottomBeforeTheme, "paging must preserve a mid-body reader, not only bottom-follow")

	require.NotNil(t, styles.ApplyThemeRef("nord"))
	m.InvalidateRenderCaches()
	m.ensureAllItemsRendered()
	_ = m.View()
	afterTheme, _, _ := shells[0].BodyScrollState()
	require.Equal(t, bodyOffset, afterTheme, "theme switch preserves the shell body scroll")
	require.True(t, shells[0].IsExpanded(), "theme switch preserves shell expansion")
	require.True(t, reasoning.IsExpanded(), "theme switch preserves nested reasoning expansion")
	fixesChildLine(tr, "theme-nord", m, shells, reasoning)

	steps := 0
	for i := 0; HistoryPending(m); i++ {
		steps++
		require.Less(t, i, 10_000, "history did not finish")
		runReplayCommand(m, ContinueHistory(m))
		fixesChildLine(tr, fmt.Sprintf("history-%d", i), m, shells, reasoning)
		if i == 4 {
			tr.oracle("oracle-history-4", m)
			writeFixesArtifact(t, frames, "compact", 100, 16, "history-4", m, false)
		}
	}
	require.Positive(t, steps, "fixture must exercise history steps with expanded children")
	require.Contains(t, ansi.Strip(reasoning.View()), "NESTED_CHILD_OUTPUT", "nested tool remains expanded")
	afterPaging, _, _ := shells[0].BodyScrollState()
	require.Equal(t, bodyOffset, afterPaging, "history paging preserves the shell body scroll")
	require.True(t, shells[0].IsExpanded(), "history paging preserves shell expansion")
	require.False(t, shells[1].IsExpanded(), "history paging keeps the sibling collapsed")
	require.True(t, reasoning.IsExpanded(), "history paging preserves nested reasoning expansion")
	tr.oracle("oracle-complete", m)
	fixesChildLine(tr, "complete", m, shells, reasoning)
	writeFixesArtifact(t, frames, "compact", 100, 16, "complete", m, true)
	tr.write(t, dir, "compact")
}

func fixesChildLine(tr *fixesTrace, label string, m *model, shells []fixesShellView, reasoning *reasoningblock.Model) {
	var parts []string
	for i, shell := range shells {
		offset, atBottom, ok := shell.BodyScrollState()
		parts = append(parts, fmt.Sprintf("shell%d:expanded=%v,body=%d,bottom=%v,ok=%v", i, shell.IsExpanded(), offset, atBottom, ok))
	}
	if reasoning != nil {
		parts = append(parts, fmt.Sprintf("reasoning:expanded=%v", reasoning.IsExpanded()))
	}
	fmt.Fprintf(&tr.b, "%s child=[%s]\n", label, strings.Join(parts, " "))
	tr.step(label, m)
}
