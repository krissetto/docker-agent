package messages

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// Assistant replies retained beside Work keep one separator per block, and
// expanded Work keeps one breathing row under its header.
func newWorkSpacingFixture(t *testing.T, workExpanded bool) *model {
	t.Helper()
	ar := animation.NewRuntime()
	m := NewScrollableView(ar, 84, 60, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	m.SetPosition(0, 0)
	m.SetCompactWorkLog(true)
	m.SetVisibleReplies(messages.VisibleRepliesThree)

	user := types.User("request")
	m.messages = append(m.messages, user)
	m.views = append(m.views, m.createMessageView(user))
	m.AppendReasoning("root", "planning the work")
	call := tools.ToolCall{ID: "spacing-tool", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"printf spacing"}`}}
	m.AddOrUpdateToolCall("root", call, tools.Tool{Name: "shell"}, types.ToolStatusCompleted)
	m.AppendToolOutput(&runtime.ToolCallOutputEvent{ToolCallID: call.ID, Output: "spacing output"})
	for _, text := range []string{"first retained answer", "para one\n\npara two", "third retained answer"} {
		msg := types.Agent(types.MessageTypeAssistant, "root", text)
		m.messages = append(m.messages, msg)
		m.views = append(m.views, m.createMessageView(msg))
	}
	for i, msg := range m.messages {
		m.responseGroups[msg] = uint64(i + 1)
	}
	m.nextResponseGroup = uint64(len(m.messages) + 1)
	m.workProjectionDirty = true
	m.syncWorkProjection()
	if workExpanded {
		if work := firstWorkBlock(m); work != nil {
			work.SetExpandedImmediately(true)
		}
		for _, v := range m.views {
			if block, ok := v.(*reasoningblock.Model); ok {
				block.SetExpandedImmediately(true)
				block.SetToolCallExpandedImmediately(call.ID, true)
			}
		}
	}
	m.invalidateAllItems()
	m.ensureAllItemsRendered()
	m.userHasScrolled = false
	m.scrollOffset = 0
	m.View()
	return m
}

func maxBlankRun(rows []string) int {
	best, run := 0, 0
	for _, row := range rows {
		if strings.TrimSpace(row) == "" {
			run++
			best = max(best, run)
			continue
		}
		run = 0
	}
	return best
}

func TestWorkSpacingCollapsedRetainedAssistantRepliesUseSingleSeparators(t *testing.T) {
	m := newWorkSpacingFixture(t, false)
	rows := strings.Split(ansi.Strip(m.View()), "\n")
	work, last := -1, -1
	for i, row := range rows {
		if work < 0 && strings.Contains(row, "Work [+]") {
			work = i
		}
		if strings.Contains(row, "third retained answer") {
			last = i
		}
	}
	require.GreaterOrEqual(t, work, 0, "collapsed Work header is visible")
	require.Greater(t, last, work)
	require.LessOrEqual(t, maxBlankRun(rows[work:last+1]), 1, "no doubled gap from the Work header through the retained replies")
}

func TestWorkSpacingExpandedKeepsHeaderBreathingRowAndParagraphs(t *testing.T) {
	m := newWorkSpacingFixture(t, true)
	rows := strings.Split(ansi.Strip(m.View()), "\n")
	work := -1
	for i, row := range rows {
		if strings.Contains(row, "Work [-]") {
			work = i
			break
		}
	}
	require.GreaterOrEqual(t, work, 0)
	require.Empty(t, strings.TrimSpace(rows[work+1]), "one breathing row under the expanded Work header")
	require.Contains(t, rows[work+2], "Thinking")

	para := -1
	for i, row := range rows {
		if strings.Contains(row, "para one") {
			para = i
			break
		}
	}
	require.GreaterOrEqual(t, para, 0)
	blankBetween := 0
	for i := para + 1; i < len(rows) && strings.TrimSpace(rows[i]) == ""; i++ {
		blankBetween++
	}
	require.Equal(t, 1, blankBetween, "paragraph break inside a reply is preserved as exactly one row")
}

// Layout separators outside compact Work and for agent-origin replies must stay as before.
func TestDisplayNeedsSeparatorKeepsNonCompactAndAgentOriginRules(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)

	plain := NewScrollableView(ar, 84, 20, &service.SessionState{}).(*model)
	t.Cleanup(plain.StopAnimations)
	user := types.User("request")
	reply := types.Agent(types.MessageTypeAssistant, "root", "plain reply")
	plain.messages = append(plain.messages, user, reply)
	require.True(t, plain.displayNeedsSeparator(0), "non-compact user to assistant keeps its separator")

	compact := NewScrollableView(ar, 84, 20, &service.SessionState{}).(*model)
	t.Cleanup(compact.StopAnimations)
	compact.SetCompactWorkLog(true)
	child := types.Agent(types.MessageTypeAssistant, "child", "agent-origin reply")
	child.InputOrigin = session.InputOriginAgent
	compact.messages = append(compact.messages, types.User("request"), child)
	require.True(t, compact.displayNeedsSeparator(0), "agent-origin replies keep their separator in compact mode")
}

// Rendering must not mutate source messages.
func TestWorkSpacingFixtureLeavesSourceMessagesUnchanged(t *testing.T) {
	m := newWorkSpacingFixture(t, false)
	before := messageContents(m.messages)
	_ = m.View()
	_ = m.View()
	require.Equal(t, before, messageContents(m.messages))
}
