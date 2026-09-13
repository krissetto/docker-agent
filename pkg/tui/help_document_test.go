package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/components/completion"
	messagelist "github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/help"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func (m *mockChatPage) IsTitleEditing() bool        { return false }
func (m *mockEditor) HistoryNavigationActive() bool { return false }

func helpKeys(sections []help.Section, id string) []string {
	for _, section := range sections {
		for _, entry := range section.Entries {
			if entry.ID == id {
				return entry.Keys
			}
		}
	}
	return nil
}

func TestHelpDocumentEffectiveContextsAndAliases(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	root.focusedPanel = PanelEditor
	doc := root.helpDocument()
	require.Contains(t, helpKeys(doc.Current, "input.composer.DeleteWordBackward"), "ctrl+w")
	require.NotContains(t, helpKeys(doc.Current, "input.composer.CharacterBackward"), "ctrl+b", "disabled sidebar still consumes its binding")
	require.NotContains(t, helpKeys(doc.Current, "input.composer.DeleteCharacterBackward"), "ctrl+h")
	require.Contains(t, helpKeys(doc.Reference, "input.inline.DeleteCharacterBackward"), "ctrl+h", "reference retains other context aliases")
	require.Empty(t, helpKeys(doc.Current, "tab.close"))
	require.Empty(t, helpKeys(doc.Current, "tab.next"))
	require.Contains(t, helpKeys(doc.Current, "input.composer.LineNext"), "ctrl+n", "single-tab fallthrough")
	root.tabBar.SetTabs(layoutTabs(), 0)
	doc = root.helpDocument()
	require.Contains(t, helpKeys(doc.Current, "tab.next"), "ctrl+n")
	require.NotContains(t, helpKeys(doc.Current, "input.composer.LineNext"), "ctrl+n")
	root.focusedPanel = PanelContent
	doc = root.helpDocument()
	require.Equal(t, "Transcript", doc.Context)
	require.Equal(t, []string{"ctrl+w"}, helpKeys(doc.Current, "tab.close"))
	require.Equal(t, []string{"home", "g"}, helpKeys(doc.Current, "transcript.top"))
	require.Equal(t, []string{"alt+up"}, helpKeys(doc.Current, "queue.restore"))
	root.leanMode = true
	doc = root.helpDocument()
	require.Empty(t, helpKeys(doc.Current, "tab.new"))
	require.Empty(t, helpKeys(doc.Current, "sidebar.toggle"))
	require.Contains(t, helpKeys(doc.Current, "app.help"), "f1")
	require.Contains(t, doc.Context, "lean layout")
	root.leanMode = false
	root.focusedPanel = PanelEditor
	root.editor.EnterHistorySearch()
	doc = root.helpDocument()
	require.Equal(t, "History search", doc.Context)
	require.Equal(t, []string{"up", "ctrl+p"}, helpKeys(doc.Current, "history.previous-match"))
	require.Empty(t, helpKeys(doc.Current, "tab.next"))
	require.Empty(t, helpKeys(doc.Current, "composer.send"))
	require.Equal(t, []string{"ctrl+k"}, helpKeys(doc.Current, "app.commands"))
	root.editor.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	root.transcriber = &fakeTranscriber{running: true}
	doc = root.helpDocument()
	require.Equal(t, []string{"enter"}, helpKeys(doc.Current, "transcription.send"))
	require.Empty(t, helpKeys(doc.Current, "composer.send"))
	root.transcriber = &fakeTranscriber{}
	root.tour.Start()
	root.editor.SetValue("")
	doc = root.helpDocument()
	require.Equal(t, []string{"enter"}, helpKeys(doc.Current, "tour.next"))
	require.Empty(t, helpKeys(doc.Current, "composer.send"))
}

