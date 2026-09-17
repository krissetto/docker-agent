package leantui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/runtime"
)

func leanHelpText(m *model, width int) string {
	return ansi.Strip(strings.Join(m.screen.Transcript.BlockLines(0, width), "\n"))
}

func leanHelpKey(t *testing.T, sequence string) ui.Key {
	t.Helper()
	var parser ui.InputParser
	keys := parser.Feed([]byte(sequence))
	require.Len(t, keys, 1)
	return keys[0]
}

func TestLeanHelpCategoriesAndWidth(t *testing.T) {
	t.Parallel()
	m := bareModel(24)
	require.True(t, m.handleSlash(t.Context(), "/help", busySubmitSteer))
	require.Equal(t, 1, m.screen.Transcript.BlockCount())
	text := leanHelpText(m, 120)
	previous := -1
	for _, heading := range []string{
		"Lean terminal help", "Composer and sending", "Cursor and history", "Editing",
		"Command and argument completion", "Response and application controls",
		"Tool confirmation (takes priority over all other keys)", "Terminal key aliases",
	} {
		position := strings.Index(text, heading)
		require.Greater(t, position, previous, heading)
		previous = position
	}
	for _, claim := range []string{
		"Independent lean app only", "/help: show this help", "Type / for the available command list",
		"LF / Ctrl+J and CR / Ctrl+M decode as Enter", "Shift+Enter: insert newline",
		"! prefix", "No app-level selection", "withdraw all eligible pending messages",
		"This replaces the draft", "follows interrupt-confirmation", "current model/runtime supports it",
		"including Enter, Ctrl+C and Ctrl+D, are ignored", "CSI / SS3", "Kitty",
	} {
		assert.Contains(t, text, claim)
	}
	for _, obsolete := range []string{"F1", "Ctrl+?", "newest pending", "Option+Up  edit", "/new       ", "/model     "} {
		assert.NotContains(t, text, obsolete)
	}
	for _, width := range []int{24, 80, 120} {
		for _, line := range m.screen.Transcript.BlockLines(0, width) {
			assert.LessOrEqual(t, ui.DisplayWidth(line), width)
		}
		// Wrapping must not truncate any content, including at ordinary 80 columns.
		normalize := func(s string) string { return strings.Join(strings.Fields(s), "") }
		assert.Equal(t, normalize(text), normalize(leanHelpText(m, width)))
	}
}

func TestLeanHelpParserAliases(t *testing.T) {
	t.Parallel()
	m := bareModel(24)
	m.commitHelp()
	text := leanHelpText(m, 120)
	for _, tc := range []struct {
		label     string
		sequences []string
		key       ui.KeyType
	}{
		{"Enter", []string{"\r", "\n", "\x1b[13u"}, ui.KeyEnter},
		{"Shift+Enter", []string{"\x1b[13;2u"}, ui.KeyShiftEnter},
		{"Alt+Enter", []string{"\x1b\r", "\x1b\n", "\x1b[13;3u"}, ui.KeyAltEnter},
		{"Alt+Up", []string{"\x1b[1;3A"}, ui.KeyAltUp},
		{"Tab", []string{"\t", "\x1b[9u"}, ui.KeyTab},
		{"Shift+Tab", []string{"\x1b[Z", "\x1b[9;2u"}, ui.KeyShiftTab},
		{"Backspace / Ctrl+H", []string{"\x7f", "\x08"}, ui.KeyBackspace},
		{"Delete", []string{"\x1b[3~"}, ui.KeyDelete},
		{"Up / Down", []string{"\x1b[A", "\x1bOA"}, ui.KeyUp},
		{"Up / Down", []string{"\x1b[B", "\x1bOB"}, ui.KeyDown},
		{"Left / Right", []string{"\x1b[D", "\x1bOD"}, ui.KeyLeft},
		{"Left / Right", []string{"\x1b[C", "\x1bOC"}, ui.KeyRight},
		{"Alt+B / Alt+F", []string{"\x1bb", "\x1b[1;5D", "\x1b[1;3D", "\x1b[1;2D"}, ui.KeyWordLeft},
		{"Alt+B / Alt+F", []string{"\x1bf", "\x1b[1;5C", "\x1b[1;3C", "\x1b[1;2C"}, ui.KeyWordRight},
		{"Home / Ctrl+A", []string{"\x01", "\x1b[H", "\x1bOH", "\x1b[1~", "\x1b[7~", "\x1b[97;5u", "\x1b[65;5u"}, ui.KeyHome},
		{"End / Ctrl+E", []string{"\x05", "\x1b[F", "\x1bOF", "\x1b[4~", "\x1b[8~", "\x1b[101;5u", "\x1b[69;5u"}, ui.KeyEnd},
		{"Esc", []string{"\x1b", "\x1b[27u"}, ui.KeyEsc},
		{"Ctrl+C", []string{"\x03", "\x1b[99;5u", "\x1b[67;5u"}, ui.KeyCtrlC},
		{"Ctrl+D", []string{"\x04", "\x1b[100;5u", "\x1b[68;5u"}, ui.KeyCtrlD},
		{"Ctrl+U / Ctrl+K", []string{"\x15", "\x1b[117;5u", "\x1b[85;5u"}, ui.KeyCtrlU},
		{"Ctrl+U / Ctrl+K", []string{"\x0b", "\x1b[107;5u", "\x1b[75;5u"}, ui.KeyCtrlK},
		{"Ctrl+W / Alt+Backspace", []string{"\x17", "\x1b\x7f", "\x1b\x08", "\x1b[127;3u", "\x1b[8;3u", "\x1b[119;5u", "\x1b[87;5u"}, ui.KeyCtrlW},
		{"Ctrl+L", []string{"\x0c", "\x1b[108;5u", "\x1b[76;5u"}, ui.KeyCtrlL},
	} {
		assert.Contains(t, text, tc.label)
		for _, sequence := range tc.sequences {
			assert.Equal(t, tc.key, leanHelpKey(t, sequence).Typ, "%s: %q", tc.label, sequence)
		}
	}
}

