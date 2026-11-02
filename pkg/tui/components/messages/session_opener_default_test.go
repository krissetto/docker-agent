package messages

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// toggleOpenerView flips the view's own disclosure the way a click or Enter does,
// then drives its transition to rest.
func toggleOpenerView(t *testing.T, m *model, index int) {
	t.Helper()
	toggler, ok := m.views[index].(interface{ Toggle() })
	require.True(t, ok)
	toggler.Toggle()
	m.invalidateItem(index)
	openerSettle(t, m.ar, m)
}

func TestSessionOpenerDefaultOpensBodyOnceAboveWork(t *testing.T) {
	m := openerModel(t)
	m.SetCompactWorkLog(true)
	m.addMessage(types.Input(openerAgentInput("assignment body")))
	for i := range 3 {
		call := tools.ToolCall{ID: fmt.Sprintf("default-call-%d", i), Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"true"}`}}
		m.addMessage(types.ToolCallMessage("root", call, tools.Tool{Name: "shell"}, types.ToolStatusCompleted))
	}
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "FINAL_DEFAULT_REPLY"))
	openerSettle(t, m.ar, m)

	plain := ansi.Strip(m.View())
	require.Equal(t, 1, openerCount(plain, "assignment body"), "the opener body is visible exactly once")
	require.Less(t, strings.Index(plain, "assignment body"), strings.Index(plain, "Work ["), "the opener body stays above collapsed Work")
	require.Equal(t, 1, openerCount(plain, openerHeader))
	require.Equal(t, 1, openerCount(plain, "FINAL_DEFAULT_REPLY"))
}

func TestSessionOpenerDefaultLeavesLaterReportsCollapsed(t *testing.T) {
	m := openerModel(t)
	m.SetCompactWorkLog(true)
	m.addMessage(types.User("human request"))
	m.addMessage(types.Input(openerAgentInput("first report body")))
	m.addMessage(types.Input(openerAgentInput("LATER_REPORT_BODY")))
	openerSettle(t, m.ar, m)

	plain := ansi.Strip(m.View())
	require.NotContains(t, plain, "first report body", "a report after a human request is not the opener")
	require.NotContains(t, plain, "LATER_REPORT_BODY", "later incoming reports keep their collapsed default")
	require.Zero(t, openerCount(plain, openerHeader), "later reports stay folded inside collapsed Work")
	require.Contains(t, plain, "Work [+]")

	m.toggleWork(firstWorkBlock(m))
	openerSettle(t, m.ar, m)
	expanded := ansi.Strip(m.View())
	require.Contains(t, expanded, "Work [-]")
	require.Equal(t, 2, openerCount(expanded, openerHeader), "expanded Work shows each report header")
	require.NotContains(t, expanded, "first report body", "expanded Work keeps report bodies collapsed")
	require.NotContains(t, expanded, "LATER_REPORT_BODY", "expanded Work keeps report bodies collapsed")
}

func TestSessionOpenerDefaultFollowsCompactModeAndYieldsToUser(t *testing.T) {
	m := openerModel(t)
	m.addMessage(types.Input(openerAgentInput("assignment body")))
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "FINAL_TOGGLE_REPLY"))
	openerSettle(t, m.ar, m)
	require.NotContains(t, ansi.Strip(m.View()), "assignment body", "compact off keeps the collapsed default")

	m.SetCompactWorkLog(true)
	openerSettle(t, m.ar, m)
	require.Equal(t, 1, openerCount(ansi.Strip(m.View()), "assignment body"), "compact on opens the session opener")

	toggleOpenerView(t, m, 0)
	require.NotContains(t, ansi.Strip(m.View()), "assignment body", "the user collapse is not overridden")
	m.SetCompactWorkLog(false)
	openerSettle(t, m.ar, m)
	m.SetCompactWorkLog(true)
	openerSettle(t, m.ar, m)
	require.NotContains(t, ansi.Strip(m.View()), "assignment body", "the user collapse persists across compact toggles")
	toggleOpenerView(t, m, 0)
	require.Equal(t, 1, openerCount(ansi.Strip(m.View()), "assignment body"), "the user reopen persists")
}

func TestSessionOpenerDefaultReplayOpensOpenerOnly(t *testing.T) {
	sess := session.New(session.WithID("opener-default-replay"))
	sess.AddMessage(openerAgentInput("replay body"))
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{
		Role:         chat.MessageRoleAssistant,
		Content:      "REPLAY_DEFAULT_FINAL",
		Presentation: []chat.AssistantPart{{Type: chat.AssistantPartContent, Text: "REPLAY_DEFAULT_FINAL"}},
	}))
	m := openerModel(t)
	m.SetCompactWorkLog(true)
	BeginReplay(m, PrepareReplay(sess), nil)
	finishPreparedReplay(t, m)
	openerSettle(t, m.ar, m)
	require.Equal(t, 1, openerCount(ansi.Strip(m.View()), "replay body"))
}

// A clean View must not re-resolve the opener: identity is refreshed only on
// dirty syncs, so cached frames never rescan the transcript.
func TestSessionOpenerResolvesOnlyOnDirtySync(t *testing.T) {
	m := openerModel(t)
	m.SetCompactWorkLog(true)
	m.addMessage(types.Input(openerAgentInput("assignment body")))
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "FINAL_CLEAN_REPLY"))
	openerSettle(t, m.ar, m)
	opener := m.messages[0]
	require.Same(t, opener, m.sessionOpener)

	m.sessionOpener = nil
	m.workProjectionDirty = false
	_ = m.View()
	require.Nil(t, m.sessionOpener, "a clean View does not re-resolve the opener")

	// The projection and its opener depend on message structure and grouping, not
	// on width or theme, so a pure epoch is not a projection input: it must not
	// re-resolve the opener or rebuild the projection.
	before := m.layoutWorkStats().projectionRebuilds
	m.invalidateAllItems()
	_ = m.View()
	require.Nil(t, m.sessionOpener, "a pure epoch does not re-resolve the opener")
	require.Equal(t, before, m.layoutWorkStats().projectionRebuilds, "a pure epoch rebuilds no projection")

	// A structural mutation dirties the projection, and the next View resolves it.
	m.invalidateTranscriptStructure()
	_ = m.View()
	require.Same(t, opener, m.sessionOpener, "a structural mutation resolves the opener again")
}