func TestHelpCompletionRoutesTabExclusivelyAndCursorToEditor(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	root.editor.SetValue("abcd")
	root.editor.Focus()
	root.updateCompletionsCmd(completion.OpenMsg{Items: []completion.Item{{Label: "candidate", Value: "candidate"}}})
	_, _ = root.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	_, _ = root.Update(tea.KeyPressMsg{Code: 'X', Text: "X"})
	require.Equal(t, "abcXd", root.editor.Value(), "popup must not swallow Left")
	_, _ = root.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	_, _ = root.Update(tea.KeyPressMsg{Code: 'Y', Text: "Y"})
	require.Equal(t, "abcXdY", root.editor.Value(), "Right reaches composer once")
	before := root.editor.Value()
	_, cmd := root.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	selected, ok := firstOfType[completion.SelectedMsg](collectMsgs(cmd))
	require.True(t, ok)
	require.False(t, selected.AutoSubmit)
	require.Equal(t, before, root.editor.Value(), "Tab is popup-only before selection delivery")
	root.updateCompletionsCmd(completion.OpenMsg{Items: []completion.Item{{Label: "candidate", Value: "candidate"}}})
	_, cmd = root.Update(tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl})
	require.False(t, hasMsg[dialog.OpenDialogMsg](collectMsgs(cmd)), "other popup/global precedence is unchanged")
}

// helpProgramProbe reads model state on the program event loop, not from the
// test goroutine. It also proves the snapshot was captured before Help push.
type helpProgramProbe struct{ reply chan helpProgramState }

type helpProgramState struct {
	doc                          help.Document
	draft                        string
	top                          dialog.Dialog
	layers                       int
	inline, title, closing       bool
	topView, content, savedFirst string
	sent                         []messages.SendMsg
	commits                      []messagelist.InlineEditCommittedMsg
	tabs                         int
	contentHeight                int
	sidebarView                  string
	sidebarCollapsed             bool
	focus                        FocusedPanel
}

type helpProgramModel struct {
	root               *appModel
	captureSubmissions bool
	sent               []messages.SendMsg
	commits            []messagelist.InlineEditCommittedMsg
}

func (m *helpProgramModel) Init() tea.Cmd  { return nil }
func (m *helpProgramModel) View() tea.View { return m.root.View() }
func (m *helpProgramModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.captureSubmissions {
		switch value := msg.(type) {
		case messages.SendMsg:
			m.sent = append(m.sent, value)
			return m, nil
		case messagelist.InlineEditCommittedMsg:
			m.commits = append(m.commits, value)
			return m, nil
		}
	}
	if query, ok := msg.(helpProgramProbe); ok {
		savedFirst := ""
		if items := m.root.application.Session().ItemsSnapshot(); len(items) > 0 && items[0].Message != nil {
			savedFirst = items[0].Message.Message.Content
		}
		topView := ""
		if top := m.root.dialogMgr.TopDialog(); top != nil {
			topView = top.View()
		}
		sidebarView := ""
		if page, ok := m.root.chatPage.(interface{ SidebarView() string }); ok {
			sidebarView = page.SidebarView()
		}
		query.reply <- helpProgramState{contentHeight: m.root.contentHeight, sidebarView: sidebarView, savedFirst: savedFirst, sent: append([]messages.SendMsg(nil), m.sent...), commits: append([]messagelist.InlineEditCommittedMsg(nil), m.commits...), topView: topView, content: m.root.View().Content, tabs: m.root.supervisor.Count(), doc: m.root.helpDocument(), draft: m.root.editor.Value(), top: m.root.dialogMgr.TopDialog(), layers: len(m.root.dialogMgr.GetLayers()), inline: m.root.chatPage.IsInlineEditing(), title: m.root.chatPage.IsTitleEditing(), closing: m.root.dialogMgr.Closing(), sidebarCollapsed: m.root.chatPage.GetSidebarSettings().Collapsed, focus: m.root.focusedPanel}
		return m, nil
	}
	_, cmd := m.root.Update(msg)
	return m, cmd
}

func helpProgramSnapshot(t *testing.T, program *tea.Program) helpProgramState {
	t.Helper()
	reply := make(chan helpProgramState, 1)
	program.Send(helpProgramProbe{reply: reply})
	select {
	case state := <-reply:
		return state
	case <-time.After(time.Second):
		t.Fatal("help program probe not answered")
		return helpProgramState{}
	}
}

