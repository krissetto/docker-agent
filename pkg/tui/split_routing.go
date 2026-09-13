package tui

import (
	"reflect"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// Pinned Bubble Tea v2.0.9 dependency seam: Sequence returns private
// sequenceMsg []Cmd, and execSequenceMsg recursively interprets command results
// before delivering leaves to Update. Transforming it in Update would lose
// nested ordering. Match its EXACT sampled type, not arbitrary reflected slices.
var (
	paneSequenceType   = reflect.TypeOf(tea.Sequence(func() tea.Msg { return nil }, func() tea.Msg { return nil })())
	paneCommandsType   = reflect.TypeFor[[]tea.Cmd]()
	paneClipboardTypes = map[reflect.Type]bool{
		reflect.TypeOf(tea.SetClipboard("")()):        true,
		reflect.TypeOf(tea.SetPrimaryClipboard("")()): true,
	}
)

func (m *appModel) routePaneCmd(id string, cmd tea.Cmd) tea.Cmd {
	generation, _ := m.supervisor.RouteGeneration(id)
	return paneOriginCommand(id, generation, cmd)
}

// No root state is read from the command goroutine. Generation is captured on
// the event loop and checked by handleRoutedMsg, including nested sequences.
func paneOriginCommand(id string, generation uint64, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		result := cmd()
		if result == nil {
			return nil
		}
		switch msg := result.(type) {
		case tea.BatchMsg:
			cmds := make([]tea.Cmd, len(msg))
			for i, child := range msg {
				cmds[i] = paneOriginCommand(id, generation, child)
			}
			if batch := tea.Batch(cmds...); batch != nil {
				return batch()
			}
			return nil
		case animation.TickMsg, messages.RoutedMsg:
			return result
		}
		typ := reflect.TypeOf(result)
		if typ == paneSequenceType && typ.ConvertibleTo(paneCommandsType) {
			children := reflect.ValueOf(result).Convert(paneCommandsType).Interface().([]tea.Cmd)
			cmds := make([]tea.Cmd, len(children))
			for i, child := range children {
				cmds[i] = paneOriginCommand(id, generation, child)
			}
			if sequence := tea.Sequence(cmds...); sequence != nil {
				return sequence()
			}
			return nil
		}
		// Transcript selection/copy emits OSC52 clipboard writes. Let the
		// framework consume those exact private result types, not the page.
		// Do not pass through arbitrary terminal, quit or focus controls.
		if paneClipboardTypes[typ] {
			return result
		}
		return messages.RoutedMsg{SessionID: id, RouteGeneration: generation, Inner: result}
	}
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
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		result := cmd()
		switch msg := result.(type) {
		case messages.SendMsg:
			return messages.RoutedMsg{SessionID: id, RouteGeneration: generation, Inner: msg}
		case tea.BatchMsg:
			cmds := make([]tea.Cmd, len(msg))
			for i, child := range msg {
				cmds[i] = stampSendCommand(id, generation, child)
			}
			if batch := tea.Batch(cmds...); batch != nil {
				return batch()
			}
			return nil
		}
		if reflect.TypeOf(result) == paneSequenceType {
			children := reflect.ValueOf(result).Convert(paneCommandsType).Interface().([]tea.Cmd)
			cmds := make([]tea.Cmd, len(children))
			for i, child := range children {
				cmds[i] = stampSendCommand(id, generation, child)
			}
			if sequence := tea.Sequence(cmds...); sequence != nil {
				return sequence()
			}
			return nil
		}
		return result
	}
}