func TestLeanHelpEditingHandlerContext(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, sequence, draft, want string
	}{
		{"character left", "\x1b[D", "one two", "one tw|o"},
		{"word left", "\x1b[1;2D", "one two", "one |two"},
		{"line start", "\x01", "one\ntwo", "one\n|two"},
		{"backspace", "\x08", "one two", "one tw|"},
		{"word delete", "\x1b\x7f", "one two", "one |"},
		{"delete to start", "\x15", "one\ntwo", "one\n|"},
		{"newline", "\x1b[13;2u", "one", "one\n|"},
		{"paste", "\x1b[200~two\r\nthree\x1b[201~", "one", "onetwo\nthree|"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := bareModel(24)
			m.screen.Editor.SetText(tc.draft)
			m.handleKey(t.Context(), leanHelpKey(t, tc.sequence))
			m.screen.Editor.Insert([]rune{'|'})
			assert.Equal(t, tc.want, m.screen.Editor.Text())
		})
	}
	for _, sequence := range []string{"\x1b[3~", "\x04", "\x0b"} {
		m := bareModel(24)
		m.screen.Editor.SetText("abc")
		m.screen.Editor.MoveLeft()
		m.handleKey(t.Context(), leanHelpKey(t, sequence))
		assert.Equal(t, "ab", m.screen.Editor.Text())
		assert.False(t, m.quitting)
	}
}

func TestLeanHelpHistoryAndCompletionContext(t *testing.T) {
	t.Parallel()
	m := bareModel(24)
	require.NoError(t, m.screen.Editor.RememberHistory("history"))
	m.screen.Editor.SetText("first\nlast")
	m.handleKey(t.Context(), leanHelpKey(t, "\x1b[A"))
	assert.Equal(t, "first\nlast", m.screen.Editor.Text(), "visual row before history")
	m.handleKey(t.Context(), leanHelpKey(t, "\x1b[A"))
	assert.Equal(t, "history", m.screen.Editor.Text())
	m.handleKey(t.Context(), leanHelpKey(t, "\x1b[B"))
	assert.Equal(t, "first\nlast", m.screen.Editor.Text(), "restore saved draft")

	m.screen.Autocomplete.SetCommands([]ui.Command{{Name: "help"}, {Name: "hero"}})
	m.screen.Editor.SetText("/he")
	require.True(t, m.screen.Autocomplete.Sync(m.screen.Editor.Text()))
	m.handleKey(t.Context(), leanHelpKey(t, "\x1b[B"))
	selected, ok := m.screen.Autocomplete.Current()
	require.True(t, ok)
	assert.Equal(t, "hero", selected.Name)
	m.handleKey(t.Context(), leanHelpKey(t, "\x1b[B"))
	selected, _ = m.screen.Autocomplete.Current()
	assert.Equal(t, "hero", selected.Name, "no wrapping")
	m.handleKey(t.Context(), leanHelpKey(t, "\t"))
	assert.Equal(t, "/hero ", m.screen.Editor.Text())
	assert.Zero(t, m.screen.Transcript.BlockCount(), "Tab does not submit")

	m.screen.Autocomplete.SetScopedCommands("model ", []ui.Command{{Name: "choice", Value: "provider/model"}})
	m.screen.Editor.SetText("/model ")
	require.True(t, m.screen.Autocomplete.Sync(m.screen.Editor.Text()))
	m.handleKey(t.Context(), leanHelpKey(t, "\t"))
	assert.Equal(t, "/model provider/model ", m.screen.Editor.Text())
}

