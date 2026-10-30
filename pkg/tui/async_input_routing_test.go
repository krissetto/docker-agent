package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
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