func TestActualProgramHelpCompletionSnapshotAndSafeReturn(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	root.editor.SetValue("KEEP-DRAFT")
	root.editor.Focus()
	root.updateCompletionsCmd(completion.OpenMsg{Items: []completion.Item{{Label: "candidate", Value: "candidate"}}})
	expected := root.helpDocument()
	require.Equal(t, "Completion popup — composer input", expected.Context)
	program := startTestProgram(t, root, &helpProgramModel{root: root}, tea.WithOutput(&cacheProgramWriter{}))
	program.Send(tea.KeyPressMsg{Code: 'h', Mod: tea.ModCtrl})
	require.Eventually(t, func() bool { return dialog.IsHelpDialog(helpProgramSnapshot(t, program).top) }, time.Second, time.Millisecond)
	snapshot := helpProgramSnapshot(t, program)
	require.Equal(t, "KEEP-DRAFT", snapshot.draft)
	expectedDialog := dialog.NewHelpDialog(expected)
	expectedDialog.SetSize(120, 40)
	// Both models contain the source context, not a snapshot of Help itself.
	require.Contains(t, snapshot.topView, expected.Context)
	require.Contains(t, expectedDialog.View(), expected.Context)
	layers := snapshot.layers
	program.Send(tea.KeyPressMsg{Code: tea.KeyF1})
	require.Equal(t, layers, helpProgramSnapshot(t, program).layers, "F1 must not stack Help")
	program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Eventually(t, func() bool { s := helpProgramSnapshot(t, program); return s.top == nil && !s.closing }, time.Second, time.Millisecond)
	snapshot = helpProgramSnapshot(t, program)
	require.Equal(t, "KEEP-DRAFT", snapshot.draft)
	require.Equal(t, expected.Context, snapshot.doc.Context, "completion remains active underneath")
	program.Send(tea.KeyPressMsg{Code: tea.KeyLeft})
	program.Send(tea.KeyPressMsg{Code: '!', Text: "!"})
	require.Equal(t, "KEEP-DRAF!T", helpProgramSnapshot(t, program).draft)
}

func TestActualProgramModalHelpPreservesInputAndSelectedAction(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	root.editor.SetValue("COMPOSER-UNCHANGED")
	saved := make(chan string, 1)
	underlying := dialog.NewPendingMessageEditDialog("profile", "pending-ID", "modal draft", 1, func(value string) tea.Cmd { return func() tea.Msg { saved <- value; return nil } })
	program := startTestProgram(t, root, &helpProgramModel{root: root}, tea.WithOutput(&cacheProgramWriter{}))
	program.Send(dialog.OpenDialogMsg{Model: underlying})
	require.Eventually(t, func() bool { return helpProgramSnapshot(t, program).top == underlying }, time.Second, time.Millisecond)
	program.Send(tea.KeyPressMsg{Code: 'h', Mod: tea.ModCtrl})
	require.Same(t, underlying, helpProgramSnapshot(t, program).top, "modal Ctrl+h remains backspace")
	before := helpProgramSnapshot(t, program)
	program.Send(tea.KeyPressMsg{Code: tea.KeyF1})
	require.Eventually(t, func() bool { return dialog.IsHelpDialog(helpProgramSnapshot(t, program).top) }, time.Second, time.Millisecond)
	program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Eventually(t, func() bool { s := helpProgramSnapshot(t, program); return s.top == underlying && !s.closing }, time.Second, time.Millisecond)
	require.Equal(t, before.doc.Context, helpProgramSnapshot(t, program).doc.Context)
	program.Send(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl})
	select {
	case value := <-saved:
		require.Equal(t, "modal draf", value)
	case <-time.After(time.Second):
		t.Fatal("modal save action did not run")
	}
	require.Equal(t, "COMPOSER-UNCHANGED", helpProgramSnapshot(t, program).draft)
}