func TestLeanHelpEnterAliasesExecuteCompletion(t *testing.T) {
	t.Parallel()
	for _, sequence := range []string{"\r", "\n", "\x1b[13u"} {
		m := bareModel(24)
		m.screen.Autocomplete.SetCommands([]ui.Command{{Name: "help"}})
		m.screen.Editor.SetText("/he")
		require.True(t, m.screen.Autocomplete.Sync(m.screen.Editor.Text()))
		m.handleKey(t.Context(), leanHelpKey(t, sequence))
		assert.Equal(t, 1, m.screen.Transcript.BlockCount())
		assert.Contains(t, leanHelpText(m, 120), "Lean terminal help")
		assert.True(t, m.screen.Editor.IsEmpty())
	}
}

func TestLeanHelpIdleEscapeKeepsMatchingDraftDismissed(t *testing.T) {
	t.Parallel()
	for _, sequence := range []string{"\x1b", "\x1b[27u"} {
		m := bareModel(24)
		m.screen.Autocomplete.SetCommands([]ui.Command{{Name: "help"}})
		m.screen.Editor.SetText("/he")
		require.True(t, m.screen.Autocomplete.Sync(m.screen.Editor.Text()))
		m.handleKey(t.Context(), leanHelpKey(t, sequence))
		assert.False(t, m.screen.Autocomplete.Active)
		assert.Equal(t, "/he", m.screen.Editor.Text())
		assert.False(t, m.quitting)
		m.handleKey(t.Context(), leanHelpKey(t, "l"))
		assert.True(t, m.screen.Autocomplete.Active, "subsequent typing can reopen completion")
	}
}

func TestLeanHelpInterruptConditions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, sequence, draft string
		busy, runHandle       bool
		wantDraft             string
		quit, cancelled       bool
	}{
		{"clear", "\x03", "draft", false, false, "", false, false},
		{"quit", "\x03", "", false, false, "", true, false},
		{"EOF quit", "\x04", "", false, false, "", true, false},
		{"busy cancel", "\x03", "draft", true, true, "draft", false, true},
		{"busy escape", "\x1b", "draft", true, true, "draft", false, true},
		{"remaining handle clear", "\x1b", "draft", false, true, "", false, false},
		{"remaining handle quit", "\x1b", "", false, true, "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := bareModel(24)
			m.screen.Editor.SetText(tc.draft)
			m.busy = tc.busy
			cancelled := false
			if tc.runHandle {
				m.runCancel = func() { cancelled = true }
			}
			m.handleKey(t.Context(), leanHelpKey(t, tc.sequence))
			assert.Equal(t, tc.wantDraft, m.screen.Editor.Text())
			assert.Equal(t, tc.quit, m.quitting)
			assert.Equal(t, tc.cancelled, cancelled)
		})
	}
}

func TestLeanHelpConfirmationAliasesAndPrecedence(t *testing.T) {
	for _, tc := range []struct {
		keys []string
		want runtime.ResumeRequest
	}{
		{[]string{"y", "Y"}, runtime.ResumeApprove()},
		{[]string{"a", "A"}, runtime.ResumeApproveTool("shell")},
		{[]string{"b", "B"}, runtime.ResumeApproveBalanced()},
		{[]string{"s", "S"}, runtime.ResumeApproveAutonomous()},
		{[]string{"n", "N", "\x1b", "\x1b[27u"}, runtime.ResumeReject("rejected by user")},
	} {
		for _, sequence := range tc.keys {
			m, handle := sessionModel(t)
			m.busy = true
			m.screen.Editor.SetText("draft")
			m.screen.Confirm = &ui.ConfirmModel{Tool: "shell", SessionID: handle.id, RequestID: "help-confirm"}
			m.handleKey(t.Context(), leanHelpKey(t, sequence))
			require.Len(t, handle.responses, 1)
			assert.Equal(t, tc.want, handle.responses[0].Resume)
			assert.Equal(t, "help-confirm", handle.responses[0].InteractionID)
			assert.Nil(t, m.screen.Confirm)
			assert.Zero(t, handle.stops)
			assert.Equal(t, "draft", m.screen.Editor.Text())
		}
	}
	for _, sequence := range []string{"\r", "\n", "\x03", "\x04", "\t", "\x1b[1;3A", "\x1b[A", "\x1b[200~y\x1b[201~", "x"} {
		m := bareModel(24)
		m.busy = true
		m.screen.Editor.SetText("draft")
		confirm := &ui.ConfirmModel{Tool: "shell"}
		m.screen.Confirm = confirm
		m.handleKey(t.Context(), leanHelpKey(t, sequence))
		assert.Same(t, confirm, m.screen.Confirm)
		assert.Equal(t, "draft", m.screen.Editor.Text())
		assert.False(t, m.quitting)
		assert.False(t, m.cancelMarkerPending)
	}
}

