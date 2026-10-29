package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	chatmessage "github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/userconfig"
)

type compactSettingPage struct {
	chat.Page

	enabled bool
}

func (p *compactSettingPage) SetCompactWorkLog(enabled bool) tea.Cmd {
	p.enabled = enabled
	if owner, ok := p.Page.(interface{ SetCompactWorkLog(enabled bool) tea.Cmd }); ok {
		return owner.SetCompactWorkLog(enabled)
	}
	return nil
}

func TestCompactWorkLogSettingsPreviewCancelSaveAndRestart(t *testing.T) {
	setupSettingsConfigTest(t)
	root, _, _ := frozenClockRoot(t, 120, 50)
	require.True(t, root.compactWorkLog)
	page := &compactSettingPage{Page: root.chatPage, enabled: true}
	root.chatPage, root.chatPages[root.paneFocus()] = page, page
	d := settingsFromRoot(t, root)
	settingsPreview(t, root, d, "Compact work log")
	require.False(t, root.compactWorkLog)
	require.False(t, page.enabled)
	require.True(t, userconfig.Get().GetCompactWorkLog(), "preview is not persisted")
	settingsCancel(t, root, d)
	require.True(t, root.compactWorkLog)
	require.True(t, page.enabled)

	d = settingsFromRoot(t, root)
	settingsPreview(t, root, d, "Compact work log")
	settingsSave(t, root, d)
	require.False(t, userconfig.Get().GetCompactWorkLog())
	restarted, _, _ := frozenClockRoot(t, 120, 50)
	require.False(t, restarted.compactWorkLog)
}

func TestCompactWorkLogPreferenceDefaultsAndConcurrentMerge(t *testing.T) {
	setupSettingsConfigTest(t)
	original := preferencesFromConfig(userconfig.Get())
	require.True(t, original.CompactWorkLog)
	draft := original
	draft.ShowBanner = !draft.ShowBanner
	require.NoError(t, userconfig.Update(func(config *userconfig.Config) error {
		disabled := false
		config.Settings = &userconfig.Settings{CompactWorkLog: &disabled}
		return nil
	}))
	require.NoError(t, saveChangedPreferences(original, draft))
	require.False(t, userconfig.Get().GetCompactWorkLog(), "unrelated settings draft cannot undo the opt-out")

	current := preferencesFromConfig(userconfig.Get())
	current.CompactWorkLog = true
	require.NoError(t, savePreferences(current))
	config, err := userconfig.Load()
	require.NoError(t, err)
	require.Nil(t, config.Settings.CompactWorkLog, "enabled default does not need an explicit config entry")
	require.True(t, config.GetSettings().GetCompactWorkLog())
}

func TestCompactWorkLogPreviewReprojectsAllPagesAndNewPages(t *testing.T) {
	setupSettingsConfigTest(t)
	root := splitTestRoot(t)
	for _, id := range []string{"profile", "second", "third"} {
		sess := root.supervisor.GetRunner(id).App.Session()
		sess.AddMessage(&session.Message{Message: chatmessage.Message{Role: chatmessage.MessageRoleUser, Content: "Question"}})
		sess.AddMessage(&session.Message{AgentName: "root", Message: chatmessage.Message{
			Role: chatmessage.MessageRoleAssistant, ReasoningContent: "Intermediate detail", Content: "Final response",
		}})
		root.chatPages[id].Init()
		root.chatPages[id].SetSize(120, 40)
	}
	assertLayout := func(compact bool) {
		t.Helper()
		for id, page := range root.chatPages {
			view := page.View()
			require.Contains(t, view, "Final response", id)
			if compact {
				require.Contains(t, view, "Work", id)
				require.NotContains(t, view, "Intermediate detail", id)
			} else {
				require.NotContains(t, view, "Work", id)
				require.Contains(t, view, "Intermediate detail", id)
			}
		}
	}
	assertLayout(true)
	d := settingsFromRoot(t, root)
	settingsPreview(t, root, d, "Compact work log")
	assertLayout(false)

	sess := root.application.Session().Clone()
	sess.ID = "opened-during-preview"
	a := app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	root.createSessionComponents(sess.ID, a, sess)
	root.chatPages[sess.ID].Init()
	root.chatPages[sess.ID].SetSize(120, 40)
	assertLayout(false)
	settingsCancel(t, root, d)
	assertLayout(true)
	require.Equal(t, 2, sess.ItemCount(), "presentation changes preserve the source history")
}

func TestVisibleRepliesSettingsPreviewCancelSaveRestartAndNewPage(t *testing.T) {
	setupSettingsConfigTest(t)
	root := splitTestRoot(t)
	for _, id := range []string{"profile", "second", "third"} {
		sess := root.supervisor.GetRunner(id).App.Session()
		sess.AddMessage(session.UserMessage("Question"))
		for i := range 4 {
			sess.AddMessage(session.NewAgentMessage("root", &chatmessage.Message{Role: chatmessage.MessageRoleAssistant, Content: "retained answer " + string(rune('A'+i))}))
		}
		root.chatPages[id].Init()
		root.chatPages[id].SetSize(120, 40)
	}
	assertPolicy := func(policy string, retained int) {
		t.Helper()
		for id, page := range root.chatPages {
			view := ansi.Strip(page.View())
			for i := range 4 {
				marker := "retained answer " + string(rune('A'+i))
				if i >= 4-retained {
					require.Contains(t, view, marker, id)
				} else {
					require.NotContains(t, view, marker, id)
				}
			}
		}
		require.Equal(t, messages.VisibleReplies(policy), root.visibleReplies)
	}
	assertPolicy("3", 3)
	original := preferencesFromConfig(userconfig.Get())
	draft := original
	draft.VisibleReplies = messages.VisibleRepliesFive
	root.previewSettings(original, draft)
	assertPolicy("5", 4)
	require.Equal(t, "3", userconfig.Get().GetVisibleReplies(), "preview does not persist")
	newSession := session.New(session.WithID("opened-during-reply-preview"))
	newSession.AddMessage(session.UserMessage("Question"))
	for i := range 4 {
		newSession.AddMessage(session.NewAgentMessage("root", &chatmessage.Message{Role: chatmessage.MessageRoleAssistant, Content: "retained answer " + string(rune('A'+i))}))
	}
	a := app.New(t.Context(), nil, newSession, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	root.createSessionComponents(newSession.ID, a, newSession)
	root.chatPages[newSession.ID].Init()
	root.chatPages[newSession.ID].SetSize(120, 40)
	assertPolicy("5", 4)
	root.previewSettings(draft, original)
	assertPolicy("3", 3)

	draft = original
	draft.VisibleReplies = messages.VisibleRepliesFive
	root.previewSettings(original, draft)
	require.NoError(t, savePreferences(draft))
	require.Equal(t, "5", userconfig.Get().GetVisibleReplies())
	assertPolicy("5", 4)
	restarted, _, _ := frozenClockRoot(t, 120, 50)
	require.Equal(t, messages.VisibleRepliesFive, restarted.visibleReplies)
}
