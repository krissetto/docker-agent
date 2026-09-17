package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func (m *appModel) routePaneCmd(id string, cmd tea.Cmd) tea.Cmd {
	generation, _ := m.supervisor.RouteGeneration(id)
	return paneOriginCommand(id, generation, cmd)
}

// No root state is read from the command goroutine. Generation is captured on
// the event loop and checked by handleRoutedMsg, including nested sequences.
func paneOriginCommand(id string, generation uint64, cmd tea.Cmd) tea.Cmd {
	return core.MapCommand(cmd, func(result tea.Msg) tea.Msg {
		switch result.(type) {
		case animation.TickMsg, messages.RoutedMsg:
			return result
		default:
			return messages.RoutedMsg{SessionID: id, RouteGeneration: generation, Inner: result}
		}
	})
}

// Only SendMsg is origin-stamped from editor commands. Completion, blink,
// attachment loading and framework messages retain their existing routing.
func (m *appModel) stampEditorSend(cmd tea.Cmd) tea.Cmd {
	if cmd == nil || m.supervisor == nil {
		return cmd
	}
	id := m.paneFocus()
	generation, _ := m.supervisor.RouteGeneration(id)
	return stampSendCommand(id, generation, cmd)
}

func stampSendCommand(id string, generation uint64, cmd tea.Cmd) tea.Cmd {
	return core.MapCommand(cmd, func(result tea.Msg) tea.Msg {
		if msg, ok := result.(messages.SendMsg); ok {
			return messages.RoutedMsg{SessionID: id, RouteGeneration: generation, Inner: msg}
		}
		return result
	})
}

// Stamp only the footer thinking action, preserving all unrelated command and
// framework results. Origin/generation are captured before the page Update.
func stampThinkingCycleCommand(origin string, generation uint64, cmd tea.Cmd) tea.Cmd {
	return core.MapCommand(cmd, func(result tea.Msg) tea.Msg {
		switch msg := result.(type) {
		case messages.ResumeSessionMsg:
			msg.RouteGeneration = generation
			return messages.RoutedMsg{SessionID: origin, RouteGeneration: generation, Inner: msg}
		case messages.CycleThinkingLevelMsg:
			msg.RouteGeneration = generation
			return messages.RoutedMsg{SessionID: origin, RouteGeneration: generation, Inner: msg}
		}
		return result
	})
}