func TestLeanHelpPreservesDynamicCommandList(t *testing.T) {
	t.Parallel()
	m := bareModel(24)
	m.screen.Autocomplete.SetCommands([]ui.Command{
		{Name: "help", Desc: "Show keyboard shortcuts"},
		{Name: "runtime-task", Desc: "Current agent task", Kind: ui.CmdAgent},
	})
	m.screen.Editor.SetText("/r")
	require.True(t, m.screen.Autocomplete.Sync(m.screen.Editor.Text()))
	m.commitHelp()
	selected, ok := m.screen.Autocomplete.Current()
	require.True(t, ok)
	assert.Equal(t, "runtime-task", selected.Name)
	assert.Equal(t, "/r", m.screen.Editor.Text())
	assert.Contains(t, leanHelpText(m, 120), "Type / for the available command list")
	assert.Contains(t, strings.Join(m.screen.Autocomplete.Render(80), "\n"), "/runtime-task")
}

func TestLeanHelpPendingRecallAndThinkingContext(t *testing.T) {
	m, handle := sessionModel(t)
	m.busy = true
	m.screen.Editor.SetText("replaced draft")
	m.pendingUsers = []ui.PendingUserMessage{
		{TurnID: "consumed", Display: "stay pending", Kind: ui.PendingUserSteer},
		{TurnID: "first", Display: "first display", Kind: ui.PendingUserSteer},
		{TurnID: "second", Display: "second display", Kind: ui.PendingUserFollowUp},
	}
	handle.cancelable = map[string]bool{"first": true, "second": true}
	m.handleKey(t.Context(), leanHelpKey(t, "\x1b[1;3A"))
	assert.Equal(t, "first display\nsecond display", m.screen.Editor.Text())
	require.Len(t, m.pendingUsers, 1)
	assert.Equal(t, "consumed", m.pendingUsers[0].TurnID)
	assert.Zero(t, handle.stops)
	assert.True(t, m.busy)

	m.handleKey(t.Context(), leanHelpKey(t, "\x1b[Z"))
	assert.Equal(t, "high", m.status.Thinking)
	handle.thinkingLevels = false
	m.status.Thinking = "unchanged"
	m.handleKey(t.Context(), leanHelpKey(t, "\x1b[9;2u"))
	assert.Equal(t, "unchanged", m.status.Thinking)
}

func TestLeanHelpBusySendAliases(t *testing.T) {
	for _, tc := range []struct {
		sequence string
		followUp bool
	}{
		{"\r", false}, {"\n", false}, {"\x1b\r", true}, {"\x1b[13;3u", true},
	} {
		m, handle := sessionModel(t)
		m.busy = true
		before := len(handle.submitted)
		m.screen.Editor.SetText("message")
		m.handleKey(t.Context(), leanHelpKey(t, tc.sequence))
		require.Len(t, m.pendingUsers, 1)
		if tc.followUp {
			assert.Len(t, handle.submitted, before+1)
			assert.Empty(t, handle.sent)
			assert.Equal(t, ui.PendingUserFollowUp, m.pendingUsers[0].Kind)
		} else {
			assert.Len(t, handle.submitted, before)
			require.Len(t, handle.sent, 1)
			assert.Equal(t, "message", handle.sent[0].Content)
			assert.Equal(t, ui.PendingUserSteer, m.pendingUsers[0].Kind)
		}
		assert.True(t, m.screen.Editor.IsEmpty())
	}
}

func TestLeanHelpRedrawPreservesConversation(t *testing.T) {
	t.Parallel()
	m := bareModel(24)
	m.commitHelp()
	m.screen.Editor.SetText("draft")
	before := leanHelpText(m, 80)
	m.handleKey(t.Context(), leanHelpKey(t, "\x0c"))
	assert.Equal(t, before, leanHelpText(m, 80))
	assert.Equal(t, "draft", m.screen.Editor.Text())
	assert.False(t, m.quitting)
}