func TestActualProgramSidebarRemapComposerAndContent(t *testing.T) {
	setupSettingsConfigTest(t)
	require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
		cfg.Settings = &userconfig.Settings{Keybindings: []userconfig.Keybinding{{Action: "toggle_sidebar", Keys: []string{"f6"}}, {Action: "help", Keys: []string{"f7"}}}}
		return nil
	}))
	core.ResetKeys()
	t.Cleanup(core.ResetKeys)
	root, _, _ := wallClockRoot(t, 120, 40)
	root.hideSidebar = false
	root.initSessionComponents("profile", root.application, root.application.Session())
	root.resizeAll()
	root.editor.SetValue("abcd")
	root.editor.Focus()
	program := startTestProgram(t, root, &helpProgramModel{root: root}, tea.WithOutput(&cacheProgramWriter{}))
	before := helpProgramSnapshot(t, program)
	program.Send(tea.KeyPressMsg{Code: tea.KeyF6})
	require.Eventually(t, func() bool { return helpProgramSnapshot(t, program).sidebarCollapsed != before.sidebarCollapsed }, time.Second, time.Millisecond)
	program.Send(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
	program.Send(tea.KeyPressMsg{Code: 'X', Text: "X"})
	state := helpProgramSnapshot(t, program)
	require.Equal(t, "abcXd", state.draft, "obsolete Ctrl+b returns to character movement")
	require.NotEqual(t, before.sidebarCollapsed, state.sidebarCollapsed)
	program.Send(messages.RequestFocusMsg{Target: messages.PanelMessages})
	program.Send(tea.KeyPressMsg{Code: tea.KeyF6})
	require.Eventually(t, func() bool { return helpProgramSnapshot(t, program).sidebarCollapsed == before.sidebarCollapsed }, time.Second, time.Millisecond)
	program.Send(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
	require.Equal(t, before.sidebarCollapsed, helpProgramSnapshot(t, program).sidebarCollapsed, "old alias cannot toggle from content")
	program.Send(tea.KeyPressMsg{Code: tea.KeyF7})
	require.Eventually(t, func() bool { return dialog.IsHelpDialog(helpProgramSnapshot(t, program).top) }, time.Second, time.Millisecond)
}

func TestHelpContextBarCanonicalSpaceEncoding(t *testing.T) {
	for _, text := range []string{"", " "} {
		t.Run(strings.ReplaceAll("text-"+text, " ", "space"), func(t *testing.T) {
			root, _, _ := wallClockRoot(t, 120, 40)
			root.editor.SetValue("draft")
			file := filepath.Join(t.TempDir(), "attachment.txt")
			require.NoError(t, os.WriteFile(file, []byte("attachment"), 0o600))
			require.NoError(t, root.editor.AttachFile(file))
			draft := root.editor.Value()
			root.editor.SetContextBarFocused(true)
			key := tea.KeyPressMsg{Code: ' ', Text: text}
			require.Equal(t, "space", key.String())
			before := root.editor.BannerView(120)
			_, _ = root.Update(key)
			require.True(t, root.editor.IsContextBarFocused(), "Space toggles without leaving bar")
			require.Equal(t, draft, root.editor.Value())
			require.NotEqual(t, before, root.editor.BannerView(120), "Space actually expands/collapses bar")
		})
	}
}

func TestHelpDocumentDoesNotRunCommandActions(t *testing.T) {
	root, _ := newTestModel(t)
	executed := false
	root.buildCommandCategories = func(context.Context, tea.Model) []commands.Category {
		return []commands.Category{{Name: "Runtime", Commands: []commands.Item{{ID: "dynamic", SlashCommand: "/dynamic", Description: "Dynamic action", Execute: func(string) tea.Cmd { executed = true; return nil }}}}}
	}
	require.Equal(t, []string{"/dynamic"}, helpKeys(root.helpDocument().Reference, "command.dynamic"))
	require.False(t, executed)
}

func TestActualProgramLocalEditsKeepDangerousTabKeysAndPasteLocal(t *testing.T) {
	for _, mode := range []string{"inline", "title"} {
		t.Run(mode, func(t *testing.T) {
			root, _, _ := wallClockRoot(t, 120, 40)
			root.hideSidebar = false
			root.application.Session().SetTitle("TITLE-EDIT-TARGET")
			root.application.Session().AddMessage(session.UserMessage("ORIGINAL USER TEXT"))
			root.initSessionComponents("profile", root.application, root.application.Session())
			root.chatPage.Init()
			root.resizeAll()
			root.editor.SetValue("COMPOSER-KEEP")
			addCloseTabTestSession(t, root, "other-tab", newCloseTabRuntime("root", false), nil)
			root.tabBar.SetTabs([]messages.TabInfo{{SessionID: "profile", Title: "TAB-ONE"}, {SessionID: "other-tab", Title: "TAB-TWO"}}, 0)
			program := startTestProgram(t, root, &helpProgramModel{root: root}, tea.WithOutput(&cacheProgramWriter{}))
			if mode == "inline" {
				program.Send(messages.EditUserMessageMsg{MsgIndex: 0, SessionPosition: 0, OriginalContent: "ORIGINAL USER TEXT"})
				require.Eventually(t, func() bool { return helpProgramSnapshot(t, program).inline }, time.Second, time.Millisecond)
			} else {
				const title = "TITLE-EDIT-TARGET"
				var frame helpProgramState
				require.Eventually(t, func() bool {
					frame = helpProgramSnapshot(t, program)
					return strings.Count(ansi.Strip(frame.sidebarView), title) == 1 && strings.Contains(ansi.Strip(frame.content), title)
				}, time.Second, time.Millisecond, "rendered sidebar title must be present")
				require.Contains(t, ansi.Strip(frame.sidebarView), title, "target belongs to sidebar, not tab strip")
				x, y, matches := -1, -1, 0
				for row, line := range strings.Split(ansi.Strip(frame.content), "\n") {
					if row >= frame.contentHeight {
						break
					}
					if before, _, ok := strings.Cut(line, title); ok {
						// Aim inside the title, away from its leading star hit zone.
						x, y = ansi.StringWidth(before)+ansi.StringWidth(title)/2, row
						matches++
					}
				}
				require.Equal(t, 1, matches, "exactly one target in the measured content area")
				require.GreaterOrEqual(t, x, 0)
				require.Less(t, y, frame.contentHeight)
				program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
				require.Eventually(t, func() bool {
					state := helpProgramSnapshot(t, program)
					return !state.title && strings.Contains(ansi.Strip(state.content), "Double-click to rename the session")
				}, time.Second, time.Millisecond, "first real mouse click proves the canonical title route")
				program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
				require.Eventually(t, func() bool { return helpProgramSnapshot(t, program).title }, time.Second, time.Millisecond)
			}
			before := helpProgramSnapshot(t, program)
			require.Contains(t, helpKeys(before.doc.Current, "input."+mode+".DeleteWordBackward"), "ctrl+w")
			for _, code := range []rune{'w', 'n', 'p'} {
				program.Send(tea.KeyPressMsg{Code: code, Mod: tea.ModCtrl})
			}
			program.Send(tea.PasteMsg{Content: "LOCAL-PASTE"})
			state := helpProgramSnapshot(t, program)
			require.Equal(t, 2, state.tabs, "Ctrl+w cannot close edited tab")
			require.Equal(t, "COMPOSER-KEEP", state.draft, "title/inline paste never touches composer")
			require.Contains(t, ansi.Strip(state.content), "LOCAL-PASTE")
			require.Nil(t, state.top, "no exit or tab-close dialog")
			require.True(t, state.inline || state.title, "Ctrl+n/p do not switch away")
			program.Send(tea.KeyPressMsg{Code: tea.KeyF1})
			require.Eventually(t, func() bool { return dialog.IsHelpDialog(helpProgramSnapshot(t, program).top) }, time.Second, time.Millisecond)
			program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
			require.Eventually(t, func() bool { s := helpProgramSnapshot(t, program); return s.top == nil && !s.closing }, time.Second, time.Millisecond)
			require.Contains(t, ansi.Strip(helpProgramSnapshot(t, program).content), "LOCAL-PASTE", "Help cancellation keeps local draft")
			program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
			require.Eventually(t, func() bool { s := helpProgramSnapshot(t, program); return !s.inline && !s.title }, time.Second, time.Millisecond)
			require.Equal(t, "COMPOSER-KEEP", helpProgramSnapshot(t, program).draft)
			require.Equal(t, "ORIGINAL USER TEXT", helpProgramSnapshot(t, program).savedFirst, "local Escape never commits edit")
		})
	}
}

func TestActualProgramEnhancedNewlineAndConfiguredShiftEnterOwnership(t *testing.T) {
	for _, owner := range []string{"newline-fallback", "editor_send", "commands"} {
		t.Run(owner, func(t *testing.T) {
			setupSettingsConfigTest(t)
			if owner != "newline-fallback" {
				require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
					cfg.Settings = &userconfig.Settings{Keybindings: []userconfig.Keybinding{{Action: owner, Keys: []string{"shift+enter"}}}}
					return nil
				}))
			}
			core.ResetKeys()
			t.Cleanup(core.ResetKeys)
			root, _, _ := wallClockRoot(t, 120, 40)
			root.editor.SetValue("first")
			root.editor.Focus()
			root.application.Session().AddMessage(session.UserMessage("INLINE ORIGINAL"))
			root.chatPage.Init()
			program := startTestProgram(t, root, &helpProgramModel{root: root, captureSubmissions: true}, tea.WithOutput(&cacheProgramWriter{}))
			program.Send(tea.KeyboardEnhancementsMsg{Flags: 1})
			doc := helpProgramSnapshot(t, program).doc
			if owner == "newline-fallback" {
				require.Contains(t, helpKeys(doc.Current, "composer.newline"), "shift+enter")
			} else {
				require.NotContains(t, helpKeys(doc.Current, "composer.newline"), "shift+enter")
			}
			program.Send(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
			require.Equal(t, "first\n", helpProgramSnapshot(t, program).draft, "configured newline remains live with enhanced terminal")
			program.Send(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
			switch owner {
			case "newline-fallback":
				require.Equal(t, "first\n\n", helpProgramSnapshot(t, program).draft)
			case "editor_send":
				require.Eventually(t, func() bool { return len(helpProgramSnapshot(t, program).sent) == 1 }, time.Second, time.Millisecond)
				require.Equal(t, "first\n", helpProgramSnapshot(t, program).sent[0].Content)
				require.Empty(t, helpProgramSnapshot(t, program).draft)
				program.Send(messages.EditUserMessageMsg{MsgIndex: 0, SessionPosition: 0, OriginalContent: "INLINE ORIGINAL"})
				require.Eventually(t, func() bool { return helpProgramSnapshot(t, program).inline }, time.Second, time.Millisecond)
				require.NotContains(t, helpKeys(helpProgramSnapshot(t, program).doc.Current, "inline.newline"), "shift+enter")
				program.Send(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
				require.Eventually(t, func() bool { return len(helpProgramSnapshot(t, program).commits) == 1 }, time.Second, time.Millisecond)
				require.False(t, helpProgramSnapshot(t, program).inline, "custom send saves rather than inserting newline inline")
			case "commands":
				require.Eventually(t, func() bool { return helpProgramSnapshot(t, program).top != nil }, time.Second, time.Millisecond)
				require.Equal(t, "first\n", helpProgramSnapshot(t, program).draft, "global Shift+Enter is not injected as newline")
			}
		})
	}
}

func TestActualProgramToolApprovalAndReasonHelpPreserveDecisionCorrelation(t *testing.T) {
	for _, reason := range []bool{false, true} {
		name := "approval-default-no"
		if reason {
			name = "reason-selected"
		}
		t.Run(name, func(t *testing.T) {
			program, handle := startToolConfirmationProgram(t)
			if reason {
				program.Send(tea.KeyPressMsg{Code: 'R', Text: "R"})
				require.Eventually(t, func() bool {
					return strings.Contains(ansi.Strip(sidebarProgramSnapshot(t, program).content), "Why reject")
				}, time.Second, time.Millisecond)
				program.Send(tea.KeyPressMsg{Code: '1', Text: "1"})
			}
			program.Send(tea.KeyPressMsg{Code: tea.KeyF1})
			require.Eventually(t, func() bool {
				return strings.Contains(ansi.Strip(sidebarProgramSnapshot(t, program).content), "Help ·")
			}, time.Second, time.Millisecond)
			program.Send(tea.KeyPressMsg{Code: tea.KeyF1})
			program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
			require.Eventually(t, func() bool {
				s := sidebarProgramSnapshot(t, program)
				return s.open && !s.closing && !strings.Contains(ansi.Strip(s.content), "Help ·")
			}, time.Second, time.Millisecond)
			select {
			case response := <-handle.responses:
				t.Fatalf("Help roundtrip answered tool: %+v", response)
			default:
			}
			if reason {
				require.Contains(t, ansi.Strip(sidebarProgramSnapshot(t, program).content), "Why reject")
			}
			program.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
			select {
			case response := <-handle.responses:
				require.Equal(t, "confirmation-request-full-ID", response.InteractionID)
				require.Equal(t, runtime.InteractionConfirmation, response.Kind)
				text := ""
				if reason {
					text = "The arguments provided are incorrect or invalid."
				}
				require.Equal(t, runtime.ResumeReject(text), response.Resume, "Help preserves default No or exact selected reason")
			case <-time.After(time.Second):
				t.Fatal("preserved tool decision was not dispatched")
			}
			require.Eventually(t, func() bool { s := sidebarProgramSnapshot(t, program); return !s.open && !s.closing }, time.Second, time.Millisecond)
			select {
			case response := <-handle.responses:
				t.Fatalf("duplicate correlated tool response: %+v", response)
			default:
			}
		})
	}
}

func TestActualProgramHistoryHelpReturnsToSearchWithoutSubmitting(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	root.editor.SetValue("HISTORY-ORIGINAL")
	root.editor.Focus()
	program := startTestProgram(t, root, &helpProgramModel{root: root, captureSubmissions: true}, tea.WithOutput(&cacheProgramWriter{}))
	program.Send(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	require.Equal(t, "History search", helpProgramSnapshot(t, program).doc.Context)
	program.Send(tea.KeyPressMsg{Code: tea.KeyF1})
	require.Eventually(t, func() bool { return dialog.IsHelpDialog(helpProgramSnapshot(t, program).top) }, time.Second, time.Millisecond)
	program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Eventually(t, func() bool { s := helpProgramSnapshot(t, program); return s.top == nil && !s.closing }, time.Second, time.Millisecond)
	require.Equal(t, "History search", helpProgramSnapshot(t, program).doc.Context)
	program.Send(tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl})
	program.Send(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	require.Equal(t, "History search", helpProgramSnapshot(t, program).doc.Context)
	program.Send(tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
	state := helpProgramSnapshot(t, program)
	require.Equal(t, "Composer", state.doc.Context)
	require.Empty(t, state.sent, "accepting or leaving search never submits")
	require.Equal(t, "HISTORY-ORIGINAL", state.draft)
}

func TestHelpBackgroundDialogPreservesTabNavigation(t *testing.T) {
	root, _ := newTestModel(t)
	root.tabBar.SetTabs(layoutTabs(), 0)
	underlying := &stubDialog{id: "background"}
	root.Update(dialog.OpenDialogMsg{Model: underlying, OriginatingEvent: "background"})
	doc := root.helpDocument()
	require.Equal(t, []string{"ctrl+n"}, helpKeys(doc.Current, "tab.next"))
	_, cmd := root.Update(tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl})
	require.True(t, hasMsg[messages.SwitchTabMsg](collectMsgs(cmd)))
	_, cmd = root.Update(tea.KeyPressMsg{Code: tea.KeyF1})
	open, ok := firstOfType[dialog.OpenDialogMsg](collectMsgs(cmd))
	require.True(t, ok)
	require.True(t, dialog.IsHelpDialog(open.Model))
	require.Same(t, underlying, root.dialogMgr.TopDialog(), "snapshot occurs before command pushes Help")
}
