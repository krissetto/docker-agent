package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

type (
	hiddenProbeMsg       struct{}
	hiddenLocalResultMsg struct{}
)

// hiddenEffectsPage splits each update into a Local result and a Visible notification.
type hiddenEffectsPage struct{ chat.Page }

func (p *hiddenEffectsPage) UpdateEffects(tea.Msg) (layout.Model, chat.Effects) {
	return p, chat.Effects{
		Local:   func() tea.Msg { return hiddenLocalResultMsg{} },
		Visible: func() tea.Msg { return notification.ShowMsg{Text: "visible chrome"} },
	}
}

// A hidden tab runs its Local work and routes the result back to itself, while
// Visible chrome is never dispatched.
func TestHiddenRoutedUpdateDispatchesLocalWorkOnly(t *testing.T) {
	root := splitTestRoot(t)
	root.chatPages["second"] = &hiddenEffectsPage{Page: root.chatPages["second"]}

	_, cmd := root.Update(messages.RoutedMsg{SessionID: "second", Inner: hiddenProbeMsg{}})
	msgs := collectMsgs(cmd)

	var local, visible bool
	for _, msg := range msgs {
		routed, ok := msg.(messages.RoutedMsg)
		if ok {
			assert.Equal(t, "second", routed.SessionID, "hidden page results route back to their tab")
			if _, ok := routed.Inner.(hiddenLocalResultMsg); ok {
				local = true
			}
		}
		if _, ok := msg.(notification.ShowMsg); ok {
			visible = true
		}
	}
	assert.True(t, local, "Local work of a hidden page still runs")
	assert.False(t, visible, "Visible chrome of a hidden page is dropped")
	require.NotEmpty(t, msgs)
}

type globalProbeMsg struct{}

// globalProbePage emits one page-owned application effect (exit after the first response).
type globalProbePage struct {
	chat.Page

	tabID       string
	application *app.App
}

func (p *globalProbePage) UpdateEffects(tea.Msg) (layout.Model, chat.Effects) {
	return p, chat.Effects{Global: func() tea.Msg {
		return chat.GlobalMsg{TabID: p.tabID, Origin: p, Application: p.application, Inner: messages.ExitAfterFirstResponseMsg{}}
	}}
}

// A hidden tab's global exit still applies; a retired origin's does not.
func TestHiddenPageGlobalEffectAppliesOnlyToLiveOrigin(t *testing.T) {
	for _, change := range []string{"none", "switch", "close", "replace-app"} {
		t.Run(change, func(t *testing.T) {
			root := splitTestRoot(t)
			const id = "second"
			runner := root.supervisor.GetRunner(id)
			require.NotNil(t, runner)
			probe := &globalProbePage{Page: root.chatPages[id], tabID: id, application: runner.App}
			root.chatPages[id] = probe

			_, cmd := root.Update(messages.RoutedMsg{SessionID: id, Inner: globalProbeMsg{}})
			var global chat.GlobalMsg
			for _, msg := range collectMsgs(cmd) {
				if effect, ok := msg.(chat.GlobalMsg); ok {
					global = effect
				}
			}
			require.NotNil(t, global.Origin, "a hidden page's global effect reaches the host unwrapped")

			switch change {
			case "switch":
				_, _ = root.handleSwitchTab("third")
			case "close":
				_, _ = root.handleCloseTab(id)
			case "replace-app":
				a := app.New(t.Context(), nil, session.New(), runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
				root.supervisor.ReplaceRunnerApp(t.Context(), id, supervisor.SpawnedSession{App: a}, "")
			}
			_, next := root.Update(global)
			if change == "none" || change == "switch" {
				assert.True(t, hasMsg[tea.QuitMsg](collectMsgs(next)), "a live origin still exits after its first response")
			} else {
				assert.Nil(t, next, "a retired origin must not terminate its replacement")
			}
		})
	}
}
