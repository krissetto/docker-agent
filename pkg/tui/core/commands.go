package core

import (
	"reflect"

	tea "charm.land/bubbletea/v2"
)

// SequenceMsg is the application's owned ordered-command protocol. Producers
// that can cross a pane boundary use Sequence rather than a private Tea result.
// The root or a command mapper adapts it to Bubble Tea before execution.
type SequenceMsg []tea.Cmd

func Sequence(cmds ...tea.Cmd) tea.Cmd {
	var commands SequenceMsg
	for _, cmd := range cmds {
		if cmd != nil {
			commands = append(commands, cmd)
		}
	}
	if len(commands) == 0 {
		return nil
	}
	return CmdHandler(commands)
}

// ClipboardMsg explicitly authorizes an OSC52 write, not arbitrary framework
// controls emitted by an unfocused pane.
type ClipboardMsg struct {
	Text    string
	Primary bool
}

func SetClipboard(text string) tea.Cmd { return CmdHandler(ClipboardMsg{Text: text}) }
func SetPrimaryClipboard(text string) tea.Cmd {
	return CmdHandler(ClipboardMsg{Text: text, Primary: true})
}

// Bubble Tea v2.0.9 compatibility adapter. Third-party components and legacy
// producers still return its private sequenceMsg. Match ONLY that exact sampled
// type. This private dependency remains until those producers expose an owned
// command protocol; it is deliberately isolated here, not hidden as type safety.
var (
	teaSequenceType = reflect.TypeOf(tea.Sequence(func() tea.Msg { return nil }, func() tea.Msg { return nil })())
	commandsType    = reflect.TypeFor[[]tea.Cmd]()
)

// MapCommand maps application leaves without changing nested ordering or batch
// concurrency. It reads no model state in the command goroutine. Owned terminal
// effects are adapted explicitly; other framework controls are ordinary leaves.
func MapCommand(cmd tea.Cmd, leaf func(tea.Msg) tea.Msg) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		result := cmd()
		if result == nil {
			return nil
		}
		var sequence []tea.Cmd
		switch msg := result.(type) {
		case SequenceMsg:
			sequence = msg
		case tea.BatchMsg:
			commands := make([]tea.Cmd, len(msg))
			for i, child := range msg {
				commands[i] = MapCommand(child, leaf)
			}
			if batch := tea.Batch(commands...); batch != nil {
				return batch()
			}
			return nil
		case ClipboardMsg:
			if msg.Primary {
				return tea.SetPrimaryClipboard(msg.Text)()
			}
			return tea.SetClipboard(msg.Text)()
		default:
			if reflect.TypeOf(result) == teaSequenceType {
				sequence = reflect.ValueOf(result).Convert(commandsType).Interface().([]tea.Cmd)
			} else {
				if leaf != nil {
					return leaf(result)
				}
				return result
			}
		}
		commands := make([]tea.Cmd, len(sequence))
		for i, child := range sequence {
			commands[i] = MapCommand(child, leaf)
		}
		if ordered := tea.Sequence(commands...); ordered != nil {
			return ordered()
		}
		return nil
	}
}
